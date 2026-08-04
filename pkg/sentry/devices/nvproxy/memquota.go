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
	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
)

// memKind distinguishes kinds of memory that are accounted separately.
//
// GPU device memory and host memory pinned for GPU DMA are deliberately not
// conflated: they are drawn from different pools, and charging one against the
// other's budget would produce both spurious rejections and unaccounted usage.
type memKind int

const (
	// memKindNone indicates an object that holds no accounted memory. This
	// includes objects representing purely virtual address space, which
	// commits no physical memory until it is backed.
	memKindNone memKind = iota

	// memKindVRAM is GPU device memory.
	memKindVRAM

	// memKindPinnedHost is host memory pinned for access by the GPU.
	memKindPinnedHost

	// memKindUVMVA is address space reserved on /dev/nvidia-uvm, which backs
	// CUDA unified ("managed") memory.
	//
	// Unlike the other kinds, this is a reservation rather than a measurement
	// of committed memory: UVM commits device memory lazily, in response to
	// GPU page faults serviced entirely within the host nvidia-uvm module,
	// which calls into the resource manager through in-kernel symbols rather
	// than through ioctls. The sentry therefore cannot observe the commitment
	// itself, only the address space it may consume.
	//
	// Charging the reservation is sound as a bound, because committed memory
	// can never exceed the address space reserved for it. It is not tight: a
	// workload that deliberately oversubscribes, reserving far more than it
	// backs, is charged for what it reserved.
	memKindUVMVA
)

// String implements fmt.Stringer.String.
func (k memKind) String() string {
	switch k {
	case memKindNone:
		return "none"
	case memKindVRAM:
		return "vram"
	case memKindPinnedHost:
		return "pinned-host"
	case memKindUVMVA:
		return "uvm-va"
	default:
		return "unknown"
	}
}

// countsAgainstGPULimit returns true if memory of this kind consumes the GPU
// memory budget. Host memory pinned for DMA does not: it is host memory, and
// is bounded by the sandbox's memory limit instead.
func (k memKind) countsAgainstGPULimit() bool {
	return k == memKindVRAM || k == memKindUVMVA
}

// memKindOfClass returns the kind of memory held by driver objects of the
// given class.
//
// Classes not listed here hold no accounted memory, or are not yet accounted;
// see the "Unaccounted" note below. Implementors adding a new allocation class
// to version.go must extend this function if the class commits memory.
func memKindOfClass(class nvgpu.ClassID) memKind {
	switch class {
	case nvgpu.NV01_MEMORY_LOCAL_USER, nvgpu.NV01_MEMORY_LOCAL_PRIVILEGED:
		// Device memory. Allocated by NV_ESC_RM_ALLOC, by
		// NV_ESC_RM_ALLOC_MEMORY, and by NV_ESC_RM_VID_HEAP_CONTROL (which
		// normalizes to this class in rmVidHeapControlAllocSize()).
		return memKindVRAM

	case nvgpu.NV01_MEMORY_SYSTEM, nvgpu.NV01_MEMORY_SYSTEM_OS_DESCRIPTOR:
		// Host memory pinned for GPU DMA, not device memory, despite being
		// reached through the same allocation ioctls.
		return memKindPinnedHost

	case nvgpu.NV01_MEMORY_VIRTUAL, nvgpu.NV50_MEMORY_VIRTUAL:
		// Virtual address space only. Physical memory is committed by the
		// allocations that back these ranges, and is accounted there; charging
		// the reservation as well would double-count.
		return memKindNone

	default:
		// Unaccounted:
		//
		// - NV_MEMORY_EXTENDED_USER (EGM) is device-adjacent memory whose
		// backing pool depends on platform configuration.
		//
		// - NV_MEMORY_FABRIC, NV_MEMORY_MULTICAST_FABRIC and
		// NV_MEMORY_FABRIC_IMPORTED_REF may be backed by local device memory
		// or by memory imported from another node, which determines whether
		// this sandbox should be charged at all.
		//
		// Both require validation against hardware before being charged; a
		// wrong answer here silently over- or under-counts. They are left
		// unaccounted deliberately rather than guessed at, and must be
		// resolved before quota enforcement can claim completeness.
		return memKindNone
	}
}

// memCharge records memory charged to a sandbox's account.
//
// A single underlying driver allocation may be referred to by more than one
// nvproxy object: NV_ESC_RM_DUP_OBJECT duplicates a memory handle into another
// client, and objDup() records the duplicate as a distinct object aliasing the
// same memory. A memCharge is therefore shared by all objects aliasing one
// allocation and reference-counted, so that the allocation is charged exactly
// once however many handles refer to it, and is released only when the last
// handle goes away.
//
// +stateify savable
type memCharge struct {
	// kind and size are immutable.
	kind memKind
	size uint64

	// refs is the number of objects aliasing this charge. When refs reaches
	// zero the charge is released from the account.
	refs atomicbitops.Int64
}

// incRef records a new object aliasing c.
func (c *memCharge) incRef() {
	if c == nil {
		return
	}
	c.refs.Add(1)
}

// memAccount tracks memory charged to a sandbox and enforces its limit.
//
// A mutex is used rather than atomics because the GPU memory limit spans more
// than one counter: device memory and reserved unified-memory address space
// both consume the GPU's memory, so a limit check across separate atomics
// could not be made atomic with the reservation that follows it, and
// concurrent allocations could both be admitted against headroom that only one
// of them can have.
//
// mu is a leaf: no other lock is acquired while it is held.
//
// +stateify savable
type memAccount struct {
	mu sync.Mutex `state:"nosave"`

	// The following fields are protected by mu.
	vram       uint64
	pinnedHost uint64
	uvmVA      uint64

	// gpuLimit is the maximum number of bytes of GPU memory, being the sum of
	// vram and uvmVA, that may be charged to this sandbox. Zero means no
	// limit. gpuLimit is immutable after Register().
	gpuLimit uint64
}

// Preconditions: a.mu must be locked.
func (a *memAccount) addLocked(kind memKind, delta uint64) uint64 {
	switch kind {
	case memKindVRAM:
		a.vram += delta
		return a.vram
	case memKindPinnedHost:
		a.pinnedHost += delta
		return a.pinnedHost
	case memKindUVMVA:
		a.uvmVA += delta
		return a.uvmVA
	}
	return 0
}

// admitLocked returns true if size bytes of the given kind may be charged
// without exceeding the account's limit.
//
// Preconditions: a.mu must be locked.
func (a *memAccount) admitLocked(kind memKind, size uint64) bool {
	if a.gpuLimit == 0 || !kind.countsAgainstGPULimit() {
		return true
	}
	// Written so that a size chosen by the application cannot overflow the
	// comparison and be wrongly admitted.
	used := a.vram + a.uvmVA
	return size <= a.gpuLimit && used <= a.gpuLimit-size
}

// reserve charges size bytes of the given kind against a, unless doing so
// would exceed the account's limit, and returns the resulting charge. It
// returns nil, false if the limit would be exceeded, in which case nothing is
// charged. A size of zero, or a kind that holds no accounted memory, charges
// nothing and succeeds.
//
// The charge is taken before the allocation is forwarded to the driver, so
// that concurrent allocations cannot both be admitted against the same
// headroom. If the driver then fails the allocation, the caller must return
// the charge with release().
func (a *memAccount) reserve(ctx context.Context, kind memKind, size uint64) (*memCharge, bool) {
	if size == 0 || kind == memKindNone {
		return nil, true
	}
	a.mu.Lock()
	if !a.admitLocked(kind, size) {
		used, limit := a.vram+a.uvmVA, a.gpuLimit
		a.mu.Unlock()
		if ctx.IsLogging(log.Debug) {
			ctx.Debugf("nvproxy: denied %d bytes of %s memory: %d of %d bytes of GPU memory already in use", size, kind, used, limit)
		}
		return nil, false
	}
	total := a.addLocked(kind, size)
	a.mu.Unlock()
	if ctx.IsLogging(log.Debug) {
		ctx.Debugf("nvproxy: charged %d bytes of %s memory, total %d", size, kind, total)
	}
	return &memCharge{
		kind: kind,
		size: size,
		refs: atomicbitops.FromInt64(1),
	}, true
}

// reserveForClass is reserve() for an allocation of the given class.
func (a *memAccount) reserveForClass(ctx context.Context, class nvgpu.ClassID, size uint64) (*memCharge, bool) {
	return a.reserve(ctx, memKindOfClass(class), size)
}

// release drops a reference on c, releasing its charge from a if it was the
// last reference. release does nothing if c is nil.
func (a *memAccount) release(ctx context.Context, c *memCharge) {
	if c == nil {
		return
	}
	if remaining := c.refs.Add(-1); remaining > 0 {
		return
	} else if remaining < 0 {
		// The charge has already been released; releasing it again would
		// subtract it from the account twice, permanently understating usage.
		log.Traceback("nvproxy: memCharge released more times than referenced")
		return
	}
	a.mu.Lock()
	total := a.addLocked(c.kind, ^(c.size - 1))
	a.mu.Unlock()
	if ctx.IsLogging(log.Debug) {
		ctx.Debugf("nvproxy: released %d bytes of %s memory, total %d", c.size, c.kind, total)
	}
}

// reserveUVMVA charges size bytes of newly reserved UVM address space,
// enforcing the GPU memory limit. It returns false if the limit would be
// exceeded, in which case nothing is charged.
//
// Unlike reserve(), this returns no charge: UVM reservations are tracked by
// the mapping set of the /dev/nvidia-uvm file description, which accounts for
// overlapping and duplicated mappings and reports only the ranges whose mapped
// status actually changed.
func (a *memAccount) reserveUVMVA(ctx context.Context, size uint64) bool {
	if size == 0 {
		return true
	}
	a.mu.Lock()
	if !a.admitLocked(memKindUVMVA, size) {
		used, limit := a.vram+a.uvmVA, a.gpuLimit
		a.mu.Unlock()
		if ctx.IsLogging(log.Debug) {
			ctx.Debugf("nvproxy: denied %d bytes of %s: %d of %d bytes of GPU memory already in use", size, memKindUVMVA, used, limit)
		}
		return false
	}
	total := a.addLocked(memKindUVMVA, size)
	a.mu.Unlock()
	if ctx.IsLogging(log.Debug) {
		ctx.Debugf("nvproxy: charged %d bytes of %s, total %d", size, memKindUVMVA, total)
	}
	return true
}

// releaseUVMVA releases size bytes of UVM address space that is no longer
// reserved.
func (a *memAccount) releaseUVMVA(ctx context.Context, size uint64) {
	if size == 0 {
		return
	}
	a.mu.Lock()
	total := a.addLocked(memKindUVMVA, ^(size - 1))
	a.mu.Unlock()
	if ctx.IsLogging(log.Debug) {
		ctx.Debugf("nvproxy: released %d bytes of %s, total %d", size, memKindUVMVA, total)
	}
}

// usage returns the memory currently charged to a, in bytes, by kind.
func (a *memAccount) usage() (vram, pinnedHost, uvmVA uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.vram, a.pinnedHost, a.uvmVA
}
