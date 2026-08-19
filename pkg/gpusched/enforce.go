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
// Every sandbox is held to a runlist timeslice in proportion to its weight,
// computed from the configured weights alone. It does *not* detach idle
// sandboxes, and it does *not* rescale timeslices when a sandbox reads idle.
// It does not need to -- the GSP already skips an idle (empty) TSG and hands
// its slice to whoever has work, so leaving every sandbox attached with a
// stable proportional timeslice is fully work-conserving. Detaching would be
// worse than useless: a detached sandbox stops advancing its GP_PUT, so the
// activity signal would read it idle forever and never re-attach it. Commands
// are emitted only where the timeslice differs from last tick, so a steady
// state is silent.
//
// The timeslice is deliberately independent of the idle signal. An earlier
// version anchored the scale on the smallest weight *among active sandboxes*
// and pinned idle ones to the floor, so a neighbour reading idle for a single
// sample raised the anchor and collapsed a larger tenant's slice. A busy
// cuBLAS tenant drains its GPFIFO between kernels often enough to sample as
// momentarily idle, so this flapped continuously: a 3:1 request measured
// 1.47:1 as the larger tenant's slice oscillated between its full share and
// the floor, even though the mechanism holds a hand-set 3:1 at 3.10:1. Scaling
// on the fixed configured weights of every registered sandbox removes the
// coupling entirely; work-conservation is left to the GSP, where it belongs.
func enforcePlan(clients []EnforceClient, prev map[int]pidState) ([]enforceCmd, map[int]pidState) {
	// The smallest configured weight -- over every registered sandbox, active
	// or idle -- anchors the timeslice scale, so a sandbox's slice is a stable
	// function of the weights and never lurches on a neighbour's activity blip.
	var minW uint64
	for _, c := range clients {
		w := c.Weight
		if w == 0 {
			w = 1
		}
		if minW == 0 || w < minW {
			minW = w
		}
	}
	if minW == 0 {
		minW = 1
	}

	next := make(map[int]pidState, len(clients))
	var cmds []enforceCmd
	// Deterministic order so the emitted commands (and tests) are stable.
	sort.Slice(clients, func(i, j int) bool { return clients[i].PID < clients[j].PID })

	for _, c := range clients {
		w := c.Weight
		if w == 0 {
			w = 1
		}
		us := uint64(minTimesliceUs) * w / minW
		if us < minTimesliceUs {
			us = minTimesliceUs
		}
		if us > maxTimesliceUs {
			us = maxTimesliceUs
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
	e     Enforcer
	prev  map[int]pidState
	ticks int
}

func newRunlistEnforcer(e Enforcer) *runlistEnforcer {
	return &runlistEnforcer{e: e, prev: map[int]pidState{}}
}

// reassertEveryTicks is how often the enforcer re-issues every timeslice even
// when nothing changed. A timeslice is set on the channel groups a process has
// *at the time it is written*, but a CUDA workload creates its real compute
// TSG lazily -- a container that has only just started is still importing its
// runtime and has no GPFIFO yet, so an edge-triggered write lands on nothing
// (or on a transient warmup context) and the cuBLAS TSG that appears a moment
// later runs at the driver default. That is exactly how a correctly-computed
// 3:1 plan measured 1:1: the plan was written once, three seconds in, while
// the tenant read idle with no channels, and never re-asserted. Re-issuing the
// full plan on a slow cadence catches those late TSGs without restarting the
// runlist so often that the restarts themselves cost throughput. At a 100ms
// period this is roughly every two seconds.
const reassertEveryTicks = 20

// apply issues the commands for the given clients and records the new state.
func (r *runlistEnforcer) apply(clients []EnforceClient) {
	r.ticks++

	// Genuine changes since the last tick -- a client joining or leaving, or a
	// weight change -- are what is logged, so a steady division is quiet.
	changed, next := enforcePlan(clients, r.prev)
	if len(changed) > 0 {
		log.Infof("gpusched: runlist enforce: %d clients %+v -> %d commands %+v", len(clients), clients, len(changed), changed)
	}

	// What to actually write to the driver: the genuine changes, or -- on the
	// slow re-assert cadence -- every timeslice, computed against an empty
	// prior so the plan re-emits all of them. That re-binds any TSG the
	// workload created since the last write, which an edge-triggered plan would
	// otherwise leave at the driver default. The re-issue is not logged.
	toWrite := changed
	if r.ticks%reassertEveryTicks == 0 {
		toWrite, _ = enforcePlan(clients, nil)
	}
	for _, c := range toWrite {
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
