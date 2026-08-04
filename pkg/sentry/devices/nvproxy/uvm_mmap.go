// Copyright 2023 The gVisor Authors.
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

package nvproxy

import (
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (fd *uvmFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	// UVM_VALIDATE_VA_RANGE, and probably other ioctls, expect that
	// application mmaps of /dev/nvidia-uvm are immediately visible to the
	// driver.
	return vfs.GenericProxyDeviceConfigureMMap(&fd.vfsfd, fd, opts)
}

// AddMapping implements memmap.Mappable.AddMapping.
//
// Application mappings of /dev/nvidia-uvm reserve the address space that CUDA
// unified memory is committed into, so newly mapped ranges are charged to the
// sandbox's account. Only ranges that were not already mapped are charged;
// mapping the same range twice, including via fork(), reserves no additional
// address space and so must not be charged twice.
func (fd *uvmFD) AddMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) error {
	fd.mappingsMu.Lock()
	defer fd.mappingsMu.Unlock()
	mapped := fd.mappings.AddMapping(ms, ar, offset, writable)
	var mappedBytes uint64
	for _, r := range mapped {
		mappedBytes += r.Length()
	}
	if !fd.dev.nvp.memAcct.reserveUVMVA(ctx, mappedBytes) {
		// Undo the mapping, so that the mapping set continues to reflect only
		// the reservations actually charged.
		fd.mappings.RemoveMapping(ms, ar, offset, writable)
		return linuxerr.ENOMEM
	}
	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
func (fd *uvmFD) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
	fd.mappingsMu.Lock()
	defer fd.mappingsMu.Unlock()
	var unmapped uint64
	for _, r := range fd.mappings.RemoveMapping(ms, ar, offset, writable) {
		unmapped += r.Length()
	}
	// Release only ranges that no longer have any mapping; a range that is
	// still mapped elsewhere still reserves its address space. Without this,
	// a process that repeatedly allocates and frees unified memory would
	// accumulate charges it never releases.
	fd.dev.nvp.memAcct.releaseUVMVA(ctx, unmapped)
}

// CopyMapping implements memmap.Mappable.CopyMapping.
func (fd *uvmFD) CopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	return fd.AddMapping(ctx, ms, dstAR, offset, writable)
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable.
//
// This file's memmap.File and offsets remain consistent across save/restore,
// so its mappings never require invalidation.
func (fd *uvmFD) InvalidateUnsavable(ctx context.Context) error {
	return nil
}

// Translate implements memmap.Mappable.Translate.
func (fd *uvmFD) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	return []memmap.Translation{
		{
			Source: optional,
			File:   &fd.memmapFile,
			Offset: optional.Start,
			Perms:  hostarch.AnyAccess,
		},
	}, nil
}

// uvmFDMemmapFile implements fsutil.MmapFile by extending
// fsutil.MmapPreciseFile with fallback buffered I/O.
//
// +stateify savable
type uvmFDMemmapFile struct {
	fsutil.MmapPreciseFile
}

// MapInternal implements memmap.File.MapInternal.
func (mf *uvmFDMemmapFile) MapInternal(fr memmap.FileRange, at hostarch.AccessType) (safemem.BlockSeq, error) {
	bs, err := mf.MmapPreciseFile.MapInternal(fr, at)
	if err != nil {
		log.Warningf("uvmFDMemmapFile.MapInternal(%v) failed: %v; falling back to buffered I/O", fr, err)
		return safemem.BlockSeq{}, memmap.BufferedIOFallbackErr{}
	}
	return bs, nil
}
