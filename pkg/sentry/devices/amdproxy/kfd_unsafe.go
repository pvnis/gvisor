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
	"gvisor.dev/gvisor/pkg/sentry/devices/amdproxy/amdconf"
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
//
// The requested mask is intersected with the sandbox's ceiling, so an
// application may confine itself to fewer compute units but cannot reach one
// it was not given.
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
	if ceiling := ki.fd.dev.amdp.cuMask; len(ceiling) > 0 {
		before := amdconf.CUMask(append([]uint32(nil), cuMask...))
		ceiling.Clamp(cuMask)
		if got := amdconf.CUMask(cuMask); got.Count() != before.Count() {
			ki.ctx.Debugf("amdproxy: SET_CU_MASK on queue %d narrowed from %v to %v by this sandbox's mask %v", params.QueueID, before, got, ceiling)
		}
		if amdconf.CUMask(cuMask).Empty() {
			// Every unit asked for lies outside the sandbox's share. A queue
			// with no compute units cannot run, so report this rather than
			// configuring one that silently never executes.
			ki.ctx.Warningf("amdproxy: SET_CU_MASK requested no compute units within this sandbox's mask %v", ceiling)
			return 0, linuxerr.EINVAL
		}
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

// maxKFDTileConfigs is an upper bound on the number of tile or macro-tile
// config entries KFD may return. GCN hardware has at most 44 tile modes and
// 16 macro-tile modes; 256 gives ample room for future expansion.
const maxKFDTileConfigs = 256

// maxKFDWaitEvents is an upper bound on the number of events in a single
// WAIT_EVENTS call. 256 is generous; real code rarely exceeds a handful.
const maxKFDWaitEvents = 256

// maxKFDSVMAttrs is an upper bound on the number of SVM attributes in a
// single AMDKFD_IOC_SVM call. 8 attribute types times up to 8 GPUs gives 64.
const maxKFDSVMAttrs = 64

// kfdSVM handles AMDKFD_IOC_SVM, which sets or queries shared virtual memory
// properties for a virtual address range.
//
// The parameter layout is a fixed 24-byte header (KFDIoctlSVMArgs) followed
// immediately by NAttr KFDIoctlSVMAttribute structs. The ioctl command
// encodes only the header size, but the kernel reads the full flat buffer.
// kfdSVM copies the entire block from the application, forwards it to the
// host, and copies the result back so that GET_ATTR can fill in attr values.
func kfdSVM(ki *kfdIoctlState) (uintptr, error) {
	var header amdgpu.KFDIoctlSVMArgs
	if _, err := header.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	if header.NAttr > maxKFDSVMAttrs {
		return 0, linuxerr.EINVAL
	}
	attrBytes := int(header.NAttr) * amdgpu.SizeofKFDIoctlSVMAttribute
	buf := make([]byte, amdgpu.SizeofKFDIoctlSVMArgs+attrBytes)
	header.MarshalBytes(buf[:amdgpu.SizeofKFDIoctlSVMArgs])
	if header.NAttr > 0 {
		if _, err := ki.t.CopyInBytes(ki.argAddr+hostarch.Addr(amdgpu.SizeofKFDIoctlSVMArgs), buf[amdgpu.SizeofKFDIoctlSVMArgs:]); err != nil {
			return 0, err
		}
	}
	defer runtime.KeepAlive(buf)
	n, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(ki.fd.hostFD), uintptr(ki.cmd), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return n, errno
	}
	if _, err := ki.t.CopyOutBytes(ki.argAddr, buf[:amdgpu.SizeofKFDIoctlSVMArgs]); err != nil {
		return n, err
	}
	if header.NAttr > 0 {
		if _, err := ki.t.CopyOutBytes(ki.argAddr+hostarch.Addr(amdgpu.SizeofKFDIoctlSVMArgs), buf[amdgpu.SizeofKFDIoctlSVMArgs:]); err != nil {
			return n, err
		}
	}
	return n, nil
}

// kfdGetTileConfig handles AMDKFD_IOC_GET_TILE_CONFIG. The parameters point
// at two optional application arrays (tile configs and macro-tile configs)
// that the driver fills in, so each non-null pointer is retargeted at a
// Sentry buffer for the duration of the call and the result copied back.
func kfdGetTileConfig(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlGetTileConfigArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}

	origTilePtr := params.TileConfigPtr
	origMacroPtr := params.MacroTileConfigPtr

	var tileConfigs []uint32
	if params.NumTileConfigs > 0 && params.TileConfigPtr != 0 {
		if params.NumTileConfigs > maxKFDTileConfigs {
			return 0, linuxerr.EINVAL
		}
		tileConfigs = make([]uint32, params.NumTileConfigs)
		defer runtime.KeepAlive(tileConfigs)
		params.TileConfigPtr = uint64(uintptr(unsafe.Pointer(&tileConfigs[0])))
	} else {
		params.TileConfigPtr = 0
	}

	var macroTileConfigs []uint32
	if params.NumMacroTileConfigs > 0 && params.MacroTileConfigPtr != 0 {
		if params.NumMacroTileConfigs > maxKFDTileConfigs {
			return 0, linuxerr.EINVAL
		}
		macroTileConfigs = make([]uint32, params.NumMacroTileConfigs)
		defer runtime.KeepAlive(macroTileConfigs)
		params.MacroTileConfigPtr = uint64(uintptr(unsafe.Pointer(&macroTileConfigs[0])))
	} else {
		params.MacroTileConfigPtr = 0
	}

	n, err := kfdIoctlInvoke(ki, &params)
	params.TileConfigPtr = origTilePtr
	params.MacroTileConfigPtr = origMacroPtr
	if err != nil {
		return n, err
	}

	if len(tileConfigs) > 0 && params.NumTileConfigs > 0 {
		if params.NumTileConfigs > uint32(len(tileConfigs)) {
			return n, linuxerr.EINVAL
		}
		if _, err := primitive.CopyUint32SliceOut(ki.t, hostarch.Addr(origTilePtr), tileConfigs[:params.NumTileConfigs]); err != nil {
			return n, err
		}
	}
	if len(macroTileConfigs) > 0 && params.NumMacroTileConfigs > 0 {
		if params.NumMacroTileConfigs > uint32(len(macroTileConfigs)) {
			return n, linuxerr.EINVAL
		}
		if _, err := primitive.CopyUint32SliceOut(ki.t, hostarch.Addr(origMacroPtr), macroTileConfigs[:params.NumMacroTileConfigs]); err != nil {
			return n, err
		}
	}
	if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
		return n, err
	}
	return n, nil
}

// kfdWaitEvents handles AMDKFD_IOC_WAIT_EVENTS. The parameters point at an
// application array of kfd_event_data structs, each holding an event_id input
// and exception-data outputs, so the pointer is retargeted at a Sentry buffer.
//
// WAIT_EVENTS can block for its Timeout duration; unix.Syscall is used so
// the Go runtime may schedule other goroutines on this OS thread while waiting.
func kfdWaitEvents(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlWaitEventsArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	if params.NumEvents == 0 || params.NumEvents > maxKFDWaitEvents {
		return 0, linuxerr.EINVAL
	}
	events := make([]amdgpu.KFDEventData, params.NumEvents)
	if _, err := amdgpu.CopyKFDEventDataSliceIn(ki.t, hostarch.Addr(params.EventsPtr), events); err != nil {
		return 0, err
	}
	defer runtime.KeepAlive(events)
	origPtr := params.EventsPtr
	params.EventsPtr = uint64(uintptr(unsafe.Pointer(&events[0])))
	n, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(ki.fd.hostFD), uintptr(ki.cmd), uintptr(unsafe.Pointer(&params)))
	params.EventsPtr = origPtr
	if errno != 0 {
		return n, errno
	}
	if _, err := amdgpu.CopyKFDEventDataSliceOut(ki.t, hostarch.Addr(origPtr), events); err != nil {
		return n, err
	}
	if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
		return n, err
	}
	return n, nil
}

// kfdCreateQueue handles AMDKFD_IOC_CREATE_QUEUE.
//
// The parameters name GPU virtual addresses rather than pointers the Sentry
// dereferences, so the ioctl itself needs no translation. What it does need is
// the sandbox's compute unit mask: a queue is created spanning the whole
// device, and AMDKFD_IOC_SET_CU_MASK is the only way to narrow it, so amdproxy
// applies the mask itself rather than relying on the application to ask.
func kfdCreateQueue(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlCreateQueueArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	n, err := kfdIoctlInvoke(ki, &params)
	if err != nil {
		return n, err
	}
	if ceiling := ki.fd.dev.amdp.cuMask; len(ceiling) > 0 {
		if err := ki.fd.setQueueCUMask(params.QueueID, ceiling); err != nil {
			// A queue that outlived this failure would run on the whole
			// device, so destroy it rather than return one that escaped the
			// limit.
			ki.ctx.Warningf("amdproxy: applying CU mask to queue %d failed (%v); destroying it", params.QueueID, err)
			if derr := ki.fd.destroyQueue(params.QueueID); derr != nil {
				ki.ctx.Warningf("amdproxy: destroying queue %d also failed: %v", params.QueueID, derr)
			}
			return 0, err
		}
	}
	if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
		return n, err
	}
	return n, nil
}

// setQueueCUMask issues AMDKFD_IOC_SET_CU_MASK on the host for a queue this
// package created, using a mask held in Sentry memory.
func (fd *kfdFD) setQueueCUMask(queueID uint32, mask amdconf.CUMask) error {
	defer runtime.KeepAlive(mask)
	params := amdgpu.KFDIoctlSetCUMaskArgs{
		QueueID:   queueID,
		NumCUMask: mask.Bits(),
		CUMaskPtr: uint64(uintptr(unsafe.Pointer(&mask[0]))),
	}
	_, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(fd.hostFD), uintptr(amdgpu.AMDKFD_IOC_SET_CU_MASK), uintptr(unsafe.Pointer(&params)))
	if errno != 0 {
		return errno
	}
	return nil
}

// destroyQueue issues AMDKFD_IOC_DESTROY_QUEUE on the host.
func (fd *kfdFD) destroyQueue(queueID uint32) error {
	params := amdgpu.KFDIoctlDestroyQueueArgs{QueueID: queueID}
	_, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(fd.hostFD), uintptr(amdgpu.AMDKFD_IOC_DESTROY_QUEUE), uintptr(unsafe.Pointer(&params)))
	if errno != 0 {
		return errno
	}
	return nil
}

// kfdAllocMemoryOfGPU handles AMDKFD_IOC_ALLOC_MEMORY_OF_GPU, charging the
// allocation against the sandbox's GPU memory limit.
//
// The charge is taken before the ioctl is forwarded, so that two concurrent
// allocations cannot both be admitted against the same headroom, and returned
// if the driver refuses the allocation for its own reasons. When the limit
// would be exceeded the driver is never called: the request is refused with
// the error it would itself have returned, which the ROCm runtime reports as
// an out-of-memory condition.
func kfdAllocMemoryOfGPU(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlAllocMemoryOfGPUArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	acct := &ki.fd.dev.amdp.memAcct
	kind := memKindOfFlags(params.Flags)
	if !acct.admit(ki.ctx, kind, params.Size) {
		return 0, linuxerr.ENOMEM
	}
	// When the sandbox's processes share one GPU address space, two of them
	// can pick the same address believing they each have their own. Refusing
	// is an ordinary allocation failure the runtime handles; aliasing would be
	// silent data corruption.
	guard := &ki.fd.dev.amdp.vaGuard
	tgid := ki.t.ThreadGroup().ID()
	if kind != memKindNone && !guard.admit(ki.ctx, params.VAAddr, params.Size, tgid) {
		acct.release(kind, params.Size)
		return 0, linuxerr.ENOMEM
	}
	n, err := kfdIoctlInvoke(ki, &params)
	if err != nil {
		acct.release(kind, params.Size)
		return n, err
	}
	acct.record(params.Handle, kind, params.Size)
	if kind != memKindNone {
		guard.record(params.Handle, params.VAAddr, params.Size, tgid)
	}
	if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
		return n, err
	}
	return n, nil
}

// kfdFreeMemoryOfGPU handles AMDKFD_IOC_FREE_MEMORY_OF_GPU, returning what the
// allocation was charged.
func kfdFreeMemoryOfGPU(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlFreeMemoryOfGPUArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	n, err := kfdIoctlInvoke(ki, &params)
	if err != nil {
		// The allocation still exists, so it stays charged.
		return n, err
	}
	ki.fd.dev.amdp.memAcct.forget(params.Handle)
	ki.fd.dev.amdp.vaGuard.forget(params.Handle)
	if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
		return n, err
	}
	return n, nil
}

// kfdAvailableMemory handles AMDKFD_IOC_AVAILABLE_MEMORY, reporting what the
// sandbox may still allocate rather than what the device has.
//
// Frameworks size their allocations against this, so reporting the device's
// free memory would have them plan for memory they can never get. It would
// also disclose how much of the GPU the rest of the machine is using.
func kfdAvailableMemory(ki *kfdIoctlState) (uintptr, error) {
	var params amdgpu.KFDIoctlGetAvailableMemoryArgs
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	n, err := kfdIoctlInvoke(ki, &params)
	if err != nil {
		return n, err
	}
	if headroom, limited := ki.fd.dev.amdp.memAcct.availableVRAM(); limited && headroom < params.Available {
		params.Available = headroom
	}
	if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
		return n, err
	}
	return n, nil
}
