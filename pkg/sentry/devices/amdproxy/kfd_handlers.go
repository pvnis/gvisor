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
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

// Bounds on the variable-length arrays reachable through KFD ioctls. The
// driver has limits of its own, but amdproxy must bound these before it
// allocates, so that an application cannot size a Sentry allocation
// arbitrarily. Both are far above what real hardware needs: no system
// presents anywhere near this many GPUs to one process, and the largest AMD
// GPUs have a few hundred compute units.
const (
	maxKFDDevices   = 64
	maxKFDCUMaskLen = 1024 // in bits
)

// kfdCreateEvent handles AMDKFD_IOC_CREATE_EVENT.
//
// A caller that passes an EventPageOffset is proposing a buffer as the KFD
// process's signal page, which is where the driver records that an event has
// fired. A KFD process has exactly one, and a sandbox that shares a context
// has one KFD process, so only the first of its processes gets its page
// accepted and the rest are refused EINVAL. Measured:
//
//	tgid=6   page_in=0xf97400000001  err=<nil>
//	tgid=18  page_in=0xf97400000002  err=invalid argument
//	tgid=29  page_in=0xf97400000022  err=invalid argument
//
// Their later events still get distinct slots, so event IDs do not collide —
// but those slots index the page the *first* process registered, which the
// others never mapped. So the driver signals into memory the waiter cannot
// see, and the waiter would block in WAIT_EVENTS forever; eventShare caps
// those waits so it polls its own memory instead.
//
// What the cap could not save is a process that never gets an event at all.
// The driver handles the page proposal *before* it creates anything, and
// returns as soon as it fails (kfd_chardev.c):
//
//	if (args->event_page_offset) {
//		err = kfd_kmap_event_page(p, args->event_page_offset);
//		if (err)
//			return err;
//	}
//	err = kfd_event_create(...);
//
// and kfd_kmap_event_page() refuses with EINVAL the moment p->signal_page is
// set. So forwarding a second process's proposal costs it the event, not just
// the page — and ROCr, which does not check, dereferences the event it did not
// get. Measured: a null read at offset 4 in its async handler thread, killing
// the process at init, before it ever created a queue. That is why a second
// gpuburn in a shared sandbox died with SIGSEGV while the first ran.
//
// So a proposal from any process but the page's owner is dropped rather than
// forwarded. The driver then skips kfd_kmap_event_page() entirely, and
// kfd_event_create() allocates a slot on the page the owner registered, which
// is the outcome the caller needed.
func kfdCreateEvent(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlCreateEventArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	tgid := ki.t.ThreadGroup().ID()
	es := &ki.fd.dev.amdp.eventShare
	inPage := params.EventPageOffset
	if inPage != 0 && !es.claimPage(tgid) {
		// Not the owner. Ask for the event without the page, so the driver
		// creates one instead of refusing outright.
		es.logDrop(ki.ctx, tgid, es.owner())
		params.EventPageOffset = 0
		inPage = 0
	}
	n, err := kfdIoctlInvoke(ki, &params)
	if inPage != 0 {
		ki.ctx.Infof("amdproxy: CREATE_EVENT signal page tgid=%d page=%#x err=%v", tgid, inPage, err)
		if err != nil {
			// The owner's own proposal failed, so nothing in this sandbox has
			// a usable page. Let it try again rather than holding the claim.
			es.releasePage(tgid)
		}
	}
	if err != nil {
		return n, err
	}
	if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
		return n, err
	}
	return n, nil
}

// kfdRuntimeEnable handles AMDKFD_IOC_RUNTIME_ENABLE, which brings the KFD
// process's debug runtime up or takes it down.
//
// The driver keeps that state once per KFD process, and a sandbox has one, so
// when its processes share an address space they must share this too;
// runtimeShare says what that costs. Without sharing this is an ordinary
// forwarded ioctl.
func kfdRuntimeEnable(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlRuntimeEnableArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	share := &ki.fd.dev.amdp.runtimeShare
	tgid := ki.t.ThreadGroup().ID()
	enabling := params.ModeMask&amdgpu.KFD_RUNTIME_ENABLE_MODE_ENABLE_MASK != 0
	if enabling {
		if share.enter(tgid) {
			// Another process has already brought the runtime up, which is the
			// state this caller is asking for.
			return 0, nil
		}
	} else if share.leave(tgid) {
		// Someone else is still using it.
		return 0, nil
	} else {
		// The last user is taking the debug runtime down, and the driver will
		// not let it go while a debug session is open: it raises a runtime
		// event to the debugger and waits, indefinitely, for the debugger to
		// answer. Nothing in this package answers it, so the time slicer's
		// session has to be closed first.
		//
		// Found the hard way. The application blocked inside RUNTIME_ENABLE
		// with no error and no driver message; only a syscall trace showed
		// which ioctl had been entered and never left. The interposer this was
		// ported from never hit it because it closed its session from an
		// atexit handler, which happens to run before the runtime is taken
		// down.
		ki.fd.dev.amdp.timeSlicer.shutdown()
	}
	n, err := kfdIoctlInvoke(ki, &params)
	if err != nil {
		if enabling {
			share.enterFailed(tgid)
		}
		return n, err
	}
	if enabling {
		// The one moment at which a debug session may be opened: the debug
		// runtime is up, and has not yet become busy. See openSession.
		ki.fd.dev.amdp.timeSlicer.openSession(ki.fd.hostFD)
	}
	if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
		return n, err
	}
	return n, nil
}

// kfdAcquireVM handles AMDKFD_IOC_ACQUIRE_VM, which binds the calling process
// to the GPU address space owned by a render node. DRMFD names a file
// descriptor in the application's table, so it must be translated to the host
// file descriptor that backs it before the ioctl is forwarded.
func kfdAcquireVM(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlAcquireVMArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	renderFileGeneric, _ := ki.t.FDTable().Get(int32(params.DRMFD))
	if renderFileGeneric == nil {
		return 0, linuxerr.EINVAL
	}
	defer renderFileGeneric.DecRef(ki.ctx)
	renderFile, ok := renderFileGeneric.Impl().(*renderFD)
	if !ok {
		return 0, linuxerr.EINVAL
	}
	if renderFile.isRestored() {
		return 0, linuxerr.EBADF
	}
	// A sandbox gets one GPU address space, because the driver binds one per
	// process and the Sentry is the process it sees. If another process here
	// already holds this GPU's, forwarding would earn EBUSY and leave this
	// process unable to use the GPU at all; sharedvm.go explains what is being
	// traded away by reporting success instead.
	tgid := ki.t.ThreadGroup().ID()
	if ki.fd.dev.amdp.vmShare.shareFor(ki.ctx, params.GPUID, tgid) {
		return 0, nil
	}
	params.DRMFD = uint32(renderFile.hostFD)
	// The driver does not modify the struct, so there is nothing to copy out.
	// In particular the host file descriptor must not be written back to the
	// application.
	n, err := kfdIoctlInvoke(ki, &params)
	if err == nil {
		ki.fd.dev.amdp.vmShare.claim(params.GPUID, tgid)
	}
	return n, err
}
