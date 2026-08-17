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
- **Every temporal lever is inert on the A100 too** — and that *was* expected to
  be the datacenter difference. TSG detach, per-TSG timeslice (16:1 → measured
  1:1), and runlist interleave HIGH-vs-LOW (→ 1:1) are all accepted with
  `NV_OK` at kernel privilege and all ignored by GSP. So "datacenter firmware
  honors detach/timeslice, which is why Ghost works on A100" is **false for
  driver 610.43.02**; whatever Ghost does, it is not these controls on this
  firmware.
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
| **Static TSG timeslice** (`NVA06C_CTRL_CMD_SET_TIMESLICE`) | Sentry/driver | GSP runlist | **inert** — 16:1 ratio → 1:1 division; value round-trips but GSP ignores it | **inert** — 8000 vs 500 µs at kernel priv, both `NV_OK`, both read back; division 708.9/706.7 = 1:1 |
| **TSG detach** (`GPFIFO_SCHEDULE(disable)`) | Sentry/driver | GSP runlist | returns `NV_OK` but **ineffective** — doorbell workload runs at full rate | **ineffective** — `disable -> 0x0` on all 4 TSGs, burn keeps its full 1547 matmul/s |
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
| 0b/0g/0h: `NV_OK` but rate unchanged | the runlist lever is inert — measured on **both** GB205 and GA100, so expect this |
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
