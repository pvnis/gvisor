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

package nvproxy

import (
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/gpusched"
)

func newGate(percent uint64) *computeGate {
	g := &computeGate{}
	g.init(percent, false /* scheduled */)
	return g
}

// atPhase returns a time at the given offset into a period.
func atPhase(d time.Duration) time.Time {
	base := time.Unix(0, 0).Add(1000 * computeGatePeriod)
	return base.Add(d)
}

func TestComputeGateEnabled(t *testing.T) {
	for _, test := range []struct {
		percent uint64
		want    bool
	}{
		{0, false},   // unset
		{100, false}, // the whole period is permitted
		{1, true},
		{50, true},
		{99, true},
	} {
		if got := newGate(test.percent).enabled(); got != test.want {
			t.Errorf("percent %d: enabled() = %v, want %v", test.percent, got, test.want)
		}
	}
}

// TestComputeGateWaitUntil tests how long a submission is held, which is what
// determines the share of time a sandbox receives.
func TestComputeGateWaitUntil(t *testing.T) {
	for _, test := range []struct {
		name    string
		percent uint64
		phase   time.Duration
		want    time.Duration
	}{
		{
			// Inside the allowance, submission proceeds immediately.
			name: "within allowance", percent: 50,
			phase: 10 * time.Millisecond, want: 0,
		},
		{
			// At the very end of the allowance it is still permitted.
			name: "last instant of allowance", percent: 50,
			phase: 50*time.Millisecond - time.Nanosecond, want: 0,
		},
		{
			// Just past it, the wait runs to the end of the period.
			name: "just past allowance", percent: 50,
			phase: 50 * time.Millisecond, want: 50 * time.Millisecond,
		},
		{
			name: "late in period", percent: 50,
			phase: 90 * time.Millisecond, want: 10 * time.Millisecond,
		},
		{
			// A small share waits for most of the period.
			name: "small share", percent: 10,
			phase: 20 * time.Millisecond, want: 80 * time.Millisecond,
		},
		{
			// No limit configured: never held.
			name: "disabled", percent: 0,
			phase: 90 * time.Millisecond, want: 0,
		},
		{
			name: "full share", percent: 100,
			phase: 90 * time.Millisecond, want: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := newGate(test.percent).waitUntil(atPhase(test.phase)); got != test.want {
				t.Errorf("waitUntil(phase=%v) = %v, want %v", test.phase, got, test.want)
			}
		})
	}
}

// TestComputeGateWaitNeverExceedsPeriod tests that a submission is never held
// for longer than one period, so that a gated sandbox always makes progress.
func TestComputeGateWaitNeverExceedsPeriod(t *testing.T) {
	for _, percent := range []uint64{1, 25, 50, 99} {
		g := newGate(percent)
		for phase := time.Duration(0); phase < computeGatePeriod; phase += time.Millisecond {
			if d := g.waitUntil(atPhase(phase)); d < 0 || d >= computeGatePeriod {
				t.Errorf("percent %d, phase %v: wait = %v, want within [0, %v)", percent, phase, d, computeGatePeriod)
			}
		}
	}
}

// TestComputeGateAllowanceFraction tests that the permitted portion of each
// period matches the configured percentage, since the share a sandbox receives
// is proportional to it.
func TestComputeGateAllowanceFraction(t *testing.T) {
	for _, percent := range []uint64{10, 25, 50, 75} {
		g := newGate(percent)
		var permitted int
		const samples = 1000
		for i := 0; i < samples; i++ {
			phase := time.Duration(i) * computeGatePeriod / samples
			if g.waitUntil(atPhase(phase)) == 0 {
				permitted++
			}
		}
		got := uint64(permitted * 100 / samples)
		if got != percent {
			t.Errorf("percent %d: %d%% of the period was permitted", percent, got)
		}
	}
}

// TestComputeGateResolvesEitherOrder tests that a command buffer is gated
// whether the channel is created before or after its memory is mapped. An
// application maps first, but nothing guarantees it.
func TestComputeGateResolvesEitherOrder(t *testing.T) {
	h := nvgpu.Handle{Val: 0x5c000015}

	t.Run("mapped first", func(t *testing.T) {
		g, fd := newGate(50), &frontendFD{}
		g.noteMapping(h, fd)
		if fd.gated.Load() {
			t.Errorf("gated before the channel identified the buffer")
		}
		g.addCommandBuffer(h)
		if !fd.gated.Load() {
			t.Errorf("not gated after the channel identified the buffer")
		}
	})

	t.Run("channel first", func(t *testing.T) {
		g, fd := newGate(50), &frontendFD{}
		g.addCommandBuffer(h)
		g.noteMapping(h, fd)
		if !fd.gated.Load() {
			t.Errorf("not gated after the mapping appeared")
		}
	})
}

// TestComputeGateDisabledTracksNothing tests that an unlimited sandbox does no
// bookkeeping.
func TestComputeGateDisabledTracksNothing(t *testing.T) {
	h := nvgpu.Handle{Val: 0x5c000015}
	g, fd := newGate(0), &frontendFD{}
	g.noteMapping(h, fd)
	g.addCommandBuffer(h)
	if fd.gated.Load() || len(g.gated) != 0 || len(g.byMem) != 0 {
		t.Errorf("gating disabled but state was recorded")
	}
}

// TestComputeGateForget tests that a released file description is dropped, so
// that the gate does not retain it.
func TestComputeGateForget(t *testing.T) {
	h := nvgpu.Handle{Val: 0x5c000015}
	g, fd := newGate(50), &frontendFD{}
	fd.mappedMem = h
	g.noteMapping(h, fd)
	g.addCommandBuffer(h)
	if len(g.gated) != 1 || len(g.byMem) != 1 {
		t.Fatalf("gated=%d byMem=%d, want 1 and 1", len(g.gated), len(g.byMem))
	}
	g.forget(fd)
	if len(g.gated) != 0 || len(g.byMem) != 0 {
		t.Errorf("gated=%d byMem=%d after release, want 0 and 0", len(g.gated), len(g.byMem))
	}
}

// TestComputeGateRestoreKeepsState tests that restoring preserves what the
// gate had learned, so that a restored sandbox keeps being held to its limit
// rather than having to rediscover its command buffers.
func TestComputeGateRestoreKeepsState(t *testing.T) {
	h := nvgpu.Handle{Val: 0x5c000015}
	g, fd := newGate(50), &frontendFD{}
	fd.mappedMem = h
	g.noteMapping(h, fd)
	g.addCommandBuffer(h)

	g.restore()

	if _, ok := g.cmdBufs[h]; !ok {
		t.Errorf("command buffer %v forgotten across restore", h)
	}
	if _, ok := g.gated[fd]; !ok {
		t.Errorf("gated file description forgotten across restore")
	}
	if !fd.gated.Load() {
		t.Errorf("file description no longer gated after restore")
	}
}

// TestComputeGateRestoreRebuildsNilMaps tests that a gate whose maps came back
// nil is still usable, since empty maps need not survive serialization.
func TestComputeGateRestoreRebuildsNilMaps(t *testing.T) {
	g := &computeGate{percent: 50}
	g.restore()
	g.setGrant(gpusched.Grant{Period: computeGatePeriod, Allowance: computeGatePeriod / 2})
	// Must not panic on a nil map.
	h := nvgpu.Handle{Val: 0x5c000015}
	fd := &frontendFD{}
	g.noteMapping(h, fd)
	g.addCommandBuffer(h)
	if !fd.gated.Load() {
		t.Errorf("gating did not work after restoring nil maps")
	}
}

// TestComputeGateRestoreDisabledStartsNothing tests that a sandbox with no
// limit does not acquire one across restore.
func TestComputeGateRestoreDisabledStartsNothing(t *testing.T) {
	g := &computeGate{percent: 0}
	g.restore()
	if g.enabled() {
		t.Errorf("gate became enabled across restore")
	}
	h := nvgpu.Handle{Val: 0x5c000015}
	fd := &frontendFD{}
	g.noteMapping(h, fd)
	g.addCommandBuffer(h)
	if fd.gated.Load() {
		t.Errorf("file description gated despite no limit")
	}
}

// TestComputeGateHonoursPhase tests that a window beginning partway through the
// period is respected at both ends. Sandboxes sharing a GPU are given
// non-overlapping windows so that they take turns, which only works if each
// waits for its own.
func TestComputeGateHonoursPhase(t *testing.T) {
	g := newGate(50)
	// A window covering the second half of each period.
	g.setGrant(gpusched.Grant{
		Period:    computeGatePeriod,
		Allowance: computeGatePeriod / 2,
		Phase:     computeGatePeriod / 2,
	})
	for _, test := range []struct {
		name  string
		phase time.Duration
		want  time.Duration
	}{
		{"before the window", 10 * time.Millisecond, 40 * time.Millisecond},
		{"window opens", 50 * time.Millisecond, 0},
		{"within the window", 70 * time.Millisecond, 0},
		{"last instant", 100*time.Millisecond - time.Nanosecond, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := g.waitUntil(atPhase(test.phase)); got != test.want {
				t.Errorf("waitUntil(phase=%v) = %v, want %v", test.phase, got, test.want)
			}
		})
	}
}

// TestComputeGateDisjointWindowsDoNotOverlap tests that two sandboxes given
// adjacent windows are never permitted at the same instant, which is what makes
// them take turns rather than contend.
func TestComputeGateDisjointWindowsDoNotOverlap(t *testing.T) {
	a, b := newGate(50), newGate(50)
	half := computeGatePeriod / 2
	a.setGrant(gpusched.Grant{Period: computeGatePeriod, Allowance: half})
	b.setGrant(gpusched.Grant{Period: computeGatePeriod, Allowance: half, Phase: half})

	var bothPermitted, neitherPermitted int
	for ph := time.Duration(0); ph < computeGatePeriod; ph += time.Millisecond {
		aOK := a.waitUntil(atPhase(ph)) == 0
		bOK := b.waitUntil(atPhase(ph)) == 0
		if aOK && bOK {
			bothPermitted++
		}
		if !aOK && !bOK {
			neitherPermitted++
		}
	}
	if bothPermitted != 0 {
		t.Errorf("both sandboxes were permitted at %d instants, want none", bothPermitted)
	}
	// The GPU should never be left with nobody able to use it.
	if neitherPermitted != 0 {
		t.Errorf("neither sandbox was permitted at %d instants, leaving the GPU idle", neitherPermitted)
	}
}

// TestComputeGateWindowEnd tests that revocation is scheduled for the end of
// the window rather than the end of the period.
func TestComputeGateWindowEnd(t *testing.T) {
	g := newGate(50)
	g.setGrant(gpusched.Grant{
		Period:    computeGatePeriod,
		Allowance: 30 * time.Millisecond,
		Phase:     20 * time.Millisecond,
	})
	// The window closes at 50ms.
	for _, test := range []struct {
		phase, want time.Duration
	}{
		{0, 50 * time.Millisecond},
		{40 * time.Millisecond, 10 * time.Millisecond},
		{60 * time.Millisecond, 90 * time.Millisecond},
	} {
		if got := g.untilWindowEnd(atPhase(test.phase)); got != test.want {
			t.Errorf("untilWindowEnd(phase=%v) = %v, want %v", test.phase, got, test.want)
		}
	}
}

// TestComputeGateFullAllowanceNeverWaits tests that a sandbox granted the whole
// period is never held, which is what a sandbox running alone should receive.
func TestComputeGateFullAllowanceNeverWaits(t *testing.T) {
	g := newGate(50)
	g.setGrant(gpusched.Grant{Period: computeGatePeriod, Allowance: computeGatePeriod})
	for ph := time.Duration(0); ph < computeGatePeriod; ph += time.Millisecond {
		if d := g.waitUntil(atPhase(ph)); d != 0 {
			t.Fatalf("waitUntil(phase=%v) = %v with the whole period granted, want 0", ph, d)
		}
	}
}
