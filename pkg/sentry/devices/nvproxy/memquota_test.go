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

package nvproxy

import (
	"sort"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// newTestClient returns a rootClient usable with objAdd()/objFree() without a
// real driver FD.
func newTestClient(nvp *nvproxy, h uint32) *rootClient {
	c := &rootClient{
		resources: make(map[nvgpu.Handle]*object),
	}
	c.nvp = nvp
	c.handle = nvgpu.Handle{Val: h}
	return c
}

// addMem reserves size bytes for class and records an object holding it, as
// the allocation paths do. It fails the test if the reservation is denied.
func addMem(t *testing.T, nvp *nvproxy, client *rootClient, h nvgpu.Handle, class nvgpu.ClassID, size uint64, parent nvgpu.Handle) {
	t.Helper()
	ctx := context.Background()
	charge, ok := nvp.memAcct.reserveForClass(ctx, class, size)
	if !ok {
		t.Fatalf("reserveForClass(%v, %d) denied", class, size)
	}
	nvp.objAddMem(ctx, client, h, class, &miscObject{}, charge, parent)
}

func handle(val uint32) nvgpu.Handle {
	return nvgpu.Handle{Val: val}
}

// checkUsage fails the test if the account's counters do not match.
func checkUsage(t *testing.T, nvp *nvproxy, wantVRAM, wantPinnedHost uint64) {
	t.Helper()
	vram, pinnedHost, _ := nvp.memAcct.usage()
	if vram != wantVRAM {
		t.Errorf("VRAM usage = %d, want %d", vram, wantVRAM)
	}
	if pinnedHost != wantPinnedHost {
		t.Errorf("pinned host usage = %d, want %d", pinnedHost, wantPinnedHost)
	}
}

// checkUVMUsage fails the test if the account's UVM address space reservation
// does not match.
func checkUVMUsage(t *testing.T, nvp *nvproxy, want uint64) {
	t.Helper()
	if _, _, uvmVA := nvp.memAcct.usage(); uvmVA != want {
		t.Errorf("UVM VA usage = %d, want %d", uvmVA, want)
	}
}

// TestMemAccountChargeAndRelease tests that a memory object is charged when
// allocated and released when freed.
func TestMemAccountChargeAndRelease(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	client := newTestClient(nvp, 1)

	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 4096, handle(nvgpu.NV01_NULL_OBJECT))
	checkUsage(t, nvp, 4096, 0)

	nvp.objFree(ctx, client, handle(10))
	checkUsage(t, nvp, 0, 0)
}

// TestMemAccountKindsAreSeparate tests that device memory and host memory
// pinned for DMA are accounted against separate counters. Conflating them
// would let allocations of one kind consume the other's budget.
func TestMemAccountKindsAreSeparate(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	client := newTestClient(nvp, 1)

	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 1024, handle(nvgpu.NV01_NULL_OBJECT))
	addMem(t, nvp, client, handle(11), nvgpu.NV01_MEMORY_SYSTEM, 2048, handle(nvgpu.NV01_NULL_OBJECT))
	checkUsage(t, nvp, 1024, 2048)

	nvp.objFree(ctx, client, handle(10))
	checkUsage(t, nvp, 0, 2048)

	nvp.objFree(ctx, client, handle(11))
	checkUsage(t, nvp, 0, 0)
}

// TestMemAccountVirtualNotCharged tests that virtual address space
// reservations commit no physical memory and so are not charged. The memory
// backing such a range is charged to the allocation that provides it.
func TestMemAccountVirtualNotCharged(t *testing.T) {
	nvp := &nvproxy{}
	client := newTestClient(nvp, 1)

	for i, class := range []nvgpu.ClassID{nvgpu.NV01_MEMORY_VIRTUAL, nvgpu.NV50_MEMORY_VIRTUAL} {
		addMem(t, nvp, client, handle(uint32(20+i)), class, 1<<30, handle(nvgpu.NV01_NULL_OBJECT))
	}
	checkUsage(t, nvp, 0, 0)
}

// TestMemAccountCascadeFree tests that freeing an object also releases the
// charges of the objects freed as a consequence of it. Freeing an object
// cascades to its dependents, so a single NV_ESC_RM_FREE may release many
// charged objects; releasing only the directly-named handle would leak the
// rest, permanently overstating the sandbox's usage.
func TestMemAccountCascadeFree(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	client := newTestClient(nvp, 1)

	// A device object holding no memory, with two memory objects as children.
	nvp.objAdd(ctx, client, handle(10), nvgpu.NV01_DEVICE_0, &miscObject{}, handle(nvgpu.NV01_NULL_OBJECT))
	addMem(t, nvp, client, handle(11), nvgpu.NV01_MEMORY_LOCAL_USER, 4096, handle(10))
	addMem(t, nvp, client, handle(12), nvgpu.NV01_MEMORY_LOCAL_USER, 8192, handle(10))
	checkUsage(t, nvp, 4096+8192, 0)

	// Freeing the parent must release both children's charges.
	nvp.objFree(ctx, client, handle(10))
	checkUsage(t, nvp, 0, 0)
}

// TestMemAccountCascadeFreeTransitive tests that charges are released through
// a multi-level dependency chain, not just from direct children.
func TestMemAccountCascadeFreeTransitive(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	client := newTestClient(nvp, 1)

	nvp.objAdd(ctx, client, handle(10), nvgpu.NV01_DEVICE_0, &miscObject{}, handle(nvgpu.NV01_NULL_OBJECT))
	addMem(t, nvp, client, handle(11), nvgpu.NV01_MEMORY_LOCAL_USER, 1000, handle(10))
	addMem(t, nvp, client, handle(12), nvgpu.NV01_MEMORY_LOCAL_USER, 2000, handle(11))
	addMem(t, nvp, client, handle(13), nvgpu.NV01_MEMORY_LOCAL_USER, 3000, handle(12))
	checkUsage(t, nvp, 6000, 0)

	nvp.objFree(ctx, client, handle(10))
	checkUsage(t, nvp, 0, 0)
}

// TestMemAccountDupNotDoubleCharged tests that duplicating a memory handle
// into another client does not charge the underlying allocation twice: a
// duplicate aliases existing memory rather than committing more.
func TestMemAccountDupNotDoubleCharged(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	clientSrc := newTestClient(nvp, 1)
	clientDst := newTestClient(nvp, 2)

	addMem(t, nvp, clientSrc, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 4096, handle(nvgpu.NV01_NULL_OBJECT))
	checkUsage(t, nvp, 4096, 0)

	nvp.objDup(ctx, clientDst, clientSrc, handle(20), handle(nvgpu.NV01_NULL_OBJECT), handle(10))
	// The duplicate refers to the same memory; usage must not change.
	checkUsage(t, nvp, 4096, 0)

	// Freeing the original must not release memory still referenced by the
	// duplicate, or the sandbox could hold memory it is no longer charged for.
	nvp.objFree(ctx, clientSrc, handle(10))
	checkUsage(t, nvp, 4096, 0)

	// Freeing the last alias releases the charge.
	nvp.objFree(ctx, clientDst, handle(20))
	checkUsage(t, nvp, 0, 0)
}

// TestMemAccountDupFreedInReverseOrder tests that alias accounting does not
// depend on the order in which aliases are freed.
func TestMemAccountDupFreedInReverseOrder(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	clientSrc := newTestClient(nvp, 1)
	clientDst := newTestClient(nvp, 2)

	addMem(t, nvp, clientSrc, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 4096, handle(nvgpu.NV01_NULL_OBJECT))
	nvp.objDup(ctx, clientDst, clientSrc, handle(20), handle(nvgpu.NV01_NULL_OBJECT), handle(10))
	checkUsage(t, nvp, 4096, 0)

	nvp.objFree(ctx, clientDst, handle(20))
	checkUsage(t, nvp, 4096, 0)

	nvp.objFree(ctx, clientSrc, handle(10))
	checkUsage(t, nvp, 0, 0)
}

// TestMemAccountRepeatedDup tests that many aliases of one allocation are
// charged exactly once, and released only when the last is freed.
func TestMemAccountRepeatedDup(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	clientSrc := newTestClient(nvp, 1)
	clientDst := newTestClient(nvp, 2)

	const numDups = 16
	addMem(t, nvp, clientSrc, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 4096, handle(nvgpu.NV01_NULL_OBJECT))
	for i := 0; i < numDups; i++ {
		nvp.objDup(ctx, clientDst, clientSrc, handle(uint32(100+i)), handle(nvgpu.NV01_NULL_OBJECT), handle(10))
	}
	checkUsage(t, nvp, 4096, 0)

	nvp.objFree(ctx, clientSrc, handle(10))
	for i := 0; i < numDups-1; i++ {
		nvp.objFree(ctx, clientDst, handle(uint32(100+i)))
		checkUsage(t, nvp, 4096, 0)
	}
	nvp.objFree(ctx, clientDst, handle(uint32(100+numDups-1)))
	checkUsage(t, nvp, 0, 0)
}

// TestMemAccountDupInvalidHandle tests that duplicating to an invalid handle
// takes no reference on the original's charge. objAdd() ignores such handles,
// so the duplicate is never recorded and would never be freed; taking a
// reference would hold the charge outstanding for the sandbox's lifetime.
func TestMemAccountDupInvalidHandle(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	clientSrc := newTestClient(nvp, 1)
	clientDst := newTestClient(nvp, 2)

	addMem(t, nvp, clientSrc, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 4096, handle(nvgpu.NV01_NULL_OBJECT))
	nvp.objDup(ctx, clientDst, clientSrc, handle(nvgpu.NV01_NULL_OBJECT), handle(nvgpu.NV01_NULL_OBJECT), handle(10))
	checkUsage(t, nvp, 4096, 0)

	// Freeing the original must release the charge despite the failed dup.
	nvp.objFree(ctx, clientSrc, handle(10))
	checkUsage(t, nvp, 0, 0)
}

// TestMemAccountHandleCollision tests that reusing an in-use handle does not
// leak the displaced object's charge. The displaced object is no longer
// reachable by handle and so will never be freed; leaving it charged would let
// repeated collisions inflate accounted usage without committing memory.
func TestMemAccountHandleCollision(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	client := newTestClient(nvp, 1)

	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 4096, handle(nvgpu.NV01_NULL_OBJECT))
	checkUsage(t, nvp, 4096, 0)

	// Reuse the same handle for a different allocation.
	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 1024, handle(nvgpu.NV01_NULL_OBJECT))
	checkUsage(t, nvp, 1024, 0)

	nvp.objFree(ctx, client, handle(10))
	checkUsage(t, nvp, 0, 0)
}

// TestMemAccountZeroSizeNotCharged tests that an allocation of zero bytes
// creates no charge.
func TestMemAccountZeroSizeNotCharged(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	client := newTestClient(nvp, 1)

	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 0, handle(nvgpu.NV01_NULL_OBJECT))
	checkUsage(t, nvp, 0, 0)

	nvp.objFree(ctx, client, handle(10))
	checkUsage(t, nvp, 0, 0)
}

// testMappingSpace is a memmap.MappingSpace that records nothing; the UVM
// accounting under test never invalidates.
type testMappingSpace struct{}

// Invalidate implements memmap.MappingSpace.Invalidate.
func (testMappingSpace) Invalidate(ar hostarch.AddrRange, opts memmap.InvalidateOpts) {}

func newTestUVMFD(nvp *nvproxy) *uvmFD {
	return &uvmFD{dev: &uvmDevice{nvp: nvp}}
}

func addrRange(start, length uint64) hostarch.AddrRange {
	return hostarch.AddrRange{Start: hostarch.Addr(start), End: hostarch.Addr(start + length)}
}

// TestUVMAddRemoveMapping tests that mapping /dev/nvidia-uvm charges the
// address space reserved for unified memory, and that unmapping releases it.
func TestUVMAddRemoveMapping(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	fd := newTestUVMFD(nvp)
	var ms testMappingSpace
	const size = 64 << 20

	if err := fd.AddMapping(ctx, ms, addrRange(0x100000, size), 0, true); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	checkUVMUsage(t, nvp, size)

	fd.RemoveMapping(ctx, ms, addrRange(0x100000, size), 0, true)
	checkUVMUsage(t, nvp, 0)
}

// TestUVMDuplicateMappingChargedOnce tests that mapping the same file range
// from two address spaces, as happens across fork(), reserves no additional
// address space and so is charged only once.
func TestUVMDuplicateMappingChargedOnce(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	fd := newTestUVMFD(nvp)
	var ms1, ms2 testMappingSpace
	const size = 64 << 20

	if err := fd.AddMapping(ctx, ms1, addrRange(0x100000, size), 0, true); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	checkUVMUsage(t, nvp, size)

	// A second mapping of the same file range at a different address.
	if err := fd.AddMapping(ctx, &ms2, addrRange(0x800000, size), 0, true); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	checkUVMUsage(t, nvp, size)

	// Removing one mapping leaves the range reserved by the other.
	fd.RemoveMapping(ctx, ms1, addrRange(0x100000, size), 0, true)
	checkUVMUsage(t, nvp, size)

	fd.RemoveMapping(ctx, &ms2, addrRange(0x800000, size), 0, true)
	checkUVMUsage(t, nvp, 0)
}

// TestUVMPartialUnmap tests that unmapping part of a reserved range releases
// only the part that is no longer mapped.
func TestUVMPartialUnmap(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	fd := newTestUVMFD(nvp)
	var ms testMappingSpace
	const size = 64 << 20
	const half = size / 2

	if err := fd.AddMapping(ctx, ms, addrRange(0x100000, size), 0, true); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	checkUVMUsage(t, nvp, size)

	fd.RemoveMapping(ctx, ms, addrRange(0x100000, half), 0, true)
	checkUVMUsage(t, nvp, half)

	fd.RemoveMapping(ctx, ms, addrRange(0x100000+half, half), half, true)
	checkUVMUsage(t, nvp, 0)
}

// TestUVMDisjointRangesAccumulate tests that separate reservations sum, as
// they do when an application makes several unified memory allocations.
func TestUVMDisjointRangesAccumulate(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	fd := newTestUVMFD(nvp)
	var ms testMappingSpace
	const size = 64 << 20
	const n = 4

	for i := uint64(0); i < n; i++ {
		off := i * size
		if err := fd.AddMapping(ctx, ms, addrRange(0x100000+off, size), off, true); err != nil {
			t.Fatalf("AddMapping: %v", err)
		}
	}
	checkUVMUsage(t, nvp, n*size)

	for i := uint64(0); i < n; i++ {
		off := i * size
		fd.RemoveMapping(ctx, ms, addrRange(0x100000+off, size), off, true)
	}
	checkUVMUsage(t, nvp, 0)
}

// TestUVMCopyMapping tests that copying a mapping to a new address range,
// which maps the same file range again, is not charged twice.
func TestUVMCopyMapping(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	fd := newTestUVMFD(nvp)
	var ms testMappingSpace
	const size = 64 << 20

	if err := fd.AddMapping(ctx, ms, addrRange(0x100000, size), 0, true); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	checkUVMUsage(t, nvp, size)

	if err := fd.CopyMapping(ctx, ms, addrRange(0x100000, size), addrRange(0x900000, size), 0, true); err != nil {
		t.Fatalf("CopyMapping: %v", err)
	}
	checkUVMUsage(t, nvp, size)
}

// TestAllocParamsSize tests extraction of the requested allocation size from
// allocation parameter structs.
func TestAllocParamsSize(t *testing.T) {
	// A parameter type that reports a size.
	if got := allocParamsSize(&nvgpu.NV_MEMORY_ALLOCATION_PARAMS{Size: 4096}); got != 4096 {
		t.Errorf("allocParamsSize(NV_MEMORY_ALLOCATION_PARAMS{Size: 4096}) = %d, want 4096", got)
	}

	// NV_MEMORY_ALLOCATION_PARAMS_V545 embeds NV_MEMORY_ALLOCATION_PARAMS, so
	// it reports a size through the promoted method.
	v545 := &nvgpu.NV_MEMORY_ALLOCATION_PARAMS_V545{}
	v545.Size = 8192
	if got := allocParamsSize(v545); got != 8192 {
		t.Errorf("allocParamsSize(NV_MEMORY_ALLOCATION_PARAMS_V545{Size: 8192}) = %d, want 8192", got)
	}

	// A parameter type that does not report a size.
	if got := allocParamsSize(&nvgpu.NV0005_ALLOC_PARAMETERS{}); got != 0 {
		t.Errorf("allocParamsSize(NV0005_ALLOC_PARAMETERS{}) = %d, want 0", got)
	}

	// Allocations may carry no parameters at all; a nil pointer of a type that
	// implements the interface must not be dereferenced.
	if got := allocParamsSize[nvgpu.NV_MEMORY_ALLOCATION_PARAMS](nil); got != 0 {
		t.Errorf("allocParamsSize(nil) = %d, want 0", got)
	}
}

// TestNVOS02AllocSize tests conversion of NV_ESC_RM_ALLOC_MEMORY's inclusive
// upper bound into a length, including the overflow case an application can
// request directly.
func TestNVOS02AllocSize(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit uint64
		want  uint64
	}{
		{"one byte", 0, 1},
		{"page", 4095, 4096},
		{"overflow", ^uint64(0), 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := nvos02AllocSize(&nvgpu.NVOS02_PARAMETERS{Limit: test.limit})
			if got != test.want {
				t.Errorf("nvos02AllocSize(Limit=%#x) = %d, want %d", test.limit, got, test.want)
			}
		})
	}
}

// TestMemKindOfClass tests the mapping from allocation class to accounted
// memory kind. NV01_MEMORY_SYSTEM in particular is host memory pinned for DMA
// rather than device memory, despite being reached through the same
// allocation ioctls as device memory.
func TestMemKindOfClass(t *testing.T) {
	for _, test := range []struct {
		name  string
		class nvgpu.ClassID
		want  memKind
	}{
		{"local user", nvgpu.NV01_MEMORY_LOCAL_USER, memKindVRAM},
		{"local privileged", nvgpu.NV01_MEMORY_LOCAL_PRIVILEGED, memKindVRAM},
		{"system", nvgpu.NV01_MEMORY_SYSTEM, memKindPinnedHost},
		{"os descriptor", nvgpu.NV01_MEMORY_SYSTEM_OS_DESCRIPTOR, memKindPinnedHost},
		{"virtual", nvgpu.NV01_MEMORY_VIRTUAL, memKindNone},
		{"virtual 50", nvgpu.NV50_MEMORY_VIRTUAL, memKindNone},
		{"device", nvgpu.NV01_DEVICE_0, memKindNone},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := memKindOfClass(test.class); got != test.want {
				t.Errorf("memKindOfClass(%v) = %v, want %v", test.class, got, test.want)
			}
		})
	}
}

// TestLimitAdmitsUpToLimit tests that allocations are admitted while they fit
// within the limit, and denied once they do not.
func TestLimitAdmitsUpToLimit(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 4096
	client := newTestClient(nvp, 1)

	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 3072, handle(nvgpu.NV01_NULL_OBJECT))
	checkUsage(t, nvp, 3072, 0)

	// Exactly filling the remaining headroom is allowed.
	addMem(t, nvp, client, handle(11), nvgpu.NV01_MEMORY_LOCAL_USER, 1024, handle(nvgpu.NV01_NULL_OBJECT))
	checkUsage(t, nvp, 4096, 0)

	// One more byte is not.
	if _, ok := nvp.memAcct.reserveForClass(ctx, nvgpu.NV01_MEMORY_LOCAL_USER, 1); ok {
		t.Errorf("reserve beyond limit succeeded, want denial")
	}
	checkUsage(t, nvp, 4096, 0)
}

// TestLimitDeniedReservationChargesNothing tests that a denied reservation
// leaves the account unchanged, so that repeated failures cannot accumulate.
func TestLimitDeniedReservationChargesNothing(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 4096

	for i := 0; i < 10; i++ {
		if _, ok := nvp.memAcct.reserveForClass(ctx, nvgpu.NV01_MEMORY_LOCAL_USER, 8192); ok {
			t.Fatalf("reserve of 8192 with limit 4096 succeeded, want denial")
		}
	}
	checkUsage(t, nvp, 0, 0)
}

// TestLimitReleaseRestoresHeadroom tests that freeing memory allows a
// subsequent allocation that would otherwise be denied.
func TestLimitReleaseRestoresHeadroom(t *testing.T) {
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 4096
	client := newTestClient(nvp, 1)

	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 4096, handle(nvgpu.NV01_NULL_OBJECT))
	if _, ok := nvp.memAcct.reserveForClass(context.Background(), nvgpu.NV01_MEMORY_LOCAL_USER, 4096); ok {
		t.Fatalf("reserve at limit succeeded, want denial")
	}

	nvp.objFree(context.Background(), client, handle(10))
	checkUsage(t, nvp, 0, 0)
	addMem(t, nvp, client, handle(11), nvgpu.NV01_MEMORY_LOCAL_USER, 4096, handle(nvgpu.NV01_NULL_OBJECT))
	checkUsage(t, nvp, 4096, 0)
}

// TestLimitOverflowDenied tests that a size chosen to overflow the limit
// comparison is denied rather than admitted.
func TestLimitOverflowDenied(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 4096

	for _, size := range []uint64{^uint64(0), ^uint64(0) - 4095, 1 << 63} {
		if _, ok := nvp.memAcct.reserveForClass(ctx, nvgpu.NV01_MEMORY_LOCAL_USER, size); ok {
			t.Errorf("reserve of %d with limit 4096 succeeded, want denial", size)
		}
		checkUsage(t, nvp, 0, 0)
	}
}

// TestLimitSharedBetweenVRAMAndUVM tests that device memory and reserved
// unified memory address space draw on the same GPU memory budget: unified
// memory is committed into device memory, so accounting them separately would
// let a sandbox obtain twice its limit.
func TestLimitSharedBetweenVRAMAndUVM(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 8192
	client := newTestClient(nvp, 1)
	fd := newTestUVMFD(nvp)
	var ms testMappingSpace

	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 4096, handle(nvgpu.NV01_NULL_OBJECT))

	// UVM reservation draws on the same budget.
	if err := fd.AddMapping(ctx, ms, addrRange(0x100000, 4096), 0, true); err != nil {
		t.Fatalf("AddMapping within limit: %v", err)
	}
	checkUVMUsage(t, nvp, 4096)

	// The budget is now exhausted by the two together.
	if err := fd.AddMapping(ctx, ms, addrRange(0x200000, 4096), 4096, true); err == nil {
		t.Errorf("AddMapping beyond limit succeeded, want ENOMEM")
	}
	checkUVMUsage(t, nvp, 4096)
}

// TestLimitDeniedMappingNotTracked tests that a mapping refused for exceeding
// the limit is not left in the mapping set, which would otherwise release a
// charge that was never taken when it is later unmapped.
func TestLimitDeniedMappingNotTracked(t *testing.T) {
	ctx := context.Background()
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 4096
	fd := newTestUVMFD(nvp)
	var ms testMappingSpace

	if err := fd.AddMapping(ctx, ms, addrRange(0x100000, 4096), 0, true); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	checkUVMUsage(t, nvp, 4096)

	if err := fd.AddMapping(ctx, ms, addrRange(0x200000, 4096), 4096, true); err == nil {
		t.Fatalf("AddMapping beyond limit succeeded, want ENOMEM")
	}

	// Removing the mapping that was actually made returns the account to zero.
	fd.RemoveMapping(ctx, ms, addrRange(0x100000, 4096), 0, true)
	checkUVMUsage(t, nvp, 0)
}

// TestPinnedHostNotLimited tests that host memory pinned for DMA does not
// consume the GPU memory budget, since it is host memory.
func TestPinnedHostNotLimited(t *testing.T) {
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 4096
	client := newTestClient(nvp, 1)

	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_SYSTEM, 1<<30, handle(nvgpu.NV01_NULL_OBJECT))
	checkUsage(t, nvp, 0, 1<<30)
}

// TestNoLimitAdmitsEverything tests that an unconfigured account enforces
// nothing, so that sandboxes without a quota behave as before.
func TestNoLimitAdmitsEverything(t *testing.T) {
	nvp := &nvproxy{}
	client := newTestClient(nvp, 1)

	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, 1<<40, handle(nvgpu.NV01_NULL_OBJECT))
	checkUsage(t, nvp, 1<<40, 0)
}

// fbParams builds NV2080_CTRL_CMD_FB_GET_INFO_V2 parameters reporting the
// given total and free sizes, in bytes.
func fbParams(total, free uint64) *nvgpu.NV2080_CTRL_FB_GET_INFO_V2_PARAMS {
	p := &nvgpu.NV2080_CTRL_FB_GET_INFO_V2_PARAMS{FBInfoListSize: 2}
	p.FBInfoList[0].Index = nvgpu.NV2080_CTRL_FB_INFO_INDEX_HEAP_SIZE
	p.FBInfoList[0].Data = uint32(total / fbInfoUnitBytes)
	p.FBInfoList[1].Index = nvgpu.NV2080_CTRL_FB_INFO_INDEX_HEAP_FREE
	p.FBInfoList[1].Data = uint32(free / fbInfoUnitBytes)
	return p
}

func fbResult(p *nvgpu.NV2080_CTRL_FB_GET_INFO_V2_PARAMS) (total, free uint64) {
	return uint64(p.FBInfoList[0].Data) * fbInfoUnitBytes, uint64(p.FBInfoList[1].Data) * fbInfoUnitBytes
}

// TestVirtualFBReportsQuota tests that the framebuffer sizes reported to the
// application reflect its quota rather than the whole device.
func TestVirtualFBReportsQuota(t *testing.T) {
	const gib = 1 << 30
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 2 * gib

	p := fbParams(12*gib, 11*gib)
	fbInfoApplyQuota(&nvp.memAcct, p)
	total, free := fbResult(p)
	if total != 2*gib {
		t.Errorf("total = %d, want %d", total, 2*gib)
	}
	if free != 2*gib {
		t.Errorf("free = %d, want %d", free, 2*gib)
	}
}

// TestVirtualFBFreeShrinksWithUse tests that reported free memory decreases as
// the sandbox allocates, so that an application polling it sees its own usage.
func TestVirtualFBFreeShrinksWithUse(t *testing.T) {
	const gib = 1 << 30
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 2 * gib
	client := newTestClient(nvp, 1)

	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, gib/2, handle(nvgpu.NV01_NULL_OBJECT))

	p := fbParams(12*gib, 11*gib)
	fbInfoApplyQuota(&nvp.memAcct, p)
	if _, free := fbResult(p); free != 2*gib-gib/2 {
		t.Errorf("free = %d, want %d", free, 2*gib-gib/2)
	}
}

// TestVirtualFBFreeBoundedByDevice tests that reported free memory never
// exceeds what the device actually has free, since memory consumed by other
// sandboxes is not available to this one.
func TestVirtualFBFreeBoundedByDevice(t *testing.T) {
	const gib = 1 << 30
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 8 * gib

	// The device has only 1 GiB free, well below this sandbox's headroom.
	p := fbParams(12*gib, gib)
	fbInfoApplyQuota(&nvp.memAcct, p)
	if _, free := fbResult(p); free != gib {
		t.Errorf("free = %d, want %d", free, gib)
	}
}

// TestVirtualFBUnlimitedPassesThrough tests that a sandbox with no quota sees
// the device's real sizes.
func TestVirtualFBUnlimitedPassesThrough(t *testing.T) {
	const gib = 1 << 30
	nvp := &nvproxy{}

	p := fbParams(12*gib, 11*gib)
	fbInfoApplyQuota(&nvp.memAcct, p)
	total, free := fbResult(p)
	if total != 12*gib || free != 11*gib {
		t.Errorf("total, free = %d, %d; want %d, %d", total, free, 12*gib, 11*gib)
	}
}

// TestVirtualFBOverQuotaReportsNoFree tests that a sandbox at or beyond its
// limit is reported no free memory rather than an underflowed size.
func TestVirtualFBOverQuotaReportsNoFree(t *testing.T) {
	const gib = 1 << 30
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = gib
	client := newTestClient(nvp, 1)

	addMem(t, nvp, client, handle(10), nvgpu.NV01_MEMORY_LOCAL_USER, gib, handle(nvgpu.NV01_NULL_OBJECT))

	p := fbParams(12*gib, 11*gib)
	fbInfoApplyQuota(&nvp.memAcct, p)
	if _, free := fbResult(p); free != 0 {
		t.Errorf("free = %d, want 0", free)
	}
}

// TestVirtualFBIgnoresOversizedList tests that a list size larger than the
// array is clamped rather than indexed out of bounds.
func TestVirtualFBIgnoresOversizedList(t *testing.T) {
	const gib = 1 << 30
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 2 * gib

	p := fbParams(12*gib, 11*gib)
	p.FBInfoListSize = ^uint32(0)
	fbInfoApplyQuota(&nvp.memAcct, p)
	if total, _ := fbResult(p); total != 2*gib {
		t.Errorf("total = %d, want %d", total, 2*gib)
	}
}

// TestVirtualFBPartialList tests that a request for only one of the two sizes
// is handled, since applications may ask for either alone.
func TestVirtualFBPartialList(t *testing.T) {
	const gib = 1 << 30
	nvp := &nvproxy{}
	nvp.memAcct.gpuLimit = 2 * gib

	p := &nvgpu.NV2080_CTRL_FB_GET_INFO_V2_PARAMS{FBInfoListSize: 1}
	p.FBInfoList[0].Index = nvgpu.NV2080_CTRL_FB_INFO_INDEX_HEAP_FREE
	p.FBInfoList[0].Data = uint32(11 * gib / fbInfoUnitBytes)
	fbInfoApplyQuota(&nvp.memAcct, p)
	if got := uint64(p.FBInfoList[0].Data) * fbInfoUnitBytes; got != 2*gib {
		t.Errorf("free = %d, want %d", got, 2*gib)
	}
}

// TestAllocationClassesReviewed tests that every allocation class nvproxy
// supports has a recorded accounting decision.
//
// This is a drift guard rather than a test of behavior. Support for a new
// driver version is added by extending the tables in version.go, and a version
// that introduces an allocation class committing GPU memory would otherwise
// leave it unaccounted, silently weakening every quota, with nothing failing to
// say so. Requiring the class to be listed in memClassKinds forces whoever adds
// it to decide whether it commits memory.
func TestAllocationClassesReviewed(t *testing.T) {
	Init()
	// Report each unreviewed class once rather than once per driver version
	// that registers it.
	missing := make(map[nvgpu.ClassID][]string)
	for version, abiEntry := range abis {
		abi := abiEntry.cons()
		for class := range abi.allocationClass {
			if _, ok := memClassKinds[class]; !ok {
				missing[class] = append(missing[class], version.String())
			}
		}
	}
	for class, versions := range missing {
		sort.Strings(versions)
		t.Errorf("allocation class %v has no accounting decision; add it to memClassKinds in memquota.go, "+
			"charging it if it commits GPU memory (registered by driver version(s) %v)", class, versions)
	}
}

// TestMemClassKindsCoversAccountedClasses tests that the classes the
// allocation paths charge are all recorded as accounted. These are reached
// through NV_ESC_RM_ALLOC_MEMORY and NV_ESC_RM_VID_HEAP_CONTROL rather than
// the allocation class table, so TestAllocationClassesReviewed does not see
// them.
func TestMemClassKindsCoversAccountedClasses(t *testing.T) {
	for _, test := range []struct {
		class nvgpu.ClassID
		want  memKind
	}{
		{nvgpu.NV01_MEMORY_LOCAL_USER, memKindVRAM},
		{nvgpu.NV01_MEMORY_LOCAL_PRIVILEGED, memKindVRAM},
		{nvgpu.NV01_MEMORY_SYSTEM, memKindPinnedHost},
		{nvgpu.NV01_MEMORY_SYSTEM_OS_DESCRIPTOR, memKindPinnedHost},
		{nvgpu.NV_MEMORY_EXTENDED_USER, memKindNone},
	} {
		if got := memKindOfClass(test.class); got != test.want {
			t.Errorf("memKindOfClass(%v) = %v, want %v", test.class, got, test.want)
		}
	}
}
