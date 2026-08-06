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

### gVisor's KVM platform cannot map device memory past its first page

Under `--platform=kvm`, only the first page of a proxied device mapping is
usable. Every later page faults with `SIGBUS`.

`~/amdtest-pagewalk.c` maps a 2 MiB AMD GPU buffer and touches it page by
page, catching `SIGBUS` to report the first failure:

```
native:   mapped 2048 KiB -> all 512 pages OK
systrap:  mapped 2048 KiB -> all 512 pages OK
kvm:      mapped 2048 KiB -> FIRST FAULT at offset 4 KiB (page 1)
```

gVisor's own side is correct. Instrumenting `MapFile` shows it is asked to
map the whole range as one block, and it does:

```
MapFile addr=7f88bb200000 fr=[100043000,100243000) len=200000 mt=WriteBack numBlocks=1
```

The following were each tested and ruled out, so none of them is the cause:

- gVisor installing a huge PTE over device memory (disabling super pages in
  `mapLocked` does not help)
- the mapping's size, or its 2 MiB alignment (1 MiB through 4 MiB all behave
  the same: page 0 works, page 1 does not)
- `MAP_FIXED` over a previously reserved range, which is what ROCr does
- pages not being faulted into the Sentry first (forcing `precommit` does not
  help, including with a touch loop the compiler cannot elide; note that the
  existing loop's `_ = s[i]` is a discarded read)
- the host being unable to reach the memory at all. Reading every page of the
  mapping from the Sentry with `safecopy.LoadUint32` succeeds for all of them:

  ```
  sentry-read fr=[100043000,100243000) okPages=512 badPages=0 firstErr=<nil>
  ```

That last measurement is the important one, and it settles where the bug is.
The Sentry is an ordinary host process; if it can read all 512 pages, then the
amdgpu driver, the device VMA and the host page tables are all fine, and the
host PTEs are present by the time the guest runs. The failure is therefore
entirely in how gVisor's KVM platform makes that memory visible to the guest,
not in the host's ability to provide it.

**This is a gVisor bug, in `pkg/sentry/platform/kvm`.** An earlier revision of
this note attributed it to Linux KVM being unable to back guest memory with a
`VM_PFNMAP` VMA. That was an inference, and the measurement above contradicts
it: the pages are readable and mapped on the host, so `hva_to_pfn_remapped`
has a present PTE to follow.

Both of the candidates that were open have since been checked, and neither is
at fault. Tracing the whole translation for a failing 2 MiB mapping gives:

```
MapFile     gva=7f1e94400000 fr=[100043000,100243000) blocks=1
  block     hva=340557200000 len=200000
mapLocked   gva=7f1e94400000 hva=340557200000 gpa=340757200000 len=200000
mapPhysical gpa=340757200000 -> vstart=340400001000 pstart=340600001000
            len=1977ff000 mmio=false hasSlot=true
            prVirt=1000 prPhys=200001000 prLen=3405977ff000
```

Every step of that is right:

- the guest-physical address follows the region's linear map,
  `prPhys + (hva - prVirt)` = `200001000 + 3405571ff000` = `340757200000`
- the enclosing slot maps that address back to exactly the Sentry mapping,
  `vstart + (gpa - pstart)` = `340400001000 + 1571ff000` = `340557200000`
- the slot is not `mmio`, so it is a real memory slot, and it covers the range
- the guest page tables are asked for the whole `200000` bytes at once

So the memory slot and the guest page tables are both correct, and the Sentry
can read every page of the mapping they point at.

What remains is that this is specific to *device* memory. The same page walk
over an ordinary `MAP_SHARED` file mapping of the same size, in the same
sandbox on the same platform, succeeds for all 512 pages:

```
kvm, MAP_SHARED file:   mapped 2048 KiB -> all 512 pages OK
kvm, GPU device memory: mapped 2048 KiB -> FIRST FAULT at page 1
```

The only difference left between those two is that the device's VMA is
`VM_PFNMAP`/`VM_IO`. gVisor maps it into the guest through the ordinary
memory-slot path, which is correct for normal memory and is where this breaks
for device memory. The fix therefore belongs in gVisor, which has to
recognise device memory and map it differently; the constraint it is running
into is how KVM resolves `VM_PFNMAP` ranges behind a memslot.

The next step is to find what KVM does with the guest fault for such a range:
whether it exits to userspace as MMIO, or fails `hva_to_pfn_remapped`, which
`kvm_stat` or a trace of `kvm_page_fault` on the host would show.

gVisor already warns that `cudaMallocManaged()` is "flaky on -platform=kvm";
this reduces that to a deterministic one-line reproducer.

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
| kvm | works | first page only |

A minimal HIP program (`hipMalloc`, `hipMemcpy`, one kernel launch) that
passes on the host dies in its first HIP call under both: `SIGSEGV` on
systrap, `SIGBUS` on kvm.

The compute-unit and GPU-memory limits are unaffected by any of this. They
are enforced where ioctls are interpreted, and are verified against hardware.
