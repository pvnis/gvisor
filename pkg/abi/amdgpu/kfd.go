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

// Package amdgpu tracks the ABI of the AMD GPU Linux kernel drivers: the
// Kernel Fusion Driver (KFD, /dev/kfd) and amdgpu's DRM render nodes
// (/dev/dri/renderD*).
//
// This file describes KFD, from include/uapi/linux/kfd_ioctl.h.
package amdgpu

import (
	"fmt"
	"structs"

	"gvisor.dev/gvisor/pkg/abi/linux"
)

// KFD ioctl interface version supported by this package. KFD promises
// backward compatibility within a major version, so a host minor version
// greater than ours is usable; a different major version is not.
const (
	KFD_IOCTL_MAJOR_VERSION = 1
	KFD_IOCTL_MINOR_VERSION = 14
)

// KFD_IOCTL_BASE is AMDKFD_IOCTL_BASE, the ioctl type ('K').
const KFD_IOCTL_BASE = uint32('K')

// Sizes of the ioctl parameter structs below, verified against
// include/uapi/linux/kfd_ioctl.h. These are needed to compute ioctl command
// numbers, which encode the size of their parameter struct.
const (
	SizeofKFDIoctlGetVersionArgs             = 8
	SizeofKFDIoctlCreateQueueArgs            = 96
	SizeofKFDIoctlDestroyQueueArgs           = 8
	SizeofKFDIoctlSetMemoryPolicyArgs        = 32
	SizeofKFDIoctlGetClockCountersArgs       = 40
	SizeofKFDIoctlGetProcessAperturesArgs    = 400
	SizeofKFDIoctlUpdateQueueArgs            = 24
	SizeofKFDIoctlCreateEventArgs            = 32
	SizeofKFDIoctlDestroyEventArgs           = 8
	SizeofKFDIoctlSetEventArgs               = 8
	SizeofKFDIoctlResetEventArgs             = 8
	SizeofKFDIoctlWaitEventsArgs             = 24
	SizeofKFDIoctlSetScratchBackingVAArgs    = 16
	SizeofKFDIoctlGetTileConfigArgs          = 40
	SizeofKFDIoctlSetTrapHandlerArgs         = 24
	SizeofKFDIoctlGetProcessAperturesNewArgs = 16
	SizeofKFDIoctlAcquireVMArgs              = 8
	SizeofKFDIoctlAllocMemoryOfGPUArgs       = 40
	SizeofKFDIoctlFreeMemoryOfGPUArgs        = 8
	SizeofKFDIoctlMapMemoryToGPUArgs         = 24
	SizeofKFDIoctlUnmapMemoryFromGPUArgs     = 24
	SizeofKFDIoctlSetCUMaskArgs              = 16
	SizeofKFDIoctlGetQueueWaveStateArgs      = 24
	SizeofKFDIoctlGetDmabufInfoArgs          = 32
	SizeofKFDIoctlImportDmabufArgs           = 24
	SizeofKFDIoctlAllocQueueGWSArgs          = 16
	SizeofKFDIoctlSMIEventsArgs              = 8
	SizeofKFDIoctlSVMArgs                    = 24
	SizeofKFDIoctlSetXNACKModeArgs           = 4
	SizeofKFDIoctlCRIUArgs                   = 56
	SizeofKFDIoctlGetAvailableMemoryArgs     = 16
	SizeofKFDIoctlExportDmabufArgs           = 16
	SizeofKFDIoctlRuntimeEnableArgs          = 16
	SizeofKFDIoctlDbgTrapArgs                = 32

	SizeofKFDProcessDeviceApertures = 56
)

// KFDIoctl represents a KFD ioctl command number.
type KFDIoctl uint32

// KFD ioctl command numbers, from the AMDKFD_IOC_* macros.
var (
	AMDKFD_IOC_GET_VERSION               = KFDIoctl(linux.IOR(KFD_IOCTL_BASE, 0x01, SizeofKFDIoctlGetVersionArgs))
	AMDKFD_IOC_CREATE_QUEUE              = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x02, SizeofKFDIoctlCreateQueueArgs))
	AMDKFD_IOC_DESTROY_QUEUE             = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x03, SizeofKFDIoctlDestroyQueueArgs))
	AMDKFD_IOC_SET_MEMORY_POLICY         = KFDIoctl(linux.IOW(KFD_IOCTL_BASE, 0x04, SizeofKFDIoctlSetMemoryPolicyArgs))
	AMDKFD_IOC_GET_CLOCK_COUNTERS        = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x05, SizeofKFDIoctlGetClockCountersArgs))
	AMDKFD_IOC_GET_PROCESS_APERTURES     = KFDIoctl(linux.IOR(KFD_IOCTL_BASE, 0x06, SizeofKFDIoctlGetProcessAperturesArgs))
	AMDKFD_IOC_UPDATE_QUEUE              = KFDIoctl(linux.IOW(KFD_IOCTL_BASE, 0x07, SizeofKFDIoctlUpdateQueueArgs))
	AMDKFD_IOC_CREATE_EVENT              = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x08, SizeofKFDIoctlCreateEventArgs))
	AMDKFD_IOC_DESTROY_EVENT             = KFDIoctl(linux.IOW(KFD_IOCTL_BASE, 0x09, SizeofKFDIoctlDestroyEventArgs))
	AMDKFD_IOC_SET_EVENT                 = KFDIoctl(linux.IOW(KFD_IOCTL_BASE, 0x0A, SizeofKFDIoctlSetEventArgs))
	AMDKFD_IOC_RESET_EVENT               = KFDIoctl(linux.IOW(KFD_IOCTL_BASE, 0x0B, SizeofKFDIoctlResetEventArgs))
	AMDKFD_IOC_WAIT_EVENTS               = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x0C, SizeofKFDIoctlWaitEventsArgs))
	AMDKFD_IOC_SET_SCRATCH_BACKING_VA    = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x11, SizeofKFDIoctlSetScratchBackingVAArgs))
	AMDKFD_IOC_GET_TILE_CONFIG           = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x12, SizeofKFDIoctlGetTileConfigArgs))
	AMDKFD_IOC_SET_TRAP_HANDLER          = KFDIoctl(linux.IOW(KFD_IOCTL_BASE, 0x13, SizeofKFDIoctlSetTrapHandlerArgs))
	AMDKFD_IOC_GET_PROCESS_APERTURES_NEW = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x14, SizeofKFDIoctlGetProcessAperturesNewArgs))
	AMDKFD_IOC_ACQUIRE_VM                = KFDIoctl(linux.IOW(KFD_IOCTL_BASE, 0x15, SizeofKFDIoctlAcquireVMArgs))
	AMDKFD_IOC_ALLOC_MEMORY_OF_GPU       = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x16, SizeofKFDIoctlAllocMemoryOfGPUArgs))
	AMDKFD_IOC_FREE_MEMORY_OF_GPU        = KFDIoctl(linux.IOW(KFD_IOCTL_BASE, 0x17, SizeofKFDIoctlFreeMemoryOfGPUArgs))
	AMDKFD_IOC_MAP_MEMORY_TO_GPU         = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x18, SizeofKFDIoctlMapMemoryToGPUArgs))
	AMDKFD_IOC_UNMAP_MEMORY_FROM_GPU     = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x19, SizeofKFDIoctlUnmapMemoryFromGPUArgs))
	AMDKFD_IOC_SET_CU_MASK               = KFDIoctl(linux.IOW(KFD_IOCTL_BASE, 0x1A, SizeofKFDIoctlSetCUMaskArgs))
	AMDKFD_IOC_GET_QUEUE_WAVE_STATE      = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x1B, SizeofKFDIoctlGetQueueWaveStateArgs))
	AMDKFD_IOC_GET_DMABUF_INFO           = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x1C, SizeofKFDIoctlGetDmabufInfoArgs))
	AMDKFD_IOC_IMPORT_DMABUF             = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x1D, SizeofKFDIoctlImportDmabufArgs))
	AMDKFD_IOC_ALLOC_QUEUE_GWS           = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x1E, SizeofKFDIoctlAllocQueueGWSArgs))
	AMDKFD_IOC_SMI_EVENTS                = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x1F, SizeofKFDIoctlSMIEventsArgs))
	AMDKFD_IOC_SVM                       = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x20, SizeofKFDIoctlSVMArgs))
	AMDKFD_IOC_SET_XNACK_MODE            = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x21, SizeofKFDIoctlSetXNACKModeArgs))
	AMDKFD_IOC_CRIU_OP                   = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x22, SizeofKFDIoctlCRIUArgs))
	AMDKFD_IOC_AVAILABLE_MEMORY          = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x23, SizeofKFDIoctlGetAvailableMemoryArgs))
	AMDKFD_IOC_EXPORT_DMABUF             = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x24, SizeofKFDIoctlExportDmabufArgs))
	AMDKFD_IOC_RUNTIME_ENABLE            = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x25, SizeofKFDIoctlRuntimeEnableArgs))
	AMDKFD_IOC_DBG_TRAP                  = KFDIoctl(linux.IOWR(KFD_IOCTL_BASE, 0x26, SizeofKFDIoctlDbgTrapArgs))
)

// String implements fmt.Stringer.String.
func (i KFDIoctl) String() string {
	switch i {
	case AMDKFD_IOC_GET_VERSION:
		return "AMDKFD_IOC_GET_VERSION"
	case AMDKFD_IOC_CREATE_QUEUE:
		return "AMDKFD_IOC_CREATE_QUEUE"
	case AMDKFD_IOC_DESTROY_QUEUE:
		return "AMDKFD_IOC_DESTROY_QUEUE"
	case AMDKFD_IOC_SET_MEMORY_POLICY:
		return "AMDKFD_IOC_SET_MEMORY_POLICY"
	case AMDKFD_IOC_GET_CLOCK_COUNTERS:
		return "AMDKFD_IOC_GET_CLOCK_COUNTERS"
	case AMDKFD_IOC_GET_PROCESS_APERTURES:
		return "AMDKFD_IOC_GET_PROCESS_APERTURES"
	case AMDKFD_IOC_UPDATE_QUEUE:
		return "AMDKFD_IOC_UPDATE_QUEUE"
	case AMDKFD_IOC_CREATE_EVENT:
		return "AMDKFD_IOC_CREATE_EVENT"
	case AMDKFD_IOC_DESTROY_EVENT:
		return "AMDKFD_IOC_DESTROY_EVENT"
	case AMDKFD_IOC_SET_EVENT:
		return "AMDKFD_IOC_SET_EVENT"
	case AMDKFD_IOC_RESET_EVENT:
		return "AMDKFD_IOC_RESET_EVENT"
	case AMDKFD_IOC_WAIT_EVENTS:
		return "AMDKFD_IOC_WAIT_EVENTS"
	case AMDKFD_IOC_SET_SCRATCH_BACKING_VA:
		return "AMDKFD_IOC_SET_SCRATCH_BACKING_VA"
	case AMDKFD_IOC_GET_TILE_CONFIG:
		return "AMDKFD_IOC_GET_TILE_CONFIG"
	case AMDKFD_IOC_SET_TRAP_HANDLER:
		return "AMDKFD_IOC_SET_TRAP_HANDLER"
	case AMDKFD_IOC_GET_PROCESS_APERTURES_NEW:
		return "AMDKFD_IOC_GET_PROCESS_APERTURES_NEW"
	case AMDKFD_IOC_ACQUIRE_VM:
		return "AMDKFD_IOC_ACQUIRE_VM"
	case AMDKFD_IOC_ALLOC_MEMORY_OF_GPU:
		return "AMDKFD_IOC_ALLOC_MEMORY_OF_GPU"
	case AMDKFD_IOC_FREE_MEMORY_OF_GPU:
		return "AMDKFD_IOC_FREE_MEMORY_OF_GPU"
	case AMDKFD_IOC_MAP_MEMORY_TO_GPU:
		return "AMDKFD_IOC_MAP_MEMORY_TO_GPU"
	case AMDKFD_IOC_UNMAP_MEMORY_FROM_GPU:
		return "AMDKFD_IOC_UNMAP_MEMORY_FROM_GPU"
	case AMDKFD_IOC_SET_CU_MASK:
		return "AMDKFD_IOC_SET_CU_MASK"
	case AMDKFD_IOC_GET_QUEUE_WAVE_STATE:
		return "AMDKFD_IOC_GET_QUEUE_WAVE_STATE"
	case AMDKFD_IOC_GET_DMABUF_INFO:
		return "AMDKFD_IOC_GET_DMABUF_INFO"
	case AMDKFD_IOC_IMPORT_DMABUF:
		return "AMDKFD_IOC_IMPORT_DMABUF"
	case AMDKFD_IOC_ALLOC_QUEUE_GWS:
		return "AMDKFD_IOC_ALLOC_QUEUE_GWS"
	case AMDKFD_IOC_SMI_EVENTS:
		return "AMDKFD_IOC_SMI_EVENTS"
	case AMDKFD_IOC_SVM:
		return "AMDKFD_IOC_SVM"
	case AMDKFD_IOC_SET_XNACK_MODE:
		return "AMDKFD_IOC_SET_XNACK_MODE"
	case AMDKFD_IOC_CRIU_OP:
		return "AMDKFD_IOC_CRIU_OP"
	case AMDKFD_IOC_AVAILABLE_MEMORY:
		return "AMDKFD_IOC_AVAILABLE_MEMORY"
	case AMDKFD_IOC_EXPORT_DMABUF:
		return "AMDKFD_IOC_EXPORT_DMABUF"
	case AMDKFD_IOC_RUNTIME_ENABLE:
		return "AMDKFD_IOC_RUNTIME_ENABLE"
	case AMDKFD_IOC_DBG_TRAP:
		return "AMDKFD_IOC_DBG_TRAP"
	default:
		return fmt.Sprintf("UNKNOWN KFD COMMAND %#x", uint32(i))
	}
}

// Memory types and attributes for KFDIoctlAllocMemoryOfGPUArgs.Flags, from the
// KFD_IOC_ALLOC_MEM_FLAGS_* macros.
const (
	KFD_IOC_ALLOC_MEM_FLAGS_VRAM          = 1 << 0
	KFD_IOC_ALLOC_MEM_FLAGS_GTT           = 1 << 1
	KFD_IOC_ALLOC_MEM_FLAGS_USERPTR       = 1 << 2
	KFD_IOC_ALLOC_MEM_FLAGS_DOORBELL      = 1 << 3
	KFD_IOC_ALLOC_MEM_FLAGS_MMIO_REMAP    = 1 << 4
	KFD_IOC_ALLOC_MEM_FLAGS_WRITABLE      = 1 << 31
	KFD_IOC_ALLOC_MEM_FLAGS_EXECUTABLE    = 1 << 30
	KFD_IOC_ALLOC_MEM_FLAGS_PUBLIC        = 1 << 29
	KFD_IOC_ALLOC_MEM_FLAGS_NO_SUBSTITUTE = 1 << 28
	KFD_IOC_ALLOC_MEM_FLAGS_AQL_QUEUE_MEM = 1 << 27
	KFD_IOC_ALLOC_MEM_FLAGS_COHERENT      = 1 << 26
	KFD_IOC_ALLOC_MEM_FLAGS_UNCACHED      = 1 << 25
	KFD_IOC_ALLOC_MEM_FLAGS_EXT_COHERENT  = 1 << 24
)

// Queue types for KFDIoctlCreateQueueArgs.QueueType.
const (
	KFD_IOC_QUEUE_TYPE_COMPUTE     = 0x0
	KFD_IOC_QUEUE_TYPE_SDMA        = 0x1
	KFD_IOC_QUEUE_TYPE_COMPUTE_AQL = 0x2
	KFD_IOC_QUEUE_TYPE_SDMA_XGMI   = 0x3
)

// NUM_OF_SUPPORTED_GPUS is the fixed number of entries in
// KFDIoctlGetProcessAperturesArgs.ProcessApertures.
const NUM_OF_SUPPORTED_GPUS = 7

// KFDIoctlGetVersionArgs is struct kfd_ioctl_get_version_args.
//
// +marshal
type KFDIoctlGetVersionArgs struct {
	_            structs.HostLayout
	MajorVersion uint32
	MinorVersion uint32
}

// KFDIoctlCreateQueueArgs is struct kfd_ioctl_create_queue_args.
//
// +marshal
type KFDIoctlCreateQueueArgs struct {
	_                     structs.HostLayout
	RingBaseAddress       uint64
	WritePointerAddress   uint64
	ReadPointerAddress    uint64
	DoorbellOffset        uint64
	RingSize              uint32
	GPUID                 uint32
	QueueType             uint32
	QueuePercentage       uint32
	QueuePriority         uint32
	QueueID               uint32
	EOPBufferAddress      uint64
	EOPBufferSize         uint64
	CtxSaveRestoreAddress uint64
	CtxSaveRestoreSize    uint32
	CtlStackSize          uint32
	SdmaEngineID          uint32
	MetadataRingSize      uint32
}

// KFDIoctlDestroyQueueArgs is struct kfd_ioctl_destroy_queue_args.
//
// +marshal
type KFDIoctlDestroyQueueArgs struct {
	_       structs.HostLayout
	QueueID uint32
	Pad     uint32
}

// KFDIoctlUpdateQueueArgs is struct kfd_ioctl_update_queue_args.
//
// +marshal
type KFDIoctlUpdateQueueArgs struct {
	_               structs.HostLayout
	RingBaseAddress uint64
	QueueID         uint32
	RingSize        uint32
	QueuePercentage uint32
	QueuePriority   uint32
}

// KFDIoctlSetCUMaskArgs is struct kfd_ioctl_set_cu_mask_args.
//
// +marshal
type KFDIoctlSetCUMaskArgs struct {
	_         structs.HostLayout
	QueueID   uint32
	NumCUMask uint32
	CUMaskPtr uint64
}

// KFDIoctlGetQueueWaveStateArgs is struct kfd_ioctl_get_queue_wave_state_args.
//
// +marshal
type KFDIoctlGetQueueWaveStateArgs struct {
	_                structs.HostLayout
	CtlStackAddress  uint64
	CtlStackUsedSize uint32
	SaveAreaUsedSize uint32
	QueueID          uint32
	Pad              uint32
}

// KFDIoctlSetMemoryPolicyArgs is struct kfd_ioctl_set_memory_policy_args.
//
// +marshal
type KFDIoctlSetMemoryPolicyArgs struct {
	_                     structs.HostLayout
	AlternateApertureBase uint64
	AlternateApertureSize uint64
	GPUID                 uint32
	DefaultPolicy         uint32
	AlternatePolicy       uint32
	Pad                   uint32
}

// KFDIoctlGetClockCountersArgs is struct kfd_ioctl_get_clock_counters_args.
//
// +marshal
type KFDIoctlGetClockCountersArgs struct {
	_                  structs.HostLayout
	GPUClockCounter    uint64
	CPUClockCounter    uint64
	SystemClockCounter uint64
	SystemClockFreq    uint64
	GPUID              uint32
	Pad                uint32
}

// KFDProcessDeviceApertures is struct kfd_process_device_apertures.
//
// +marshal slice:KFDProcessDeviceAperturesSlice
type KFDProcessDeviceApertures struct {
	_            structs.HostLayout
	LDSBase      uint64
	LDSLimit     uint64
	ScratchBase  uint64
	ScratchLimit uint64
	GPUVMBase    uint64
	GPUVMLimit   uint64
	GPUID        uint32
	Pad          uint32
}

// KFDIoctlGetProcessAperturesArgs is struct
// kfd_ioctl_get_process_apertures_args.
//
// +marshal
type KFDIoctlGetProcessAperturesArgs struct {
	_                structs.HostLayout
	ProcessApertures [NUM_OF_SUPPORTED_GPUS]KFDProcessDeviceApertures
	NumOfNodes       uint32
	Pad              uint32
}

// KFDIoctlGetProcessAperturesNewArgs is struct
// kfd_ioctl_get_process_apertures_new_args.
//
// +marshal
type KFDIoctlGetProcessAperturesNewArgs struct {
	_                            structs.HostLayout
	KFDProcessDeviceAperturesPtr uint64
	NumOfNodes                   uint32
	Pad                          uint32
}

// KFDIoctlAcquireVMArgs is struct kfd_ioctl_acquire_vm_args.
//
// +marshal
type KFDIoctlAcquireVMArgs struct {
	_     structs.HostLayout
	DRMFD uint32
	GPUID uint32
}

// KFDIoctlAllocMemoryOfGPUArgs is struct kfd_ioctl_alloc_memory_of_gpu_args.
//
// +marshal
type KFDIoctlAllocMemoryOfGPUArgs struct {
	_          structs.HostLayout
	VAAddr     uint64
	Size       uint64
	Handle     uint64
	MmapOffset uint64
	GPUID      uint32
	Flags      uint32
}

// KFDIoctlFreeMemoryOfGPUArgs is struct kfd_ioctl_free_memory_of_gpu_args.
//
// +marshal
type KFDIoctlFreeMemoryOfGPUArgs struct {
	_      structs.HostLayout
	Handle uint64
}

// KFDIoctlMapMemoryToGPUArgs is struct kfd_ioctl_map_memory_to_gpu_args.
//
// +marshal
type KFDIoctlMapMemoryToGPUArgs struct {
	_                 structs.HostLayout
	Handle            uint64
	DeviceIDsArrayPtr uint64
	NDevices          uint32
	NSuccess          uint32
}

// KFDIoctlUnmapMemoryFromGPUArgs is struct
// kfd_ioctl_unmap_memory_from_gpu_args.
//
// +marshal
type KFDIoctlUnmapMemoryFromGPUArgs struct {
	_                 structs.HostLayout
	Handle            uint64
	DeviceIDsArrayPtr uint64
	NDevices          uint32
	NSuccess          uint32
}

// KFDIoctlGetAvailableMemoryArgs is struct
// kfd_ioctl_get_available_memory_args.
//
// +marshal
type KFDIoctlGetAvailableMemoryArgs struct {
	_         structs.HostLayout
	Available uint64
	GPUID     uint32
	Pad       uint32
}

// KFDIoctlSetScratchBackingVAArgs is struct
// kfd_ioctl_set_scratch_backing_va_args.
//
// +marshal
type KFDIoctlSetScratchBackingVAArgs struct {
	_      structs.HostLayout
	VAAddr uint64
	GPUID  uint32
	Pad    uint32
}

// KFDIoctlGetTileConfigArgs is struct kfd_ioctl_get_tile_config_args.
//
// +marshal
type KFDIoctlGetTileConfigArgs struct {
	_                   structs.HostLayout
	TileConfigPtr       uint64
	MacroTileConfigPtr  uint64
	NumTileConfigs      uint32
	NumMacroTileConfigs uint32
	GPUID               uint32
	GBAddrConfig        uint32
	NumBanks            uint32
	NumRanks            uint32
}

// KFDIoctlSetTrapHandlerArgs is struct kfd_ioctl_set_trap_handler_args.
//
// +marshal
type KFDIoctlSetTrapHandlerArgs struct {
	_       structs.HostLayout
	TBAAddr uint64
	TMAAddr uint64
	GPUID   uint32
	Pad     uint32
}

// KFDIoctlCreateEventArgs is struct kfd_ioctl_create_event_args.
//
// +marshal
type KFDIoctlCreateEventArgs struct {
	_                structs.HostLayout
	EventPageOffset  uint64
	EventTriggerData uint32
	EventType        uint32
	AutoReset        uint32
	NodeID           uint32
	EventID          uint32
	EventSlotIndex   uint32
}

// KFDIoctlDestroyEventArgs is struct kfd_ioctl_destroy_event_args.
//
// +marshal
type KFDIoctlDestroyEventArgs struct {
	_       structs.HostLayout
	EventID uint32
	Pad     uint32
}

// KFDIoctlSetEventArgs is struct kfd_ioctl_set_event_args.
//
// +marshal
type KFDIoctlSetEventArgs struct {
	_       structs.HostLayout
	EventID uint32
	Pad     uint32
}

// KFDIoctlResetEventArgs is struct kfd_ioctl_reset_event_args.
//
// +marshal
type KFDIoctlResetEventArgs struct {
	_       structs.HostLayout
	EventID uint32
	Pad     uint32
}

// KFDEventData is struct kfd_event_data.
//
// The union (memory_exception_data / hw_exception_data / signal_event_data)
// is represented as a raw byte array; the sentry does not interpret it.
//
// +marshal slice:KFDEventDataSlice
type KFDEventData struct {
	_               structs.HostLayout
	ExceptionData   [32]byte
	KFDEventDataExt uint64
	EventID         uint32
	Pad             uint32
}

// KFDIoctlWaitEventsArgs is struct kfd_ioctl_wait_events_args.
//
// +marshal
type KFDIoctlWaitEventsArgs struct {
	_          structs.HostLayout
	EventsPtr  uint64
	NumEvents  uint32
	WaitForAll uint32
	Timeout    uint32
	WaitResult uint32
}

// KFDIoctlAllocQueueGWSArgs is struct kfd_ioctl_alloc_queue_gws_args.
//
// +marshal
type KFDIoctlAllocQueueGWSArgs struct {
	_        structs.HostLayout
	QueueID  uint32
	NumGWS   uint32
	FirstGWS uint32
	Pad      uint32
}

// KFDIoctlSMIEventsArgs is struct kfd_ioctl_smi_events_args.
//
// +marshal
type KFDIoctlSMIEventsArgs struct {
	_      structs.HostLayout
	GPUID  uint32
	AnonFD uint32
}

// KFDIoctlSetXNACKModeArgs is struct kfd_ioctl_set_xnack_mode_args.
//
// +marshal
type KFDIoctlSetXNACKModeArgs struct {
	_            structs.HostLayout
	XNACKEnabled int32
}

// KFDIoctlGetDmabufInfoArgs is struct kfd_ioctl_get_dmabuf_info_args.
//
// +marshal
type KFDIoctlGetDmabufInfoArgs struct {
	_            structs.HostLayout
	Size         uint64
	MetadataPtr  uint64
	MetadataSize uint32
	GPUID        uint32
	Flags        uint32
	DmabufFD     uint32
}

// KFDIoctlImportDmabufArgs is struct kfd_ioctl_import_dmabuf_args.
//
// +marshal
type KFDIoctlImportDmabufArgs struct {
	_        structs.HostLayout
	VAAddr   uint64
	Handle   uint64
	GPUID    uint32
	DmabufFD uint32
}

// KFDIoctlExportDmabufArgs is struct kfd_ioctl_export_dmabuf_args.
//
// +marshal
type KFDIoctlExportDmabufArgs struct {
	_        structs.HostLayout
	Handle   uint64
	Flags    uint32
	DmabufFD uint32
}

// KFDIoctlRuntimeEnableArgs is struct kfd_ioctl_runtime_enable_args.
//
// +marshal
type KFDIoctlRuntimeEnableArgs struct {
	_                structs.HostLayout
	RDebug           uint64
	ModeMask         uint32
	CapabilitiesMask uint32
}
