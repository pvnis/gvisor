# amdgpuslicing — handoff

GPU slicing for AMD GPUs in gVisor: an `amdproxy` alongside `nvproxy` and
`tpuproxy`, with both memory and compute quotas. Compute uses CU (compute unit)
masks, which are enforced by the GPU's command processor and so are a real
hardware partition rather than a time slice.

**The governing constraint:** the same adversarial model as the Nvidia GPU
slicing work — *nothing may depend on changes inside the container*. Every limit
is enforced in the Sentry, where ioctls are interpreted, and a hostile container
cannot lift it. HAMi's AMD vGPU design was a reference for what to enforce, not
for how; it relies on an in-container shim, which is not acceptable here.

Work happens directly in the working directory, not a worktree.

## Where the work runs

It moved from **sens1** to **sensnucbox2** on 2026-08-06, because the KVM bug
below needs a kernel >= 6.13 and sens1 cannot be upgraded.

| | sens1 | sensnucbox2 |
| --- | --- | --- |
| OS / kernel | Ubuntu 24.04.4, 6.8.0-134-lowlatency | Ubuntu 26.04, 7.0.0-generic |
| GPU | discrete, 54 CUs, 12032 MiB | **Phoenix1 APU**, 12 CUs, ~14170 MiB |
| `gpu_id` | 13623 | 40786 |
| `/dev/kfd` major | 235 | **511** |
| docker | yes | **no** |
| `sudo` | passwordless | **needs a password** |

Nothing should hardcode a `gpu_id` or a KFD major: the major is dynamically
allocated and genuinely differs. `~/amdtest/gputest.sh` reads both from the host.

**Building on sensnucbox2:** `make build TARGETS=//runsc:runsc` runs bazel inside
a docker container, and there is no docker there. Until that is set up, use the
prebuilt static `~/amdtest/runsc` (from sens1 at `58f9fa8d4`). Getting a real
build working is a prerequisite for any code change.

`~/amdtest/` holds every reproducer, a `Makefile`, `gputest.sh`, and baselines
measured on sens1. Read `~/amdtest/README.md` first.

## Done, and verified against hardware

- `pkg/abi/amdgpu/{kfd,drm}.go` — KFD and DRM ABI. Every struct size and ioctl
  number was ground-truthed by compiling C against the real
  `linux/kfd_ioctl.h`, not read off documentation.
- `pkg/sentry/devices/amdproxy/` — the proxy. `/dev/kfd` plus amdgpu render
  nodes; a per-ioctl allowlist, with everything unhandled denied rather than
  passed through.
- **CU-mask enforcement.** `--amdproxy-cu-mask=0x3f`: a request for all CUs is
  silently narrowed to the mask, and a disjoint request gets `EINVAL`.
- **GPU memory quota.** `--amdproxy-gpu-memory-limit`: admit-before-forward
  accounting on `ALLOC_MEMORY_OF_GPU`. 2 GiB reports 2048 MiB, cuts off at
  exactly 2048 MiB, 0 after, 1024 MiB after freeing half.
- `pkg/sentry/devices/amdproxy/amdconf` — leaf package for the `CUMask` type, so
  `runsc/config` does not import sentry code. Mirrors `nvconf`.
- `pkg/amdsysfs` + `pkg/sentry/fsimpl/sys/amdgpu.go` — bounded host sysfs
  snapshot and the synthetic KFD topology / PCI / DRM tree.
- runsc plumbing, including annotation overrides with narrow-only / lower-only
  validators, so a container cannot widen its own mask or raise its own limit.

## Two fixes that belong upstream, not here

Both are gVisor bugs with nothing AMD-specific about them; they affect any
proxied device. See `UPSTREAM-NOTES.md`. **Not yet sent.**

- `797e29b80` — `GenericConfigureMMap` rejected any mmap with offset+length >
  `MaxInt64`. Correct for a file, wrong for a device, where the offset is an
  opaque token with type bits in the high bits. Every KFD doorbell mapping was
  refused with `EOVERFLOW`.
- `6bfb3c267` — a container could panic the Sentry by mmaping a proxied device
  whose host mmap fails: the wrapped error reached `ExtractErrno`, matched no
  case, and hit the panic. A denial of service from inside the sandbox.

## Next

1. **Confirm the KVM fix on sensnucbox2's kernel** (task #24). This is the first
   thing to do, and it is one command:
   `cd ~/amdtest && ./gputest.sh pagewalk3 16384 fwd`.
   On sens1 this prints `OK pages: 0 1024 2048 2560 3072 3584`. On >= 6.13 it
   should print all 4096. If it does, the KVM platform is usable for AMD and the
   whole picture changes — a HIP program becomes the next thing to try
   (`make vecadd`, then run it under runsc).
2. **`/dev/kfd` mappings are impossible on systrap** (task #18). Not a bug: KFD
   binds each mapping to the process holding the KFD context, and systrap maps
   from a stub process, so `mmap` returns `EINVAL`. Only the KFD mapping is
   process-bound; the render node's is not. If KVM works on the new kernel this
   stops being the blocker it was, but systrap remains unusable for AMD.
3. **sysfs topology leaks the real VRAM size** (task #22). A container under a
   quota still reads the device's full memory from the synthetic sysfs. An
   information leak and an inconsistency — a runtime sizing its pools from sysfs
   will over-commit. Rewrite the sizes to the sandbox's quota.
4. Send the two fixes above upstream, and file the KVM issue.

## Context worth having

**The KVM device-memory bug is not a gVisor bug.** Under `--platform=kvm` on a
kernel < 6.13, only the head page of each compound allocation behind an amdgpu
mapping is usable; every tail page gets `SIGBUS`. `hva_to_pfn_remapped()` calls
`kvm_try_get_pfn()` => `get_page_unless_zero()`, a tail page's refcount lives on
its head, so it returns `-EFAULT`, and gVisor turns the unservicable fault into
`SIGBUS`. TTM allocates GTT in high orders, so 511 of every 512 pages are tail
pages. Fixed upstream for 6.13 by "KVM: Stop grabbing references to PFNMAP'd
pages" — a series whose own motivation cites exposing GPU TTM buffers to KVM
guests. `UPSTREAM-NOTES.md` has the full trace, the ruled-out hypotheses, and
the affected-kernel table. **6.12 is the LTS and is one release short.**

**nvproxy escapes that by luck, not design.** Identical gVisor code path; the
difference is that Nvidia's driver maps BAR pfns (no `struct page`) and order-0
system pages, both of which KVM accepts.

**Measure, do not infer.** This bug got attributed wrongly twice — first to a
KVM limitation, then over-corrected to purely gVisor's fault — both times from
plausible reasoning about code that was never run. What settled it was a
tracepoint and a probe that printed *which* pages worked. If a claim about this
codebase matters, there is almost always a reproducer that can decide it, and
`~/amdtest/` is where they live.

**Conventions.** Hand-written Go needs gofmt's alignment; use bazel's gofmt
binary rather than eyeballing struct tags. New ABI structs need `+marshal` and
the matching BUILD deps. On sens1 git had no configured identity, so commits
needed `-c user.name=... -c user.email=...`.
