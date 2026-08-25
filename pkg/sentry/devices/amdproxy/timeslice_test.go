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

package amdproxy

import (
	"strings"
	"testing"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/gpusched"
	"gvisor.dev/gvisor/pkg/sentry/devices/amdproxy/amdconf"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
)

// TestDbgTrapArgSizes tests that every DBG_TRAP parameter struct is the size
// the ioctl command number encodes.
//
// The kernel's is a union, so each of these spells out one member padded to
// the whole. A struct that came out shorter would have the driver read past
// what was passed, and one that came out longer would not match the command
// number at all.
func TestDbgTrapArgSizes(t *testing.T) {
	for _, test := range []struct {
		name string
		size uintptr
	}{
		{"enable", unsafe.Sizeof(amdgpu.KFDIoctlDbgTrapEnableArgs{})},
		{"suspend_queues", unsafe.Sizeof(amdgpu.KFDIoctlDbgTrapSuspendQueuesArgs{})},
		{"resume_queues", unsafe.Sizeof(amdgpu.KFDIoctlDbgTrapResumeQueuesArgs{})},
		{"query_debug_event", unsafe.Sizeof(amdgpu.KFDIoctlDbgTrapQueryDebugEventArgs{})},
	} {
		if test.size != amdgpu.SizeofKFDIoctlDbgTrapArgs {
			t.Errorf("%s is %d bytes, want %d", test.name, test.size, amdgpu.SizeofKFDIoctlDbgTrapArgs)
		}
	}
	// EC_QUEUE_NEW is code 31, and KFD_EC_MASK subtracts one, so it is bit 30.
	// Getting this wrong is silent: the queue is simply never suspended.
	if got, want := uint64(amdgpu.KFD_EC_MASK_QUEUE_NEW), uint64(0x40000000); got != want {
		t.Errorf("KFD_EC_MASK_QUEUE_NEW = %#x, want %#x", got, want)
	}
}

// TestDesiredWindow tests that the queues run exactly during the granted
// window, and that the answer is stable across periods.
func TestDesiredWindow(t *testing.T) {
	const period = 100 * time.Millisecond
	ts := &timeSlicer{}
	ts.setGrant(gpusched.Grant{
		Period:    period,
		Allowance: 25 * time.Millisecond,
		Phase:     50 * time.Millisecond,
	})
	for _, test := range []struct {
		offset time.Duration
		want   bool
	}{
		// The granted window is [50ms, 75ms); resumeSlack shifts the whole of
		// it a millisecond earlier, to [49ms, 74ms), so that the queues are
		// running by the time the granted slot begins. It is shifted, not
		// lengthened: it is still 25ms long.
		{0, false},
		{40 * time.Millisecond, false},
		{48*time.Millisecond + 500*time.Microsecond, false},
		{49 * time.Millisecond, true},
		{50 * time.Millisecond, true},
		{60 * time.Millisecond, true},
		{73*time.Millisecond + 999*time.Microsecond, true},
		{74 * time.Millisecond, false},
		{75 * time.Millisecond, false},
		{99 * time.Millisecond, false},
	} {
		// Anchor to an exact period boundary so the offset is the position
		// within the period.
		at := time.Unix(0, int64(10*period+test.offset))
		if got, _ := ts.desired(at); got != test.want {
			t.Errorf("at +%v: running = %v, want %v", test.offset, got, test.want)
		}
	}
}

// TestDesiredSleepsToTransition tests that the duration returned reaches the
// next change of state and not past it, since run() sleeps for exactly it.
//
// Phase zero is the case that matters and the one an earlier version of this
// test missed by only ever using phase 50ms: resumeSlack puts the opening edge
// of a phase-zero window in the previous period, and the arithmetic has to wrap
// rather than go negative. It did not, the sleep came out non-positive, and the
// tenant holding phase zero was left suspended for a whole extra period --
// halving its share while still looking like it was being scheduled.
func TestDesiredSleepsToTransition(t *testing.T) {
	const period = 100 * time.Millisecond
	for _, phase := range []time.Duration{
		0,
		25 * time.Millisecond,
		50 * time.Millisecond,
		75 * time.Millisecond,
		// A window whose end wraps past the end of the period.
		90 * time.Millisecond,
	} {
		t.Run(phase.String(), func(t *testing.T) {
			ts := &timeSlicer{}
			ts.setGrant(gpusched.Grant{
				Period:    period,
				Allowance: 25 * time.Millisecond,
				Phase:     phase,
			})
			var running time.Duration
			for offset := time.Duration(0); offset < period; offset += 100 * time.Microsecond {
				at := time.Unix(0, int64(10*period+offset))
				want, until := ts.desired(at)
				if want {
					running += 100 * time.Microsecond
				}
				if until <= 0 {
					t.Fatalf("at +%v: sleep of %v would spin", offset, until)
				}
				if until > period {
					t.Fatalf("at +%v: sleep of %v exceeds the period", offset, until)
				}
				// Just before the deadline the answer must still hold...
				if got, _ := ts.desired(at.Add(until - time.Microsecond)); got != want {
					t.Errorf("at +%v: state changed before the %v sleep elapsed", offset, until)
				}
				// ...and at it, it must have changed.
				if got, _ := ts.desired(at.Add(until)); got == want {
					t.Errorf("at +%v: state unchanged after sleeping %v", offset, until)
				}
			}
			// The window has to be the granted allowance exactly, wherever it
			// sits in the period. resumeSlack shifts it, and must not lengthen
			// it: lengthening is a bigger relative gift to a small share than
			// a large one, so it compresses the division toward equal without
			// ever looking wrong.
			want := 25 * time.Millisecond
			if d := running - want; d < -200*time.Microsecond || d > 200*time.Microsecond {
				t.Errorf("running for %v of every %v, want %v", running, period, want)
			}
		})
	}
}

// TestDesiredUngated tests that a sandbox the scheduler has not answered for,
// or has granted the whole period, is never suspended.
//
// This is the direction that must not fail closed: a sandbox stalled because
// no window ever arrived would look exactly like a hung GPU.
func TestDesiredUngated(t *testing.T) {
	for _, test := range []struct {
		name  string
		grant gpusched.Grant
	}{
		{"no grant yet", gpusched.Grant{}},
		{"whole period", gpusched.Grant{Period: 100 * time.Millisecond, Allowance: 100 * time.Millisecond}},
		{"more than the period", gpusched.Grant{Period: 100 * time.Millisecond, Allowance: 150 * time.Millisecond}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := &timeSlicer{}
			ts.setGrant(test.grant)
			for offset := time.Duration(0); offset < 100*time.Millisecond; offset += time.Millisecond {
				if got, _ := ts.desired(time.Unix(0, int64(offset))); !got {
					t.Fatalf("at +%v: suspended, want running", offset)
				}
			}
		})
	}
}

// TestWindowsTileThePeriod tests that the windows the scheduler hands out to a
// set of tenants cover the period exactly once: no instant with two tenants
// running, and none with the GPU handed to nobody.
//
// This is what makes the division proportional. It held for the granted
// allowances by construction, and then resumeSlack was applied to only one
// edge, which lengthened every window by the same absolute amount -- leaving
// the tenants overlapping, and each one's share inflated by an amount that
// depended on how small its share was.
func TestWindowsTileThePeriod(t *testing.T) {
	const period = 100 * time.Millisecond
	// What the scheduler produces for weights 3:1, placed end to end.
	tenants := []gpusched.Grant{
		{Period: period, Allowance: 75 * time.Millisecond, Phase: 0},
		{Period: period, Allowance: 25 * time.Millisecond, Phase: 75 * time.Millisecond},
	}
	slicers := make([]*timeSlicer, len(tenants))
	for i, g := range tenants {
		slicers[i] = &timeSlicer{}
		slicers[i].setGrant(g)
	}
	var running [2]time.Duration
	const step = 50 * time.Microsecond
	for offset := time.Duration(0); offset < period; offset += step {
		at := time.Unix(0, int64(10*period+offset))
		n := 0
		for i, ts := range slicers {
			if want, _ := ts.desired(at); want {
				n++
				running[i] += step
			}
		}
		if n != 1 {
			t.Fatalf("at +%v: %d tenants running, want exactly 1", offset, n)
		}
	}
	// And the shares are the ones asked for.
	if got := float64(running[0]) / float64(running[1]); got < 2.9 || got > 3.1 {
		t.Errorf("shares %v : %v = %.2f:1, want 3:1", running[0], running[1], got)
	}
}

// TestNeedsTransition tests the one comparison that decides whether an ioctl
// is issued at all. Inverting it is silent in both directions: nothing is ever
// suspended, and unmatched resumes stop the queues from running.
func TestNeedsTransition(t *testing.T) {
	for _, test := range []struct {
		suspended   bool
		wantRunning bool
		want        bool
	}{
		// Already in the wanted state; issuing anything would be an unmatched
		// suspend or resume.
		{suspended: false, wantRunning: true, want: false},
		{suspended: true, wantRunning: false, want: false},
		// A real transition.
		{suspended: true, wantRunning: true, want: true},
		{suspended: false, wantRunning: false, want: true},
	} {
		if got := needsTransition(test.suspended, test.wantRunning); got != test.want {
			t.Errorf("needsTransition(suspended=%v, wantRunning=%v) = %v, want %v",
				test.suspended, test.wantRunning, got, test.want)
		}
	}
}

// TestQuietPeriodsBeforeIdle tests that one quiet sample is not idleness.
//
// A suspension catches whatever waves are resident at that instant, and a
// latency-bound workload is between kernels more often than inside one. Acting
// on a single sample made the scheduler oscillate between handing a tenant the
// whole period and the 5 ms floor, and two vLLM tenants weighted 3:1 came out
// 1.06:1.
func TestQuietPeriodsBeforeIdle(t *testing.T) {
	ts := &timeSlicer{queues: map[uint32]uint32{1: 24576}}
	active := func() bool {
		samples, busy := ts.samples, ts.busySamples
		ts.samples, ts.busySamples = 0, 0
		if samples > 0 {
			if busy > 0 {
				ts.quietPeriods = 0
			} else {
				ts.quietPeriods++
			}
		}
		return len(ts.queues) > 0 && (samples == 0 || ts.quietPeriods < activeMemory)
	}
	quiet := func() { ts.samples, ts.busySamples = 1, 0 }
	busy := func() { ts.samples, ts.busySamples = 1, 1 }

	// A run of quiet periods shorter than activeMemory keeps the tenant active.
	for i := 0; i < activeMemory-1; i++ {
		quiet()
		if !active() {
			t.Fatalf("reported idle after %d quiet periods, want active until %d", i+1, activeMemory)
		}
	}
	// One more tips it over.
	quiet()
	if active() {
		t.Errorf("still active after %d quiet periods", activeMemory)
	}
	// Any wave seen resets it immediately.
	busy()
	if !active() {
		t.Error("reported idle in a period whose sample saw waves")
	}
	// A tenant that has gone quiet and is then left unsuspended -- which is
	// what happens the moment it is granted the whole period -- must report
	// active again, because there is no longer any evidence and wave state
	// only exists where a suspension put it. Reporting idle here latches: the
	// tenant is granted everything, so it is never suspended, so it never
	// samples, so it can never be seen to resume.
	for i := 0; i < activeMemory; i++ {
		quiet()
		active()
	}
	for i := 0; i < 3*activeMemory; i++ {
		if !active() { // no samples this period
			t.Fatal("a tenant with no evidence reported idle; it can never recover")
		}
	}
}

// TestOnlyComputeQueuesAreSliced tests that SDMA queues are left alone.
// Suspending those stalls memory copies without gating any compute.
func TestOnlyComputeQueuesAreSliced(t *testing.T) {
	for _, test := range []struct {
		queueType uint32
		want      bool
	}{
		{amdgpu.KFD_IOC_QUEUE_TYPE_COMPUTE, true},
		{amdgpu.KFD_IOC_QUEUE_TYPE_COMPUTE_AQL, true},
		{amdgpu.KFD_IOC_QUEUE_TYPE_SDMA, false},
		{amdgpu.KFD_IOC_QUEUE_TYPE_SDMA_XGMI, false},
	} {
		if got := isComputeQueueType(test.queueType); got != test.want {
			t.Errorf("isComputeQueueType(%d) = %v, want %v", test.queueType, got, test.want)
		}
	}
}

// TestCUMaskAndWeightAreExclusive tests that a sandbox cannot be configured
// with both a compute unit mask and a time slice.
//
// The driver refuses the combination -- kfd_dbg_set_queue_workaround() returns
// EBUSY for a debug session's CWSR workaround on a CU-masked queue -- and it
// refuses it far too late to be diagnosable: the queue is destroyed and ROCr
// dies on a null dereference. Registration is where it has to be caught.
func TestCUMaskAndWeightAreExclusive(t *testing.T) {
	mask, err := amdconf.ParseCUMask("0x3f")
	if err != nil {
		t.Fatalf("parsing a CU mask: %v", err)
	}
	for _, test := range []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{name: "mask alone", opts: Options{CUMask: mask, SchedulerFD: -1}},
		{name: "weight alone", opts: Options{SchedulerFD: 7, SchedulerWeight: 100}},
		{name: "neither", opts: Options{SchedulerFD: -1}},
		{name: "both", opts: Options{CUMask: mask, SchedulerFD: 7, SchedulerWeight: 100}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkSliceOrMask(&test.opts)
			if test.wantErr {
				if err == nil {
					t.Fatal("accepted both a CU mask and a time slice, want a refusal")
				}
				// The message has to name both flags: an operator seeing this
				// has set one of them somewhere they may have forgotten, very
				// likely a node-wide default.
				for _, want := range []string{"amdproxy-cu-mask", "amdproxy-gpu-weight"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal does not mention %q: %v", want, err)
					}
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected refusal: %v", err)
			}
		})
	}
}

// TestDisabledSlicerIsInert tests that a sandbox with no scheduler connection
// takes none of this path, since that is every sandbox by default.
func TestDisabledSlicerIsInert(t *testing.T) {
	var ts timeSlicer
	ts.init(0, -1, "")
	if ts.enabled() {
		t.Fatal("a sandbox without a scheduler connection is being sliced")
	}
	// None of these may touch the driver, start a goroutine, or panic.
	ts.trackQueue(7, 1, amdgpu.KFD_IOC_QUEUE_TYPE_COMPUTE, 24576)
	ts.beforeDestroyQueue(1)()
	ts.forgetHostFD(7, 8)
	ts.shutdown()
	if len(ts.queues) != 0 {
		t.Errorf("queues = %v, want none tracked", ts.queues)
	}
}

// TestSessionMovesToASurvivingFD tests that closing the descriptor a debug
// session was opened on moves the session to another of the sandbox's
// descriptors rather than abandoning it.
//
// Every descriptor reaches the same kfd_process, so the session is still
// there; what would be lost is the Sentry's way of naming it. Tearing it down
// instead left the driver with a debugger attached that could no longer be
// reached, and the next RUNTIME_ENABLE(disable) blocked forever.
func TestSessionMovesToASurvivingFD(t *testing.T) {
	var ts timeSlicer
	ts.init(100, 3, "test")
	defer func() { ts.schedFD = -1 }()
	ts.mu.Lock()
	ts.hostFD = 7
	ts.dbgFD = 9
	open := ts.sessionOpenLocked()
	ts.mu.Unlock()
	if !open {
		t.Fatal("test setup did not produce an open session")
	}

	ts.forgetHostFD(7, 8)

	ts.mu.Lock()
	got, stillOpen := ts.hostFD, ts.sessionOpenLocked()
	ts.mu.Unlock()
	if got != 8 {
		t.Errorf("hostFD = %d, want 8 (the surviving descriptor)", got)
	}
	if !stillOpen {
		t.Error("the session was closed, but another descriptor of the same kfd_process was open")
	}
}

// TestForgetHostFDIgnoresOtherFDs tests that closing a descriptor the session
// was not opened on leaves it alone. Every process in a sharing sandbox holds
// its own, so most closes are of no interest.
func TestForgetHostFDIgnoresOtherFDs(t *testing.T) {
	var ts timeSlicer
	ts.init(100, 3, "test")
	defer func() { ts.schedFD = -1 }()
	ts.mu.Lock()
	ts.hostFD = 7
	ts.dbgFD = 9
	ts.mu.Unlock()

	ts.forgetHostFD(11, 12)

	ts.mu.Lock()
	got := ts.hostFD
	ts.mu.Unlock()
	if got != 7 {
		t.Errorf("hostFD = %d, want 7 unchanged", got)
	}
}

// TestSignalPageGoesToOneProcess tests that exactly one process may register
// the sandbox's signal page, and that the rest are told to drop the proposal
// rather than have it forwarded and refused.
//
// The distinction is the whole bug: the driver handles the page before it
// creates the event and returns early if the page fails, so forwarding a
// doomed proposal costs the caller its event and ROCr then dereferences null.
func TestSignalPageGoesToOneProcess(t *testing.T) {
	var es eventShare
	es.init(true)

	if !es.claimPage(2) {
		t.Fatal("the first process was refused the signal page")
	}
	if !es.claimPage(2) {
		t.Error("the owner was refused its own page on a second proposal")
	}
	if es.claimPage(6) {
		t.Error("a second process was allowed to propose a signal page")
	}
	if es.owner() != 2 {
		t.Errorf("owner = %d, want 2", es.owner())
	}
	if !es.mustPoll(6) {
		t.Error("the refused process is not marked as needing to poll")
	}
	if es.mustPoll(2) {
		t.Error("the page owner was marked as needing to poll")
	}

	// The owner leaving lets a later process register a page, since the
	// driver's own page belongs to the KFD process, not to any one of them.
	es.releasePage(2)
	if es.owner() != 0 {
		t.Errorf("owner = %d after release, want none", es.owner())
	}
	if !es.claimPage(6) {
		t.Error("no process could claim the page after the owner left")
	}
}

// TestSignalPageUnsharedIsUnrestricted tests that without sharing every
// process may propose a page, because each has its own kfd_process.
func TestSignalPageUnsharedIsUnrestricted(t *testing.T) {
	var es eventShare
	es.init(false)
	for _, tgid := range []int32{2, 6, 10} {
		if !es.claimPage(kernel.ThreadID(tgid)) {
			t.Errorf("thread group %d was refused a signal page with sharing off", tgid)
		}
	}
	if es.mustPoll(6) {
		t.Error("a process must not be made to poll when sharing is off")
	}
}
