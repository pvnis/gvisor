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

// Sharing one KFD GPU address space between the processes of a sandbox.
//
// # The limitation this works around
//
// KFD keys its per-process state (struct kfd_process) on the mm of the process
// that calls it. Under --platform=kvm the caller is the Sentry, which is one
// host process for the whole sandbox, so every sandboxed process that opens
// /dev/kfd gets its own host file descriptor but lands on a single shared
// kfd_process. AMDKFD_IOC_ACQUIRE_VM binds a GPU address space to that
// structure and refuses to bind a second one, so:
//
//	the first sandboxed process to initialise the GPU succeeds, and every
//	process after it fails with EBUSY.
//
// It does not recover when the first process exits. KFD deliberately keeps the
// kfd_process alive for as long as the mm exists, because queues may outlive
// the file descriptor that created them, and the Sentry's mm lives as long as
// the sandbox.
//
// Measured on sens1's Navi 32: the same command run twice in one sandbox gives
// INIT OK then EBUSY on ACQUIRE_VM, while the identical pod under runc gives
// INIT OK twice. This is therefore the Sentry's process identity, not the
// driver refusing a second client.
//
// This is the same root as the two limits already recorded for this package —
// the KFD mmap process binding, and SVM's EFAULT on guest virtual addresses.
// All three are the driver seeing the Sentry where it expects the application.
//
// # What this does about it
//
// With sharing enabled, a second acquisition of a GPU whose VM this sandbox
// already holds reports success without forwarding anything, and the caller
// proceeds against the address space the first process acquired.
//
// # The risk, and why the guard below exists
//
// Sharing is not free: the processes are now allocating out of one GPU address
// space while each believes it has its own. Nothing stops two of them choosing
// the same GPU virtual address, and the loser would silently read and write
// another process's buffers.
//
// So this does not merely hope. vaGuard records the range every accounted
// allocation occupies together with the process that made it, and refuses an
// allocation that would overlap a range belonging to a different process. A
// refused allocation is an ordinary out-of-memory error the runtime already
// handles, and it names the collision in the log; silent aliasing would be
// neither visible nor recoverable.
//
// The blast radius is bounded to the sandbox that opts in. Every sandbox has
// its own Sentry and therefore its own kfd_process, so nothing here is shared
// between containers and this is not a weakening of isolation between tenants.
// It is a correctness risk taken by, and confined to, the container that
// enables it — which is why it is off by default.

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sync"
)

// renderShare hands every process in the sandbox the same open render node.
//
// Sharing the address space alone is not enough, and the reason is worth
// stating because it is not obvious. ACQUIRE_VM binds the address space owned
// by a particular render node *file* to the sandbox's KFD process. Buffers
// allocated afterwards belong to that file: DRM decides whether an mmap is
// allowed by looking the offset up in the file it is called on
// (drm_vma_node_is_allowed), so a second process that opened its own render
// node cannot map them. Measured: with the address space shared but the file
// not, the second process got past ACQUIRE_VM and then failed with
//
//	mmap(..., MAP_SHARED|MAP_FIXED, <render fd>, 0x10004c000) = -1 EACCES
//
// at an offset just above DRM's 0x100000000 mmap base, which is what a GEM
// object's offset looks like.
//
// So later opens return a dup of the first one. A dup shares the underlying
// open file, and therefore the drm_file that owns the buffers and the address
// space, while still giving each file description a descriptor it alone
// closes. The canonical descriptor is held for the sandbox's lifetime, since
// the address space bound to it outlives any one process that used it.
//
// GEM handles are allocated by the driver per open file, so sharing one file
// gives the processes one handle namespace rather than colliding namespaces.
// GPU virtual addresses are chosen by the runtimes and are not protected this
// way, which is what vaGuard is for.
//
// +stateify savable
type renderShare struct {
	// enabled reports whether sharing was asked for. Immutable.
	enabled bool

	mu sync.Mutex `state:"nosave"`

	// canonical maps a render node's minor number to a host descriptor kept
	// open for the sandbox's lifetime, which later opens are duplicated from.
	// +checklocks:mu
	canonical map[uint32]int32

	// names remembers the container each node was opened for, which a dup
	// cannot recover and which the file description reports.
	// +checklocks:mu
	names map[uint32]string
}

func (rs *renderShare) init(enabled bool) {
	rs.enabled = enabled
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.canonical = make(map[uint32]int32)
	rs.names = make(map[uint32]string)
}

// open returns a host descriptor for the render node with the given minor
// number, calling openHost at most once per minor for the sandbox's lifetime.
//
// With sharing off it is exactly openHost, so the ordinary path keeps one open
// file per descriptor as before.
func (rs *renderShare) open(minor uint32, openHost func() (int32, string, error)) (int32, string, error) {
	if !rs.enabled {
		return openHost()
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if hostFD, ok := rs.canonical[minor]; ok {
		dup, err := unix.Dup(int(hostFD))
		if err != nil {
			return -1, "", err
		}
		return int32(dup), rs.names[minor], nil
	}
	hostFD, containerName, err := openHost()
	if err != nil {
		return -1, "", err
	}
	// Keep a descriptor of our own so that the open file survives every
	// sandboxed process closing theirs. The address space KFD bound to this
	// file is the sandbox's only one, and reopening the node would not get it
	// back.
	keep, err := unix.Dup(int(hostFD))
	if err != nil {
		unix.Close(int(hostFD))
		return -1, "", err
	}
	rs.canonical[minor] = int32(keep)
	rs.names[minor] = containerName
	return hostFD, containerName, nil
}

// vmShare tracks which GPUs this sandbox has already acquired a VM for, so
// that a later acquisition by another of its processes can be answered from
// here instead of being refused by the driver.
//
// +stateify savable
type vmShare struct {
	// enabled reports whether sharing was asked for. Immutable.
	enabled bool

	mu sync.Mutex `state:"nosave"`

	// owners maps a GPU ID to the thread group that acquired its address
	// space. Only the first acquisition reaches the driver.
	// +checklocks:mu
	owners map[uint32]kernel.ThreadID

	// warned records the GPUs a share has already been logged for, so a
	// workload that initialises repeatedly does not fill the log.
	// +checklocks:mu
	warned map[uint32]bool
}

func (vs *vmShare) init(enabled bool) {
	vs.enabled = enabled
	vs.mu.Lock()
	defer vs.mu.Unlock()
	vs.owners = make(map[uint32]kernel.ThreadID)
	vs.warned = make(map[uint32]bool)
}

// shareFor reports whether an acquisition of gpuID by tgid should be answered
// with success without reaching the driver, which is the case when a different
// process in this sandbox already holds that GPU's address space.
func (vs *vmShare) shareFor(ctx context.Context, gpuID uint32, tgid kernel.ThreadID) bool {
	if !vs.enabled {
		return false
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	owner, ok := vs.owners[gpuID]
	if !ok || owner == tgid {
		return false
	}
	if !vs.warned[gpuID] {
		vs.warned[gpuID] = true
		ctx.Warningf("amdproxy: sharing the GPU %d address space acquired by thread group %d with thread group %d, "+
			"because a sandbox has one KFD process context and the driver binds only one address space to it. "+
			"These processes now allocate from one address space; overlapping allocations will be refused rather than aliased",
			gpuID, owner, tgid)
	}
	return true
}

// claim records that tgid has acquired gpuID's address space. It is called
// only after the driver has accepted the acquisition.
func (vs *vmShare) claim(gpuID uint32, tgid kernel.ThreadID) {
	if !vs.enabled {
		return
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if _, ok := vs.owners[gpuID]; !ok {
		vs.owners[gpuID] = tgid
	}
}

// runtimeShare makes AMDKFD_IOC_RUNTIME_ENABLE idempotent across the
// processes of a sandbox.
//
// The debug runtime is another piece of state the driver keeps once per KFD
// process, and enabling it twice earns EBUSY. It is what ROCr turns on during
// initialisation, so with the address space shared it becomes the next thing
// to fail: measured, the second process got past ACQUIRE_VM and the shared
// render node and then failed on
//
//	ioctl(3, _IOC(_IOC_READ|_IOC_WRITE, 0x4b, 0x25, 0x10), ...) = -1 EBUSY
//
// This is a gentler share than the address space. The caller asks for a state
// that is already true, so reporting success tells it no lie and there is
// nothing to alias. What has to be handled is the other direction: the ioctl
// also disables, and one process tearing the runtime down while another is
// still using it would break the other. So the runtime is taken down only when
// the last process holding it lets go.
//
// +stateify savable
type runtimeShare struct {
	// enabled reports whether sharing was asked for. Immutable.
	enabled bool

	mu sync.Mutex `state:"nosave"`

	// holders is the set of thread groups that have enabled the runtime and
	// not disabled it.
	// +checklocks:mu
	holders map[kernel.ThreadID]struct{}
}

func (rs *runtimeShare) init(enabled bool) {
	rs.enabled = enabled
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.holders = make(map[kernel.ThreadID]struct{})
}

// enter reports whether an enable request should be answered here rather than
// forwarded, which is so once another process has already enabled the runtime.
func (rs *runtimeShare) enter(tgid kernel.ThreadID) bool {
	if !rs.enabled {
		return false
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	shared := len(rs.holders) > 0
	rs.holders[tgid] = struct{}{}
	return shared
}

// enterFailed undoes enter for a caller whose forwarded request the driver
// refused, so that a failed initialisation does not leave the runtime looking
// held.
func (rs *runtimeShare) enterFailed(tgid kernel.ThreadID) {
	if !rs.enabled {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	delete(rs.holders, tgid)
}

// leave reports whether a disable request should be answered here rather than
// forwarded, which is so while any other process still holds the runtime.
func (rs *runtimeShare) leave(tgid kernel.ThreadID) bool {
	if !rs.enabled {
		return false
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	delete(rs.holders, tgid)
	return len(rs.holders) > 0
}

// vaRange is the GPU address range one allocation occupies, and the process
// that asked for it.
//
// +stateify savable
type vaRange struct {
	start uint64
	end   uint64
	owner kernel.ThreadID
}

// vaGuard refuses an allocation that would overlap one already made by a
// different process in this sandbox.
//
// It exists only because vmShare lets several processes allocate out of one
// GPU address space. Without sharing there is nothing to collide: the driver
// gives each sandbox one address space and refuses the rest.
//
// Ranges are keyed by the driver's buffer handle so that freeing an allocation
// drops its range, exactly as the memory account drops its charge.
//
// +stateify savable
type vaGuard struct {
	// enabled reports whether VM sharing is on, and so whether there is
	// anything to guard against. Immutable.
	enabled bool

	mu sync.Mutex `state:"nosave"`

	// ranges maps a driver buffer handle to the range it occupies.
	// +checklocks:mu
	ranges map[uint64]vaRange
}

func (vg *vaGuard) init(enabled bool) {
	vg.enabled = enabled
	vg.mu.Lock()
	defer vg.mu.Unlock()
	vg.ranges = make(map[uint64]vaRange)
}

// admit reports whether an allocation of size bytes at va may proceed. A zero
// size, or a zero address, is nothing this can reason about and is allowed
// through: the driver remains the authority on whether the request is valid.
func (vg *vaGuard) admit(ctx context.Context, va, size uint64, tgid kernel.ThreadID) bool {
	if !vg.enabled || va == 0 || size == 0 {
		return true
	}
	end := va + size
	if end < va {
		// An overflowing range cannot be a real allocation, and comparing it
		// against anything would be meaningless. Let the driver reject it.
		return true
	}
	vg.mu.Lock()
	defer vg.mu.Unlock()
	for _, r := range vg.ranges {
		if r.owner == tgid || end <= r.start || r.end <= va {
			continue
		}
		ctx.Warningf("amdproxy: refusing an allocation at GPU address %#x-%#x for thread group %d because it overlaps "+
			"%#x-%#x held by thread group %d in the same sandbox. These processes share one GPU address space; see "+
			"--amdproxy-share-kfd-vm", va, end, tgid, r.start, r.end, r.owner)
		return false
	}
	return true
}

// record associates a range with the handle the driver returned.
func (vg *vaGuard) record(handle, va, size uint64, tgid kernel.ThreadID) {
	if !vg.enabled || va == 0 || size == 0 {
		return
	}
	end := va + size
	if end < va {
		return
	}
	vg.mu.Lock()
	defer vg.mu.Unlock()
	vg.ranges[handle] = vaRange{start: va, end: end, owner: tgid}
}

// forget drops the range recorded against a handle. It is a no-op for a handle
// that was never recorded.
func (vg *vaGuard) forget(handle uint64) {
	if !vg.enabled {
		return
	}
	vg.mu.Lock()
	defer vg.mu.Unlock()
	delete(vg.ranges, handle)
}
