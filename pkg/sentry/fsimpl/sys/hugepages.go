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

package sys

import (
	"bytes"
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/hugetlb"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
)

// hugepagesStat selects the hugetlb pool counter a hugepagesFile reports.
type hugepagesStat int

const (
	hugepagesTotal hugepagesStat = iota
	hugepagesFree
	hugepagesZero
)

// hugepagesFile implements kernfs.Inode for the counters in
// /sys/kernel/mm/hugepages/hugepages-$SIZEkB.
//
// +stateify savable
type hugepagesFile struct {
	implStatFS
	kernfs.DynamicBytesFile

	pool *hugetlb.Pool
	stat hugepagesStat
}

// Generate implements vfs.DynamicBytesSource.Generate.
func (f *hugepagesFile) Generate(ctx context.Context, buf *bytes.Buffer) error {
	var n uint64
	switch f.stat {
	case hugepagesTotal:
		n = f.pool.TotalPages()
	case hugepagesFree:
		n = f.pool.FreePages()
	}
	fmt.Fprintf(buf, "%d\n", n)
	return nil
}

func (fs *filesystem) newHugepagesFile(ctx context.Context, creds *auth.Credentials, pool *hugetlb.Pool, stat hugepagesStat) kernfs.Inode {
	f := &hugepagesFile{pool: pool, stat: stat}
	f.DynamicBytesFile.Init(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), f, defaultSysMode)
	return f
}

// newHugepagesDir returns /sys/kernel/mm/hugepages, which describes the
// sandbox's hugetlb pool as mm/hugetlb_sysfs.c does. DPDK's EAL enumerates it
// to find huge page sizes and how many pages are free. The pool's size is
// fixed, so the files are read-only.
func (fs *filesystem) newHugepagesDir(ctx context.Context, creds *auth.Credentials) kernfs.Inode {
	var pool *hugetlb.Pool
	if k := kernel.KernelFromContext(ctx); k != nil {
		pool = k.HugetlbPool()
	}
	hstate := fs.newDir(ctx, creds, defaultSysDirMode, map[string]kernfs.Inode{
		"nr_hugepages":            fs.newHugepagesFile(ctx, creds, pool, hugepagesTotal),
		"nr_hugepages_mempolicy":  fs.newHugepagesFile(ctx, creds, pool, hugepagesTotal),
		"free_hugepages":          fs.newHugepagesFile(ctx, creds, pool, hugepagesFree),
		"resv_hugepages":          fs.newHugepagesFile(ctx, creds, pool, hugepagesZero),
		"surplus_hugepages":       fs.newHugepagesFile(ctx, creds, pool, hugepagesZero),
		"nr_overcommit_hugepages": fs.newHugepagesFile(ctx, creds, pool, hugepagesZero),
	})
	return fs.newDir(ctx, creds, defaultSysDirMode, map[string]kernfs.Inode{
		fmt.Sprintf("hugepages-%dkB", pool.HugePageSize()/1024): hstate,
	})
}
