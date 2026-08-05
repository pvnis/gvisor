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

// kfdIoctlInvoke forwards an ioctl to the host KFD device. params must point
// to Sentry memory, not application memory.
func kfdIoctlInvoke[Params any](ki *kfdIoctlState, params *Params) (uintptr, error) {
	n, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(ki.fd.hostFD), uintptr(ki.cmd), uintptr(unsafe.Pointer(params)))
	if errno != 0 {
		return n, errno
	}
	return n, nil
}

// kfdGetProcessAperturesNew handles AMDKFD_IOC_GET_PROCESS_APERTURES_NEW. The
// parameters point at an application array that the driver fills in with one
// entry per GPU, so the pointer is retargeted at a Sentry buffer for the
// duration of the call and the result copied back out.
func kfdGetProcessAperturesNew(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlGetProcessAperturesNewArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	if params.NumOfNodes > maxKFDDevices {
		return 0, linuxerr.EINVAL
	}
	// A null pointer, or a zero node count, asks for the number of nodes
	// alone and leaves the array untouched; see
	// kfd_ioctl_get_process_apertures_new(). Null the pointer so that no
	// application address is handed to the host kernel even though the driver
	// will not dereference it, and restore it afterwards so that the
	// application's own copy is unchanged.
	if params.KFDProcessDeviceAperturesPtr == 0 || params.NumOfNodes == 0 {
		origPtr := params.KFDProcessDeviceAperturesPtr
		params.KFDProcessDeviceAperturesPtr = 0
		n, err := kfdIoctlInvoke(ki, &params)
		params.KFDProcessDeviceAperturesPtr = origPtr
		if err != nil {
			return n, err
		}
		if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
			return n, err
		}
		return n, nil
	}

	apertures := make([]amdgpu.KFDProcessDeviceApertures, params.NumOfNodes)
	defer runtime.KeepAlive(apertures)
	origPtr := params.KFDProcessDeviceAperturesPtr
	params.KFDProcessDeviceAperturesPtr = uint64(uintptr(unsafe.Pointer(&apertures[0])))
	n, err := kfdIoctlInvoke(ki, &params)
	params.KFDProcessDeviceAperturesPtr = origPtr
	if err != nil {
		return n, err
	}
	// The driver reports how many entries it filled, which cannot exceed the
	// number requested.
	if params.NumOfNodes > uint32(len(apertures)) {
		return n, linuxerr.EINVAL
	}
	if _, err := amdgpu.CopyKFDProcessDeviceAperturesSliceOut(ki.t, hostarch.Addr(origPtr), apertures[:params.NumOfNodes]); err != nil {
		return n, err
	}
	if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
		return n, err
	}
	return n, nil
}

// kfdMapMemoryToGPU handles both AMDKFD_IOC_MAP_MEMORY_TO_GPU and
// AMDKFD_IOC_UNMAP_MEMORY_FROM_GPU, which share a parameter layout; the
// command number in ki.cmd selects which the host performs. The parameters
// point at an application array of GPU ids naming the devices to map to.
func kfdMapMemoryToGPU(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlMapMemoryToGPUArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	if params.NDevices == 0 || params.NDevices > maxKFDDevices {
		return 0, linuxerr.EINVAL
	}
	deviceIDs := make([]uint32, params.NDevices)
	if _, err := primitive.CopyUint32SliceIn(ki.t, hostarch.Addr(params.DeviceIDsArrayPtr), deviceIDs); err != nil {
		return 0, err
	}
	defer runtime.KeepAlive(deviceIDs)
	origPtr := params.DeviceIDsArrayPtr
	params.DeviceIDsArrayPtr = uint64(uintptr(unsafe.Pointer(&deviceIDs[0])))
	n, err := kfdIoctlInvoke(ki, &params)
	params.DeviceIDsArrayPtr = origPtr
	if err != nil {
		return n, err
	}
	// The id array is input only; only NSuccess is updated.
	if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
		return n, err
	}
	return n, nil
}

// kfdSetCUMask handles AMDKFD_IOC_SET_CU_MASK, which points at an application
// bitmask selecting the compute units a queue may run on.
//
// NumCUMask counts bits, not words, and the driver requires it to be a
// multiple of 32.
func kfdSetCUMask(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlSetCUMaskArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	if params.NumCUMask == 0 || params.NumCUMask%32 != 0 || params.NumCUMask > maxKFDCUMaskLen {
		return 0, linuxerr.EINVAL
	}
	cuMask := make([]uint32, params.NumCUMask/32)
	if _, err := primitive.CopyUint32SliceIn(ki.t, hostarch.Addr(params.CUMaskPtr), cuMask); err != nil {
		return 0, err
	}
	defer runtime.KeepAlive(cuMask)
	origPtr := params.CUMaskPtr
	params.CUMaskPtr = uint64(uintptr(unsafe.Pointer(&cuMask[0])))
	n, err := kfdIoctlInvoke(ki, &params)
	params.CUMaskPtr = origPtr
	// The mask is input only and the driver returns nothing else, so there is
	// nothing to copy out.
	return n, err
}
