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

// Giving back what an exited process held on the GPU.
//
// # Why the driver does not do it
//
// When an ordinary process exits, KFD tears down its kfd_process: queues,
// events, buffers and the debug runtime all go with it. That happens when the
// process's mm goes away, not when it closes /dev/kfd, because queues may
// outlive the descriptor that created them.
//
// In a sandbox the process KFD sees is the Sentry, whose mm lives as long as
// the sandbox. So when a sandboxed process exits, nothing it held on the GPU
// is released. Measured on two MI355X: a pod running the same HIP program
// twice saw its 4096 MiB quota at 1024 MiB free on the second run, because
// the first had allocated 3 GiB and exited without freeing it, as HIP
// programs ordinarily do. The second run failed with out-of-memory, and every
// later one would have too.
//
// # What is done instead
//
// Each /dev/kfd file description records what was created through it, and
// gives it back when it is released, which is when the last descriptor
// referring to it is closed: normally when its process exits. That is the
// point at which the driver would have done the same for a process of its
// own, and the boundary the Sentry can see.
//
// Queues go first, since a queue may still be reading the buffers. Buffers
// are unmapped from every GPU they were mapped to, which the driver requires
// before it will free them. The event signal page cannot be freed (the driver
// refuses with EPERM, because it keeps the page mapped for the life of the
// kfd_process) and stays charged; it is one small buffer per sandbox.
//
// Reclaiming is also what makes it safe for a later process to take over the
// GPU address space the exited one acquired; see vmShare.

import (
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sync"
)

// fdResources is what one /dev/kfd file description created in the driver
// and has not destroyed.
//
// +stateify savable
type fdResources struct {
	mu sync.Mutex `state:"nosave"`

	// queues holds the IDs of live queues.
	// +checklocks:mu
	queues map[uint32]struct{}

	// events holds the IDs of live events.
	// +checklocks:mu
	events map[uint32]struct{}

	// mem maps each live buffer handle to the GPUs it is mapped on.
	// +checklocks:mu
	mem map[uint64]map[uint32]struct{}

	// runtime reports whether the debug runtime was enabled through this
	// description and not disabled.
	// +checklocks:mu
	runtime bool
}

func (r *fdResources) init() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queues = make(map[uint32]struct{})
	r.events = make(map[uint32]struct{})
	r.mem = make(map[uint64]map[uint32]struct{})
}

func (r *fdResources) addQueue(id uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queues[id] = struct{}{}
}

func (r *fdResources) removeQueue(id uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.queues, id)
}

func (r *fdResources) addEvent(id uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events[id] = struct{}{}
}

func (r *fdResources) removeEvent(id uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.events, id)
}

func (r *fdResources) addMem(handle uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mem[handle] = make(map[uint32]struct{})
}

func (r *fdResources) removeMem(handle uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.mem, handle)
}

// mapped records that handle was mapped onto (or, if !mapped, unmapped from)
// the given GPUs. A handle this description did not allocate is ignored: it
// belongs to another process sharing the address space, which will give it
// back itself.
func (r *fdResources) mapped(handle uint64, gpuIDs []uint32, mapped bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gpus, ok := r.mem[handle]
	if !ok {
		return
	}
	for _, id := range gpuIDs {
		if mapped {
			gpus[id] = struct{}{}
		} else {
			delete(gpus, id)
		}
	}
}

func (r *fdResources) setRuntime(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runtime = enabled
}

// heldResources is a snapshot of an fdResources, taken when it is reclaimed.
type heldResources struct {
	queues  []uint32
	events  []uint32
	mem     map[uint64][]uint32
	runtime bool
}

func (h *heldResources) empty() bool {
	return len(h.queues) == 0 && len(h.events) == 0 && len(h.mem) == 0 && !h.runtime
}

// take returns everything still held and forgets it, so that it is given back
// at most once.
func (r *fdResources) take() heldResources {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := heldResources{mem: make(map[uint64][]uint32, len(r.mem)), runtime: r.runtime}
	for id := range r.queues {
		h.queues = append(h.queues, id)
	}
	for id := range r.events {
		h.events = append(h.events, id)
	}
	for handle, gpus := range r.mem {
		var ids []uint32
		for id := range gpus {
			ids = append(ids, id)
		}
		h.mem[handle] = ids
	}
	r.queues = make(map[uint32]struct{})
	r.events = make(map[uint32]struct{})
	r.mem = make(map[uint64]map[uint32]struct{})
	r.runtime = false
	return h
}

// reclaim gives back to the driver everything created through fd, in the
// order the driver needs: queues, then events, then buffers, then the debug
// runtime. It is called as fd is released, before its host descriptor is
// closed. Failures are logged and the rest carries on: a buffer that cannot
// be freed stays charged, which is the conservative direction.
func (fd *kfdFD) reclaim() {
	held := fd.res.take()
	if held.empty() {
		return
	}
	amdp := fd.dev.amdp
	var failed int
	for _, id := range held.queues {
		release := amdp.timeSlicer.beforeDestroyQueue(id)
		err := fd.destroyQueue(id)
		release()
		if err != nil {
			failed++
			log.Warningf("amdproxy: reclaiming queue %d of thread group %d: %v", id, fd.tgid, err)
		}
	}
	for _, id := range held.events {
		if err := fd.destroyEvent(id); err != nil {
			failed++
			log.Warningf("amdproxy: reclaiming event %d of thread group %d: %v", id, fd.tgid, err)
		}
	}
	var freed, kept uint64
	for handle, gpus := range held.mem {
		if len(gpus) > 0 {
			if err := fd.unmapMemory(handle, gpus); err != nil {
				log.Warningf("amdproxy: unmapping buffer %#x of thread group %d from GPUs %v: %v", handle, fd.tgid, gpus, err)
			}
		}
		if err := fd.freeMemory(handle); err != nil {
			// The signal page lands here by design (EPERM); anything else is
			// worth knowing about. Either way it stays charged.
			kept++
			log.Debugf("amdproxy: buffer %#x of thread group %d not freed: %v", handle, fd.tgid, err)
			continue
		}
		amdp.memAcct.forget(handle)
		amdp.vaGuard.forget(handle)
		freed++
	}
	if held.runtime && !amdp.runtimeShare.leave(fd.tgid) {
		// The last holder is gone. As in kfdRuntimeEnable, the debug session
		// has to be closed before the runtime is taken down, or the driver
		// waits forever for a debugger that is not there.
		amdp.timeSlicer.shutdown()
		if err := fd.disableRuntime(); err != nil {
			failed++
			log.Warningf("amdproxy: taking down the debug runtime of thread group %d: %v", fd.tgid, err)
		}
	}
	log.Infof("amdproxy: thread group %d released /dev/kfd; gave back %d queues, %d events, %d buffers (%d kept), runtime=%v, %d failures",
		fd.tgid, len(held.queues), len(held.events), freed, kept, held.runtime, failed)
}

// tgAlive reports whether tgid still has a /dev/kfd description open in this
// sandbox. A process that has none can no longer use what it acquired.
func (amdp *amdproxy) tgAlive(tgid kernel.ThreadID) bool {
	amdp.fdsMu.Lock()
	defer amdp.fdsMu.Unlock()
	for fd := range amdp.kfdFDs {
		if fd.tgid == tgid {
			return true
		}
	}
	return false
}
