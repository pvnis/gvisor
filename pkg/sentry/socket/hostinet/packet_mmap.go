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

package hostinet

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
)

// packetMMap maps the PACKET_MMAP rings (PACKET_RX_RING, PACKET_TX_RING) of a
// host AF_PACKET socket into the application, so that the host kernel fills
// and drains them directly, as for DPDK's af_packet PMD. The ring options are
// passed through to the host socket (see SockOpts).
//
// +stateify savable
type packetMMap struct {
	mu sync.Mutex `state:"nosave"`

	// +checklocks:mu
	mappings memmap.MappingSet

	// file maps the host socket. Once set, it owns the host socket fd, which
	// it closes when the socket is released and no longer mapped: like
	// Linux, the rings outlive the socket while they are mapped. Its fd is
	// set once, under mu, before the socket can be mapped, so Translate may
	// use it without mu.
	file fsutil.MmapPreciseFile
	// +checklocks:mu
	fileSet bool
}

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (s *Socket) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	if s.family != linux.AF_PACKET {
		return linuxerr.ENODEV
	}
	s.mmap.mu.Lock()
	if !s.mmap.fileSet {
		s.mmap.file.SetFD(s.fd)
		s.mmap.fileSet = true
	}
	s.mmap.mu.Unlock()
	return vfs.GenericProxyDeviceConfigureMMap(&s.vfsfd, s, opts)
}

// releaseMMap releases the host socket fd to the ring mapping, if the socket
// was mapped, and returns true if it did.
func (s *Socket) releaseMMap() bool {
	s.mmap.mu.Lock()
	defer s.mmap.mu.Unlock()
	if !s.mmap.fileSet {
		return false
	}
	s.mmap.file.MappableRelease() // Eventually closes s.fd.
	return true
}

// AddMapping implements memmap.Mappable.AddMapping.
func (s *Socket) AddMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) error {
	s.mmap.mu.Lock()
	defer s.mmap.mu.Unlock()
	s.mmap.mappings.AddMapping(ms, ar, offset, writable)
	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
func (s *Socket) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
	s.mmap.mu.Lock()
	defer s.mmap.mu.Unlock()
	s.mmap.mappings.RemoveMapping(ms, ar, offset, writable)
}

// CopyMapping implements memmap.Mappable.CopyMapping.
func (s *Socket) CopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	return s.AddMapping(ctx, ms, dstAR, offset, writable)
}

// Translate implements memmap.Mappable.Translate. The host kernel validates
// the range when it is mapped.
func (s *Socket) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	return []memmap.Translation{
		{
			Source: optional,
			File:   &s.mmap.file,
			Offset: optional.Start,
			Perms:  hostarch.AnyAccess,
		},
	}, nil
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable.
func (s *Socket) InvalidateUnsavable(ctx context.Context) error {
	s.mmap.mu.Lock()
	defer s.mmap.mu.Unlock()
	s.mmap.mappings.InvalidateAll(memmap.InvalidateOpts{InvalidatePrivate: true})
	return nil
}
