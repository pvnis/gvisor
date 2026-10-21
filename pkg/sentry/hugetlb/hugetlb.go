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

// Package hugetlb provides the sandbox's pool of huge pages, the equivalent of
// Linux's default hstate (/proc/sys/vm/nr_hugepages).
//
// Files in hugetlbfs mounts, and memfds and SysV shm segments created with
// the HUGETLB flags, allocate from the pool. gVisor has no host hugetlb pages
// of its own: the pool is an accounting limit, and its memory comes from the
// sandbox's memory file like any other, using host transparent huge pages
// where they are enabled (--app-huge-pages). It is not physically contiguous,
// so it suits applications that use IOVA-as-VA (e.g. DPDK with VFIO and an
// IOMMU, or software PMDs) but not those that need physical addresses.
package hugetlb

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
)

// smallPagesPerHugePage is the number of small pages in a huge page.
const smallPagesPerHugePage = hostarch.HugePageSize / hostarch.PageSize

// Pool is a pool of huge pages.
//
// +stateify savable
type Pool struct {
	// totalPages is the size of the pool in huge pages. Immutable.
	totalPages uint64

	// usedSmallPages is the number of small pages allocated from the pool.
	// Files in the pool are faulted in at small page granularity.
	usedSmallPages atomicbitops.Uint64
}

// NewPool returns a pool of the given number of huge pages.
func NewPool(hugePages uint64) *Pool {
	return &Pool{totalPages: hugePages}
}

// HugePageSize returns the size of the pool's pages in bytes.
func (p *Pool) HugePageSize() uint64 {
	return hostarch.HugePageSize
}

// TotalPages returns the size of the pool in huge pages.
func (p *Pool) TotalPages() uint64 {
	if p == nil {
		return 0
	}
	return p.totalPages
}

// FreePages returns the number of huge pages in the pool that have not been
// allocated. A partially used huge page counts as allocated.
func (p *Pool) FreePages() uint64 {
	if p == nil {
		return 0
	}
	used := (p.usedSmallPages.Load() + smallPagesPerHugePage - 1) / smallPagesPerHugePage
	if used >= p.totalPages {
		return 0
	}
	return p.totalPages - used
}

func (p *Pool) limit() uint64 {
	return p.totalPages * smallPagesPerHugePage
}

// Account allocates n small pages from the pool, all or nothing.
func (p *Pool) Account(n uint64) bool {
	for {
		used := p.usedSmallPages.Load()
		if used+n > p.limit() || used+n < used {
			return false
		}
		if p.usedSmallPages.CompareAndSwap(used, used+n) {
			return true
		}
	}
}

// AccountPartial allocates up to n small pages from the pool and returns how
// many it allocated.
func (p *Pool) AccountPartial(n uint64) uint64 {
	for {
		used := p.usedSmallPages.Load()
		limit := p.limit()
		if used >= limit {
			return 0
		}
		toInc := min(n, limit-used)
		if p.usedSmallPages.CompareAndSwap(used, used+toInc) {
			return toInc
		}
	}
}

// Unaccount returns n small pages to the pool.
func (p *Pool) Unaccount(n uint64) {
	for {
		used := p.usedSmallPages.Load()
		if used < n {
			panic(fmt.Sprintf("returning more pages to the hugetlb pool than allocated: used = %d, n = %d", used, n))
		}
		if p.usedSmallPages.CompareAndSwap(used, used-n) {
			return
		}
	}
}

// contextID is the hugetlb package's type for context.Context.Value keys.
type contextID int

const (
	// CtxPool is a Context.Value key for the sandbox's *Pool.
	CtxPool contextID = iota
)

// PoolFromContext returns the hugetlb pool used by ctx, or an empty pool if
// ctx has none.
func PoolFromContext(ctx context.Context) *Pool {
	if v := ctx.Value(CtxPool); v != nil {
		if p := v.(*Pool); p != nil {
			return p
		}
	}
	return NewPool(0)
}
