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
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/seccomp"
)

// Filters returns seccomp-bpf filters for this package.
//
// The ioctl allowlist must stay in sync with the dispatch switch in
// kfdFD.Ioctl: it is the host-side backstop that bounds which ioctls the
// Sentry can issue to the real KFD driver even if the Sentry itself is
// compromised. Commands are matched exactly, including the parameter struct
// size encoded in the command number.
func Filters() seccomp.SyscallRules {
	var ioctlRules []seccomp.SyscallRule
	for _, cmd := range []amdgpu.KFDIoctl{
		amdgpu.AMDKFD_IOC_GET_VERSION,
		amdgpu.AMDKFD_IOC_DESTROY_QUEUE,
		amdgpu.AMDKFD_IOC_UPDATE_QUEUE,
		amdgpu.AMDKFD_IOC_SET_MEMORY_POLICY,
		amdgpu.AMDKFD_IOC_GET_CLOCK_COUNTERS,
		amdgpu.AMDKFD_IOC_CREATE_EVENT,
		amdgpu.AMDKFD_IOC_DESTROY_EVENT,
		amdgpu.AMDKFD_IOC_SET_EVENT,
		amdgpu.AMDKFD_IOC_RESET_EVENT,
		amdgpu.AMDKFD_IOC_ALLOC_MEMORY_OF_GPU,
		amdgpu.AMDKFD_IOC_FREE_MEMORY_OF_GPU,
		amdgpu.AMDKFD_IOC_AVAILABLE_MEMORY,
		amdgpu.AMDKFD_IOC_SET_SCRATCH_BACKING_VA,
		amdgpu.AMDKFD_IOC_SET_TRAP_HANDLER,
		amdgpu.AMDKFD_IOC_ALLOC_QUEUE_GWS,
		amdgpu.AMDKFD_IOC_SET_XNACK_MODE,
		amdgpu.AMDKFD_IOC_RUNTIME_ENABLE,
		amdgpu.AMDKFD_IOC_ACQUIRE_VM,
		amdgpu.AMDKFD_IOC_GET_PROCESS_APERTURES_NEW,
		amdgpu.AMDKFD_IOC_MAP_MEMORY_TO_GPU,
		amdgpu.AMDKFD_IOC_UNMAP_MEMORY_FROM_GPU,
		amdgpu.AMDKFD_IOC_SET_CU_MASK,
	} {
		ioctlRules = append(ioctlRules, seccomp.PerArg{
			seccomp.NonNegativeFD{},
			seccomp.EqualTo(cmd),
		})
	}
	for _, cmd := range []amdgpu.DRMIoctl{
		amdgpu.DRM_IOCTL_VERSION,
		amdgpu.DRM_IOCTL_GET_CLIENT,
		amdgpu.DRM_IOCTL_AMDGPU_INFO,
	} {
		ioctlRules = append(ioctlRules, seccomp.PerArg{
			seccomp.NonNegativeFD{},
			seccomp.EqualTo(cmd),
		})
	}
	return seccomp.MakeSyscallRules(map[uintptr]seccomp.SyscallRule{
		unix.SYS_IOCTL: seccomp.Or(ioctlRules),
		unix.SYS_MMAP: seccomp.PerArg{
			seccomp.AnyValue{},
			seccomp.AnyValue{},
			seccomp.AnyValue{},
			seccomp.EqualTo(unix.MAP_SHARED | unix.MAP_FIXED_NOREPLACE),
		},
		unix.SYS_MREMAP: seccomp.PerArg{
			seccomp.AnyValue{},
			seccomp.EqualTo(0), /* old_size */
			seccomp.AnyValue{},
			seccomp.EqualTo(linux.MREMAP_MAYMOVE | linux.MREMAP_FIXED),
			seccomp.AnyValue{},
			seccomp.EqualTo(0),
		},
	})
}
