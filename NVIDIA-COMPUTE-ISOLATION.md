# NVIDIA GPU compute isolation under gVisor: findings and a per-GPU test playbook

What can and cannot enforce a *compute* share between mutually-untrusting
sandboxes sharing one NVIDIA GPU, why, and **how to determine it on a new GPU** —
because the answer is decided by the GPU die class and its GSP firmware, and we
are about to test datacenter/pro parts (RTX A6000, RTX 6000 Pro Blackwell) that
may behave differently from the consumer RTX 5070 everything below was measured
on.

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
- On the **consumer RTX 5070 (Blackwell GB205)**, *no* Sentry- or even
  driver-reachable mechanism enforces a compute share against an arbitrary
  (doorbell-driven) CUDA workload. Every temporal lever is ineffective on
  doorbell submission; every spatial control is `NV_ERR_NOT_SUPPORTED`.
- **Privilege was not the wall.** We swapped to the open kernel driver and issued
  the admin-gated controls at kernel privilege — the `0x1b`
  INSUFFICIENT_PERMISSIONS disappeared, and underneath it the spatial controls
  are simply **NOT_SUPPORTED on this die**. They are SMC/MIG-class features, and
  consumer/most-pro dies do not implement them; the GSP firmware is signed and
  cannot be changed.
- The only thing that isolates compute on the 5070 is a **green context**
  (measured: 24/48 SMs → 0.59× solo, 0.99× under a disjoint-half peer), but it is
  applied per-kernel via the TMD in userspace CUDA — **cooperative**, below the
  ioctl boundary, so it cannot be imposed on a hostile tenant.
- **The determining factor is the GPU die class.** Datacenter dies (GA100,
  GH100, GB100/GB200) support MIG and honor the partition/scheduling primitives;
  consumer/pro dies (GA102, AD102, GB202/GB205) generally do not. The RTX A6000
  and RTX 6000 Pro Blackwell are pro cards on consumer-class dies — **whether
  they enable these controls is exactly what the playbook below determines.**

## The mechanisms, and what each requires

| mechanism | layer | enforced by | requires | RTX 5070 result |
| --- | --- | --- | --- | --- |
| **Compute gate** (revoke command-buffer mappings so the next submit faults) | Sentry | fault in the Sentry | the workload rewrites a gated buffer per submit | **fails for doorbell** — cuBLAS/graph replay faults ~0/period; enforces only frequently-faulting kernel loops |
| **Scheduler window / `pkg/gpusched`** (weighted, work-conserving) | Sentry | the compute gate | the gate to be able to enforce | correct windows; **no enforcement** for doorbell (the gate can't) |
| **Static TSG timeslice** (`NVA06C_CTRL_CMD_SET_TIMESLICE`) | Sentry/driver | GSP runlist | GSP to weight engine time by per-TSG timeslice | **inert** — 16:1 ratio → 1:1 division; value round-trips but GSP ignores it |
| **TSG detach** (`GPFIFO_SCHEDULE(disable)`) | Sentry/driver | GSP runlist | GSP to evict a running channel | returns `NV_OK` but **ineffective** — doorbell workload runs at full rate; the disable only takes effect when a channel idles, which a saturating workload never does |
| **Interleave level** (`SET_INTERLEAVE_LEVEL`, LOW/MED/HIGH priority) | Sentry | GSP runlist | admin privilege + GSP to honor it | **admin-gated** (`0x1b`) from the Sentry; efficacy untested (couldn't set it) |
| **Static TPC partition table** (`NV9067_CTRL_CMD_SET_TPC_PARTITION_TABLE`) | Sentry/driver | GPU hardware (WDU) | the SMC/TPC-partition HW/firmware | admin-gated (`0x1b`) from userspace; **`NV_ERR_NOT_SUPPORTED` at kernel priv** on GB205 |
| **CWD watermark** (`NV9067_CTRL_CMD_SET_CWD_WATERMARK`, per-subcontext CUDA Work Distributor throttle) | Sentry/driver | GPU hardware (WDU) | the WDU credit HW/firmware | **`NV_ERR_NOT_SUPPORTED` at kernel priv** on GB205 |
| **Green context** (`cuGreenCtxCreate` + per-kernel TMD TPC mask) | userspace CUDA | GPU hardware (WDU) | the workload to opt in | **works and isolates**, but cooperative — the mask is in the TMD, written by userspace, below the ioctl boundary; a hostile tenant just doesn't use it |
| **MIG** | firmware/HW | hardware partition | a MIG-capable datacenter die | **absent** on GB205 |

Two structural facts underpin the table:

1. **On GSP GPUs the CPU driver does not run the scheduler.** `GpFifoSchedule`,
   `SetTimeslice`, etc. just RPC to GSP firmware; `kfifoRunlistSubmit` is the
   not-supported stub on GSP parts. So "being in the driver" is **not** a more
   forceful path to the scheduler than the userspace RM control — both end at the
   same signed GSP firmware. This is true on A100 too (the open driver requires
   GSP for every GPU), so it is not a Blackwell-specific quirk.
2. **Privilege and support are separate gates, and privilege fires first.** From
   userspace the spatial controls return `INSUFFICIENT_PERMISSIONS` (`0x1b`);
   from inside the driver at `RS_PRIV_LEVEL_KERNEL` they get past that and return
   `NOT_SUPPORTED` (`0x57`). The privilege wall is real but secondary; the real
   wall is die/firmware feature support.

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
  `SET_TPC_PARTITION_TABLE = 0x57` (NOT_SUPPORTED). **Privilege bypassed;
  feature absent.**
- **0d — CWD watermark**: same hook, `SET_CWD_WATERMARK(1) = 0x57`
  (NOT_SUPPORTED). **Feature absent.**

Conclusion: the broker does exactly what it should (unlocks kernel privilege),
and thereby proves the RTX 5070's GSP firmware implements none of these
primitives. Ghost ("Breaking the Tradeoff", OSDI-class, tested on A100) works for
the same reason this fails — datacenter GSP firmware honors detach/timeslice and
supports SMC partitioning, on the same die class where MIG exists.

## Per-GPU-class expectation (and why the new nodes matter)

| die class | examples | MIG | TPC table / CWD | detach/timeslice | practical answer |
| --- | --- | --- | --- | --- | --- |
| **consumer** | RTX 5070 (GB205), 3090 (GA102) | no | measured NOT_SUPPORTED (GB205) | ineffective (GB205) | no imposable compute isolation |
| **pro/workstation** | **RTX A6000 (GA102)**, RTX 6000 Ada (AD102), **RTX 6000 Pro Blackwell (GB202)** | usually no† | **UNKNOWN — test it** | **UNKNOWN — test it** | **the open question** |
| **datacenter** | A100 (GA100), H100 (GH100), GB200 | yes | **UNVERIFIED**‡ | honored (Ghost, measured on A100) | MIG, or Ghost-style temporal broker |

† MIG has historically been datacenter-die-only, but NVIDIA has extended some
partitioning to newer pro/server Blackwell parts — **do not assume; check.**

‡ **We have NO positive measurement of `SET_TPC_PARTITION_TABLE` (or
`SET_CWD_WATERMARK`) returning `NV_OK` on ANY GPU** — the only data point is
`NOT_SUPPORTED` on consumer GB205. The belief that DC dies support it is an
*inference* (SMC-associated control + DC dies have MIG/SMC), and it is weak:
**none of nvsplit/Ghost/LithOS use this control** — they all do spatial
partitioning via the per-kernel TMD mask + MPS's WDU credits (a different path,
below the ioctl boundary), and Ghost explicitly does no spatial SM sharing.
Plausible failure modes even on a DC GPU: the control may be **MIG-internal only**
(the interface RM uses to lay out TPCs inside a MIG compute instance, not a
general impose-on-any-context control — i.e. `NV_OK` only in MIG mode, no finer
than MIG), or a **legacy Volta/Pascal** mechanism since gated. Treat a positive
`0c` result as a pleasant surprise, not the expected outcome. The evidenced DC
options are **MIG** and **Ghost's temporal path** (measured on A100); the
imposed-spatial-TPC path is speculative until the Step-3 probe says otherwise.

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
must originate admin-gated controls at kernel privilege. The hooks are already in
the driver source used on sensai:

- `src/nvidia/src/kernel/gpu/fifo/kernel_ctxshare.c` — `kctxshareapiConstruct_IMPL`:
  originates `SET_TPC_PARTITION_MODE(STATIC)` + `SET_TPC_PARTITION_TABLE` (0c) or
  `SET_CWD_WATERMARK` (0d) at kernel priv, logging the NvStatus of each. Toggle
  which via the `if (...)` guard.
- `src/nvidia/src/kernel/gpu/fifo/kernel_channel_group_api.c` —
  `kchangrpapiCtrlCmdGpFifoSchedule_IMPL`: the 0b force-disable (`bGhostDetach`).

Build, load, and test:

```
# build (matching the installed GSP firmware version — check out the tag that
# matches /lib/firmware/nvidia/<ver>/):
cd open-gpu-kernel-modules && make modules -j$(nproc)

# swap proprietary -> open (see ~/.claude/jobs/*/tmp/ghost-{swap,reload,revert}.sh
# on sensai for the exact sequence: quiesce k3s + GPU services + gdm, kill
# leftover GPU-holding containers, rmmod, insmod, restart). Reversible by reboot
# (insmod, not modules_install).

# run a saturating doorbell workload and read the GHOST log:
kubectl apply -f burn.yaml     # torch fp16 4096^3 matmul loop, prints matmul/s
sudo dmesg | grep GHOST        # NvStatus of each originated control
```

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
| `SET_TPC_PARTITION_TABLE = 0x0` **and** burn rate drops to the TPC fraction | **imposed spatial partition works** — build the per-sandbox TPC broker (this is the goal) |
| `... = 0x0` but rate unchanged | control accepted but not enforced — investigate mode/setup |
| `... = 0x57` (NOT_SUPPORTED) | die/firmware doesn't implement it (like GB205) — dead on this GPU |
| `... = 0x1b` (INSUFFICIENT_PERMISSIONS) | you're not at kernel priv — the origination path is wrong |
| 0b detach: `disable = 0x0` but rate unchanged | detach doesn't stick (temporal dead, like GB205) |
| 0b detach: burn stalls / no matmuls | **detach sticks** — Ghost-style temporal scheduling is viable; port `pkg/gpusched` into the driver |

## If a control *does* work on the pro/DC GPUs

Then the design in `GHOST-PLAN.md` becomes live on that hardware:

- **Spatial** (TPC table works): impose disjoint per-sandbox TPC partitions from
  the driver at ctxshare creation, weight → TPC count. nvproxy's
  `pkg/sentry/devices/nvproxy/smpart_unsafe.go` is the Sentry-side scaffolding
  (ABI + origination), blocked only by privilege there; the driver finishes it.
- **Temporal** (detach sticks): a driver-resident weighted-credit round-robin
  using detach/attach + timeslice-preempt — this *is* `pkg/gpusched`'s policy,
  which Ghost independently arrived at. See `GHOST-PLAN.md` for the staged build.

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
- **Driver-level experiment hooks**: in the open-gpu-kernel-modules clone, files
  listed in Step 3 above (search `GHOST`).
- **Host swap/reload/revert + reproducers**: `ghost-{swap,reload,revert}.sh` and
  `burn.sh` (recreate from the snippet above on a new node).

## The honest bottom line

Robust *compute* isolation for arbitrary CUDA on NVIDIA is a property of the
**GPU die class**, not of gVisor, privilege, or cleverness. On consumer Blackwell
it is unattainable by any means we can reach (measured exhaustively). On
datacenter silicon it is available via MIG, and a Ghost-style driver broker can
add elastic sharing on top. **The pro cards (A6000, RTX 6000 Pro Blackwell) are
the unknown worth resolving** — and Step 3 above resolves it in one afternoon per
GPU. Until then, for untrusting tenants on non-MIG hardware, the honest options
are: memory-quota isolation (works), a cooperative green-context tier
(semi-trusted only), or capping concurrent GPU tenants.
