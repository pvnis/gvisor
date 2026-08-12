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

package amdgpu

import (
	"fmt"
	"structs"

	"gvisor.dev/gvisor/pkg/abi/linux"
)

// DRM device numbers, from include/uapi/drm/drm.h and
// drivers/gpu/drm/drm_drv.c.
const (
	// DRM_MAJOR is the device major number shared by all DRM devices.
	DRM_MAJOR = 226

	// DRM_RENDER_MINOR_BASE is the first minor number assigned to a render
	// node (/dev/dri/renderD*), and DRM_RENDER_MINOR_MAX is the last. Card
	// nodes (/dev/dri/card*) use minors below this range; amdproxy does not
	// implement them, because they carry the modesetting interface that a
	// compute workload has no need for.
	DRM_RENDER_MINOR_BASE = 128
	DRM_RENDER_MINOR_MAX  = 255
)

// DRM_IOCTL_BASE is the ioctl type shared by all DRM ioctls ('d').
const DRM_IOCTL_BASE = uint32('d')

// DRM_COMMAND_BASE is the first ioctl number reserved for driver-specific
// commands; amdgpu's own ioctls are numbered from here.
const DRM_COMMAND_BASE = 0x40

// Sizes of the DRM ioctl parameter structs below, verified against
// include/uapi/drm/drm.h and include/uapi/drm/amdgpu_drm.h.
const (
	SizeofDRMVersion          = 64
	SizeofDRMClient           = 40
	SizeofDRMAMDGPUInfo       = 32
	SizeofDRMAMDGPUInfoDev    = 448
	SizeofDRMAMDGPUVRAMGTT    = 24
	SizeofDRMAMDGPUMemoryInfo = 96
)

// AMDGPU_INFO_* query codes for DRM_IOCTL_AMDGPU_INFO's Query field, from
// include/uapi/drm/amdgpu_drm.h. Only the ones this package rewrites are
// named; every other query is forwarded to the driver unchanged.
//
// These five all report how much VRAM or GTT (host memory pinned for the GPU)
// exists or is in use, at device granularity. Forwarded unchanged, a sandboxed
// process could read the real card's capacity regardless of its quota, and
// read what every other process on the host — including other tenants — has
// allocated. Ground-truthed against /usr/include/drm/amdgpu_drm.h on sens1:
// each struct's size and field order below was read back with a C program
// issuing the ioctl directly against the real render node, not from
// documentation.
const (
	AMDGPU_INFO_VRAM_USAGE     = 0x10
	AMDGPU_INFO_GTT_USAGE      = 0x11
	AMDGPU_INFO_VRAM_GTT       = 0x14
	AMDGPU_INFO_VIS_VRAM_USAGE = 0x17
	AMDGPU_INFO_MEMORY         = 0x19
)

// DRMIoctl represents a DRM ioctl command number.
type DRMIoctl uint32

// DRM ioctl command numbers.
var (
	DRM_IOCTL_VERSION    = DRMIoctl(linux.IOWR(DRM_IOCTL_BASE, 0x00, SizeofDRMVersion))
	DRM_IOCTL_GET_CLIENT = DRMIoctl(linux.IOWR(DRM_IOCTL_BASE, 0x05, SizeofDRMClient))

	// DRM_IOCTL_AMDGPU_INFO is write-only in the ioctl encoding even though
	// the driver fills in the buffer that ReturnPointer names.
	DRM_IOCTL_AMDGPU_INFO = DRMIoctl(linux.IOW(DRM_IOCTL_BASE, DRM_COMMAND_BASE+0x05, SizeofDRMAMDGPUInfo))
)

// String implements fmt.Stringer.String.
func (i DRMIoctl) String() string {
	switch i {
	case DRM_IOCTL_VERSION:
		return "DRM_IOCTL_VERSION"
	case DRM_IOCTL_GET_CLIENT:
		return "DRM_IOCTL_GET_CLIENT"
	case DRM_IOCTL_AMDGPU_INFO:
		return "DRM_IOCTL_AMDGPU_INFO"
	default:
		return fmt.Sprintf("UNKNOWN DRM COMMAND %#x", uint32(i))
	}
}

// DRMVersion is struct drm_version. NameLen, DateLen and DescLen are both
// inputs, giving the size of each caller-provided buffer, and outputs, giving
// the size the driver wanted. A caller learns the sizes by making the call
// with null pointers first.
//
// +marshal
type DRMVersion struct {
	_                 structs.HostLayout
	VersionMajor      int32
	VersionMinor      int32
	VersionPatchLevel int32
	Pad               uint32
	NameLen           uint64
	Name              uint64
	DateLen           uint64
	Date              uint64
	DescLen           uint64
	Desc              uint64
}

// DRMClient is struct drm_client.
//
// +marshal
type DRMClient struct {
	_     structs.HostLayout
	Idx   int32
	Auth  int32
	PID   uint64
	UID   uint64
	Magic uint64
	IOCs  uint64
}

// DRMAMDGPUVRAMGTTInfo is struct drm_amdgpu_info_vram_gtt, the answer to
// AMDGPU_INFO_VRAM_GTT: the capacity of each memory pool the GPU draws from.
//
// +marshal
type DRMAMDGPUVRAMGTTInfo struct {
	_                     structs.HostLayout
	VRAMSize              uint64
	VRAMCPUAccessibleSize uint64
	GTTSize               uint64
}

// DRMAMDGPUMemoryInfo is struct drm_amdgpu_memory_info, the answer to
// AMDGPU_INFO_MEMORY: capacity and use of VRAM, the CPU-accessible part of
// VRAM, and GTT. Each is struct drm_amdgpu_heap_info (total, usable, in-use
// and maximum-single-allocation, in that order) and the three appear back to
// back with no padding, which this flattens into one struct rather than
// nesting three separately-marshalled ones.
//
// +marshal
type DRMAMDGPUMemoryInfo struct {
	_                               structs.HostLayout
	VRAMTotalHeapSize               uint64
	VRAMUsableHeapSize              uint64
	VRAMHeapUsage                   uint64
	VRAMMaxAllocation               uint64
	CPUAccessibleVRAMTotalHeapSize  uint64
	CPUAccessibleVRAMUsableHeapSize uint64
	CPUAccessibleVRAMHeapUsage      uint64
	CPUAccessibleVRAMMaxAllocation  uint64
	GTTTotalHeapSize                uint64
	GTTUsableHeapSize               uint64
	GTTHeapUsage                    uint64
	GTTMaxAllocation                uint64
}

// DRMAMDGPUInfo is struct drm_amdgpu_info. ReturnPointer names a buffer of
// ReturnSize bytes that the driver fills in with the answer to Query; the
// trailing union is query-specific input.
//
// +marshal
type DRMAMDGPUInfo struct {
	_             structs.HostLayout
	ReturnPointer uint64
	ReturnSize    uint32
	Query         uint32
	QueryUnion    [16]byte
}
