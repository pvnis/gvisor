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
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/gpusched"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
)

// The AMD compute gate: a sandbox is held to a share of the GPU by suspending
// the queues it submits through, outside the window it was granted.
//
// This is the counterpart of nvproxy's computegate.go, and it exists because
// the two vendors put the lever in different places. On NVIDIA, once a channel
// exists both packet submission and scheduling state leave the kernel, so the
// only thing a Sentry can do is revoke the mappings work is submitted through.
// On AMD only *submission* leaves: the queue lifecycle stays behind ioctls on
// /dev/kfd, which this package already interprets and already holds an open
// descriptor for. So the sandbox's queues can be stopped and started directly,
// by the same process the driver already believes owns them.
//
// Two things follow from that, and both are improvements on the NVIDIA side:
//
//   - It preempts work already running. CWSR saves and restores in-flight
//     waves, so a kernel is stopped mid-flight and resumed correctly rather
//     than having to run to completion. The NVIDIA gate can only decline to
//     let new work be submitted, which a long kernel outlives.
//   - It needs no privileged host component. Enforcement is entirely inside
//     the Sentry, which is where this branch wants every limit to live.
//
// The lever is KFD_IOC_DBG_TRAP_SUSPEND_QUEUES, the operation ROCgdb uses to
// stop a process's queues while other processes keep using the GPU. That is
// exactly the case here. Its documented alternative, UPDATE_QUEUE's
// queue_percentage, is cheaper and was measured dividing just as accurately --
// and it wedges the GPU, with a shader page fault and a mode1 reset, whenever
// a second process holds queues at the same time. It is not used.
//
// DBG_TRAP is denied to the sandbox itself, so nothing here is reachable from
// inside the container: the sandbox cannot resume its own queues, and cannot
// open a debug session that would let it.

const (
	// dbgGracePeriod is how long waves are given to reach a preemption point
	// before being forced, in units of 1K GPU clocks. Zero means preempt
	// immediately, which is what a time slice wants: the grace period exists
	// for a debugger stopping at a breakpoint, not for a scheduler.
	dbgGracePeriod = 0

	// maxDrainedEvents bounds the QUERY_DEBUG_EVENT loop. The call is drained
	// until it reports EAGAIN; this only stops a driver that never does from
	// spinning forever.
	maxDrainedEvents = 1024

	// activeMemory is how many consecutive periods must pass with no wave state
	// saved before the sandbox is reported idle.
	//
	// One period's sample is not enough. A suspension catches whatever happens
	// to be resident at that instant, and a latency-bound workload -- an LLM
	// decoding a token at a time -- is between kernels far more often than it
	// is inside one. gpuburn is caught 82% of the time; vLLM is caught rarely,
	// and reporting from the last sample alone made the scheduler oscillate:
	// the tenant reported idle, dropped to the 5 ms floor, was granted the
	// whole period back the moment its neighbour looked idle too, and the two
	// swapped places every few periods. Measured on two vLLM tenants weighted
	// 3:1, whose windows flapped between 5%, 75% and 100% and whose throughput
	// came out 1.06:1.
	//
	// Five periods is half a second at the default period: long enough to span
	// the gaps in a decode loop, short enough that a tenant which really stops
	// hands its share over promptly. The scheduler applies its own hysteresis
	// on top.
	activeMemory = 5

	// resumeSlack shifts the whole window this far earlier, so that the queues
	// are already running when the granted slot begins. A suspend and a resume
	// cost around half a millisecond each (a queue's *first* suspend costs
	// about 12 ms, taking the initial CWSR path), so without it every tenant
	// spends the start of its slot waiting to be resumed.
	//
	// It shifts the window rather than widening it, and that distinction is
	// the whole point: widening gives every tenant allowance+slack of running
	// time, which is a far larger relative gift to a small share than a large
	// one and quietly compresses the division toward equal. Measured at 100 ms
	// with 1 ms of slack, a 3:1 request came out 2.81:1 -- close enough to look
	// right, wrong for the same reason every time. Shifting keeps each window
	// exactly its allowance and keeps them tiling the period without gaps.
	resumeSlack = 1 * time.Millisecond
)

// timeSlicer holds a sandbox to a share of the GPU by suspending its queues.
//
// +stateify savable
type timeSlicer struct {
	// weight is this sandbox's share relative to the others on the GPU, and
	// schedFD is an open connection to the scheduler that divides them. Both
	// are set by Register and immutable after it.
	weight  uint64
	schedFD int
	id      string

	// grant is the window the scheduler last assigned, as a *gpusched.Grant.
	// It is read every period by run() and written by follow(), neither of
	// which should wait on the other.
	grant atomic.Pointer[gpusched.Grant] `state:"nosave"`

	// mu serializes everything that touches the driver's view of the queues:
	// the debug session, the suspend state, and the registry itself. Queue
	// creation and destruction take it too, so a queue cannot be destroyed
	// while suspended, and cannot be created and then missed by a suspend
	// that was already deciding which ids to name.
	mu sync.Mutex `state:"nosave"`

	// queues are the compute queues this sandbox has open, by KFD queue id,
	// each mapped to the size of the control stack the runtime gave it. SDMA
	// queues are deliberately absent: suspending those stalls copies without
	// gating any compute.
	//
	// The size is kept because GET_QUEUE_WAVE_STATE writes the control stack
	// to a buffer without being told how big that buffer is, so the only safe
	// bound is what the queue was created with.
	queues map[uint32]uint32

	// samples and busySamples count what the wave-state probe saw since the
	// last report to the scheduler: how many times the sandbox's queues were
	// suspended, and how many of those suspensions had waves to save.
	samples     uint64
	busySamples uint64

	// quietPeriods counts consecutive reports whose samples saw no waves at
	// all, and is what decides idleness rather than the latest sample. See
	// activeMemory.
	quietPeriods uint64

	// ctlStack is scratch for the control stack GET_QUEUE_WAVE_STATE writes,
	// kept so the probe does not allocate every period.
	ctlStack []byte `state:"nosave"`

	// hostFD is the descriptor the debug session was opened on, or -1. Any of
	// the sandbox's KFD descriptors would do -- DBG_TRAP names its target by
	// pid, and every process in the sandbox shares the Sentry's one
	// kfd_process -- so this is simply the first one to create a queue.
	hostFD int32

	// dbgFD is the eventfd handed to the driver as the session's notification
	// descriptor, or -1. The driver requires something pollable; nothing here
	// ever reads it.
	dbgFD int

	// suspended is whether the queues are currently stopped. Tracked so that
	// run() issues an ioctl only on a transition, and so that teardown knows
	// whether it has anything to undo.
	suspended bool

	// running is whether run() has been started, and stop closes to end it.
	running bool
	stop    chan struct{} `state:"nosave"`
	done    chan struct{} `state:"nosave"`
}

// enabled returns whether this sandbox is being time-sliced at all.
func (ts *timeSlicer) enabled() bool {
	return ts != nil && ts.schedFD >= 0
}

// sessionOpenLocked reports whether there is a debug session to issue DBG_TRAP
// against. Every operation but ENABLE is refused without one, so calling into
// the driver anyway would turn a sandbox that simply has not initialised the
// ROCm runtime yet into a stream of warnings.
//
// Preconditions: ts.mu is held.
func (ts *timeSlicer) sessionOpenLocked() bool {
	return ts.dbgFD >= 0 && ts.hostFD >= 0
}

func (ts *timeSlicer) init(weight uint64, schedFD int, id string) {
	ts.weight = weight
	ts.schedFD = schedFD
	ts.id = id
	ts.queues = make(map[uint32]uint32)
	ts.hostFD = -1
	ts.dbgFD = -1
	if !ts.enabled() {
		return
	}
	if ts.id == "" {
		// The scheduler only has to tell sandboxes apart, and the container ID
		// is not always known this early. The Sentry's process ID is unique
		// among the sandboxes on a host, which is enough.
		ts.id = fmt.Sprintf("sandbox-%d", os.Getpid())
	}
	// Until the scheduler says otherwise the sandbox is not held at all.
	// Starting closed would stall a sandbox whose scheduler never answers.
	ts.setGrant(gpusched.Grant{})
	log.Infof("amdproxy: GPU time-sliced at weight %d, coordinating as %q", ts.weight, ts.id)
	// Announce now rather than at the first queue. A sandbox that has not
	// reached the GPU yet still has a weight, and the scheduler dividing the
	// device should know what it will have to accommodate.
	go ts.follow()
}

func (ts *timeSlicer) setGrant(g gpusched.Grant) {
	if prev := ts.grant.Load(); prev == nil || *prev != g {
		log.Debugf("amdproxy: GPU window is now %v of every %v at phase %v (%.0f%%)",
			g.Allowance, g.Period, g.Phase, g.Fraction()*100)
	}
	ts.grant.Store(&g)
}

func (ts *timeSlicer) currentGrant() gpusched.Grant {
	if g := ts.grant.Load(); g != nil {
		return *g
	}
	return gpusched.Grant{}
}

// openSession starts slicing this sandbox, and must be called immediately
// after its first RUNTIME_ENABLE has succeeded.
//
// The timing is the whole point, and it is not a detail. DBG_TRAP_ENABLE
// requires RUNTIME_ENABLE to have happened, so the session cannot be opened
// before this; and it must not be opened much later, because attaching to a
// runtime that has become *busy* is a different operation. The driver reports
// which case it is in the runtime_state it returns: 1 is ENABLED, 2 is
// ENABLED_BUSY, and attaching to a busy runtime is ROCgdb's attach-to-a-live-
// process path, where the driver expects the debugger to answer a runtime
// event with SEND_RUNTIME_EVENT. Nothing here answers it.
//
// Measured: opening the session lazily at the first CREATE_QUEUE works for a
// single-process workload, whose runtime is still merely ENABLED by then, and
// kills a multi-process one. vLLM's engine core reached CREATE_QUEUE with the
// runtime already ENABLED_BUSY, the session opened, and the process died with
// SIGSEGV within milliseconds -- reported by vLLM only as "Engine core
// initialization failed ... {'EngineCore_DP0': -11}", with nothing in dmesg.
//
// Opening it here also puts the session before every queue the sandbox will
// ever create, which is the case EC_QUEUE_NEW exists for and the one the
// driver is built around.
func (ts *timeSlicer) openSession(hostFD int32) {
	if !ts.enabled() {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.running {
		return
	}
	if ts.hostFD < 0 {
		ts.hostFD = hostFD
	}
	if err := ts.enableSessionLocked(); err != nil {
		// This sandbox is not sliced. That is a loss of division, not of
		// isolation -- its memory quota and CU mask are untouched, and those
		// are what bound what it can reach -- so say so loudly and let the
		// workload run rather than failing it to enforce a share.
		log.Warningf("amdproxy: this sandbox will NOT be time-sliced: %v", err)
		return
	}
	// Enabling debug takes the process's queues through a suspend, and does
	// not hand them back running. There are none yet, but the bookkeeping has
	// to start from stopped so that the first window opens them.
	ts.suspended = true
	ts.running = true
	ts.stop = make(chan struct{})
	ts.done = make(chan struct{})
	go ts.run()
}

// trackQueue records a compute queue so that it is suspended along with the
// rest. Called after CREATE_QUEUE has succeeded.
func (ts *timeSlicer) trackQueue(hostFD int32, queueID uint32, queueType uint32, ctlStackSize uint32) {
	if !ts.enabled() || !isComputeQueueType(queueType) {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.queues[queueID] = ctlStackSize
	if ts.hostFD < 0 {
		ts.hostFD = hostFD
	}
	// Consume the queue-new exception this queue was just flagged with.
	//
	// Subscribing to an exception tells the driver a debugger is watching for
	// it, and the driver then expects it to be consumed; a queue whose
	// exception nobody collects does not run. Draining only before a suspend
	// -- which is all the mechanism itself needs -- leaves a sandbox that is
	// never suspended, because it is alone on the GPU and holds the whole
	// period, with a queue that never starts. Measured: gpuburn produced no
	// output at all and the sandbox sat in the watchdog with one live task.
	if !ts.sessionOpenLocked() {
		return
	}
	ts.drainEventsLocked()

	// Put the queues into whatever the window calls for right now, rather than
	// waiting for run() to notice. That both starts the queues after the
	// session was opened and stops a queue created while the window is closed
	// from running until the next transition.
	if want, _ := ts.desired(time.Now()); needsTransition(ts.suspended, want) {
		ts.applyLocked(want)
	}
}

// beforeDestroyQueue resumes the sandbox's queues so that one of them can be
// destroyed, and returns a function the caller must call once it has finished.
//
// The driver refuses to destroy a queue that is not running, answering EIO. So
// the queues have to be resumed first, and nothing may suspend them again in
// between -- which is why mu is held across the caller's DESTROY_QUEUE rather
// than released and retaken.
//
// The returned function restores whatever the window calls for at that moment,
// rather than leaving the queues running until run() next wakes. Without that
// a destroy arriving just after a window closed would hand the sandbox the
// rest of the period.
func (ts *timeSlicer) beforeDestroyQueue(queueID uint32) func() {
	if !ts.enabled() {
		return func() {}
	}
	ts.mu.Lock()
	if ts.suspended {
		ts.applyLocked(true)
	}
	return func() {
		delete(ts.queues, queueID)
		if want, _ := ts.desired(time.Now()); !want {
			ts.applyLocked(false)
		}
		ts.mu.Unlock()
	}
}

// forgetHostFD is called when one of the sandbox's KFD descriptors is closed.
//
// The debug session was opened on whichever descriptor created the first
// queue, and that descriptor can be released while others remain -- a sandbox
// sharing one GPU address space has several processes holding their own. Left
// alone the slicer would keep issuing ioctls on a closed descriptor, whose
// number the host is free to hand to something else. It reports EBADF at best,
// and at worst names a file this package never opened; either way the sandbox
// stops being sliced without anything saying so.
//
// There is nothing to move the session to: it belongs to the KFD process, but
// the *descriptor* it was opened on is gone. So the session is torn down and
// the next queue creation opens a new one on a descriptor known to be live.
func (ts *timeSlicer) forgetHostFD(hostFD int32) {
	if !ts.enabled() {
		return
	}
	ts.mu.Lock()
	if ts.hostFD != hostFD {
		ts.mu.Unlock()
		return
	}
	ts.mu.Unlock()
	log.Debugf("amdproxy: the KFD descriptor holding the debug session was closed; restarting the session on the next queue")
	ts.shutdown()
	ts.mu.Lock()
	ts.hostFD = -1
	ts.mu.Unlock()
}

// shutdown resumes everything and closes the debug session. It runs when the
// sandbox's last KFD descriptor goes away.
func (ts *timeSlicer) shutdown() {
	if !ts.enabled() {
		return
	}
	ts.mu.Lock()
	running := ts.running
	stop := ts.stop
	done := ts.done
	ts.running = false
	ts.mu.Unlock()

	if running {
		close(stop)
		<-done
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.suspended {
		ts.applyLocked(true)
	}
	ts.disableSessionLocked()
}

// run drives the window. It sleeps until the next transition rather than
// polling, so an unsliced sandbox costs one wakeup a period and a fully
// granted one costs nothing at all.
func (ts *timeSlicer) run() {
	defer close(ts.done)
	for {
		want, until := ts.desired(time.Now())
		ts.mu.Lock()
		if needsTransition(ts.suspended, want) {
			ts.applyLocked(want)
		}
		ts.mu.Unlock()

		if until <= 0 {
			until = gpusched.DefaultPeriod
		}
		select {
		case <-ts.stop:
			return
		case <-time.After(until):
		}
	}
}

// desired reports whether the queues should be running at time t, and how long
// that answer holds for.
//
// The window starts at the grant's phase rather than at the start of the
// period, so that sandboxes sharing a GPU take turns instead of all contending
// during the same part of every period. The phase is measured against the wall
// clock, so sandboxes interleave without having to coordinate with each other.
//
// The window is the granted allowance, shifted earlier by resumeSlack -- not
// lengthened by it; see the constant.
//
// All of it is modular arithmetic on the position within the period, because
// that shift puts a window at phase zero in the *previous* period. Getting that wrap wrong is expensive and
// quiet: an earlier version computed a non-positive time-to-next-edge there and
// treated it as "a whole period", so the tenant holding phase zero stayed
// suspended for an extra period each time and received 75ms out of every 200
// rather than out of every 100. It still divided the GPU, just at half the
// share the scheduler had granted -- measured as 1.57:1 for a 3:1 request,
// with the *other* tenant landing exactly on its own share.
func (ts *timeSlicer) desired(t time.Time) (bool, time.Duration) {
	g := ts.currentGrant()
	if g.Period <= 0 || g.Allowance+resumeSlack >= g.Period {
		// Not being held: either no grant has arrived yet, or this sandbox has
		// so much of the period that stopping it is not worth an ioctl.
		return true, gpusched.DefaultPeriod
	}
	if g.Allowance <= 0 {
		return false, g.Period
	}
	open := mod(g.Phase-resumeSlack, g.Period)
	shut := mod(g.Phase+g.Allowance-resumeSlack, g.Period)
	pos := mod(time.Duration(t.UnixNano()), g.Period)

	if inWindow(pos, open, shut) {
		return true, until(pos, shut, g.Period)
	}
	return false, until(pos, open, g.Period)
}

// mod is Go's % brought into [0, m), which it is not for negative values.
func mod(d, m time.Duration) time.Duration {
	d %= m
	if d < 0 {
		d += m
	}
	return d
}

// inWindow reports whether pos falls in [open, shut), which wraps around the
// end of the period whenever shut has come round past open.
func inWindow(pos, open, shut time.Duration) bool {
	if open <= shut {
		return pos >= open && pos < shut
	}
	return pos >= open || pos < shut
}

// until returns how long from pos until target comes round, which is a whole
// period when it is already there rather than zero -- a zero would spin.
func until(pos, target, period time.Duration) time.Duration {
	if d := mod(target-pos, period); d > 0 {
		return d
	}
	return period
}

// applyLocked suspends or resumes every tracked queue in one call, which is
// the shape the ioctl is built for: a per-queue loop would let the driver
// reschedule in between and leave the sandbox briefly running more than its
// share.
//
// Preconditions: ts.mu is held.
func (ts *timeSlicer) applyLocked(resume bool) {
	if !ts.sessionOpenLocked() || len(ts.queues) == 0 {
		ts.suspended = !resume
		return
	}
	ids := make([]uint32, 0, len(ts.queues))
	for id := range ts.queues {
		ids = append(ids, id)
	}
	log.Debugf("amdproxy: %s %d queues", suspendOrResume(resume), len(ids))
	if !resume {
		defer ts.sampleWavesLocked()
	}
	if err := ts.setQueuesLocked(ids, resume); err != nil {
		// Leave ts.suspended alone so the next period tries again. Failing to
		// resume is the dangerous direction -- it would strand the sandbox --
		// and retrying is what recovers it.
		log.Warningf("amdproxy: %s %d queues: %v", suspendOrResume(resume), len(ids), err)
		return
	}
	ts.suspended = !resume
}

// needsTransition reports whether the queues have to be acted on to reach the
// wanted state, given whether they are currently suspended.
//
// This is one boolean comparison and it is worth naming, because getting it
// inverted is silent: the queues are never suspended, so nothing is enforced,
// and a resume is issued for queues that were never stopped. The driver counts
// suspends and resumes against each other, so that unmatched resume leaves the
// count wrong and the queue does not run at all -- measured as a workload that
// produced no output and a sandbox idling in the watchdog.
func needsTransition(suspended, wantRunning bool) bool {
	return suspended == wantRunning
}

func suspendOrResume(resume bool) string {
	if resume {
		return "resuming"
	}
	return "suspending"
}

// isComputeQueueType reports whether a queue carries compute work, as opposed
// to an SDMA queue that only moves memory.
func isComputeQueueType(t uint32) bool {
	return t == amdgpu.KFD_IOC_QUEUE_TYPE_COMPUTE || t == amdgpu.KFD_IOC_QUEUE_TYPE_COMPUTE_AQL
}

// follow exchanges windows with the GPU scheduler for as long as it is
// listening.
//
// Nothing on the submission path waits for any of this: a scheduler that stops
// answering leaves the sandbox with the window it last had, rather than
// stalling it.
func (ts *timeSlicer) follow() {
	// The Sentry may read and write an open connection but not create one, so
	// this wraps the descriptor runsc donated rather than dialling.
	f := os.NewFile(uintptr(ts.schedFD), "gpu-scheduler")
	conn := gpusched.NewConn(f)
	defer conn.Close()

	hello := gpusched.Hello{ID: ts.id, Weight: ts.weight}
	if err := conn.SendHello(hello); err != nil {
		log.Warningf("amdproxy: announcing to the GPU scheduler: %v", err)
		return
	}
	for {
		a, err := conn.RecvAssignment()
		if err != nil {
			log.Warningf("amdproxy: GPU scheduler connection lost, keeping the last window: %v", err)
			return
		}
		ts.setGrant(a.Grant())
		ts.mu.Lock()
		samples, busy := ts.samples, ts.busySamples
		ts.samples, ts.busySamples = 0, 0
		if samples > 0 {
			// Evidence this period. A single quiet sample is not idleness;
			// activeMemory says how many in a row are.
			if busy > 0 {
				ts.quietPeriods = 0
			} else {
				ts.quietPeriods++
			}
		}
		// With no samples there is no evidence either way, and the only safe
		// answer is that the sandbox is asking for the GPU.
		//
		// Wave state exists only where a suspension put it, so a sandbox that
		// is not being suspended cannot be measured -- and a sandbox holding
		// the whole period is never suspended. Carrying the previous verdict
		// forward instead looks tidier and latches: a tenant reported idle is
		// granted everything, stops being suspended, never samples again, and
		// can never be seen to resume. Measured on two vLLM tenants weighted
		// 3:1, where the correct 75/25 windows held for 0.7 seconds before
		// both tenants latched idle, were each granted the whole period, and
		// finished 0.88:1 having been suspended six times in forty seconds.
		active := len(ts.queues) > 0 && (samples == 0 || ts.quietPeriods < activeMemory)
		ts.mu.Unlock()
		var submissions uint64
		if active {
			submissions = 1
		}
		if err := conn.SendReport(gpusched.Report{Submissions: submissions}); err != nil {
			log.Warningf("amdproxy: reporting to the GPU scheduler: %v", err)
			return
		}
	}
}
