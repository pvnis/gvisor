// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gpusched

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gvisor.dev/gvisor/pkg/log"
)

// An Enforcer applies the scheduler's decisions to the GPU through a trusted,
// privileged host control. Unlike the compute gate in the Sentry -- which
// revokes submission mappings and so cannot touch a workload that submits by
// ringing a doorbell (cuBLAS, CUDA-graph replay) -- this drives the GPU's own
// runlist: it detaches a sandbox's channel groups from the hardware runlist,
// re-attaches them, and sets their per-TSG timeslice, each committed with a
// runlist restart so the GSP acts on it immediately. That is enough to preempt
// and rebalance a *running* doorbell workload, which the gate cannot.
//
// The control is privileged (it originates NVA06F_CTRL_CMD_RESTART_RUNLIST and
// friends at kernel level), so it lives here in the host-side scheduler, never
// in a sandbox -- the same reason the scheduler itself runs outside every
// sandbox.
type Enforcer interface {
	// SetTimeslice sets the runlist timeslice for every channel group owned by
	// the process, in microseconds. Shares between co-resident sandboxes are
	// divided in proportion to their timeslices.
	SetTimeslice(pid int, us uint64) error
	// Detach removes the process's channel groups from the hardware runlist,
	// preempting whatever it is running. Its share goes to the others.
	Detach(pid int) error
	// Attach re-inserts them.
	Attach(pid int) error
}

// ProcfsEnforcer drives the ghost runlist control at /proc/driver/nvidia/gpusched.
type ProcfsEnforcer struct {
	// Path is the control file; DefaultRunlistControlPath if empty.
	Path string
}

// DefaultRunlistControlPath is where the ghost-instrumented driver exposes the
// runlist control.
const DefaultRunlistControlPath = "/proc/driver/nvidia/gpusched"

func (e *ProcfsEnforcer) write(cmd string) error {
	p := e.Path
	if p == "" {
		p = DefaultRunlistControlPath
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(cmd)
	return err
}

// SetTimeslice implements Enforcer.
func (e *ProcfsEnforcer) SetTimeslice(pid int, us uint64) error {
	return e.write(fmt.Sprintf("ts %d %d", pid, us))
}

// Detach implements Enforcer.
func (e *ProcfsEnforcer) Detach(pid int) error { return e.write(fmt.Sprintf("detach %d", pid)) }

// Attach implements Enforcer.
func (e *ProcfsEnforcer) Attach(pid int) error { return e.write(fmt.Sprintf("attach %d", pid)) }

// An ActivityPoller reports, per host pid, whether the sandbox is currently
// submitting work to the GPU. It is the trusted alternative to nvidia-smi: the
// signal is read from each channel's GP_PUT in the driver, so it is doorbell-
// aware (it sees cuBLAS submission that fault counts miss) and cannot be forged
// by the sandbox (unlike an in-container kernel counter).
type ActivityPoller interface {
	PollActive() (map[int]bool, error)
}

// PollActive refreshes the driver's activity snapshot and reads it back. It
// writes "poll" (which makes the driver re-read every tenant's GP_PUT) and then
// reads the per-pid "active" lines. The snapshot it returns is from the poll
// issued on the *previous* call, since the driver refreshes asynchronously --
// one tick of lag, which the scheduler's idle hysteresis already tolerates.
func (e *ProcfsEnforcer) PollActive() (map[int]bool, error) {
	p := e.Path
	if p == "" {
		p = DefaultRunlistControlPath
	}
	// Read the current snapshot first, then ask for a refresh for next time.
	out := map[int]bool{}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// line: "pid <p> active <0|1>"
		fields := strings.Fields(sc.Text())
		if len(fields) != 4 || fields[0] != "pid" || fields[2] != "active" {
			continue
		}
		pid, err1 := strconv.Atoi(fields[1])
		act, err2 := strconv.Atoi(fields[3])
		if err1 == nil && err2 == nil {
			out[pid] = act != 0
		}
	}
	f.Close()
	if err := e.write("poll"); err != nil {
		return out, err
	}
	return out, nil
}

// Timeslice bounds. The ratio between sandboxes is what sets their shares; the
// absolute values only set how often the GPU context-switches between them --
// smaller means finer division at the cost of more switching. The minimum-
// weight active sandbox is given minTimesliceUs and the rest scale up from it,
// capped at maxTimesliceUs so a large weight ratio cannot make one slice huge.
const (
	minTimesliceUs = 1000
	maxTimesliceUs = 16000
)

// EnforceClient is one sandbox's state for a scheduling decision.
type EnforceClient struct {
	PID    int
	Weight uint64
	Idle   bool
}

// enforceCmd is one command to issue to the driver.
type enforceCmd struct {
	op  string // "attach", "detach", "ts"
	pid int
	us  uint64
}

// enforcePlan computes the driver commands for one tick from the current client
// states and the state applied on the previous tick, and returns the commands
// plus the new applied state. It is pure so it can be tested without a GPU.
//
// Every sandbox is held to a runlist timeslice: active ones in proportion to
// their weight, idle ones to the minimum. It does *not* detach idle sandboxes.
// It does not need to -- the GSP already skips an idle (empty) TSG and hands its
// slice to whoever has work, so leaving an idle sandbox attached with a small
// timeslice is fully work-conserving. Detaching would be worse than useless: a
// detached sandbox stops advancing its GP_PUT, so the activity signal would
// read it idle forever and never re-attach it. Commands are emitted only where
// the timeslice differs from last tick, so a steady state is silent.
func enforcePlan(clients []EnforceClient, prev map[int]pidState) ([]enforceCmd, map[int]pidState) {
	// The smallest weight among active sandboxes anchors the timeslice scale.
	var minW uint64
	for _, c := range clients {
		if c.Idle {
			continue
		}
		w := c.Weight
		if w == 0 {
			w = 1
		}
		if minW == 0 || w < minW {
			minW = w
		}
	}
	if minW == 0 {
		minW = 1 // everyone idle; scale is irrelevant
	}

	next := make(map[int]pidState, len(clients))
	var cmds []enforceCmd
	// Deterministic order so the emitted commands (and tests) are stable.
	sort.Slice(clients, func(i, j int) bool { return clients[i].PID < clients[j].PID })

	for _, c := range clients {
		var us uint64
		if c.Idle {
			us = minTimesliceUs
		} else {
			w := c.Weight
			if w == 0 {
				w = 1
			}
			us = uint64(minTimesliceUs) * w / minW
			if us < minTimesliceUs {
				us = minTimesliceUs
			}
			if us > maxTimesliceUs {
				us = maxTimesliceUs
			}
		}
		if prev[c.PID].timesliceUs != us {
			cmds = append(cmds, enforceCmd{op: "ts", pid: c.PID, us: us})
		}
		next[c.PID] = pidState{timesliceUs: us}
	}
	return cmds, next
}

// pidState is the last thing applied to a process, so a steady tick is silent.
type pidState struct {
	detached    bool
	timesliceUs uint64
}

// runlistEnforcer applies enforcePlan through an Enforcer, remembering what it
// applied so it only issues commands on change.
type runlistEnforcer struct {
	e    Enforcer
	prev map[int]pidState
}

func newRunlistEnforcer(e Enforcer) *runlistEnforcer {
	return &runlistEnforcer{e: e, prev: map[int]pidState{}}
}

// apply issues the commands for the given clients and records the new state.
func (r *runlistEnforcer) apply(clients []EnforceClient) {
	cmds, next := enforcePlan(clients, r.prev)
	for _, c := range cmds {
		var err error
		switch c.op {
		case "detach":
			err = r.e.Detach(c.pid)
		case "attach":
			err = r.e.Attach(c.pid)
		case "ts":
			err = r.e.SetTimeslice(c.pid, c.us)
		}
		if err != nil {
			log.Warningf("gpusched: runlist %s pid=%d: %v", c.op, c.pid, err)
		}
	}
	r.prev = next
}
