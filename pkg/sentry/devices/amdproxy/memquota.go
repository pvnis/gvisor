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
	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sync"
)

// memKind is the sort of memory an allocation consumes.
type memKind int

const (
	// memKindNone is memory that is not accounted. Doorbells and the MMIO
	// remap page are register windows a page or two in size rather than
	// memory, and a userptr allocation registers memory the application
	// already has, which the sandbox's own memory limits already cover.
	memKindNone memKind = iota

	// memKindVRAM is memory on the device. This is what the GPU memory limit
	// bounds, and what a workload means when it asks for a GPU with so much
	// memory.
	memKindVRAM

	// memKindHostPinned is system memory made accessible to the GPU (GTT).
	// It is tracked but not limited: it is the sandbox's own memory, already
	// bounded by the sandbox's memory limits, and counting it against the
	// device's memory would deny allocations that consume none.
	memKindHostPinned
)

// memKindOfFlags returns the kind of memory an AMDKFD_IOC_ALLOC_MEMORY_OF_GPU
// request consumes.
//
// The tests are ordered so that the flags describing where memory comes from
// win over the flags describing what it is for. A userptr allocation also
// carries no VRAM bit, but is checked first because it allocates nothing at
// all.
func memKindOfFlags(flags uint32) memKind {
	switch {
	case flags&amdgpu.KFD_IOC_ALLOC_MEM_FLAGS_USERPTR != 0:
		return memKindNone
	case flags&(amdgpu.KFD_IOC_ALLOC_MEM_FLAGS_DOORBELL|amdgpu.KFD_IOC_ALLOC_MEM_FLAGS_MMIO_REMAP) != 0:
		return memKindNone
	case flags&amdgpu.KFD_IOC_ALLOC_MEM_FLAGS_VRAM != 0:
		return memKindVRAM
	case flags&amdgpu.KFD_IOC_ALLOC_MEM_FLAGS_GTT != 0:
		return memKindHostPinned
	default:
		return memKindNone
	}
}

// memCharge is what an allocation was charged, remembered so that freeing it
// returns the same amount to the same counter. The driver's free ioctl names
// only a handle, so the size and kind have to be kept here.
//
// +stateify savable
type memCharge struct {
	kind memKind
	size uint64
}

// memAccount tracks GPU memory charged to this sandbox. It is shared by every
// device and file description in the sandbox, so it accounts the sandbox's
// aggregate usage across all of its processes.
//
// Because the limit is applied here, where the application's ioctls are
// interpreted, sandboxed code cannot observe or bypass it: there is no
// in-container library to unset, preload around, or call past.
//
// +stateify savable
type memAccount struct {
	mu sync.Mutex `state:"nosave"`

	// gpuLimit is the maximum number of bytes of device memory the sandbox
	// may have allocated at once. Zero means no limit. Immutable.
	gpuLimit uint64

	// vram and hostPinned are the bytes currently charged of each kind.
	// +checklocks:mu
	vram uint64
	// +checklocks:mu
	hostPinned uint64

	// charges maps a driver buffer handle to what it was charged.
	// +checklocks:mu
	charges map[uint64]memCharge

	// warned records whether a denial has already been logged, so that a
	// workload that allocates in a loop does not fill the log.
	// +checklocks:mu
	warned bool
}

func (ma *memAccount) init(gpuLimit uint64) {
	ma.gpuLimit = gpuLimit
	ma.mu.Lock()
	defer ma.mu.Unlock()
	ma.charges = make(map[uint64]memCharge)
}

// limited returns whether a limit is in effect.
func (ma *memAccount) limited() bool {
	return ma.gpuLimit != 0
}

// admit charges size bytes of the given kind, returning false if that would
// exceed the sandbox's limit.
//
// Allocations are charged before being forwarded to the driver, so that two
// concurrent allocations cannot both be admitted against the same headroom.
// If the driver then refuses the allocation, the caller releases the charge.
func (ma *memAccount) admit(ctx context.Context, kind memKind, size uint64) bool {
	if kind == memKindNone {
		return true
	}
	ma.mu.Lock()
	defer ma.mu.Unlock()
	switch kind {
	case memKindVRAM:
		if ma.gpuLimit != 0 && ma.vram+size > ma.gpuLimit {
			if !ma.warned {
				ma.warned = true
				// The ROCm runtime reports an allocation failure as an opaque
				// error, so without this an operator has nothing to go on.
				ctx.Warningf("amdproxy: GPU memory limit of %d bytes reached; %d bytes are in use and %d more were requested", ma.gpuLimit, ma.vram, size)
			}
			return false
		}
		ma.vram += size
	case memKindHostPinned:
		ma.hostPinned += size
	}
	return true
}

// release returns a charge taken by admit that no allocation ended up using.
func (ma *memAccount) release(kind memKind, size uint64) {
	if kind == memKindNone {
		return
	}
	ma.mu.Lock()
	defer ma.mu.Unlock()
	ma.releaseLocked(kind, size)
}

// +checklocks:ma.mu
func (ma *memAccount) releaseLocked(kind memKind, size uint64) {
	switch kind {
	case memKindVRAM:
		if ma.vram < size {
			ma.vram = 0
		} else {
			ma.vram -= size
		}
	case memKindHostPinned:
		if ma.hostPinned < size {
			ma.hostPinned = 0
		} else {
			ma.hostPinned -= size
		}
	}
}

// record associates a charge with the handle the driver returned, so that
// freeing that handle returns it.
func (ma *memAccount) record(handle uint64, kind memKind, size uint64) {
	if kind == memKindNone {
		return
	}
	ma.mu.Lock()
	defer ma.mu.Unlock()
	// A handle the driver has just returned should not already be in use. If
	// it is, this package's view has drifted from the driver's, and keeping
	// the stale charge would leak it for the sandbox's lifetime.
	if old, ok := ma.charges[handle]; ok {
		ma.releaseLocked(old.kind, old.size)
	}
	ma.charges[handle] = memCharge{kind: kind, size: size}
}

// forget releases the charge associated with a handle. It is a no-op for a
// handle that was never charged, which is the case for the kinds of memory
// that are not accounted.
func (ma *memAccount) forget(handle uint64) {
	ma.mu.Lock()
	defer ma.mu.Unlock()
	c, ok := ma.charges[handle]
	if !ok {
		return
	}
	delete(ma.charges, handle)
	ma.releaseLocked(c.kind, c.size)
}

// availableVRAM returns how many more bytes of device memory the sandbox may
// allocate, and whether a limit applies at all.
func (ma *memAccount) availableVRAM() (uint64, bool) {
	if !ma.limited() {
		return 0, false
	}
	ma.mu.Lock()
	defer ma.mu.Unlock()
	if ma.vram >= ma.gpuLimit {
		return 0, true
	}
	return ma.gpuLimit - ma.vram, true
}
