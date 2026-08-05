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
	"testing"
	"time"
)

const period = DefaultPeriod

// fraction returns a grant's share of the period, rounded to whole percent.
func fraction(g Grant) int {
	return int(g.Fraction()*100 + 0.5)
}

// TestAloneGetsEverything tests that a single client using the GPU receives all
// of it. A share that reserved time for absent clients would leave the GPU idle
// for no reason.
func TestAloneGetsEverything(t *testing.T) {
	s := New(period)
	s.Add("a", 100, 0)
	s.Observe("a", period)
	if got := fraction(s.Grants()["a"]); got != 100 {
		t.Errorf("sole client received %d%% of the period, want 100%%", got)
	}
}

// TestEqualWeightsSplitEvenly tests the basic fairness property.
func TestEqualWeightsSplitEvenly(t *testing.T) {
	s := New(period)
	s.Add("a", 100, 0)
	s.Add("b", 100, 0)
	s.Observe("a", period/2)
	s.Observe("b", period/2)
	grants := s.Grants()
	for _, id := range []ID{"a", "b"} {
		if got := fraction(grants[id]); got != 50 {
			t.Errorf("%s received %d%% of the period, want 50%%", id, got)
		}
	}
}

// TestWeightsAreProportional tests that weights divide the GPU in their own
// ratio, which is what makes them meaningful without reference to a particular
// GPU's capabilities.
func TestWeightsAreProportional(t *testing.T) {
	s := New(period)
	s.Add("a", 300, 0)
	s.Add("b", 100, 0)
	s.Observe("a", period)
	s.Observe("b", period)
	grants := s.Grants()
	if got := fraction(grants["a"]); got != 75 {
		t.Errorf("weight 300 received %d%%, want 75%%", got)
	}
	if got := fraction(grants["b"]); got != 25 {
		t.Errorf("weight 100 received %d%%, want 25%%", got)
	}
}

// TestWindowsDoNotOverlap tests that clients take turns.
//
// Windows that all began at zero would put every client on the GPU during the
// same part of each period and leave it idle for the rest, which is the flaw in
// applying a fixed percentage to each sandbox independently.
func TestWindowsDoNotOverlap(t *testing.T) {
	s := New(period)
	for _, id := range []ID{"a", "b", "c", "d"} {
		s.Add(id, 100, 0)
		s.Observe(id, period/4)
	}
	grants := s.Grants()

	// Walk the period and check that no instant belongs to two clients.
	for t0 := time.Duration(0); t0 < period; t0 += time.Millisecond {
		var holders []ID
		for id, g := range grants {
			if t0 >= g.Phase && t0 < g.Phase+g.Allowance {
				holders = append(holders, id)
			}
		}
		if len(holders) > 1 {
			t.Fatalf("at %v the window is held by %v, want at most one", t0, holders)
		}
	}
}

// TestWindowsCoverThePeriod tests that dividing the GPU does not lose any of
// it: with clients to use it, no part of the period should go unassigned.
func TestWindowsCoverThePeriod(t *testing.T) {
	s := New(period)
	for _, id := range []ID{"a", "b", "c"} {
		s.Add(id, 100, 0)
		s.Observe(id, period/3)
	}
	var total time.Duration
	for _, g := range s.Grants() {
		total += g.Allowance
	}
	// Integer division of the period between three clients loses at most a
	// nanosecond each.
	if lost := period - total; lost > 3*time.Nanosecond {
		t.Errorf("windows total %v of a %v period, leaving %v unassigned", total, period, lost)
	}
}

// TestIdleClientYieldsItsShare tests that a share is only held by a client that
// uses it. This is what makes the division work-conserving: a registered but
// idle neighbour should not cost an active client half the GPU.
func TestIdleClientYieldsItsShare(t *testing.T) {
	s := New(period)
	s.Add("busy", 100, 0)
	s.Add("idle", 100, 0)
	s.Observe("busy", period)
	// "idle" observes nothing.
	grants := s.Grants()
	if got := fraction(grants["busy"]); got < 90 {
		t.Errorf("active client received %d%% beside an idle one, want nearly all", got)
	}
	if grants["idle"].Allowance != MinAllowance {
		t.Errorf("idle client kept %v, want the %v floor", grants["idle"].Allowance, MinAllowance)
	}
}

// TestIdleClientCanResume tests that an idle client is never given nothing.
// A client with no window could not submit, so it would stay idle because it
// was idle.
func TestIdleClientCanResume(t *testing.T) {
	s := New(period)
	s.Add("busy", 100, 0)
	s.Add("waking", 100, 0)
	s.Observe("busy", period)
	grants := s.Grants()
	if grants["waking"].Allowance <= 0 {
		t.Fatalf("idle client received no window and could never resume")
	}

	// Having used its floor, it competes on equal terms next period.
	s.Settle(grants)
	s.Observe("busy", period/2)
	s.Observe("waking", grants["waking"].Allowance)
	if got := fraction(s.Grants()["waking"]); got < 40 {
		t.Errorf("client received %d%% on the period after waking, want about half", got)
	}
}

// TestDepartureReleasesShare tests that a client's share becomes available to
// the others when it goes away.
func TestDepartureReleasesShare(t *testing.T) {
	s := New(period)
	s.Add("a", 100, 0)
	s.Add("b", 100, 0)
	s.Observe("a", period/2)
	s.Observe("b", period/2)
	if got := fraction(s.Grants()["a"]); got != 50 {
		t.Fatalf("with two clients a received %d%%, want 50%%", got)
	}

	s.Remove("b")
	s.Observe("a", period/2)
	if got := fraction(s.Grants()["a"]); got != 100 {
		t.Errorf("after its neighbour left a received %d%%, want 100%%", got)
	}
}

// TestCapIsRespected tests that a ceiling holds even when the GPU is otherwise
// idle, since a cap expresses "never more than this" rather than a share.
func TestCapIsRespected(t *testing.T) {
	s := New(period)
	s.Add("a", 100, 0.25)
	s.Observe("a", period)
	if got := fraction(s.Grants()["a"]); got != 25 {
		t.Errorf("capped client received %d%% while alone, want 25%%", got)
	}
}

// TestCapSlackGoesToOthers tests that time a capped client may not use is
// offered to one that can, rather than idling the GPU.
func TestCapSlackGoesToOthers(t *testing.T) {
	s := New(period)
	s.Add("capped", 100, 0.25)
	s.Add("free", 100, 0)
	s.Observe("capped", period/4)
	s.Observe("free", period/2)
	grants := s.Grants()
	if got := fraction(grants["capped"]); got != 25 {
		t.Errorf("capped client received %d%%, want 25%%", got)
	}
	if got := fraction(grants["free"]); got < 70 {
		t.Errorf("uncapped client received %d%%, want the rest of the period", got)
	}
}

// TestOverrunIsRepaid tests that time taken beyond a window is recovered from
// later ones.
//
// Work already submitted to a GPU cannot be recalled, so a client that submits
// a long kernel runs past the end of its window. Without charging the excess
// back, submitting one very long kernel per window would be a way to take more
// than a share.
func TestOverrunIsRepaid(t *testing.T) {
	s := New(period)
	s.Add("hog", 100, 0)
	s.Add("victim", 100, 0)

	// Reach a steady half each, so that the windows to overrun are the shares
	// rather than the whole idle period.
	s.Observe("hog", period/2)
	s.Observe("victim", period/2)
	grants := s.Grants()
	s.Settle(grants)

	// The hog now uses twice its window; the victim uses exactly its own.
	s.Observe("hog", period)
	s.Observe("victim", period/2)
	grants = s.Grants()
	s.Settle(grants)

	s.Observe("hog", period/2)
	s.Observe("victim", period/2)
	next := s.Grants()
	if next["hog"].Allowance >= next["victim"].Allowance {
		t.Errorf("after overrunning, hog was granted %v and victim %v; want the hog to receive less",
			next["hog"].Allowance, next["victim"].Allowance)
	}
}

// TestDebtIsBounded tests that a single very long kernel cannot starve its
// author indefinitely afterwards.
func TestDebtIsBounded(t *testing.T) {
	s := New(period)
	s.Add("a", 100, 0)
	s.Add("b", 100, 0)

	grants := s.Grants()
	// A kernel running for many periods.
	for i := 0; i < 50; i++ {
		s.Observe("a", 10*period)
		s.Observe("b", period/2)
		grants = s.Grants()
		s.Settle(grants)
	}
	// Once it stops overrunning, it must recover within a few periods.
	for i := 0; i < 5; i++ {
		s.Observe("a", grants["a"].Allowance)
		s.Observe("b", grants["b"].Allowance)
		grants = s.Grants()
		s.Settle(grants)
	}
	s.Observe("a", period/2)
	s.Observe("b", period/2)
	if got := fraction(s.Grants()["a"]); got < 40 {
		t.Errorf("client received %d%% several periods after it stopped overrunning, want about half", got)
	}
}

// TestNoClientsUsingGPU tests that when nothing is running, nobody is held
// back; restricting an idle GPU would only delay whichever client starts first.
func TestNoClientsUsingGPU(t *testing.T) {
	s := New(period)
	s.Add("a", 100, 0)
	s.Add("b", 100, 0)
	for id, g := range s.Grants() {
		if got := fraction(g); got != 100 {
			t.Errorf("%s received %d%% with the GPU idle, want 100%%", id, got)
		}
	}
}

// TestGrantsAreStable tests that a client keeps the same window while nothing
// changes, rather than being moved every period for no reason.
func TestGrantsAreStable(t *testing.T) {
	s := New(period)
	for _, id := range []ID{"a", "b", "c"} {
		s.Add(id, 100, 0)
	}
	var first map[ID]Grant
	for i := 0; i < 5; i++ {
		for _, id := range []ID{"a", "b", "c"} {
			s.Observe(id, period/3)
		}
		grants := s.Grants()
		if first == nil {
			first = grants
			continue
		}
		for id, g := range grants {
			if g != first[id] {
				t.Errorf("%s moved from %+v to %+v with nothing changed", id, first[id], g)
			}
		}
		s.Settle(grants)
	}
}

// TestWindowsStayWithinPeriod tests that no window runs past the end of the
// period it belongs to.
func TestWindowsStayWithinPeriod(t *testing.T) {
	s := New(period)
	for i, id := range []ID{"a", "b", "c", "d", "e"} {
		s.Add(id, uint64(100*(i+1)), 0)
		s.Observe(id, period/5)
	}
	for id, g := range s.Grants() {
		if g.Phase < 0 || g.Phase+g.Allowance > period {
			t.Errorf("%s was granted %v..%v, outside the %v period", id, g.Phase, g.Phase+g.Allowance, period)
		}
	}
}

// TestZeroWeightStillCompetes tests that a client which does not state a weight
// is not thereby excluded.
func TestZeroWeightStillCompetes(t *testing.T) {
	s := New(period)
	s.Add("a", 0, 0)
	s.Add("b", 0, 0)
	s.Observe("a", period/2)
	s.Observe("b", period/2)
	for id, g := range s.Grants() {
		if fraction(g) != 50 {
			t.Errorf("%s received %d%%, want an equal 50%%", id, fraction(g))
		}
	}
}

// TestGrantsHasNoSideEffects tests that computing the windows twice for the
// same period yields the same answer. Repayment that took effect merely by
// asking would make the result depend on how often it was asked for.
func TestGrantsHasNoSideEffects(t *testing.T) {
	s := New(period)
	s.Add("a", 100, 0)
	s.Add("b", 100, 0)

	// Put "a" into debt.
	s.Observe("a", period)
	s.Observe("b", period/2)
	s.Settle(s.Grants())

	s.Observe("a", period/2)
	s.Observe("b", period/2)
	first := s.Grants()
	second := s.Grants()
	for id, g := range first {
		if second[id] != g {
			t.Errorf("%s was granted %+v then %+v for the same period", id, g, second[id])
		}
	}
}
