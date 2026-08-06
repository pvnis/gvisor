# Upstream notes for the amdgpuslicing branch

Two commits on this branch fix gVisor bugs that have nothing to do with AMD
GPUs specifically. They affect any proxied device, so they should be sent
upstream separately from the amdproxy work rather than carried here
indefinitely.

A third problem is diagnosed but **not** fixed, and is the reason no ROCm or
HIP application runs under gVisor yet. It is described below with a minimal
reproducer, ready to file.

## Send upstream

### `797e29b80` vfs: Don't bound a proxied device's mmap offset like a file's

`GenericConfigureMMap` refuses any `mmap` whose offset plus length exceeds
`math.MaxInt64`. That is correct for a file, whose offset is a position in a
byte stream, but a device driver's `mmap` offset is an opaque token, and
drivers encode what is being mapped in its high bits. AMD's KFD names a
doorbell page with the top two bits set, so every doorbell mapping was
refused with `EOVERFLOW` before the driver ever saw it.

Affects any device whose driver encodes a type in the offset, not just KFD.

### `6bfb3c267` kernel: Report a host mmap failure instead of panicking on it

A sandboxed application could panic the Sentry by `mmap`ing a proxied device
file whose host `mmap` fails. `GenericProxyDeviceConfigureMMap` exists so
that such a failure reaches the application, but the error arrives at
`ExtractErrno` wrapped for context, matches no case, and reaches the panic at
the end of that function.

This is reachable today by any AMD GPU workload on the systrap platform, and
is a denial of service on the sandbox from inside it, so it is worth
upstreaming on its own.

## File as a bug, not fixed here

### KVM cannot back guest memory with tail pages of an amdgpu buffer

Under `--platform=kvm`, only one page in every compound allocation behind a
proxied amdgpu mapping is usable. Every other page faults with `SIGBUS`.

`~/amdtest-pagewalk.c` maps a GPU buffer and touches it page by page, catching
`SIGBUS` to report the first failure:

```
native:   mapped 2048 KiB -> all 512 pages OK
systrap:  mapped 2048 KiB -> all 512 pages OK
kvm:      mapped 2048 KiB -> FIRST FAULT at offset 4 KiB (page 1)
```

#### Root cause

The host's KVM rejects every page of the mapping except the head page of each
compound allocation backing it.

`~/amdtest-pagewalk3.c` reports *which* pages are usable rather than just the
first failure, and the pattern is exact:

```
kvm,  2 MiB buffer: OK pages: 0
kvm,  4 MiB buffer: OK pages: 0
kvm,  8 MiB buffer: OK pages: 0 1024
kvm, 16 MiB buffer: OK pages: 0 1024 2048 2560 3072 3584
```

Those gaps are 1024 pages (4 MiB) and then 512 pages (2 MiB): exactly one
usable page per TTM allocation, at its head. TTM allocates GTT memory from its
pool in high orders, so all but one page of each allocation is a *tail* page.

A host `kvm:kvm_page_fault` / `kvmmmu:*` trace over a failing run shows KVM
resolving the head page and silently refusing every tail page — the fault
arrives, and no SPTE is ever requested or set:

```
kvm_page_fault:       vcpu 1 address 0x32319fe00000 error_code 0xf82
kvm_mmu_spte_requested: gfn 32319fe00 pfn 1c6e00 level 1
kvm_mmu_set_spte:       gfn 32319fe00 spte 6000001c6e00b77 (rwxu) level 1
kvm_page_fault:       vcpu 1 address 0x32319fe01000 error_code 0xf82
kvm_page_fault:       vcpu 1 address 0x32319fe02000 error_code 0xf82
...
```

The mechanism is in `virt/kvm/kvm_main.c:hva_to_pfn_remapped()`, which is the
path taken for a `VM_PFNMAP`/`VM_IO` VMA. Having resolved a pfn it calls
`kvm_try_get_pfn()` => `get_page_unless_zero()`, guarding against exactly this
case; the comment above it names it:

> Certain IO or PFNMAP mappings can be backed with valid struct pages, but be
> allocated without refcounting e.g., tail pages of non-compound higher order
> allocations, which would then underflow the refcount when the caller does the
> required put_page. Don't allow those pages here.

A tail page's refcount lives on its head, so `get_page_unless_zero()` fails and
`hva_to_pfn_remapped()` returns `-EFAULT`. KVM cannot service the fault, gVisor
takes the `ring0.NMI` case in `machine_amd64.go:exceptionVector()` — "a fault is
not servicable by KVM itself" — and turns it into `SIGBUS`.

That also explains why the failure looked like "only the first page works": for
a single 2 or 4 MiB buffer the head page *is* page 0. It is page 0 specifically
and not "the first page touched", which walking the mapping backwards confirms:

```
kvm, forward walk: ok=1 firstOK=0   (touches page 0 first)
kvm, reverse walk: ok=1 firstOK=0   (touches page 511 first, still only 0 works)
```

#### What was ruled out along the way

- gVisor's own mapping is correct. `MapFile` is asked to map the whole range as
  one block and does, with the right memory type:

  ```
  DBG MapFile gva=7f06d6a00000 fr=[0x100043000, 0x100243000) mt=WriteBack precommit=false
  ```

- The Sentry's host page tables are fully populated before the guest page tables
  are installed. Reading every page from Sentry context with
  `safecopy.LoadUint32`, in the same run and immediately before `mapLocked`,
  succeeds for all of them:

  ```
  DBG touch fr=[0x100043000, 0x100243000) hva=322f9fe00000 len=200000 okPages=512 badPages=0
  ```

  So this is not a missing or lazily faulted host PTE. `follow_pte()` finds the
  pfn; KVM refuses to *take a reference* on it.

- The guest-physical translation and the memory slot are both right:

  ```
  mapLocked   gva=7f1e94400000 hva=340557200000 gpa=340757200000 len=200000
  mapPhysical gpa=340757200000 -> vstart=340400001000 pstart=340600001000
              len=1977ff000 mmio=false hasSlot=true
              prVirt=1000 prPhys=200001000 prLen=3405977ff000
  ```

  `prPhys + (hva - prVirt)` = `340757200000`, and `vstart + (gpa - pstart)` =
  `340557200000`, back to the Sentry mapping. The slot is not `mmio` and covers
  the range.

- Super pages, the mapping's size and 2 MiB alignment, `MAP_FIXED` over a
  reserved range, and `precommit` were each tested and are not the cause.

- An ordinary `MAP_SHARED` file mapping of the same size, in the same sandbox on
  the same platform, works for all 512 pages. Ordinary mappings do not go
  through `hva_to_pfn_remapped()` at all.

Note that the buffer in the reproducer is `KFD_IOC_ALLOC_MEM_FLAGS_GTT`, i.e.
host system memory, not VRAM. What matters is not that the memory is on the GPU
but that amdgpu hands back a `VM_PFNMAP` VMA over a high-order allocation.
`/proc/self/smaps` for the mapping, natively:

```
7f..-7f.. rw-s 100043000 00:05 775   /dev/dri/renderD128
Rss: 0 kB
VmFlags: rd wr sh mr mw me ms pf io de dd sd
```

`pf` is `VM_PFNMAP` and `io` is `VM_IO`.

#### Why nvproxy does not hit this

The gVisor code path is the same for both proxies: `ConfigureMMap` =>
`GenericProxyDeviceConfigureMMap`, a `memmap.File` over the host device FD, and
`addressSpace.MapFile`. The difference is entirely in what the two drivers put
behind the VMA.

`kvm_try_get_pfn()` accepts a pfn with no `struct page` at all
(`kvm_is_reserved_pfn()` short-circuits it) and accepts any refcounted head
page. It rejects only tail pages of higher-order allocations. So:

| driver mapping | backing | KVM |
| --- | --- | --- |
| Nvidia BAR / MMIO | pfns with no `struct page` | accepted |
| Nvidia system memory | individual order-0 pages | accepted |
| amdgpu VRAM | BAR pfns, no `struct page` | accepted |
| amdgpu GTT | TTM pool, order-9/order-10 | head page only |

nvproxy escapes this by the allocation strategy of the driver it proxies, not by
anything gVisor does differently. Any driver that maps high-order allocations
through a `VM_PFNMAP` VMA would hit it.

The existing note that `cudaMallocManaged()` is "flaky on -platform=kvm" is a
*different* KVM limitation, described in
`g3doc/proposals/nvidia_driver_proxy.md`: UVM requires mapping virtual addresses
to equal file offsets, which can make `MapInternal` unimplementable.

#### Possible fixes

In rough order of preference:

1. Host kernel. This is a real KVM limitation, and upstream KVM has since
   reworked `hva_to_pfn` to handle non-refcounted pfns. Worth retesting on a
   newer host kernel than the 6.8 this was measured on, and worth reporting so
   the requirement is documented either way.
2. gVisor. The KVM platform could avoid the ordinary memslot path for
   `VM_PFNMAP` device memory. This is the fix that belongs in gVisor, but it is
   not a small change.
3. amdproxy. Nothing good — the allocation order is TTM's decision, made inside
   the ioctl the sandbox issues, and forcing order-0 would be both a
   host-configuration change and a performance regression.

Until one of those lands, systrap is the platform to target for AMD, which
leaves the `/dev/kfd` problem below as the blocker.

### KFD mappings are impossible on the systrap platform

Not a bug, but the other half of why nothing runs. KFD binds each mapping to
the process that owns the KFD context. systrap performs guest mappings from a
stub process, so `mmap` of `/dev/kfd` returns `EINVAL` there.
`~/amdtest-mmaptest.c` demonstrates it directly:

| caller | `/dev/kfd` MMIO mmap | render node GEM mmap |
| --- | --- | --- |
| parent, which opened the fd | ok | ok |
| forked child, separate mm | **EINVAL** | ok |
| `CLONE_VM` child, shared mm | ok | ok |

Only the KFD mapping is process-bound; the render node's is not.

## Consequence

Neither platform supports both mapping types, for different reasons, so no
ROCm or HIP application runs yet:

| | `/dev/kfd` doorbell and MMIO | GPU memory via render node |
| --- | --- | --- |
| systrap | impossible, process-bound | works, all pages |
| kvm | works | one page per TTM allocation |

A minimal HIP program (`hipMalloc`, `hipMemcpy`, one kernel launch) that
passes on the host dies in its first HIP call under both: `SIGSEGV` on
systrap, `SIGBUS` on kvm.

The compute-unit and GPU-memory limits are unaffected by any of this. They
are enforced where ioctls are interpreted, and are verified against hardware.
