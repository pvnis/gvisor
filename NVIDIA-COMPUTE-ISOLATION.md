# NVIDIA GPU compute isolation under gVisor: findings and a per-GPU test playbook

What can and cannot enforce a *compute* share between mutually-untrusting
sandboxes sharing one NVIDIA GPU, why, and **how to determine it on a new GPU** —
because the answer is decided by the GPU die class and its GSP firmware. Measured
first on the consumer RTX 5070 (Blackwell GB205), and **since 2026-08-17 on a
datacenter A100 80GB PCIe (GA100)**, which changed two of the conclusions below.

> Scope: this is about **compute** isolation. **Memory-quota** isolation is a
> separate, solved problem on this branch (admit-before-forward on the RM/VMM
> paths, `--nvproxy-gpu-memory-limit`, with `cuMemGetInfo`/NVML rewritten) and
> works on every GPU regardless of anything here.

## CORRECTION (2026-08-18): the temporal levers we drove were the wrong primitives

We obtained the Ghost paper — "Breaking the Tradeoff: Elastic and Isolated GPU
Sharing with Ghost" (Liu, Qiao, et al.; UCLA/Berkeley/Rice). It **falsifies the
framing of the temporal-lever results below**, though not the spatial, memory,
or adversarial ones.

- **Ghost runs on GSP, on the open modules, on an A100** — the same stack we
  have. Testbed: A100-40G, Ubuntu 24.04 / Linux 6.14, CUDA 12.9, **open kernel
  modules 575.57.08**. So the discrepancy is *not* a pre-GSP driver era and not
  a datacenter-vs-consumer firmware difference.
- **Ghost targets mutually-untrusted tenants** and relies on real GSP
  preemption at timeslice boundaries. So it is not a cooperative-only scheme.
- **Ghost's compute primitive is *runlist manipulation*, via two RPCs**:
  `DetachTSG`/`AttachTSG` (remove/re-insert the TSG entry in the global
  hardware runlist) and `SetTimeslice` (applied by updating the *active
  runlist*, which "compels the GSP to preempt running kernels once the timeslice
  expires").
- **What we drove was adjacent to, but not, those primitives.** Our `0b`
  "detach" issued `NVA06C_CTRL_CMD_INTERNAL_GPFIFO_SCHEDULE(disable)` — which by
  our own measurement only takes effect when the channel *idles*, so a saturating
  workload sails through; that is not `DetachTSG` removing the runlist entry.
  Our `0g` "timeslice" set `SET_TIMESLICE` on the TSG object and read it back,
  but never rewrote/resubmitted the active runlist, so GSP kept scheduling from
  the old runlist copy.

**So "every temporal lever is inert on the A100" is measuring the wrong thing.**
The correct, narrow statement is: *the specific controls we issued
(GPFIFO_SCHEDULE-disable, SET_TIMESLICE-without-runlist-resubmit,
SET_INTERLEAVE_LEVEL) produced no division; Ghost's runlist-manipulation
primitives (DetachTSG/AttachTSG, SetTimeslice-via-active-runlist) were not
tested, and the paper reports they work on this class of hardware.* **The reproduction is now done and positive** (next section): on 575.57.08,
Ghost's real primitive — `FifoDisableChannels` with a preempt — stalls a
saturating tenant completely while its neighbour takes the whole GPU. So the
temporal lever *is* real on this A100; our earlier "inert" was driving the wrong
control. The spatial-partition, memory-quota and adversarial results in this
document are unaffected. The sections further below are left as originally
written, with this correction governing their temporal conclusions.


## Reproducing Ghost's real primitive: runlist detach + preempt (2026-08-18, `gpu0-a`) — SEE CORRECTION 2 ABOVE

After obtaining the Ghost paper (CORRECTION at top), we ported the hooks to
Ghost's exact driver — **open modules 575.57.08**, A100, GSP — and implemented
the primitive we had been missing. Ghost's `DetachTSG` is not
`GPFIFO_SCHEDULE(disable)` (which sets `bOnlyDisableScheduling=TRUE` and only
removes a TSG from *future* scheduling, taking effect when the channel idles —
so a saturating workload sails through, which is exactly our old `0b` result).
The real primitive is **`NV2080_CTRL_CMD_FIFO_DISABLE_CHANNELS` with
`bOnlyDisableScheduling=FALSE`**, which forces a **preempt** — it evicts the
running kernels immediately. New hook `0i` originates it at kernel privilege
(via `pGpu->hInternalClient/hInternalSubdevice`) on a tenant's channels.

**Result — the temporal lever is real, and our earlier "inert" was a
wrong-primitive artifact:**

| 575.57.08, two saturating cuBLAS tenants, whole card | tenant 0 | tenant 1 | aggregate |
| --- | --- | --- | --- |
| no preempt (control) | 710.9 | 710.5 | 1421 (1:1 time-share) |
| **`GhostPreempt=1` → detach+preempt tenant 1** | **1565.5** | **stalled — zero matmuls** | 1565 (tenant 0 has the whole device) |

With the preempt primitive driving it, tenant 1's channels were disabled and its
running kernels evicted; it made **no compute progress at all** (not one sample),
while tenant 0 expanded to the **entire GPU** — 1565 matmul/s, up from the 711 it
gets sharing 1:1, and equal to the ~1586 it gets running solo. The `0i` control returned `NV_OK` on tenant 1's 4–8 channels
and re-fired on every `GPFIFO_SCHEDULE`, holding the victim down.

**This confirms the paper.** On the same GSP, same die, same driver class where
our earlier probes measured "every temporal lever inert," Ghost's actual
mechanism — runlist manipulation via `FifoDisableChannels`-with-preempt — binds
a saturating doorbell workload completely. The failure to reproduce was ours:
we drove `GPFIFO_SCHEDULE(disable)` (schedule-off, idle-only) and `SET_TIMESLICE`
on the TSG object without a runlist rewrite, neither of which forces a preempt.
The correct primitive does.

**What this changes for the branch.** A driver-resident (trusted-host) broker
*can* enforce a temporal compute share against an adversarial tenant on this
A100 — the thing the whole compute-isolation investigation had concluded was
impossible here. It requires kernel privilege (the control is kernel-only), so
it belongs in a trusted host component — `runsc gpu-scheduler` or a driver hook —
not the unprivileged Sentry, consistent with the governing constraint. The next
steps are to (1) drive it as a *scheduler* (toggle detach/attach per window and
apply proportional timeslices via the active runlist, rather than a permanent
one-way stall), and (2) confirm `SetTimeslice`-via-active-runlist gives a
proportional (not just binary) dial.

**Caveats.** The `0i` test is a *one-way* stall (permanent detach), which proves
the preempt sticks but is not yet the work-conserving elastic scheduler Ghost
describes — that needs the attach/detach toggle and the timeslice dial wired to
a period. **Confirmed on the deployed driver too.** The `0i` hook was ported to
610.43.02 (the driver the k8s stack runs) and gives the identical result:
tenant 0 at **1564.9 matmul/s** (whole device), tenant 1 **stalled to zero**,
the control returning `NV_OK` on its channels. So the primitive is not specific
to 575 — it works on 575.57.08 and 610.43.02 alike, which is what lets a broker
built on it run on the node as deployed.

One caveat remains: the `0i` test is still a *one-way* permanent detach (proves
the preempt sticks, not the elastic scheduler). Turning it into Ghost's
work-conserving broker needs the attach/detach toggle per period and the
`SetTimeslice`-via-active-runlist proportional dial, wired to `pkg/gpusched`.


## CORRECTION 2 (2026-08-18): detach blocks a tenant at *enable*, but does NOT preempt a *running* one — a work-conserving time-slicer is not achievable here

Attempting to build the work-conserving scheduler the reproduction seemed to
justify exposed that the previous "the temporal lever is real" conclusion was
**over-claimed**, and this section corrects it. The scheduler needs to preempt a
*running* tenant to hand the GPU to another; deeper testing shows the reachable
controls cannot do that on this A100 GSP.

Built and tested: a dynamic driver control (`/proc/driver/nvidia/gpusched`
accepting `detach <pid>` / `attach <pid>`, issuing `FifoDisableChannels` with a
forced preempt from an RM work item at kernel privilege — the same RPC the `0i`
hook used). Results, all on 610.43.02:

| scenario | result |
| --- | --- |
| **enable-time** detach (GhostPreempt, staggered so the 2nd tenant is detached as it enables) | 2nd tenant stalls to zero, 1st runs full (1564 matmul/s) — reproduces the earlier "0i" result |
| **mid-run** detach of a lone running **saturating** tenant (repeated 10×) | no effect — 965 matmul/s throughout |
| **mid-run** detach of one of **two running** tenants | no effect — 715/715, the "detached" tenant keeps its half |
| **mid-run** detach of a **yielding** tenant (15% util, idle every 3 ms) | no effect — 270 matmul/s throughout |
| `CHANNEL_PREEMPTIVE_REMOVAL` (a dedicated forced evict) via RPC | **`NV_ERR_NOT_SUPPORTED` (0x56)** on this GSP |

The same recorded channel handles that stall a tenant at enable do **nothing**
to it once it is running, and the work-item path is not the problem (GhostPreempt
drives the identical work item and does stall at enable). So the distinction is
timing, not plumbing: **`FifoDisableChannels` governs a channel only at/before it
becomes active on the runlist; GSP does not honor a disable/detach of an
already-running channel.** This is the same shape as the timeslice and interleave
results — the driver→GSP runlist RPCs do not control work that is already
scheduled — and it means the earlier "0i" stall was a tenant **blocked from
starting** (detached the instant it enabled, repeatedly), not a running tenant
**evicted**.

**Consequences:**

- **The "reproduce Ghost's real primitive — detach+preempt WORKS" claim is
  narrowed.** What works: detaching a tenant *as it enables* keeps it off the
  GPU entirely (a hard admission block). What does *not* work: preempting or
  throttling a tenant that is already running — which is exactly what a
  time-slicer needs.
- **A work-conserving temporal scheduler is not achievable with the controls
  reachable from the open driver on this A100.** You cannot take the GPU away
  from a running tenant to give another its turn. The one lever that does bind —
  the **spatial TPC partition** — remains the only imposable compute-isolation
  primitive measured on this hardware, and it is a static ceiling, not elastic.
- **How Ghost reconciles with this is now genuinely open.** The paper reports
  `SetTimeslice`/`DetachTSG` preempting running kernels on an A100 (575.57.08);
  every runtime test here says the reachable RPCs do not. The difference must be
  either a control/parameter path we have not reproduced (their code is not
  public) or a firmware/config difference we have not isolated — but "it works,
  we just drove the wrong primitive" is no longer supported by measurement:
  we drove `FifoDisableChannels`+preempt, the paper's stated `DetachTSG`, and it
  does not evict running work here.

The driver control and the scheduler design are left in the tree
(`ogkm-610` hooks, `/proc/driver/nvidia/gpusched`) as a probe, not a working
scheduler. The node is restored to clean 610 + the k8s stack.

**Method note, against myself:** the earlier positive was a single staggered
two-tenant run where the victim produced no output, read as "evicted." It was
"never started." The lesson is the branch's own: measure the thing you're
claiming (here, *preempt a running tenant*), not a proxy that a different
mechanism also satisfies.


## CORRECTION 3 (2026-08-18): the temporal levers DO work — the missing companion was RESTART_RUNLIST

CORRECTION 2 concluded a work-conserving temporal scheduler was not achievable
here. **That was wrong, and this is the resolution.** Every earlier "inert"
measurement — SET_TIMESLICE (16:1 → 1:1), the mid-run detaches — was missing a
single companion RPC that commits the change to GSP: **`NVA06F_CTRL_CMD_RESTART_RUNLIST`**,
which *"expires the current timeslice and restarts the runlist... effectively
preempts the current channel"* (privileged-only). It returns **`NV_OK` on this
A100's GSP** (unlike `CHANNEL_PREEMPTIVE_REMOVAL`, which is NOT_SUPPORTED). Pair
either lever with it and both of Ghost's primitives work, measured on 610.43.02:

**DetachTSG/AttachTSG** (`FIFO_DISABLE_CHANNELS` + `RESTART_RUNLIST`):

| | A | B |
| --- | --- | --- |
| both attached (share) | 710 | 714 |
| **detach B + restart** | **1551 (whole device)** | evicted — no progress |
| **attach B** | 709 | 709 (resumed cleanly) |

A running tenant is genuinely evicted and cleanly restored — a full reversible
cycle, repeated over a 40 s scheduler run, not a startup artifact.

**SetTimeslice** (`SET_TIMESLICE` + `RESTART_RUNLIST`), the finer lever, no full
detach:

| timeslice A:B | measured throughput A:B |
| --- | --- |
| 8000 µs : 500 µs (16:1) | 1406 : 79 → **14:1** (42 vs 3 samples / 15 s) |

Shrinking a *running* tenant's timeslice shifts its compute share proportionally
— Ghost's exact "shrinking the timeslice compels GSP to preempt" behaviour,
which was measured inert before only because SET_TIMESLICE alone never committed
a new runlist to GSP.

### A work-conserving weighted scheduler, driven from userspace

A minimal host-side loop (`/proc/driver/nvidia/gpusched` accepting
`detach`/`attach`/`ts <pid> <us>`, the policy in userspace, the mechanism in the
driver at kernel privilege) time-slices two tenants by weight:

- **3:1 request → 71%/29% (2.49:1) measured** by total work over 40 s
  (92 vs 37 completed 500-matmul samples). The gap from 3:1 is switch overhead
  and the bash loop's timing jitter, not a mechanism limit.
- **Aggregate 1612 matmul/s** — *above* naive contention (1420) and at solo
  level (1550), because time-slicing runs one tenant at full rate instead of two
  contending.
- **Work-conserving:** kill one tenant and the survivor immediately gets the
  whole GPU (1541 matmul/s ≈ solo). Idle time is not wasted.

### What this means, and the correction to the whole arc

A **driver-resident, work-conserving temporal scheduler that binds a running
(saturating) doorbell tenant is achievable on this A100** through controls
reachable from the open driver, at kernel privilege — the thing the
compute-isolation investigation had repeatedly concluded was impossible. It uses
exactly Ghost's two primitives (`SetTimeslice`, `DetachTSG`) plus the runlist
commit (`RESTART_RUNLIST`) they depend on. It needs kernel privilege, so it
belongs in a trusted host component (`runsc gpu-scheduler` / a driver hook),
never the Sentry — consistent with the governing constraint and with Ghost's own
architecture.

The arc that led here, stated plainly so the record is not trusted too much at
any single step: (1) "every temporal lever is inert" — measured, but with
SET_TIMESLICE-without-commit and GPFIFO_SCHEDULE-disable, both missing the
restart; (2) "detach+preempt works" — an enable-time block, over-read; (3)
"not achievable" — measured with channel-level controls but still no runlist
restart; (4) **this** — the levers work once the change is committed with
`RESTART_RUNLIST`, measured two independent ways (detach and timeslice) with
quantified, reversible results. The single technical fact under all of it:
**GSP does not act on a per-object runlist change until the runlist is
restarted; `RESTART_RUNLIST` is that commit, and it is supported here.**

### Through the full Kubernetes + vCluster stack (adversarial)

The runlist scheduler was then run through the real path: the ghost driver, k3s,
HAMi, `runsc gpu-scheduler --runlist-control`, and two **cuBLAS (doorbell)** pods
deployed from separate vCluster tenants -- the workload the compute gate cannot
divide (measured 1:1). Two findings:

- **The mechanism binds a real gVisor doorbell pod.** Setting the two pods'
  timeslices 6:1 by hand through the control divided their throughput **6.33:1**
  (86%/14%), with 4 channel groups each and no dilution -- a saturating cuBLAS
  tenant inside a real sandbox, held to a weighted share the gate leaves at 1:1.
- **The gate must stand down, and it now does.** When both the gate (mapping
  revocation) and the runlist ran, the gate revoked a sandbox's mappings outside
  its window, the GPFIFO drained to `PUT==GET`, the channel-state signal read it
  idle, and the timeslice flapped. The server now sends a full-period window
  whenever the enforcer is set, so the runlist alone divides.

**One plumbing gap remains in the *automatic* path, and it is a
systrap-specific pid-identity problem.** The node runs `--platform=systrap`
(not KVM -- NVIDIA does not need KVM the way AMD's KFD does; see below), so a
sandbox is not one process but a Sentry plus a tree of stub processes. Only one
of the two co-scheduled sandboxes was auto-enforced: the pid `runsc` announces
for a sandbox did not match the tgid the driver records that sandbox's channels
under, so the other's timeslice commands hit a dead pid. Its weight was still
known (the enforced pod's slice flapped 3000<->1000 as the mis-plumbed peer's
connection came and went), so the division landed at 1.47:1 instead of 3:1. The
mechanism itself is proven (6.33:1 by hand on the same pods); what is unresolved
is the sandbox->pid mapping under systrap's multi-process model. Under KVM a
sandbox is a single process, so the announced pid and the channel-owner tgid
coincide -- which would make the scheduler's pid plumbing trivial, at the cost of
the heavier platform. Aligning the two under systrap (or announcing every
channel-owning pid a sandbox holds) is the next fix.

### Wired into `runsc gpu-scheduler`, end to end

`pkg/gpusched` now has an `Enforcer` that drives this control, and
`runsc gpu-scheduler --runlist-control=/proc/driver/nvidia/gpusched` turns it on.
The scheduler holds each active sandbox to a timeslice proportional to its
weight and detaches one idle past the threshold, committed with the runlist
restart. Verified with the real binary: two sandboxes weighted 3:1, two
saturating cuBLAS burns that otherwise share 1:1, divided **76/24 (3.10:1)** —
the scheduler set `SET_TIMESLICE` 3000/1000 µs and GSP honoured it. This is the
first time on this branch that a *doorbell* compute workload has been divided by
weight at all; the Sentry compute gate leaves it at 1:1. Enforcement is
privileged, so it runs in the host-side scheduler, never a sandbox. It requires
the ghost-instrumented driver; without `--runlist-control`, only the gate
enforces, exactly as before.

The prior CORRECTION 2, the "not achievable" lines in CLAUDE.md/GHOST-PLAN, and
the Reproducing-Ghost section above are superseded by this. The driver control
(detach/attach/ts + RESTART_RUNLIST via an RM work item) is in the `ogkm-610`
tree; the node is restored to clean 610 + the k8s stack.

## TL;DR

- The governing constraint is that enforcement must live in the **Sentry** (or a
  trusted host component), never inside the container. Work submission on
  Volta+ never enters the kernel — it is a write to a mapped pushbuffer plus a
  doorbell (MMIO) ring — so the Sentry sees **no ioctls per kernel launch** and
  cannot meter or intercept submission.
- **On the A100 an imposed spatial partition works.** `SET_TPC_PARTITION_TABLE`
  returns `NV_OK` at kernel privilege and *is enforced by the hardware*:
  granting a burn workload 13/14/27/40/54 of the GA100's 54 TPCs gives
  502/529/960/1244/1550 matmul/s against 1550 unpartitioned. This is the first
  positive measurement of this control on any GPU we have tested, and it is the
  NVIDIA analog of the AMD CU mask.
- **The GB205 "spatial is NOT_SUPPORTED" result was a mis-read and is now
  suspect.** The 5070 probe issued the control from inside the ctxshare's *own*
  constructor and got `0x57` — which is `NV_ERR_OBJECT_NOT_FOUND`, not
  `NV_ERR_NOT_SUPPORTED` (`0x56`). GSP cannot resolve a handle for an object
  whose construction has not finished. The A100 returns the identical `0x57`
  from that call site and `NV_OK` from a later one (the `GPFIFO_SCHEDULE` path).
  **The 5070 must be re-probed from the deferred call site before its negative
  stands.**
- **The temporal controls *we drove* produced no division — but see the
  CORRECTION above: they were not Ghost's primitives.** `SET_TIMESLICE` on the
  TSG object (16:1 → measured 1:1), `GPFIFO_SCHEDULE(disable)`, and
  `SET_INTERLEAVE_LEVEL` (HIGH-vs-LOW → 1:1) are all accepted with `NV_OK` at
  kernel privilege and none divided the GPU. Ghost instead *manipulates the
  global runlist* (DetachTSG/AttachTSG; SetTimeslice applied via the active
  runlist), which we did **not** test. So this is a fidelity gap in our probe,
  not a demonstrated property of the firmware.
- **A spatial partition does not buy concurrency on NVIDIA, only a cap.** Two
  processes on *disjoint* halves (27+27 TPC) and two on the *same* 27 TPCs
  perform identically (442/443 vs 440/441 matmul/s) — separate CUDA contexts
  time-slice regardless of which SMs they are allowed to use. The partition
  multiplies with the time-slice instead of replacing it, so it costs ~40% of
  aggregate throughput (885 vs 1422 for two unpartitioned tenants). It is a
  hard, imposable *ceiling* and an approximate proportional dial (40:14 TPCs →
  2.66:1 throughput), not a way to run tenants side by side. This is the sharp
  difference from AMD, where disjoint CU masks *do* run concurrently.
- **Privilege was never the wall; the call site and the die are.** At kernel
  privilege every `0x1b` INSUFFICIENT_PERMISSIONS disappears, on both dies —
  including `SET_INTERLEAVE_LEVEL`, which we could not even attempt from the
  Sentry.
- The only thing that isolates compute on the 5070 is a **green context**
  (measured: 24/48 SMs → 0.59× solo, 0.99× under a disjoint-half peer), but it is
  applied per-kernel via the TMD in userspace CUDA — **cooperative**, below the
  ioctl boundary, so it cannot be imposed on a hostile tenant.
- **The determining factor is still the GPU die class**, but the split is now
  measured on one datacenter die rather than inferred: GA100 supports MIG *and*
  honors the TPC partition table. Whether GB205/GA102/GB202 do is open again,
  because the probe that said "no" asked the question wrong.

## The mechanisms, and what each requires

| mechanism | layer | enforced by | RTX 5070 (GB205) | A100 80GB (GA100) |
| --- | --- | --- | --- | --- |
| **Compute gate** (revoke command-buffer mappings so the next submit faults) | Sentry | fault in the Sentry | **fails for doorbell** — cuBLAS/graph replay faults ~0/period; enforces only frequently-faulting kernel loops | not re-tested; same mechanism, same doorbell submission |
| **Scheduler window / `pkg/gpusched`** (weighted, work-conserving) | Sentry | the compute gate | correct windows; **no enforcement** for doorbell (the gate can't) | same |
| **Static TSG timeslice** (`NVA06C_CTRL_CMD_SET_TIMESLICE`) | Sentry/driver | GSP runlist | no division — but set on the TSG object *without resubmitting the runlist*; **not** Ghost's SetTimeslice-via-active-runlist (see CORRECTION) | no division — 8000 vs 500 µs, both `NV_OK`, both read back, 708.9/706.7 = 1:1; runlist never rewritten, so untested as Ghost drives it |
| **Channel-group disable** (`GPFIFO_SCHEDULE(disable)`) | Sentry/driver | GSP runlist | `NV_OK` but takes effect only on idle — a saturating workload runs at full rate; **this is not Ghost's DetachTSG** (runlist-entry removal), see CORRECTION | same — burn keeps its full 1547 matmul/s; the true runlist-detach RPC was not driven |
| **Interleave level** (`SET_INTERLEAVE_LEVEL`, LOW/MED/HIGH priority) | Sentry/driver | GSP runlist | **admin-gated** (`0x1b`) from the Sentry; efficacy untested | **inert** — SET HIGH vs LOW both `NV_OK` at kernel priv (no privilege wall), GET returns NOT_SUPPORTED, division 708.1/706.0 = 1:1 |
| **Static TPC partition table** (`NV9067_CTRL_CMD_SET_TPC_PARTITION_TABLE`) | Sentry/driver | GPU hardware (WDU) | `0x57` at kernel priv **from the constructor call site — that is OBJECT_NOT_FOUND, so this is not a verdict**; re-probe needed | **`NV_OK` and enforced** — 13/27/40/54 TPCs → 502/960/1244/1550 matmul/s |
| **CWD watermark** (`NV9067_CTRL_CMD_SET_CWD_WATERMARK`, per-subcontext CUDA Work Distributor throttle) | Sentry/driver | GPU hardware (WDU) | `0x57` from the same wrong call site; re-probe needed | **`NV_OK`, reads back MIN, no measurable effect** — 1547.5 matmul/s, unchanged. It throttles subcontexts *within* one context; separate tenants are separate contexts |
| **Green context** (`cuGreenCtxCreate` + per-kernel TMD TPC mask) | userspace CUDA | GPU hardware (WDU) | **works and isolates**, but cooperative — the mask is in the TMD, written by userspace, below the ioctl boundary; a hostile tenant just doesn't use it | not re-tested |
| **MIG** | firmware/HW | hardware partition | **absent** on GB205 | **present** — `1g.10gb`×7 … `7g.80gb`, currently disabled |

Three structural facts underpin the table:

1. **On GSP GPUs the CPU driver does not run the scheduler.** `GpFifoSchedule`,
   `SetTimeslice`, etc. just RPC to GSP firmware; `kfifoRunlistSubmit` is the
   not-supported stub on GSP parts. So "being in the driver" is **not** a more
   forceful path to the scheduler than the userspace RM control — both end at the
   same signed GSP firmware. Measured on both dies: every runlist lever is
   accepted and ignored, at kernel privilege, on a datacenter part too.
2. **Privilege, call site, and feature support are three separate gates.** From
   userspace the spatial controls return `INSUFFICIENT_PERMISSIONS` (`0x1b`);
   at `RS_PRIV_LEVEL_KERNEL` that disappears. From inside the ctxshare's own
   constructor they then return `NV_ERR_OBJECT_NOT_FOUND` (`0x57`) — GSP has no
   handle for a half-built object — which is *not* `NV_ERR_NOT_SUPPORTED`
   (`0x56`) and says nothing about the die. Only from a later call site (we use
   `kchangrpapiCtrlCmdGpFifoSchedule_IMPL`, which runs once the TSG is enabled)
   does the answer mean anything. **This distinction invalidated a conclusion:
   read the status code against `nvstatuscodes.h`, not from memory.**
3. **The GPU time-slices between contexts no matter what SMs each may use.**
   Two tenants on disjoint TPC halves get exactly what two tenants on the *same*
   half get. Cross-context concurrency on NVIDIA needs MPS or MIG; a TPC
   partition alone narrows each tenant *within* its slice of time, so the two
   effects multiply.

## What we proved with the driver-level broker (Phase 0, on the RTX 5070)

We built the "privileged driver-level broker" that the whole investigation
concluded was necessary — swapped the RTX 5070 to the **open** 610.43.02 kernel
modules and added hooks that originate the controls at kernel privilege.

- **0b — TSG detach**: hook in `kchangrpapiCtrlCmdGpFifoSchedule_IMPL` force-
  disables the sandbox's channel group right after it enables it. Result:
  `disable -> 0x0` (NV_OK) on every TSG, but the burn ran at the full 458
  matmul/s. **Detach does not stick.**
- **0c — TPC partition table**: hook in `kctxshareapiConstruct_IMPL` sets STATIC
  mode (`0x0`, a no-op success) then the table. Result:
  `SET_TPC_PARTITION_TABLE = 0x57`. **Read at the time as NOT_SUPPORTED. It is
  not: `0x57` is `NV_ERR_OBJECT_NOT_FOUND`, and it is what the A100 — where the
  control demonstrably works — returns from that same call site.** The 5070's
  spatial verdict is therefore *unknown*, not negative, until it is re-probed
  from the deferred call site.
- **0d — CWD watermark**: same hook, same `0x57`, same correction.

What still stands from Phase 0: the broker unlocks kernel privilege (every
`0x1b` disappears), and the *temporal* levers are genuinely ineffective on
GB205 — that was judged by throughput, not by a status code, which is why it
survived. What does not stand: "the spatial controls are absent from this die."
That was a status code read wrong, from the wrong call site.

Ghost ("Breaking the Tradeoff", OSDI-class, tested on A100) was assumed to work
because datacenter GSP honors detach/timeslice. **The A100 measurements below
falsify that assumption for driver 610.43.02** — detach, timeslice and
interleave are as inert there as on GB205.

## What the A100 measured (2026-08-17, `gpu0-a`, GA100, open 610.43.02)

The Step-3 playbook, run on a datacenter die. Same hooks, plus a **deferred
re-probe** (`0e`/`0f`) that re-issues each control from
`kchangrpapiCtrlCmdGpFifoSchedule_IMPL` once the ctxshare is registered with
GSP, and runtime knobs (`NVreg_RegistryDwords="GhostTpcCount=27;…"`) so one
build sweeps every configuration. Workload: `ghost-experiment/burn.py`, a
saturating fp16 4096³ matmul loop (doorbell/graph-replay submission, the case
that defeats every temporal lever). 54 TPCs = 108 SMs.

**Status codes, at kernel privilege:**

| probe | constructor call site | deferred call site | effect on throughput |
| --- | --- | --- | --- |
| `SET_TPC_PARTITION_MODE(STATIC)` | `NV_OK`, mode reads back 0 | `NV_OK`, mode reads back **1 (STATIC)** | — |
| `SET_TPC_PARTITION_TABLE` | `0x57` OBJECT_NOT_FOUND | **`NV_OK`** | **enforced, see below** |
| `SET_CWD_WATERMARK(MIN)` | `0x57` OBJECT_NOT_FOUND | **`NV_OK`**, reads back 1 | none (1547.5 vs 1547.1 solo) |
| `GPFIFO_SCHEDULE(disable)` | — | `NV_OK` on all 4 TSGs | none — full 1547 matmul/s |
| `SET_TIMESLICE` 8000 vs 500 µs | — | `NV_OK`, both read back | none — 708.9 / 706.7 |
| `SET_INTERLEAVE_LEVEL` HIGH vs LOW | — | SET `NV_OK`, GET `0x56` | none — 708.1 / 706.0 |

**The TPC partition is real (solo, matmul/s):**

| TPCs granted | 13 | 14 | 18 | 27 | 40 | 54 | none |
| --- | --- | --- | --- | --- | --- | --- | --- |
| rate | 502 | 529 | 669 | 960 | 1244 | 1550 | 1550–1613 |

Sub-linear the same way AMD's CU mask is: half the TPCs give 0.62× the
throughput, not 0.5×, because a narrower partition clocks and caches better.

**Two tenants (separate processes, median of the last 20 samples):**

| configuration | tenant A | tenant B | aggregate | each vs its own solo |
| --- | --- | --- | --- | --- |
| no partition | 711.1 | 710.7 | 1422 (92% of solo) | 0.459 |
| no partition, three tenants | 473.0 | 470.3 / 473.1 | 1416 | 0.305 |
| **disjoint** 27 + 27 TPC | 442.7 | 442.8 | 885 (57%) | 0.461 |
| **overlapping** 27 + 27 (same TPCs) | 440.2 | 440.7 | 881 | 0.459 |
| disjoint 40 + 14 TPC | 624.7 | 234.7 | 859 | 0.502 / 0.444 |
| **three** tenants, disjoint 18 each | 202.3 | 202.3 / 202.3 | 607 (39%) | 0.302 |

Read these four rows together:

- **Disjoint and overlapping are the same measurement.** Whatever the partition
  does, it is not letting the two tenants run at once. Contexts time-slice;
  each tenant then runs narrowed inside its own slice, so it loses ~54% to the
  time-slice *and* whatever the partition takes — the two costs multiply. This
  is the single biggest difference from AMD, where disjoint CU masks genuinely
  execute concurrently and cost each other 0.6%.
- **Time is divided per context, evenly, and the partition does not change
  that.** Three unpartitioned tenants get 1/3 each; the 14-TPC tenant still
  gets half the *time* against a 40-TPC neighbour, it just does less with it.
  The two factors multiply exactly: three tenants on disjoint 18-TPC thirds
  score 202.3 each — 0.302 of the 669 that 18 TPCs give solo, the same 1/N the
  unpartitioned three-tenant case pays (0.305). Division between equals is
  perfectly fair (202.3/202.3/202.3, Jain 1.0000); it is the aggregate that
  suffers, down to 607 of 1550.
  A tenant that opens more contexts therefore takes a larger share of time —
  the spatial control bounds a tenant's *rate*, never its share of the runlist.
- **What the partition does buy: an imposable ceiling and a proportional dial.**
  14 of 54 TPCs is a hard 529 matmul/s cap the tenant cannot exceed even when
  the GPU is otherwise idle, and 40:14 TPCs produced 2.66:1 throughput against
  a 2.86:1 width — approximate, like AMD's 2:1 CUs → 1.84:1.
- **The cost is aggregate throughput**: 885 against 1422 for the same two
  tenants unpartitioned. Idle TPCs in a tenant's slice go to nobody.

**MIG is present and disabled** (`1g.10gb`×7 through `7g.80gb`). For two or
three mutually-untrusting tenants with fixed shares, MIG is the stronger and
simpler answer on this hardware; the TPC table matters where MIG's granularity
(7 fixed instances, requires GPU reset, no memory-quota flexibility) does not
fit, or where shares must change without touching the device.


## Through the full Kubernetes stack: multi-tenant sharing, memory, and an adversarial tenant (2026-08-18, `gpu0-a`)

Everything above was measured bare-metal with native processes. This section is
the same GA100 driving **real gVisor sandboxes** through k3s + HAMi +
`runsc gpu-scheduler` + the `gpu-quota-webhook`, two vClusters available as
tenants. Workloads: `matmul` = the saturating cuBLAS loop (doorbell submission,
the gate cannot touch it); `launch` = a tight `a+b` kernel-launch loop (each
launch re-enters through a path the gate *can* revoke). Rates are the median of
the samples taken inside a 45 s window after a 25 s settle. Solo baselines on
this stack: launch **120 443/s**, matmul **1562/s**.

### Time-sharing divides correctly, and stays fair further than AMD does

| tenants | weights | workload | per-tenant | Jain | aggregate |
| --- | --- | --- | --- | --- | --- |
| 2 | 50/50 | matmul | 712 / 711 | 1.0000 | 1424 (91% of solo) |
| 3 | equal | matmul | 474 / 474 / 474 | 1.0000 | 1421 |
| 2 | 50/50 | launch | 47 514 / 47 097 | 1.0000 | 94 610 (79%) |
| 3 | equal | launch | 25 562 / 27 820 / 25 466 | 0.9983 | 78 847 |
| 4 | equal | launch | 20 082 / 19 887 / 19 848 / 19 920 | 1.0000 | 79 737 |

Equal shares are exactly fair, and — unlike AMD's Navi 32, which goes bimodal
past three tenants — **NVIDIA's time-slicing stays fair through four** (Jain
1.0000 at four launch tenants). The matmul aggregate is ~1420 regardless of
tenant count: cuBLAS contexts time-slice among themselves, so N tenants each
get 1/N and the total is conserved. The launch aggregate falls to ~79% under
the gate (the price of revoking and restoring submission mappings each period).

### Weighted shares work, and overshoot toward the heavy tenant

| tenants | weights | requested | measured | aggregate |
| --- | --- | --- | --- | --- |
| 2 | 75/25 | 3.00:1 | 82 435 / 22 108 = **3.73:1** | 104 542 (87%) |
| 3 | 60/30/10 | 6:3:1 | 60 677 / 27 942 / 8 013 = **7.57:3.49:1** | 96 632 |

The division is real — without the scheduler two equal tenants split 1:1 — and
proportional, but the heaviest tenant takes somewhat *more* than its weight,
the same approximate-dial behaviour AMD's CU masks show. Only the `launch`
workload is divided; see the adversarial section for what `matmul` does.

### Work-conserving

One busy `launch` tenant beside one registered-but-idle tenant kept **120 585/s
— 100% of solo**. The idle tenant drops to its 5 ms floor and its neighbour
expands to the whole period; a tenant pays nothing for a neighbour that is not
asking.

### Memory quotas hold under concurrency

Three sandboxes allocating at once, quotas 4 / 2 / 1 GiB, each in 64 MiB steps
until refused:

| quota | stopped at | `total` it saw | after freeing half |
| --- | --- | --- | --- |
| 4096 MiB | 3648 | 4096 | free → 1823 |
| 2048 MiB | 1600 | 2048 | free → 799 |
| 1024 MiB | 576 | 1024 | free → 287 |

Each capped at its own ceiling (the ~420 MiB shortfall is torch's CUDA context,
charged to the sandbox), each saw only its own `total` — never the device's
81 920 MiB — and freed correctly. No cross-talk.

### An adversarial tenant: memory and privilege hold, doorbell compute does not

A hostile pod requesting a 1 GiB / weight-20 slice but annotating itself for
80 GB / weight 100, with `CUDA_DISABLE_CONTROL=true` set, running a probe kit:

| the attack | result |
| --- | --- |
| annotate itself the whole card + max weight | rewritten to 1 GiB / weight 20 at admission by the webhook |
| allocate past the quota | capped at 512 MiB of its 1 GiB, `CUDA_ERROR_OUT_OF_MEMORY` |
| `CUDA_DISABLE_CONTROL=true`, check `ld.so.preload` | preload absent; `mem_get_info` still the 1 GiB quota — the Sentry limit is independent of any in-container library |
| observe neighbours (`nvidia-smi`, `--query-compute-apps`, `pmon`) | sees **only its own** process and its own 1 GiB `total`; never a neighbour, never 81 920 MiB |
| `nvidia-smi -r` (GPU reset) | refused, "not supported" |
| `nvidia-smi -lgc` (lock clocks) | refused, "no permission" |

The one boundary that does **not** hold is the compute *share* against a
doorbell workload — the documented Volta+ gate limitation, confirmed here as an
a **weight-25** attacker running cuBLAS beside a **weight-75** victim also
running cuBLAS measured **720 / 720 — 1:1, not 3:1**. The scheduler granted the
attacker a 25 ms window and the victim 75 ms (logged in both Sentries), and the
attacker took 50% of the GPU anyway. Its memory quota still held throughout;
what it escaped was the compute *share*, and only by choosing a submission path
the gate cannot revoke.

**The reading.** Every isolation this branch claims to enforce held against a
tenant actively trying to break it — memory quota, quota visibility, annotation
escalation, in-container-library standdown, and privileged device controls. The
compute *share* is the one that does not hold against an adversary on Volta+,
and it fails open (the attacker gains, it is not throttled to nothing): a tenant
that wants more than its weight submits through cuBLAS and gets an equal split
instead. The defenses that do work against this are spatial and hardware-backed
— the imposed TPC partition (a hard rate cap, below) or MIG — not the temporal
gate. This is why the branch's position is that a scheduler should **cap
concurrent GPU tenants and prefer a spatial partition for mutually-untrusting
compute**, using the temporal gate only where the tenants are not adversarial
about their submission path.


## Is the TPC partition adversarially sound? (2026-08-18, `gpu0-a`, 610.43.02)

The imposed TPC partition is the one Ghost-style primitive that works on GA100.
Everything measured before was *cooperative* — one well-behaved process per
tenant. These probes are a tenant trying to break out of its 27-TPC (of 54)
slice, driver reloaded with `GhostTpcCount=27;GhostDisjoint=1`.

| attack | result | matmul/s |
| --- | --- | --- |
| baseline, one context | capped | 963 (vs 1550 full card) |
| 8 CUDA streams in one context | no escape | 1007 |
| 4 host threads hammering one context | no escape | 956 aggregate (239×4) |
| **one tenant, 3 processes** | **no escape** | 294 each, **882 aggregate** |

**Oversubscription inside one context cannot exceed the slice.** Streams and
threads share the one ctxshare, so they share its 27 TPCs; the cap is a hardware
property of the partition and does not care how much work is thrown at it.

**Spawning processes does not escape it either — and this is the important
one.** Each new process creates its own ctxshare, which the hook grants its own
disjoint slice (dmesg: `granted 27 TPCs 0..26`, `27..53`, `0..26` as the global
slice counter advances and wraps). But separate ctxshares are separate CUDA
contexts, and **contexts time-slice** — so three of them covering the whole card
still only reach 882/s aggregate, *below* the 960 a single slice gives. The very
property that makes the spatial partition a ceiling rather than concurrency
(measured earlier: disjoint 27+27 = overlapping 27+27, both ~885) is what
defends it here: a tenant cannot buy more GPU by fragmenting into contexts,
because the hardware serialises them regardless of which TPCs each may use.

**Two real caveats, neither an escape today:**

- **The experimental hook keys slices to a global counter, not to the tenant.**
  A real broker must key the TPC range to the *sandbox* and clamp every
  ctxshare it opens to that one range; as written, a tenant that opens many
  ctxshares marches the counter through — and wraps onto other tenants' ranges.
  That is a fairness/overlap defect, not a throughput escape (time-slicing
  bounds the aggregate regardless), but it means this hook is a *probe*, not a
  finished enforcement path. The per-tenant identity needed for it already
  exists in the tree (`ghostTenantIndex_GHOST`, used for the 0g/0h probes); it
  simply is not wired into the 0c/0f grant.
- **MPS is the one vector that could turn this into an escape, and it is not
  reachable from a default sandbox.** MPS runs multiple contexts *concurrently*
  instead of time-slicing them; under MPS a tenant's fragmented contexts would
  run at once and the "ceiling" would leak. MPS needs a control daemon and a
  pipe directory the sandbox does not have, so it is out of reach here, but a
  deployment that exposes MPS must partition at the tenant level and forbid a
  tenant its own MPS control.

**Widening its own partition:** a tenant cannot. The `SET_TPC_PARTITION_TABLE`
that imposes the slice succeeds only at kernel privilege (the driver hook); the
same control issued from an unprivileged context returns
`NV_ERR_INSUFFICIENT_PERMISSIONS` (`0x1b`), and under gVisor the sandbox never
reaches even that far — the control is not in nvproxy's allowlist, so the ioctl
is refused in the Sentry before any driver code runs.


## Validating Ghost by driver generation: R535 behaves like 610 (2026-08-18, `gpu0-a`)

The premise this whole line of work rested on — "datacenter GSP firmware honors
the temporal primitives (detach / timeslice / interleave), which is why Ghost
works on A100" — was measured **false on driver 610.43.02**. The obvious
objection is that 610 is newer than whatever Ghost used, and NVIDIA changed GSP.
The paper's exact driver is not recoverable (not on this host, not findable
online), so rather than guess, the ghost hooks were **ported to R535.183.06**
— the open-kernel-module branch that is nvproxy-supported and the overwhelmingly
likely A100 driver of the paper's era — and the temporal probes re-run on the
*same GA100*, at kernel privilege, judged by throughput.

Method: the 535.183.06 open modules built from source with the GHOST hooks
(`git apply` of `driver-hooks.patch`, one additive conflict resolved), 535
userspace from the `.run` installer (`--no-kernel-modules`), the same
`burn.py` doorbell matmul workload and the same venv. The 610 boot-survival
modules were stashed so `modprobe` could not auto-reload them.

**Per-TSG timeslice, set 16:1 at kernel privilege — inert, exactly as on 610:**

| driver | tenant 0 SET | tenant 1 SET | readback | throughput |
| --- | --- | --- | --- | --- |
| 610.43.02 | 8000 µs | 500 µs | both correct | 708.9 / 706.7 (1.00:1) |
| **535.183.06** | 8000 µs | 500 µs | both correct | **442.3 / 442.1 (1.00:1)** |

The control is accepted, the runlist stores the value and reads it back, and
GSP divides the two doorbell tenants **evenly regardless**. A 16:1 timeslice
ratio produced a 1.00:1 throughput ratio on the paper-era driver. (The absolute
rate differs — 442 vs 708 — only because this run used two full-device tenants
time-slicing 1:1, i.e. ~half of the ~885 two-tenant aggregate; the *ratio* is
the measurement, and it is 1:1.)

**Runlist interleave, HIGH vs LOW at kernel privilege — inert.** Tenant 0 set
to interleave level 3 (HIGH), tenant 1 to level 1 (LOW), both accepted
(`SET_INTERLEAVE(3)`/`(1)` returned `NV_OK`, where 610 had refused the GET with
`0x56`): **441.9 / 441.9, 1.00:1**. The coarse priority knob changes nothing
either.

**TSG detach — inert, exactly as on 610.** With `GhostDetach=1` the driver
issues `GPFIFO_SCHEDULE(disable)` on every TSG at kernel privilege and each
returns `NV_OK` (dmesg: `force-disabled ... after enable -> 0x0`). The burn
kept running at the **full 1558.9 matmul/s**. A "disabled" TSG whose channel
never idles is never actually descheduled — the doorbell workload sails through,
the same result 610 gave.

**The spatial TPC partition is identical on 535 — it works.** The `0f` grant
fires from the deferred call site and the hardware enforces it, matmul/s
tracking the TPC count essentially to the digit:

| TPCs | 13 | 40 | 54 |
| --- | --- | --- | --- |
| 610.43.02 | 502 | 1244 | 1550 |
| **535.183.06** | **502.6** | **1251.6** | **1551.7** |

So the two drivers are indistinguishable on this GPU for every lever that
matters: the spatial partition works on both, and every temporal lever is inert
on both.

### What this means for "validating Ghost on this architecture"

The exercise set out to reproduce Ghost's elastic temporal sharing on an A100.
On **two** driver generations now — the deployed 610.43.02 and the paper-era
535.183.06 — the temporal primitives a driver-level broker would drive
(per-TSG timeslice, runlist interleave, TSG detach) are **accepted at kernel
privilege and ignored by GSP** for a doorbell-submission workload. The only
control that isolates compute is the **spatial TPC partition**, and that is a
hard rate-ceiling that time-slices rather than running tenants concurrently
(see the partition sections above), not the work-conserving elastic scheduler
Ghost describes.

Two honest limits on this conclusion:

- **The paper's exact driver is unconfirmed.** 535.183.06 is the most likely
  A100 open-module driver of that era and the nearest nvproxy-supported branch,
  but it is an inference, not the paper's stated version (which could not be
  located). What is now firm is that the temporal levers are inert across a
  *span* of driver generations (535 → 610), so a single intervening version
  reviving them is unlikely but not excluded.
- **These are the levers *reachable from the open kernel modules*.** Ghost may
  drive the GSP scheduler through an interface these hooks do not exercise —
  a different RM control, a GSP RPC issued from a component we do not patch, or
  the cooperative green-context/per-TMD path that lives below the ioctl boundary
  and so cannot be imposed on an adversarial tenant anyway. What is measured is
  narrow and true: *these* controls, at kernel privilege, do not divide a
  doorbell workload on this GA100 under either driver.

**Resolved by the paper (see the CORRECTION at the top).** The second bullet
above turned out to be the answer: Ghost *does* drive the GSP scheduler through
an interface these hooks did not exercise — not a different die or firmware, but
**runlist manipulation** (`DetachTSG`/`AttachTSG` and `SetTimeslice` applied to
the active runlist), on the same GSP, on open modules 575.57.08, for
mutually-untrusted tenants. Our `0b`/`0g`/`0h` probes drove
`GPFIFO_SCHEDULE(disable)` (idle-only), `SET_TIMESLICE` on the TSG object
without resubmitting the runlist, and `SET_INTERLEAVE_LEVEL` — adjacent
controls, not the runlist rewrite. So "the temporal levers are inert across
535→610" describes *our probes*, not the firmware. The reproduction with the
real primitives is in progress on 575.57.08.

Until that returns a verdict, the safe operational guidance still holds — on
this hardware, **MIG** or the **spatial TPC partition** are the *measured*
options for mutually-untrusting compute — but "do not expect a temporal broker
to bind an adversarial doorbell workload" is now **an open question, not a
finding**: Ghost claims exactly such a broker, and we have not yet tested its
actual mechanism.

## Per-GPU-class expectation

| die class | examples | MIG | TPC table | detach/timeslice/interleave | practical answer |
| --- | --- | --- | --- | --- | --- |
| **consumer** | RTX 5070 (GB205), 3090 (GA102) | no | **re-open — the probe that said no asked from the wrong call site** | measured ineffective (GB205) | unknown again; re-probe |
| **pro/workstation** | **RTX A6000 (GA102)**, RTX 6000 Ada (AD102), **RTX 6000 Pro Blackwell (GB202)** | usually no† | **UNKNOWN — test it** | **UNKNOWN — test it** | **the open question** |
| **datacenter** | **A100 (GA100) — measured**, H100 (GH100), GB200 | yes | **`NV_OK` and enforced** | **inert** (all three, at kernel priv) | MIG, or an imposed TPC partition as a capped/proportional dial |

† MIG has historically been datacenter-die-only, but NVIDIA has extended some
partitioning to newer pro/server Blackwell parts — **do not assume; check.**

The earlier footnote here said we had no positive measurement of
`SET_TPC_PARTITION_TABLE` on any GPU and warned that even a DC part might only
accept it in MIG mode. **We now have one, and it is not MIG-internal**: the
A100 honored the table with MIG disabled, on an ordinary CUDA context, and the
throughput moved with the TPC count. The other half of that footnote still
holds and matters more than it did: none of nvsplit/Ghost/LithOS use this
control, and none of the *temporal* controls Ghost is assumed to rely on did
anything here.

## Playbook: determining compute-isolation capability on a new GPU

Run these in order on each new node. Each step is self-contained; the whole thing
takes ~30 min per GPU.

### Step 1 — MIG capability (cheapest signal)

```
nvidia-smi -q | grep -i "MIG"           # "MIG Mode ... Enabled/Disabled/N/A"
nvidia-smi mig -lgip 2>&1                # lists GPU instance profiles if MIG-capable
```
If MIG is supported, that alone gives hardware compute+memory isolation (fixed
partitions) — use it directly; the rest is only needed for finer/elastic sharing.

### Step 2 — Green-context isolation baseline (cooperative, always informative)

Confirms the WDU spatial mechanism works at all on this GPU (it did on the 5070).
Run a CUDA program that splits the SMs with `cuDevSmResourceSplitByCount` /
`cuGreenCtxCreate`, launch a compute-bound kernel on a half, and check it gets
~half throughput and is unaffected by a peer on the disjoint half. (Reference
result on the 5070: 0.59× solo, 0.99× under a peer.) This tells you the ceiling a
*cooperative* tier could reach even if imposition fails.

### Step 3 — The imposition tests (the decisive ones)

These require the **open** kernel driver with the experiment hooks, because they
must originate admin-gated controls at kernel privilege.
`ghost-experiment/driver-hooks.patch` carries all of them:

- `src/nvidia/src/kernel/gpu/fifo/kernel_ctxshare.c` — `kctxshareapiConstruct_IMPL`
  runs the *early* probe (0c/0d) and records the ctxshare;
  `ghostReprobeDeferred_GHOST()` re-issues `SET_TPC_PARTITION_MODE(STATIC)` +
  `SET_TPC_PARTITION_TABLE` (and optionally `SET_CWD_WATERMARK`) later.
- `src/nvidia/src/kernel/gpu/fifo/kernel_channel_group_api.c` —
  `kchangrpapiCtrlCmdGpFifoSchedule_IMPL`: the 0b force-disable, the 0g
  timeslice, the 0h interleave level, and the call into the deferred re-probe.

**Issue every control from the deferred site.** Inside the ctxshare's own
constructor a ROUTE_TO_PHYSICAL control cannot resolve on GSP and returns
`0x57` OBJECT_NOT_FOUND — which looks like a verdict and is not one. This cost
us a wrong conclusion on GB205; it is the single most important thing in this
playbook.

Everything is a runtime knob, so one build sweeps every case:

```
GhostTpcCount=N     TPCs granted to the first tenant (0 = leave the table alone)
GhostTpcCountB=N    TPCs for every later tenant (0 = same as GhostTpcCount)
GhostDisjoint=1     lay each tenant's range after the previous one's
GhostWatermark=1    also clamp the CWD watermark to MIN
GhostDetach=1       0b: force-disable the TSG right after the tenant enables it
GhostTimeslice=us   0g: per-TSG timeslice for tenant 0 (GhostTimesliceB for later)
GhostInterleave=L   0h: runlist level 0/1/2 for tenant 0 (255 = don't touch)
```

Build, load, and test:

```
# build (matching the installed GSP firmware version — check out the tag that
# matches /lib/firmware/nvidia/<ver>/):
cd open-gpu-kernel-modules && make modules -j$(nproc)

# swap proprietary -> open. On a bare GPU box (no k8s/docker/display) this is
# just rmmod + insmod; on sensai use ghost-experiment/swap.sh, which also
# quiesces k3s, GPU services and gdm and kills leftover GPU-holding containers.
# Either way it is insmod, not modules_install, so a reboot reverts.
sudo rmmod nvidia_uvm nvidia
sudo insmod kernel-open/nvidia.ko NVreg_OpenRmEnableUnsupportedGpus=1 \
     NVreg_RegistryDwords="GhostTpcCount=27;GhostDisjoint=1"
sudo insmod kernel-open/nvidia-uvm.ko

# run a saturating doorbell workload and read the GHOST log:
python3 ghost-experiment/burn.py          # or kubectl apply -f burn.yaml
sudo dmesg | grep GHOST                   # NvStatus of each originated control
```

Judge by **throughput against a solo baseline at the same setting**, never by
the status code alone: on the A100 the CWD watermark returns `NV_OK`, reads
back, and changes nothing.

Minimal burn workload (self-contained; the doorbell/graph-replay case that
defeats every temporal lever):

```python
import torch, time
n=4096
a=torch.randn(n,n,device="cuda",dtype=torch.float16)
b=torch.randn(n,n,device="cuda",dtype=torch.float16)
for _ in range(50): c=a@b
torch.cuda.synchronize()
cnt=0; t0=time.time()
while True:
    c=a@b; cnt+=1
    if cnt%500==0:
        torch.cuda.synchronize(); dt=time.time()-t0
        print("rate=%.1f matmul/s"%(500/dt), flush=True); cnt=0; t0=time.time()
```

### Interpretation

| observed | meaning |
| --- | --- |
| `SET_TPC_PARTITION_TABLE = 0x0` **and** burn rate drops to the TPC fraction | **imposed spatial partition works** (the A100 result) — build the per-sandbox TPC broker |
| `... = 0x0` but rate unchanged | control accepted but not enforced — check `GET_TPC_PARTITION_MODE` reads back `mode=1` (STATIC); if it does, the die is storing it and ignoring it |
| `... = 0x57` (**OBJECT_NOT_FOUND**, not "not supported") | you called it before GSP knew the object — move to the deferred call site and ask again |
| `... = 0x56` (NOT_SUPPORTED) | die/firmware genuinely doesn't implement it — dead on this GPU |
| `... = 0x1b` (INSUFFICIENT_PERMISSIONS) | you're not at kernel priv — the origination path is wrong |
| 0b/0g/0h: `NV_OK` but rate unchanged | **the control we drove** is inert — but 0b/0g are GPFIFO_SCHEDULE-disable and SET_TIMESLICE-without-runlist-resubmit, **not** Ghost's DetachTSG/AttachTSG + SetTimeslice-via-active-runlist. A no-division here means *our probe* did nothing, not that the runlist lever cannot work (see CORRECTION at the top) |
| 0b detach: burn stalls / no matmuls | **detach sticks** — Ghost-style temporal scheduling is viable; port `pkg/gpusched` into the driver |

Then run the pair test, which is what actually decides the design: two tenants
on **disjoint** partitions against two on the **same** partition. If they
differ, that GPU runs partitioned contexts concurrently. On the A100 they did
not differ at all.

## What the working control does and does not license

On the A100 the spatial half is now live and the temporal half is dead, which
is the opposite of what `GHOST-PLAN.md` was written to expect:

- **Spatial (works on GA100).** Impose a per-sandbox TPC partition from the
  driver at ctxshare creation, weight → TPC count, laid out disjointly. nvproxy's
  `pkg/sentry/devices/nvproxy/smpart_unsafe.go` is the Sentry-side scaffolding
  (ABI + origination), blocked by privilege there; the driver hook is the
  kernel-privileged version and is what the broker should originate. Note what
  it gives: a **hard ceiling and an approximate proportional dial**, not
  concurrency and not work conservation — an idle tenant's TPCs stay idle, and
  every tenant still pays the context time-slice.
- **Temporal (dead on both dies tested).** Detach, timeslice and interleave are
  accepted and ignored by GSP firmware at kernel privilege. A driver-resident
  weighted-credit round-robin — `pkg/gpusched`'s policy, which Ghost
  independently arrived at — has no primitive to stand on here. Before building
  it on any GPU, run 0b/0g/0h and require a *throughput* change.
- **The honest combination on an A100**, if fixed shares are acceptable: MIG for
  hard compute+memory isolation, nvproxy's memory quota inside each instance.
  The TPC table is the tool for shares MIG cannot express.

## Artifacts and pointers

- **This investigation's record**: `SECURITY-FINDINGS.md` (the full red-team +
  every lever, with measurements), `GHOST-PLAN.md` (the driver-broker design +
  Phase 0 results).
- **Sentry-side scaffolding** (off by default, documents each lever's ceiling):
  `pkg/sentry/devices/nvproxy/smpart_unsafe.go` (TPC mode/table),
  `pkg/sentry/devices/nvproxy/computegate_unsafe.go` (timeslice/interleave/
  preempt/schedule origination), `pkg/gpusched/` (the weighted scheduler).
- **Runtime GPU-family detection**: `pkg/sentry/devices/nvproxy/gpuarch.go`
  (`archFromClass`, `submitsByDoorbell`) — behavior differs by generation, so any
  enforcement must be gated on the detected die/family, not the driver version.
- **Driver-level experiment hooks**: `ghost-experiment/driver-hooks.patch`
  (apply to the open modules; search `GHOST` in the two fifo files).
- **Host swap/reload/revert + reproducers**: `ghost-experiment/swap.sh`,
  `reload.sh`, `revert.sh` (sensai-tuned), `burn.py`, and on the A100 VM
  `~/ghost-a100/run-tenants.sh` / `run-staggered.sh` plus every raw run this
  document cites.

## The honest bottom line

Robust *compute* isolation for arbitrary CUDA on NVIDIA is a property of the
**GPU die class**, not of gVisor, privilege, or cleverness — and on the
datacenter die it is a *spatial* property, not the temporal one everyone
assumes. On the A100 an imposed TPC partition is real, hardware-enforced, and
reachable only from the driver; every runlist-scheduling lever is accepted and
ignored, exactly as on consumer Blackwell. What that buys is a hard ceiling and
a rough proportional dial at ~40% aggregate cost, because separate contexts
still time-slice — not the concurrent, work-conserving sharing AMD's CU masks
give. MIG remains the stronger answer where its granularity fits.

For untrusting tenants on hardware without MIG or a working TPC table, the
honest options are unchanged: memory-quota isolation (works everywhere), a
cooperative green-context tier (semi-trusted only), or capping concurrent GPU
tenants. **And the consumer/pro dies deserve a second look**: the probe that
declared the spatial control absent on GB205 asked from a call site where the
A100 gives the same answer while supporting the feature.
