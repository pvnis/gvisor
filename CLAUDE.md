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
| docker | yes | **yes** |
| kubernetes | no | **yes, with the AMD device plugin** |
| `sudo` | passwordless | **passwordless (`(ALL) NOPASSWD: ALL`)** |

Nothing should hardcode a `gpu_id` or a KFD major: the major is dynamically
allocated and genuinely differs. `~/amdtest/gputest.sh` reads both from the host.

**Building on sensnucbox2:** `sudo -n` is fully passwordless. The build system
invokes docker. `dmd` is in the `docker` group (added 2026-08-07), so bare
`docker` calls from `images.mk` work without the wrapper. Use:
```
sudo -u dmd -g docker make build TARGETS=//runsc:runsc DOCKER_CLI_PATH=/home/dmd/amdtest/sudodocker
```
The `sudodocker` wrapper (`exec sudo /usr/bin/docker "$@"`) covers the places
in `bazel.mk` that use `$(DOCKER_CLI_PATH)`. The `-g docker` covers the bare
`docker` calls in `images.mk`. If a new shell already has the docker group
active (`id` shows `docker`), plain `make build` works too.
After building, copy `bazel-bin/runsc/runsc_/runsc` to both `~/amdtest/runsc`
and `/usr/local/bin/runsc` — Docker and Kubernetes both run the latter.

**Check the hash, not bazel's summary.** Bazel reports "0 processes, 0 total
actions" for a build whose output did change, and its `bazel-bin` symlink can
point at a stale tree; `bazel clean` did *not* force a rebuild here. Confirm
with `sha256sum` against the deployed binary, and confirm a specific change
landed with `strings runsc | grep <a string you just added>`.

`~/amdtest/` holds every reproducer, a `Makefile`, `gputest.sh`, and baselines
measured on sens1. Read `~/amdtest/README.md` first. `~/amdtest/k8s/` holds the
Kubernetes pod specs and the shim config — read its README before touching
anything Kubernetes-side.

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
  snapshot and the synthetic KFD topology / PCI / DRM tree. The topology
  reports the *sandbox's quota* as the VRAM size, not the device's: a pod
  holding 2 GiB reads 2147483648 where the host reads 14859169792.
- runsc plumbing, including annotation overrides with narrow-only / lower-only
  validators, so a container cannot widen its own mask or raise its own limit.
- **`vecadd` runs end to end** — a real HIP kernel, correct results, on all
  three paths: `hiptest.sh`'s OCI bundle, Docker, and Kubernetes. Under
  Kubernetes the AMD device plugin supplies the per-pod quota and the Sentry
  enforces it: a pod asking for 4 × 512 MiB with `amd.com/cu-mask: "0x3f"`
  gets exactly 2 GiB and 6 CUs, narrowed from the node's 0xfff ceiling.
- **Two containers sharing the GPU simultaneously, verified end to end.**
  `vecadd-a` (cu-mask `0x03f`, CUs 0–5, 2 GiB) and `vecadd-b` (cu-mask
  `0xfc0`, CUs 6–11, 2 GiB) ran at the same time under both Docker
  (`hiptest.sh -f`) and Kubernetes (`~/amdtest/k8s/vecadd-{a,b}.yaml`); both
  produced `RESULT CORRECT`. Concurrent `memprobe` confirmed each sandbox sees
  exactly its own 2048 MiB ceiling regardless of what the other allocates.
  The boot log shows each Sentry received its own disjoint flags from the
  device plugin's per-pod annotations.

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

1. **SVM (`AMDKFD_IOC_SVM`) is forwarded but architecturally limited.** The
   handler is implemented (`ae2b9b4ab`): it copies the full `24 + nattr*8`
   contiguous buffer from the guest, passes it to the host, and copies back.
   The ioctl reaches the driver; the driver returns EFAULT when it tries to
   validate the VA range. Root cause: SVM operates on the *calling process's*
   VA space, but under KVM the Sentry is the calling process, and guest VAs are
   not in the Sentry's address space. Same root as the KFD-mmap process-binding
   limitation. hipMallocManaged will not work; hipMalloc-based workloads
   (vecadd) are unaffected.
2. **`/dev/kfd` mappings are impossible on systrap** (task #18). Not a bug: KFD
   binds each mapping to the process holding the KFD context, and systrap maps
   from a stub process, so `mmap` returns `EINVAL`. Only the KFD mapping is
   process-bound; the render node's is not. KVM is now usable.
3. Send the two upstream fixes and file the KVM issue.
4. Registration keys off the **root** container's spec (`registerFilesystems`
   takes `&l.root`), so under Kubernetes — where the root is the pause
   container and carries no devices — spec-based detection never fires and a
   pod depends on `--amdproxy` being set for the whole sandbox. nvproxy has the
   same shape, so this is upstream's design rather than a local bug, but it
   means the flag is mandatory in Kubernetes and per-pod opt-in via the spec
   alone does not work. Worth revisiting if pods without GPUs should not pay
   for the looser seccomp filter.

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

**The Kubernetes config file is not runsc's config file.** `/etc/runsc/config.toml`
— what containerd's `ConfigPath` points at — is decoded into
`pkg/shim/v1/runsc.Options`. runsc flags belong under a `[runsc_config]` table;
unknown top-level keys are dropped without a word. It had been written in
runsc's own flat format, where only `root` happened to match a field, so every
pod silently ran with no `--amdproxy`, no `--platform=kvm`, and no debug log.

That failure mode is worth recognising by shape: `open("/dev/kfd")` returned
**ENXIO**, and amdproxy logged *nothing*, because the workload's spec still
lists `/dev/kfd` so gVisor still creates the node — with the host's major
number and nothing registered behind it. The `open` dies in VFS dispatch,
before any driver code runs. **Absence of a log line from the subsystem you
suspect is evidence the subsystem was never reached, not evidence it is quiet.**
`createDeviceFile` now warns in exactly this case. `~/amdtest/k8s/` holds the
corrected config, the pod specs, and the diagnostic steps.

Two smaller Kubernetes traps, both of which cost real time here:
- `/var/log/pods/<pod>/gvisor.log` is the **user** log — compat events only.
  Sentry warnings go to `/var/log/runsc/`, and if that directory has no recent
  `*.boot.txt`, debug logging never reached runsc at all.
- containerd's `k8s.io` image store is **separate from docker's**. A stale
  `vecadd` there, invisible to `docker images`, was missing
  `libamd_comgr.so.3` and reported "no ROCm-capable device is detected" —
  which reads like a proxy failure but arrives *after* the sandbox has already
  enumerated the GPU. `AMD_LOG_LEVEL=4` plus `LD_DEBUG=libs` in the pod env
  named it in one run. Compare both stores before believing any K8s-only bug.

**Conventions.** Hand-written Go needs gofmt's alignment; use bazel's gofmt
binary rather than eyeballing struct tags. New ABI structs need `+marshal` and
the matching BUILD deps. On sens1 git had no configured identity, so commits
needed `-c user.name=... -c user.email=...`.
