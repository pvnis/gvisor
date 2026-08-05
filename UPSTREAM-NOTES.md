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

### The KVM platform cannot map device memory past its first page

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

What remains is that KVM cannot back guest memory with a `VM_PFNMAP` device
VMA through an ordinary `KVM_SET_USER_MEMORY_REGION` slot:
`hva_to_pfn_remapped` resolves such a mapping only where a host PTE is
already present. Fixing this means teaching the KVM platform to recognise
device memory and handle it outside the normal memory-slot path, which is
platform work, not an amdproxy change.

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
