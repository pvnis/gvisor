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
// see, and the waiter blocks in WAIT_EVENTS forever. This is what stops vLLM.
//
// The refusal is logged rather than worked around, because working around it
// means making every process in the sandbox map one page, which is a real
// design decision and not a patch.
func kfdCreateEvent(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlCreateEventArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	inPage := params.EventPageOffset
	n, err := kfdIoctlInvoke(ki, &params)
	if inPage != 0 {
		ki.ctx.Infof("amdproxy: CREATE_EVENT signal page tgid=%d page=%#x err=%v",
			ki.t.ThreadGroup().ID(), inPage, err)
		if err != nil && ki.fd.dev.amdp.vmShare.enabled {
			ki.ctx.Warningf("amdproxy: this sandbox's KFD signal page is already owned by another of its processes, " +
				"so events for this one will never be delivered and a wait on them will block indefinitely. " +
				"This is the known limit of --amdproxy-share-kfd-vm")
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
	}
	n, err := kfdIoctlInvoke(ki, &params)
	if err != nil {
		if enabling {
			share.enterFailed(tgid)
		}
		return n, err
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
