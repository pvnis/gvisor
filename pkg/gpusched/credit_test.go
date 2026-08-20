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

import "testing"

const testPeriodUs = 100000 // 100ms, DefaultPeriod

// runTicks advances the planner n ticks and reports, per pid, the fraction of
// ticks that tenant spent attached. With one uniform quantum per group, the
// share of the device a tenant receives is its attached-time weighted by the
// groups it holds, so that is what the tests measure.
func runTicks(t *testing.T, c *creditPlanner, clients []EnforceClient, n int) map[int]float64 {
	t.Helper()
	got := map[int]float64{}
	for i := 0; i < n; i++ {
		c.plan(clients, testPeriodUs)
		// An idle tenant's groups are empty, and the GSP skips an empty TSG and
		// hands its slice to whoever has work -- so an idle-but-attached tenant
		// takes no share. That is where work-conservation comes from.
		var totalTSGs float64
		for _, cl := range clients {
			if !c.st[cl.PID].detached && !cl.Idle {
				totalTSGs += float64(tsgsOf(cl))
			}
		}
		if totalTSGs == 0 {
			continue
		}
		for _, cl := range clients {
			if !c.st[cl.PID].detached && !cl.Idle {
				got[cl.PID] += float64(tsgsOf(cl)) / totalTSGs
			}
		}
	}
	for pid := range got {
		got[pid] /= float64(n)
	}
	return got
}

func approx(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if got < want-tol || got > want+tol {
		t.Errorf("%s = %.3f, want %.3f (+/- %.3f)", what, got, want, tol)
	}
}

// A single tenant is never detached: there is nobody to hand the time to, so
// detaching it would idle the GPU rather than divide it.
func TestCreditLoneTenantNeverDetached(t *testing.T) {
	c := newCreditPlanner(defaultCreditParams())
	cl := []EnforceClient{{PID: 1, Weight: 25, TSGs: 6}}
	for i := 0; i < 200; i++ {
		c.plan(cl, testPeriodUs)
		if c.st[1].detached {
			t.Fatalf("lone tenant detached on tick %d (credit %.1f)", i, c.st[1].credit)
		}
	}
}

// Equal weights and equal group counts divide evenly.
func TestCreditEqualWeights(t *testing.T) {
	c := newCreditPlanner(defaultCreditParams())
	cl := []EnforceClient{{PID: 1, Weight: 50, TSGs: 6}, {PID: 2, Weight: 50, TSGs: 6}}
	got := runTicks(t, c, cl, 400)
	approx(t, "pid1 share", got[1], 0.5, 0.05)
	approx(t, "pid2 share", got[2], 0.5, 0.05)
}

// Weight drives the division: 75/25 should land near 3:1.
func TestCreditWeightedDivision(t *testing.T) {
	c := newCreditPlanner(defaultCreditParams())
	cl := []EnforceClient{{PID: 1, Weight: 75, TSGs: 6}, {PID: 2, Weight: 25, TSGs: 6}}
	got := runTicks(t, c, cl, 400)
	approx(t, "pid1 share", got[1], 0.75, 0.05)
	approx(t, "pid2 share", got[2], 0.25, 0.05)
	if got[2] == 0 {
		t.Fatal("low-weight tenant starved entirely")
	}
}

// THE V4 REGRESSION TEST. The low-weight tenant forks until it owns far more
// channel groups than its peer. Under the old timeslice-as-weight scheme this
// multiplied its share and let it overtake a peer weighted 3x higher; with
// credit it must still get about its weight, because the extra groups draw on
// the same account and are charged for.
func TestCreditPackingDoesNotPay(t *testing.T) {
	honest := newCreditPlanner(defaultCreditParams())
	base := runTicks(t, honest, []EnforceClient{
		{PID: 1, Weight: 75, TSGs: 6},
		{PID: 2, Weight: 25, TSGs: 6},
	}, 400)

	packed := newCreditPlanner(defaultCreditParams())
	got := runTicks(t, packed, []EnforceClient{
		{PID: 1, Weight: 75, TSGs: 6},
		{PID: 2, Weight: 25, TSGs: 15}, // the attacker, 4 processes
	}, 400)

	approx(t, "attacker share while packed", got[2], 0.25, 0.06)
	if got[2] > base[2]+0.06 {
		t.Errorf("packing paid: attacker went %.3f -> %.3f by adding groups", base[2], got[2])
	}
	if got[2] > got[1] {
		t.Errorf("attacker (weight 25) beat victim (weight 75): %.3f > %.3f", got[2], got[1])
	}
}

// Work conservation: an idle tenant is not charged and does not hold a share,
// so its peer expands to the whole device.
func TestCreditWorkConserving(t *testing.T) {
	c := newCreditPlanner(defaultCreditParams())
	cl := []EnforceClient{
		{PID: 1, Weight: 75, TSGs: 6},
		{PID: 2, Weight: 25, TSGs: 6, Idle: true},
	}
	got := runTicks(t, c, cl, 200)
	approx(t, "busy tenant share while peer idles", got[1], 1.0, 0.05)
}

// Banked credit is bounded, so a tenant idle for a long stretch cannot return
// and monopolize the device.
func TestCreditBankIsCapped(t *testing.T) {
	p := defaultCreditParams()
	c := newCreditPlanner(p)
	busy := EnforceClient{PID: 1, Weight: 50, TSGs: 6}
	idle := EnforceClient{PID: 2, Weight: 50, TSGs: 6, Idle: true}
	for i := 0; i < 500; i++ {
		c.plan([]EnforceClient{busy, idle}, testPeriodUs)
	}
	// pid2 never participated, so it should hold no runaway balance.
	if c.st[2].credit > p.capPeriods*testPeriodUs {
		t.Errorf("idle tenant banked %.1f us, cap is %.1f", c.st[2].credit, p.capPeriods*testPeriodUs)
	}
	got := runTicks(t, c, []EnforceClient{busy, {PID: 2, Weight: 50, TSGs: 6}}, 400)
	approx(t, "returning tenant settles at its weight", got[2], 0.5, 0.08)
}

// Overdraw detaches, and recovery re-attaches -- the attach must happen even
// though a detached tenant's activity signal reads idle (its GP_PUT stops
// advancing precisely because it was detached).
func TestCreditDetachThenReattach(t *testing.T) {
	c := newCreditPlanner(defaultCreditParams())
	// A tenant with a tiny weight and many groups overdraws quickly.
	cl := []EnforceClient{{PID: 1, Weight: 99, TSGs: 1}, {PID: 2, Weight: 1, TSGs: 20}}
	sawDetached, sawReattached := false, false
	for i := 0; i < 400; i++ {
		c.plan(cl, testPeriodUs)
		if c.st[2].detached {
			sawDetached = true
		} else if sawDetached {
			sawReattached = true
		}
	}
	if !sawDetached {
		t.Error("overdrawn tenant was never detached")
	}
	if !sawReattached {
		t.Error("detached tenant was never re-attached; its activity signal reads idle once detached, so participation must not depend on it")
	}
}

// A steady division issues no commands, so the driver is not churned.
func TestCreditSteadyStateIsQuiet(t *testing.T) {
	c := newCreditPlanner(defaultCreditParams())
	cl := []EnforceClient{{PID: 1, Weight: 50, TSGs: 6}, {PID: 2, Weight: 50, TSGs: 6}}
	for i := 0; i < 50; i++ {
		c.plan(cl, testPeriodUs)
	}
	quiet := 0
	for i := 0; i < 50; i++ {
		if len(c.plan(cl, testPeriodUs)) == 0 {
			quiet++
		}
	}
	if quiet < 25 {
		t.Errorf("only %d/50 steady ticks were silent; the enforcer is churning the driver", quiet)
	}
}

// A departed tenant's account is dropped, so a restarted sandbox on the same
// pid does not inherit debt or credit.
func TestCreditForgetsDepartedTenants(t *testing.T) {
	c := newCreditPlanner(defaultCreditParams())
	c.plan([]EnforceClient{{PID: 1, Weight: 50, TSGs: 6}, {PID: 2, Weight: 50, TSGs: 6}}, testPeriodUs)
	c.plan([]EnforceClient{{PID: 1, Weight: 50, TSGs: 6}}, testPeriodUs)
	if _, ok := c.st[2]; ok {
		t.Error("departed tenant's account was retained")
	}
}
