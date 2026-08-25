# GPU isolation under gVisor

How this branch divides a single physical GPU among mutually-untrusting
containers — for both NVIDIA (`nvproxy`) and AMD (`amdproxy`) — with every limit
enforced **outside** the container. This is the top-level design overview; it
explains the architecture and points to the deep-dive documents for each piece.

---

## The governing constraint

> **Nothing may depend on changes inside the container.** Every limit is enforced
> in the gVisor **Sentry**, where the container's `ioctl`s are interpreted, and a
> hostile container cannot lift it.

Existing GPU-sharing solutions (e.g. HAMi's vGPU) enforce with an *in-container*
`LD_PRELOAD` library. That is unsafe against a hostile tenant: a limiter living
in the address space of the process it limits can be switched off from inside.
Measured — a pod capped at 512 MiB by the library allocated 768 MiB of the whole
device once `CUDA_DISABLE_CONTROL=true` was set. This branch keeps the reference
for *what* to enforce but never for *how*: enforcement moves to the Sentry.

---

## Four projects, four layers

GPU isolation spans four repositories. Each has its own docs; together they form
one stack.

| Layer | Project | Role |
| --- | --- | --- |
| **Cluster tenancy** | [`vcluster-multitenant`](../vcluster-multitenant) | Each tenant gets its own Kubernetes API server (vCluster) and network isolation (Cilium) on shared bare metal. Answers "who may run what." |
| **GPU-aware placement & admission** | [`HAMi-gvisor`](../HAMi-gvisor) (+ the in-tree webhook, [`webhook/`](webhook)) | A fork of HAMi: its scheduler and device plugin place each pod on a node/GPU and bin-pack by memory; the admission webhook restates the pod's GPU request as Sentry-enforced flags. Answers "which GPU, and what quota does it carry." |
| **Sandbox enforcement** | **`gvisor`** (this repo) | The Sentry interprets each container's GPU `ioctl`s and enforces its memory quota and compute share. Answers "how much of the GPU may this container take." |
| **Privileged driver broker** | [`open-gpu-kernel-modules`](../open-gpu-kernel-modules) | The open NVIDIA kernel driver, extended with hooks that let a trusted host component drive the hardware runlist and per-tenant UVM eviction — the things the Sentry cannot do from userspace. Answers "how is a compute/paging decision actually applied to the hardware." |

Only the last two layers *enforce* isolation against a hostile tenant; the first
two decide placement and translate requests, and are trusted infrastructure.

The two enforcement layers are **orthogonal in what they guarantee** (verified:
neither weakens the other) but interact in what they *deliver* — see
`SECURITY-FINDINGS.md` and `vcluster-multitenant/SECURITY-FINDINGS.md`.

**Why a driver broker is needed at all (NVIDIA only).** Once an NVIDIA channel is
set up, work is submitted by writing a pushbuffer in mapped memory and ringing a
doorbell through a mapped register — that path *never enters the kernel* (a run
of 13,000 kernel launches produced no `ioctl`s at all). So the Sentry cannot
meter the work itself; binding a doorbell-submission workload (cuBLAS) requires
manipulating the hardware runlist at kernel privilege. That privileged mechanism
lives in `open-gpu-kernel-modules`; the *policy* that drives it lives in the
trusted host component `runsc gpu-scheduler`, never in the Sentry. AMD needs no
such broker — its controls are `ioctl`s the Sentry already interprets.

---

## The two mechanisms are not the same shape

Both proxies enforce in the Sentry, but they partition **different resources**.

| | **AMD** (`amdproxy`) | **NVIDIA** (`nvproxy`) |
| --- | --- | --- |
| Compute divided in | **space** — CU masks **or** time — queue suspension, never both | **time** — submission windows |
| Enforced by | the GPU command processor | the Sentry (gate) / driver runlist (broker) |
| Unused share | idles (spatial) / reassigned (temporal) | reassigned (with the scheduler) |
| Memory quota | admit-before-forward on `ALLOC_MEMORY_OF_GPU` | admit-before-forward on the RM/UVM paths |

---

## NVIDIA (`nvproxy`)

**Memory quota** (`--nvproxy-gpu-memory-limit`). Admit-before-forward; device
memory and UVM address space counted together. `cuMemGetInfo()` is rewritten to
the limit, so frameworks size themselves correctly and cannot observe neighbours.

**Compute gate** (`--nvproxy-gpu-compute-percent`). Holds a sandbox to a fraction
of wall-clock submission time by revoking the submission mappings outside its
window. Binds ordinary kernel-launch workloads; **cannot** bind doorbell
submission (cuBLAS) — that is what the runlist broker is for.

**Scheduler** (`pkg/gpusched`, served by `runsc gpu-scheduler`). Clients carry a
**weight**; active clients divide each period in proportion. Two pods at weights
300/100 measured 486/162 launches/s = **3.00 : 1**, keeping 99% of solo
throughput; without the scheduler they get 324 each.

**Runlist enforcer** (`--runlist-control`, via the driver broker). Drives
per-TSG timeslice + detach/attach over `/proc/driver/nvidia/gpusched`; binds
cuBLAS (two pods 3:1 → 1067/344 = **3.10 : 1**, work-conserving).

**Credit scheduler** (`pkg/gpusched/credit.go`). Closes a process-*packing*
attack: a low-weight tenant that forks N processes multiplies its channel groups
(TSGs). Since every sandboxed process shares one Sentry host pid, a per-pid
timeslice can't see the multiplication; the credit scheduler charges **per
tenant**, weighting charge-back by the driver's per-tenant TSG count. Verified on
**GA100 (A100), GA102 (RTX A6000), and GB205 (RTX 5070)**: packing a weight-25
tenant from 3 to 12 TSGs bought it nothing and left the honest tenant unharmed.

Deep dive: **`NVIDIA-COMPUTE-ISOLATION.md`** (findings + per-GPU playbook),
**`SECURITY-FINDINGS.md`** (the packing attacks and other adversarial results).
The driver mechanisms the last two rely on are documented in
**`open-gpu-kernel-modules/DRIVER-CHANGES.md`**.

---

## AMD (`amdproxy`)

AMD can be divided by **space** or **time**, and on RDNA3 **not both** — the
operator chooses per device.

The exclusion is the driver's rule, not a policy choice.
`kfd_dbg_set_queue_workaround()` in `drivers/gpu/drm/amd/amdkfd/kfd_debug.c`
returns `-EBUSY` for a debug session's CWSR workaround on a queue carrying a
user CU mask, guarded by a check for GC versions 11.0.0 to 11.0.3 — every RDNA3
part. A time slice is enforced through exactly such a session, so the two
cannot both apply to one queue. runsc refuses a sandbox configured with both,
at startup. Read out of the source of the running kernel, not inferred.

The memory quota composes with either. Only spatial-plus-temporal is excluded.

**Spatial — CU masks** (`--amdproxy-cu-mask`, the default, fully Sentry-enforced).
A hard partition set at queue creation; tenants run concurrently on disjoint
compute units. Granularity is 2 CUs on RDNA. Isolation is the strongest result
on the branch: a tenant on a disjoint half moved −0.61% when a neighbour started.
Exactly fair up to 3 tenants (Jain 1.0000); beyond that the hardware itself
degrades — native `HSA_CU_MASK` processes behave identically.

**Temporal — queue suspend/resume** (`KFD_IOC_DBG_TRAP_SUSPEND_QUEUES`, no driver
patch needed). Tenants take turns on the whole device by weighted duty cycle
(75/25 → 3.04 : 1; 50/50 → Jain 1.0000). `vecadd` stays correct while sliced
because AMD preempts mid-kernel via CWSR. Work-conserving, and it partitions the
memory bus that CU masks cannot — but it costs occupancy on latency-hiding
compute — which makes it workload-shaped rather than a flat cost: the same
session costs a bandwidth-bound workload 0.003% and an ALU-bound one 43%.

**Enforced in the Sentry** since 2026-08-21 (`--amdproxy-gpu-weight` plus
`--amdproxy-gpu-scheduler-socket`, `pkg/sentry/devices/amdproxy/timeslice.go`),
with nothing preloaded into the container; the LD_PRELOAD interposer it was
ported from is no longer in the enforcement path. Measured there: 300:100 gives
3.05:1, 500:100 gives 5.13:1, two tenants at equal weights Jain 1.0000, and a
lone tenant reclaims the whole device. vLLM — multi-process, and so needing
`--amdproxy-share-kfd-vm` — divides 2.38:1 for a 3:1 request. It runs on stock
upstream HAMi, the weight coming from the admission webhook rather than from a
forked scheduler, because a weight needs no coordinated allocation.

**Memory quota** (`--amdproxy-gpu-memory-limit`). Admit-before-forward on
`ALLOC_MEMORY_OF_GPU`; the synthetic KFD topology reports the sandbox's quota as
the VRAM size, so `torch.cuda.mem_get_info()` sees only its own ceiling.

**Verified end to end**: `vecadd` and vLLM 0.16.0 (9.3% overhead) under gVisor;
two and three tenants concurrently on disjoint CU halves. See `CLAUDE.md` for the
full AMD record and reproducers (`~/amdtest/`).

---

## GPU-memory overcommit (feature branch `gpu-overcommit`)

Beyond a hard quota, tenants can **overcommit** GPU memory — commit more than the
physical card, paging the excess to host RAM — while a per-tenant resident cap
keeps one tenant from destabilising others. Policy/admission in the Sentry
(`--nvproxy-gpu-hmem-limit`), the paging/eviction mechanism in the driver broker.

- **The practical limit is the working set, not the allocation.** If the hot set
  fits VRAM, total commitment oversubscribes for free (20 GiB committed on a
  12 GiB card ran at full 275 GB/s); the moment the hot set exceeds VRAM,
  throughput collapses ~46× to PCIe paging speed. UVM/managed memory only —
  ordinary `cudaMalloc` stays hard-capped.
- **Per-tenant eviction** protects a well-behaved tenant's residency from a
  thrashing neighbour; a **thrash detector** flags a tenant pinned at its cap
  with a sustained eviction rate, as the basis for a configurable
  throttle/detach/kill policy (in progress).

Deep dive (on the `gpu-overcommit` branch): `OVERCOMMIT-SPIKE-FINDINGS.md`; the
driver side is in `open-gpu-kernel-modules/DRIVER-CHANGES.md`.

---

## Kubernetes integration (HAMi-gvisor)

Placement and admission run on **`HAMi-gvisor`**, a fork of HAMi (v2.9.0). Its
scheduler and device plugin place each pod on a node/GPU and bin-pack by memory.
The fork is a small delta — **4 commits, all under `pkg/device/amd/`**:

- **NVIDIA: HAMi is unmodified.** Stock upstream HAMi runs the whole NVIDIA path.
  HAMi's injected `libvgpu.so` preload — the in-container enforcement this project
  replaces — is left in place but neutralised with `CUDA_DISABLE_CONTROL`, since
  the real limit is enforced in the Sentry.
- **AMD: the fork adds automatic disjoint CU-mask assignment** (`cualloc.go` /
  `cumask.go`, `cusForRequest` proportional to the VRAM request) plus accurate
  VRAM accounting. This is the only part that *needs* the fork; without it, AMD
  pods still slice via a **manually supplied `amd.com/cu-mask` annotation** that
  the Sentry enforces — you just lose conflict-free automatic assignment.

Enforcement never depends on HAMi: the Sentry applies the memory and CU limits
from the pod's flags regardless of who set them. HAMi is trusted placement and
convenience translation, not part of the isolation guarantee.

Two admission webhooks run side by side:

- **HAMi's own webhook** (`vgpu.hami.io`) — device scheduling, unchanged.
- **The in-tree gVisor webhook** ([`webhook/pkg/gpushare`](webhook/pkg/gpushare)) —
  restates each pod's admitted request as `dev.gvisor.flag.*` Sentry flags *where
  the container cannot reach them* (`nvidia.com/gpumem` → memory limit,
  `nvidia.com/gpucores` → scheduler weight; two-vendor, narrow-only, so a pod can
  only clamp its own quota *down*). This replaces the retired standalone
  `gpu-quota-webhook`.

`failurePolicy: Fail` on the gVisor webhook is a **security property**, not an
availability preference: an unmutated pod would carry no quota and run at the
whole-device ceiling, so an unreachable webhook must fail closed. Full
deployment: **`MULTI-TENANT-SETUP.md`**; the two-vendor cluster:
**`vcluster-multitenant/TWO-VENDOR-CLUSTER.md`**.

---

## Document map

| Read this | For |
| --- | --- |
| `GPU-ISOLATION.md` (this file) | The architecture and where everything is |
| `CLAUDE.md` | The running status log and every measured result |
| `NVIDIA-COMPUTE-ISOLATION.md` | NVIDIA compute-isolation design, findings, per-GPU playbook |
| `SECURITY-FINDINGS.md` | Adversarial / red-team results (packing, quota escape) |
| `MULTI-TENANT-SETUP.md` | Standing up the whole stack |
| `A100-CLUSTER.md` | The `gpu0-a` A100 node reference |
| `UPSTREAM-NOTES.md` | Two general gVisor fixes bound for upstream |
| `webhook/pkg/gpushare` | The admission webhook that restates a pod's request as Sentry flags |
| `open-gpu-kernel-modules/DRIVER-CHANGES.md` | The privileged driver hooks the broker drives |
| `HAMi-gvisor` | The GPU-aware placement/admission fork (scheduler, device plugin, AMD CU allocator) |
| `vcluster-multitenant/README.md` | The cluster-tenancy layer above this one |
