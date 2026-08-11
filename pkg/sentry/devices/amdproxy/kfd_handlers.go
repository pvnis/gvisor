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
	params.DRMFD = uint32(renderFile.hostFD)
	// The driver does not modify the struct, so there is nothing to copy out.
	// In particular the host file descriptor must not be written back to the
	// application.
	return kfdIoctlInvoke(ki, &params)
}
