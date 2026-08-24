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

import "sort"

// Credit-based division of the GPU between tenants.
//
// This replaces encoding a tenant's weight in its per-TSG timeslice, which was
// measured to be exploitable and expensive (SECURITY-FINDINGS.md V4):
//
//   - Exploitable: the timeslice binds a *channel group*, and a tenant owns
//     many. Share tracked (nTSGs x timeslice), so a tenant that forked four
//     processes inside its own pod multiplied its share and overtook a peer
//     weighted 3x higher (0.78:1 against a granted 1:3). Under gVisor every
//     sandboxed process shares one Sentry host pid, so forking adds TSGs
//     without adding pids and the multiplication was invisible.
//   - Expensive: dividing the tenant's budget across its TSGs instead fixed the
//     ratio but cost 42% of aggregate throughput, because shrinking the quantum
//     multiplies context switching.
//
// The scheme here is GVM's ("GVM: OS-Level GPU Virtualization", Berkeley/UCLA):
// every tenant runs at the same large quantum, weight instead drives how fast a
// tenant accrues *credit*, and the time a tenant actually consumes is charged
// back against it. A tenant with fifteen channel groups burns one credit pool
// fifteen times faster and gains nothing by forking, and because the quantum is
// unchanged there is no context-switch tax.
//
// Two details are what make it correct rather than merely plausible:
//
//   - Charge by TSG share, not per capita. With a fixed quantum on every group,
//     a tenant holding n of the N attached groups really does take n/N of the
//     device. Charging every attached tenant the same amount would under-charge
//     exactly the packed attacker this exists to stop.
//   - A detached tenant still counts as wanting to run. Detaching stops its
//     GP_PUT advancing, so the driver's activity signal reads it idle forever;
//     deciding participation from that signal alone would never re-admit it.
//     The scheduler detached it, so the scheduler knows better.

// creditParams are the tunables of the credit scheduler.
type creditParams struct {
	// quantumUs is the per-TSG timeslice handed to *every* tenant. It is
	// deliberately uniform and large: it sets how often the GPU context-
	// switches, not who gets what.
	quantumUs uint64
	// capPeriods bounds how much credit an idle tenant may bank, in periods of
	// its own fair share. Without a cap a tenant idle for a minute returns with
	// a minute of credit and monopolizes the device.
	capPeriods float64
	// detachDebtUs is how far a tenant must overdraw before it is taken off the
	// runlist. A dead band is required, not a nicety: tenants that are getting
	// exactly their share sit at a credit of ~0, and detaching on `credit <= 0`
	// makes them flap on and off every tick, churning the runlist for no
	// division at all.
	detachDebtUs float64
}

func defaultCreditParams() creditParams {
	return creditParams{quantumUs: 4000, capPeriods: 2, detachDebtUs: 4000}
}

// creditState is one tenant's running account.
type creditState struct {
	// credit is GPU microseconds owed to this tenant. Positive means it is due
	// time; negative means it has overdrawn and is detached until it recovers.
	credit float64
	// detached is whether the scheduler has taken it off the runlist.
	detached bool
	// timesliceUs is the last quantum written, so a steady tick is silent.
	timesliceUs uint64
	// written / everWritten track the attach/detach state already issued, so
	// only genuine transitions reach the driver.
	written     bool
	everWritten bool
}

// creditPlanner computes runlist commands from tenant credit. It is pure --
// no clock, no I/O -- so the policy is unit-testable without a GPU.
type creditPlanner struct {
	params creditParams
	st     map[int]*creditState
}

func newCreditPlanner(p creditParams) *creditPlanner {
	return &creditPlanner{params: p, st: map[int]*creditState{}}
}

// plan advances every tenant's account by one period and returns the driver
// commands that follow. periodUs is the wall-clock length of the tick.
func (c *creditPlanner) plan(clients []EnforceClient, periodUs float64) []enforceCmd {
	// Deterministic order so emitted commands (and tests) are stable.
	sort.Slice(clients, func(i, j int) bool { return clients[i].PID < clients[j].PID })

	// Forget tenants that have gone away, so a restarted sandbox does not
	// inherit the debt or the credit of its predecessor.
	live := make(map[int]bool, len(clients))
	for _, cl := range clients {
		live[cl.PID] = true
		if _, ok := c.st[cl.PID]; !ok {
			c.st[cl.PID] = &creditState{}
		}
	}
	for pid := range c.st {
		if !live[pid] {
			delete(c.st, pid)
		}
	}

	// Participants are tenants that want the GPU: those the driver reports
	// submitting, plus any the scheduler itself has detached (whose activity
	// signal is silent *because* of that detachment).
	var participants []EnforceClient
	var totalWeight float64
	for _, cl := range clients {
		if !cl.Idle || c.st[cl.PID].detached {
			participants = append(participants, cl)
			w := cl.Weight
			if w == 0 {
				w = 1
			}
			totalWeight += float64(w)
		}
	}
	if len(participants) == 0 || totalWeight == 0 {
		return c.emit(clients)
	}

	// Accrue in proportion to weight, over the tenants actually contending.
	// Normalising over contenders rather than over everyone registered is what
	// keeps a lone tenant's account stable: it accrues the whole period and is
	// charged the whole period, so it never overdraws and is never detached.
	for _, cl := range participants {
		w := cl.Weight
		if w == 0 {
			w = 1
		}
		s := c.st[cl.PID]
		s.credit += periodUs * float64(w) / totalWeight
	}

	// Charge the attached tenants for the share they actually took. With one
	// uniform quantum per group, that share is their fraction of the attached
	// groups -- which is precisely why packing cannot pay: more groups means a
	// bigger charge against the same account.
	var attachedTSGs float64
	for _, cl := range participants {
		if !c.st[cl.PID].detached {
			attachedTSGs += float64(tsgsOf(cl))
		}
	}
	if attachedTSGs > 0 {
		for _, cl := range participants {
			s := c.st[cl.PID]
			if s.detached {
				continue
			}
			s.credit -= periodUs * float64(tsgsOf(cl)) / attachedTSGs
		}
	}

	// Bound banked credit so an idle tenant cannot return and monopolize.
	for _, cl := range participants {
		w := cl.Weight
		if w == 0 {
			w = 1
		}
		cap := c.params.capPeriods * periodUs * float64(w) / totalWeight
		s := c.st[cl.PID]
		if s.credit > cap {
			s.credit = cap
		}
	}

	// Overdrawn tenants come off the runlist; recovered ones go back on. Never
	// detach the last runnable tenant: with nobody to hand the time to, that
	// would idle the GPU rather than divide it.
	runnable := 0
	for _, cl := range participants {
		if !c.st[cl.PID].detached {
			runnable++
		}
	}
	for _, cl := range participants {
		s := c.st[cl.PID]
		switch {
		case !s.detached && s.credit <= -c.params.detachDebtUs && runnable > 1:
			s.detached = true
			runnable--
		case s.detached && s.credit > 0:
			s.detached = false
			runnable++
		}
	}
	return c.emit(clients)
}

// tsgsOf is a tenant's channel-group count, floored at one: a tenant the driver
// has not yet reported groups for still takes a share once it starts.
func tsgsOf(cl EnforceClient) int {
	if cl.TSGs < 1 {
		return 1
	}
	return cl.TSGs
}

// forgetWritten drops the memory of what was last written, so the next plan
// re-issues every timeslice. The scheduler does this on a slow cadence because
// a timeslice binds the channel groups that exist when it is written, and a
// CUDA workload creates its real compute TSG lazily -- an edge-triggered write
// lands on nothing and the group that appears a moment later runs at the driver
// default. See reassertEveryTicks.
func (c *creditPlanner) forgetWritten() {
	for _, s := range c.st {
		s.timesliceUs = 0
	}
}

// emit turns the current account state into the commands that differ from what
// was last written, so a steady division issues nothing.
func (c *creditPlanner) emit(clients []EnforceClient) []enforceCmd {
	var cmds []enforceCmd
	for _, cl := range clients {
		s, ok := c.st[cl.PID]
		if !ok {
			continue
		}
		if s.timesliceUs != c.params.quantumUs {
			cmds = append(cmds, enforceCmd{op: "ts", pid: cl.PID, us: c.params.quantumUs})
			s.timesliceUs = c.params.quantumUs
		}
	}
	for _, cl := range clients {
		s, ok := c.st[cl.PID]
		if !ok {
			continue
		}
		if s.detached != s.lastDetachedWritten() {
			op := "attach"
			if s.detached {
				op = "detach"
			}
			cmds = append(cmds, enforceCmd{op: op, pid: cl.PID})
			s.written = s.detached
			s.everWritten = true
		}
	}
	return cmds
}

// lastDetachedWritten reports the attach/detach state already issued to the
// driver. Before anything has been written the driver has every group attached,
// which is the zero value, so only a detach needs emitting.
func (s *creditState) lastDetachedWritten() bool {
	if !s.everWritten {
		return false
	}
	return s.written
}
