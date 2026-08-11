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
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
)

// Bounds on the variable-length buffers reachable through DRM ioctls, so that
// an application cannot size a Sentry allocation. Driver version strings are
// a few dozen bytes and the largest amdgpu info query returns well under a
// kilobyte, so both of these are far above what the driver produces.
const (
	maxDRMVersionStrLen = 4096
	maxDRMInfoRetSize   = 64 * 1024
)

// renderIoctlInvoke forwards an ioctl to the host render node. params must
// point to Sentry memory, not application memory.
func renderIoctlInvoke[Params any](ri *renderIoctlState, params *Params) (uintptr, error) {
	n, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(ri.fd.hostFD), uintptr(ri.cmd), uintptr(unsafe.Pointer(params)))
	if errno != 0 {
		return n, errno
	}
	return n, nil
}

// renderIoctlSimple implements a render node ioctl whose parameters contain
// no pointers requiring translation and no file descriptors.
func renderIoctlSimple[Params any, PtrParams marshalPtr[Params]](ri *renderIoctlState) (uintptr, error) {
	var paramsValue Params
	params := PtrParams(&paramsValue)
	if _, err := params.CopyIn(ri.t, ri.argAddr); err != nil {
		return 0, err
	}
	n, err := renderIoctlInvoke(ri, params)
	if err != nil {
		return n, err
	}
	if _, err := params.CopyOut(ri.t, ri.argAddr); err != nil {
		return n, err
	}
	return n, nil
}

// drmVersion handles DRM_IOCTL_VERSION, which names three application buffers
// that the driver fills with the driver's name, date and description. Callers
// conventionally issue it once with null pointers to learn the lengths, then
// again with buffers of that size.
func drmVersion(ri *renderIoctlState) (uintptr, error) {
	var params amdgpu.DRMVersion
	if _, err := params.CopyIn(ri.t, ri.argAddr); err != nil {
		return 0, err
	}
	// TEMPORARY instrumentation: libdrm issues this twice, once to learn the
	// lengths and once to fill the buffers it sized from them. If the second
	// pass reports a longer length than the first, libdrm's NUL at
	// name[name_len] lands one byte past its allocation.
	ri.t.Infof("amdproxy: DRM_VERSION in  name=%#x nameLen=%d date=%#x dateLen=%d desc=%#x descLen=%d",
		params.Name, params.NameLen, params.Date, params.DateLen, params.Desc, params.DescLen)

	if params.NameLen > maxDRMVersionStrLen || params.DateLen > maxDRMVersionStrLen || params.DescLen > maxDRMVersionStrLen {
		return 0, linuxerr.EINVAL
	}

	// A null pointer means the caller only wants the corresponding length, so
	// allocate nothing and leave the pointer null.
	nameBuf := make([]byte, params.NameLen)
	dateBuf := make([]byte, params.DateLen)
	descBuf := make([]byte, params.DescLen)
	defer runtime.KeepAlive(nameBuf)
	defer runtime.KeepAlive(dateBuf)
	defer runtime.KeepAlive(descBuf)

	origName, origDate, origDesc := params.Name, params.Date, params.Desc
	params.Name = sliceAddr(nameBuf, origName)
	params.Date = sliceAddr(dateBuf, origDate)
	params.Desc = sliceAddr(descBuf, origDesc)
	n, err := renderIoctlInvoke(ri, &params)
	params.Name, params.Date, params.Desc = origName, origDate, origDesc
	ri.t.Infof("amdproxy: DRM_VERSION out nameLen=%d dateLen=%d descLen=%d err=%v",
		params.NameLen, params.DateLen, params.DescLen, err)
	if err != nil {
		return n, err
	}

	// The driver clamps each length to the buffer it was given, so the
	// returned lengths bound what it wrote.
	for _, out := range []struct {
		addr uint64
		buf  []byte
		n    uint64
	}{
		{origName, nameBuf, params.NameLen},
		{origDate, dateBuf, params.DateLen},
		{origDesc, descBuf, params.DescLen},
	} {
		if out.addr == 0 || out.n == 0 {
			continue
		}
		if out.n > uint64(len(out.buf)) {
			return n, linuxerr.EINVAL
		}
		if _, err := primitive.CopyByteSliceOut(ri.t, hostarch.Addr(out.addr), out.buf[:out.n]); err != nil {
			return n, err
		}
	}
	if _, err := params.CopyOut(ri.t, ri.argAddr); err != nil {
		return n, err
	}
	return n, nil
}

// drmAMDGPUInfo handles DRM_IOCTL_AMDGPU_INFO, whose ReturnPointer names an
// application buffer that the driver fills with the answer to the query.
func drmAMDGPUInfo(ri *renderIoctlState) (uintptr, error) {
	var params amdgpu.DRMAMDGPUInfo
	if _, err := params.CopyIn(ri.t, ri.argAddr); err != nil {
		return 0, err
	}
	if params.ReturnSize == 0 || params.ReturnSize > maxDRMInfoRetSize || params.ReturnPointer == 0 {
		return 0, linuxerr.EINVAL
	}
	retBuf := make([]byte, params.ReturnSize)
	defer runtime.KeepAlive(retBuf)

	origPtr := params.ReturnPointer
	params.ReturnPointer = uint64(uintptr(unsafe.Pointer(&retBuf[0])))
	n, err := renderIoctlInvoke(ri, &params)
	params.ReturnPointer = origPtr
	if err != nil {
		return n, err
	}
	if _, err := primitive.CopyByteSliceOut(ri.t, hostarch.Addr(origPtr), retBuf); err != nil {
		return n, err
	}
	// The driver does not modify the parameter struct itself, so there is
	// nothing else to copy out.
	return n, nil
}

// sliceAddr returns the address of buf's first byte, or 0 if the application
// passed a null pointer, in which case the driver is being asked for a length
// rather than the contents.
func sliceAddr(buf []byte, origAddr uint64) uint64 {
	if origAddr == 0 || len(buf) == 0 {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(&buf[0])))
}
