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
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
)

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (fd *kfdFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	return vfs.GenericProxyDeviceConfigureMMap(&fd.vfsfd, fd, opts)
}

// Translate implements memmap.Mappable.Translate.
func (fd *kfdFD) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	return []memmap.Translation{
		{
			Source: optional,
			File:   &fd.memmapFile,
			Offset: optional.Start,
			Perms:  hostarch.AnyAccess,
		},
	}, nil
}

// Mapping /dev/kfd from a stub process.
//
// KFD refuses an mmap of /dev/kfd from any process other than the one that
// owns the KFD context, and the owner is always the Sentry: the Sentry opens
// the host device and issues every ioctl. Platforms that map from the Sentry's
// own address space, such as KVM, therefore just work. Systrap does not: it
// maps from a stub process that shares the Sentry's file table, so the
// driver sees the right file but the wrong process.
//
// A modified amdgpu driver adds AMDKFD_IOC_SET_MMAP_TARGET, through which the
// owner names one other process that may receive mappings. The Sentry names
// the stub immediately before the stub's mmap and clears it immediately
// after, so the grant exists only for the duration of a mapping the Sentry
// itself is making. Application code running in the stub cannot use the
// window: systrap's seccomp filter stops it from making host system calls at
// all.
//
// This covers only the pages mmap()ed from /dev/kfd: doorbells and the MMIO
// remap page. GPU memory is mapped through the render node, whose driver
// checks the open file rather than the calling process, and needs nothing.

// kfdMmapTargetMu serialises naming a target with the mmap that uses it.
//
// The driver holds one target per KFD context, and every /dev/kfd this
// sandbox opens shares a single context, because KFD keys contexts on the
// opening process's mm and that is always the Sentry's. Two stubs mapping
// concurrently through different descriptors would otherwise race to name
// themselves.
var kfdMmapTargetMu sync.Mutex

// kfdMmapTargetUnsupported is logged once, when the driver does not implement
// AMDKFD_IOC_SET_MMAP_TARGET.
var kfdMmapTargetUnsupported sync.Once

// kfdMemmapFile is the memmap.File for /dev/kfd.
//
// +stateify savable
type kfdMemmapFile struct {
	fsutil.MmapPreciseFile
}

// WithHostMapTarget implements memmap.HostMapTargetFile.WithHostMapTarget.
func (f *kfdMemmapFile) WithHostMapTarget(tgid int32, mapFn func() error) error {
	kfdMmapTargetMu.Lock()
	defer kfdMmapTargetMu.Unlock()

	hostFD := int32(f.FD())
	if err := kfdSetMmapTarget(hostFD, tgid); err != nil {
		if err == unix.EINVAL || err == unix.ENOTTY {
			kfdMmapTargetUnsupported.Do(func() {
				log.Warningf("amdproxy: the host amdgpu driver does not support AMDKFD_IOC_SET_MMAP_TARGET, so /dev/kfd cannot be mapped on this platform; use a driver that does, or --platform=kvm")
			})
		} else {
			log.Warningf("amdproxy: naming stub %d as the /dev/kfd mmap target failed: %v", tgid, err)
		}
		// Attempt the mapping anyway, so the application sees the error the
		// driver gives for it.
		return mapFn()
	}
	err := mapFn()
	if cerr := kfdSetMmapTarget(hostFD, 0); cerr != nil {
		log.Warningf("amdproxy: clearing the /dev/kfd mmap target failed: %v", cerr)
	}
	return err
}
