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
	"fmt"
	"os"
	"time"

	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/gpusched"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
)

// computeGatePeriod is the length of one allow/deny cycle.
//
// The period trades responsiveness against overhead: a sandbox is admitted at
// most once per period, so a shorter period spreads its share more evenly,
// while a longer one costs fewer faults. 100ms matches the token length used
// by the scheme this implements.
const computeGatePeriod = 100 * time.Millisecond

// computeGate limits the fraction of wall-clock time during which a sandbox
// may submit work to the GPU.
//
// Work submission does not pass through the Sentry: an application writes
// commands to a ring buffer in memory it has mapped, then rings a doorbell
// through a mapped register, neither of which enters the kernel. The Sentry
// therefore cannot intercept submission directly. What it can do is revoke its
// own mapping of the ring buffer, so that the next write to it faults, and hold
// that fault until the sandbox is permitted to run.
//
// The result is a cap rather than a share. Each sandbox limits only itself, and
// nothing redistributes time a sandbox does not use, because a Sentry sees only
// its own sandbox. Enforcing shares between sandboxes would require a
// coordinator outside all of them.
//
// +stateify savable
type computeGate struct {
	// percent is the share of each period during which submission is permitted
	// when no scheduler is supplying one. Zero or 100 disables gating. It is
	// immutable after Register().
	percent uint64

	// scheduled is true if a scheduler outside the sandbox supplies the window,
	// in which case gating is in effect regardless of percent. It is immutable
	// after Register().
	scheduled bool

	// grantMu protects the window, which a scheduler may replace at any time.
	// It is separate from mu so that reading the window on the submission path
	// does not wait on the bookkeeping below, and is held only for the few
	// reads needed to copy it.
	grantMu sync.Mutex `state:"nosave"`
	grant   gpusched.Grant

	mu sync.Mutex `state:"nosave"`

	// cmdBufs is the set of memory objects that hold channel command buffers,
	// learned from channel allocations. It is protected by mu.
	cmdBufs map[nvgpu.Handle]struct{}

	// byMem maps each memory object to the file description through which it
	// is mapped, so that a command buffer can be tied to its mapping. It is
	// protected by mu.
	byMem map[nvgpu.Handle]*frontendFD

	// gated is the set of file descriptions whose mappings must be revoked at
	// the end of each period. It is protected by mu.
	gated map[*frontendFD]struct{}

	// preempt is true if the sandbox's channel groups should be preempted when
	// its window closes, rather than only being prevented from submitting more
	// work. It is immutable after Register().
	preempt bool

	// tsgs is the set of channel groups (KEPLER_CHANNEL_GROUP_A) the sandbox
	// has allocated, which are the units the GPU schedules and therefore the
	// units that can be preempted. It is protected by mu.
	tsgs map[nvgpu.Handle]channelGroup

	// submissions counts attempts to submit work since the last report to the
	// scheduler, which uses it to tell a sandbox that is using the GPU from one
	// that is merely connected.
	submissions atomicbitops.Uint64

	// warnedPreempt records that a failed preempt has already been reported, so
	// that a driver which cannot preempt at all does not log once per period
	// forever.
	warnedPreempt atomicbitops.Bool

	// notedPreempt records that a successful preempt has already been reported,
	// for the same reason in the other direction.
	notedPreempt atomicbitops.Bool
}

// init prepares g to gate submission, permitting it for the given share of each
// period until a scheduler supplies a window. If scheduled is true, gating is in
// effect even when percent is unset, because the scheduler decides the share.
func (g *computeGate) init(percent uint64, scheduled, preempt bool) {
	g.percent = percent
	g.scheduled = scheduled
	g.preempt = preempt
	g.cmdBufs = make(map[nvgpu.Handle]struct{})
	g.byMem = make(map[nvgpu.Handle]*frontendFD)
	g.gated = make(map[*frontendFD]struct{})
	g.tsgs = make(map[nvgpu.Handle]channelGroup)
	// Until a scheduler says otherwise, hold the sandbox to its configured
	// share, or to all of the period if it has none. Starting at nothing would
	// stall a sandbox whose scheduler never appears.
	allowance := computeGatePeriod
	if percent > 0 && percent < 100 {
		allowance = time.Duration(uint64(computeGatePeriod) * percent / 100)
	}
	g.grant = gpusched.Grant{Period: computeGatePeriod, Allowance: allowance}
}

// setGrant replaces the window during which submission is permitted.
func (g *computeGate) setGrant(grant gpusched.Grant) {
	if grant.Period <= 0 {
		return
	}
	if grant.Allowance > grant.Period {
		grant.Allowance = grant.Period
	}
	g.grantMu.Lock()
	changed := g.grant != grant
	g.grant = grant
	g.grantMu.Unlock()
	if changed {
		log.Infof("nvproxy: GPU window is now %v of every %v at phase %v", grant.Allowance, grant.Period, grant.Phase)
	}
}

// currentGrant returns the window in effect.
func (g *computeGate) currentGrant() gpusched.Grant {
	g.grantMu.Lock()
	defer g.grantMu.Unlock()
	return g.grant
}

// restore resumes gating after the sandbox has been restored.
//
// The gate's state is saved with the sandbox, so the command buffers it had
// learned and the file descriptions it was gating come back with it, but the
// goroutine that revokes their mappings does not. Without restarting it a
// restored sandbox would keep its configured limit and stop being held to it.
//
// The percentage is the one the sandbox was created with rather than whatever
// the restoring command line specifies, as with the other limits nvproxy
// enforces.
func (g *computeGate) restore() {
	// Empty maps may come back nil.
	if g.cmdBufs == nil {
		g.cmdBufs = make(map[nvgpu.Handle]struct{})
	}
	if g.byMem == nil {
		g.byMem = make(map[nvgpu.Handle]*frontendFD)
	}
	if g.gated == nil {
		g.gated = make(map[*frontendFD]struct{})
	}
	if g.tsgs == nil {
		g.tsgs = make(map[nvgpu.Handle]channelGroup)
	}
	if g.grant.Period <= 0 {
		g.grant = gpusched.Grant{Period: computeGatePeriod, Allowance: computeGatePeriod}
	}
	if g.enabled() {
		log.Infof("nvproxy: resuming GPU compute limit after restore")
		go g.run()
	}
}

// enabled returns true if g limits submission.
func (g *computeGate) enabled() bool {
	return g.scheduled || (g.percent != 0 && g.percent < 100)
}

// noteMapping records that the memory object h is mapped through fd.
//
// An application maps a channel's command buffer before creating the channel
// that identifies it as one, so the association is recorded here and resolved
// by addCommandBuffer() once the channel appears.
func (g *computeGate) noteMapping(h nvgpu.Handle, fd *frontendFD) {
	if !g.enabled() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.byMem[h] = fd
	if _, ok := g.cmdBufs[h]; ok {
		g.gateLocked(h, fd)
	}
}

// addCommandBuffer records that h is a memory object holding a channel's
// command buffer, and begins gating whichever file description maps it.
func (g *computeGate) addCommandBuffer(h nvgpu.Handle) {
	if !g.enabled() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cmdBufs[h] = struct{}{}
	if fd, ok := g.byMem[h]; ok {
		g.gateLocked(h, fd)
	}
}

// Preconditions: g.mu must be locked.
func (g *computeGate) gateLocked(h nvgpu.Handle, fd *frontendFD) {
	if _, ok := g.gated[fd]; ok {
		return
	}
	g.gated[fd] = struct{}{}
	fd.gated.Store(true)
	log.Infof("nvproxy: gating GPU submission through command buffer %v", h)
}

// channelGroup identifies one of the sandbox's channel groups well enough to
// issue an RM control against it: the file description the control must be
// sent through, and the client that owns the object.
//
// +stateify savable
type channelGroup struct {
	fd      *frontendFD
	hClient nvgpu.Handle
}

// addChannelGroup records a channel group allocated by the sandbox, so that it
// can be preempted when the sandbox's window closes.
func (g *computeGate) addChannelGroup(fd *frontendFD, hClient, hObject nvgpu.Handle) {
	if !g.enabled() || !g.preempt {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tsgs[hObject] = channelGroup{fd: fd, hClient: hClient}
}

// removeChannelGroup drops a channel group the sandbox has freed. Preempting a
// freed object would merely fail, but retaining the handle would let a later
// allocation reuse it and be preempted on the strength of a stale record.
func (g *computeGate) removeChannelGroup(hObject nvgpu.Handle) {
	if !g.enabled() || !g.preempt {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.tsgs, hObject)
}

// forget stops gating fd and drops any record of it, and is called when fd is
// released so that the gate does not retain it.
func (g *computeGate) forget(fd *frontendFD) {
	if !g.enabled() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.gated, fd)
	fd.mappedMemMu.Lock()
	h := fd.mappedMem
	fd.mappedMemMu.Unlock()
	if g.byMem[h] == fd {
		delete(g.byMem, h)
	}
	// Channel groups reached through this file description can no longer be
	// preempted through it, and its host FD is about to become invalid.
	for hObject, tsg := range g.tsgs {
		if tsg.fd == fd {
			delete(g.tsgs, hObject)
		}
	}
}

// waitUntil returns how long a submission arriving at time t must wait before
// it is permitted, which is zero if t falls within the sandbox's window.
//
// The window begins at the grant's phase rather than at the start of the period,
// so that sandboxes sharing a GPU take turns instead of all contending during
// the same part of every period.
func (g *computeGate) waitUntil(t time.Time) time.Duration {
	if !g.enabled() {
		return 0
	}
	grant := g.currentGrant()
	if grant.Allowance >= grant.Period {
		return 0
	}
	pos := time.Duration(t.UnixNano()) % grant.Period
	switch {
	case pos < grant.Phase:
		// Before the window opens.
		return grant.Phase - pos
	case pos < grant.Phase+grant.Allowance:
		// Within it.
		return 0
	default:
		// Past it; wait for the window in the next period.
		return grant.Period - pos + grant.Phase
	}
}

// wait blocks the calling task until the sandbox is permitted to submit work.
//
// The wait is against a deadline rather than a signal from revoke(): this is
// called from memmap.Mappable.Translate, which runs with the address space's
// activeMu held for writing, and revoke() needs that same lock to invalidate
// mappings. A task that waited to be woken by revoke() would deadlock with it,
// whereas a task waiting on a deadline leaves revoke() merely delayed until the
// task wakes and releases the lock.
//
// The sleep is uninterruptible so that the sandbox cannot escape its limit by
// arranging to be signalled, and is bounded by one period.
func (g *computeGate) wait(ctx context.Context) {
	g.noteSubmission()
	d := g.waitUntil(time.Now())
	if d <= 0 {
		return
	}
	ctx.UninterruptibleSleepStart()
	time.Sleep(d)
	ctx.UninterruptibleSleepFinish()
}

// run revokes the gated mappings at the end of each period, so that the next
// submission faults into the Sentry and is held by wait().
//
// It runs until the sandbox exits; there is no shutdown path because a Sentry
// outlives every sandbox process that could be gated.
func (g *computeGate) run() {
	for {
		time.Sleep(g.untilWindowEnd(time.Now()))
		// Revoke before preempting, not after. Revoking first shuts the door on
		// new submissions, so that work evicted by the preempt cannot simply be
		// replaced in the gap between the two.
		g.revoke()
		g.preemptAll()
	}
}

// preemptChannelGroupTimeout is how long a preempt may take before nvproxy
// stops waiting for it.
//
// The driver permits up to NVA06C_CTRL_CMD_PREEMPT_MAX_MANUAL_TIMEOUT_US, a
// full second, which is ten of our periods: a single slow preempt would do more
// damage to the schedule than the work it was trying to evict. This is chosen
// instead to be affordable within one window while still being long enough that
// a preempt which is merely slow is not abandoned.
const preemptChannelGroupTimeout = 5 * time.Millisecond

// preemptAll asks the GPU to switch each of the sandbox's channel groups off
// its engine, evicting work that is already running.
//
// This is what revoking mappings cannot do. Revocation stops the sandbox
// submitting anything new, but says nothing about what the GPU is already
// executing on its behalf; a single long kernel submitted just before the
// window closed otherwise runs on into other sandboxes' windows.
//
// Failures are logged once and otherwise ignored. A driver that does not
// support the control, or a channel group freed between the snapshot below and
// the call, must not cost every tenant on the machine access to the GPU --
// isolation falls back to revocation alone, which is where it was before.
func (g *computeGate) preemptAll() {
	if !g.preempt {
		return
	}
	g.mu.Lock()
	tsgs := make(map[nvgpu.Handle]channelGroup, len(g.tsgs))
	for hObject, tsg := range g.tsgs {
		tsgs[hObject] = tsg
	}
	g.mu.Unlock()
	for hObject, tsg := range tsgs {
		if err := preemptChannelGroup(tsg.fd, tsg.hClient, hObject); err != nil {
			if !g.warnedPreempt.Swap(true) {
				log.Warningf("nvproxy: failed to preempt GPU channel group %v: %v; "+
					"GPU time is bounded by revoking submission alone, so work already running may overrun the sandbox's window", hObject, err)
			}
			continue
		}
		// Say so once. Without this there is no way to tell a working preempt
		// from one that never ran because the sandbox had no channel groups to
		// preempt -- both are silent, and only one of them is enforcement.
		if !g.notedPreempt.Swap(true) {
			log.Infof("nvproxy: preempted GPU channel group %v at the end of the sandbox's window", hObject)
		}
	}
}

// untilWindowEnd returns how long until the end of the window containing or
// following t.
//
// Mappings are revoked exactly then, so that a sandbox is shut out for the
// remainder of the period. Revoking at any other point would let an application
// that faulted early run on past the end of its window.
func (g *computeGate) untilWindowEnd(t time.Time) time.Duration {
	grant := g.currentGrant()
	if grant.Period <= 0 {
		return computeGatePeriod
	}
	end := grant.Phase + grant.Allowance
	pos := time.Duration(t.UnixNano()) % grant.Period
	if pos < end {
		return end - pos
	}
	return grant.Period - pos + end
}

// revoke invalidates every mapping of every gated file description.
func (g *computeGate) revoke() {
	g.mu.Lock()
	fds := make([]*frontendFD, 0, len(g.gated))
	for fd := range g.gated {
		fds = append(fds, fd)
	}
	g.mu.Unlock()
	for _, fd := range fds {
		fd.revokeMappings()
	}
}

// noteSubmission records that the sandbox tried to submit work, which is what
// the scheduler is told in order to distinguish a sandbox using the GPU from
// one merely holding a share of it.
func (g *computeGate) noteSubmission() {
	g.submissions.Add(1)
}

// follow takes the sandbox's window from a scheduler reached over fd.
//
// The exchange runs on its own goroutine and is driven by the scheduler: each
// window it sends is applied, and a report of what the sandbox did with the
// last one is sent back. Nothing on the submission path waits for any of it, so
// a scheduler that stops responding leaves the sandbox with the window it last
// had rather than stalling it.
func (g *computeGate) follow(schedFD int, id string, weight uint64, percent uint64) {
	// The Sentry may read and write an open connection, but not create one, so
	// this wraps the donated descriptor rather than dialling.
	f := os.NewFile(uintptr(schedFD), "gpu-scheduler")
	conn := gpusched.NewConn(f)

	var maxFraction float64
	if percent > 0 && percent < 100 {
		// A configured limit remains a ceiling that the scheduler may not
		// raise, so that being scheduled cannot grant more than was allowed.
		maxFraction = float64(percent) / 100
	}
	if id == "" {
		// The scheduler only needs to tell sandboxes apart, and the container
		// ID is not yet known this early in a sandbox's life. The process ID
		// is unique among the sandboxes on a host, which is all that is
		// required.
		id = fmt.Sprintf("sandbox-%d", os.Getpid())
	}
	go func() {
		defer conn.Close()
		// No process ID is sent: this sandbox runs in a namespace of its own
		// where it is process 1, while the GPU driver reports the number it
		// has on the host. runsc announces that separately.
		hello := gpusched.Hello{ID: id, Weight: weight, MaxFraction: maxFraction}
		if err := conn.SendHello(hello); err != nil {
			log.Warningf("nvproxy: announcing to the GPU scheduler: %v", err)
			return
		}
		for {
			a, err := conn.RecvAssignment()
			if err != nil {
				log.Warningf("nvproxy: GPU scheduler connection lost, keeping the last window: %v", err)
				return
			}
			g.setGrant(a.Grant())
			if err := conn.SendReport(gpusched.Report{Submissions: g.submissions.Swap(0)}); err != nil {
				log.Warningf("nvproxy: reporting to the GPU scheduler: %v", err)
				return
			}
		}
	}()
}
