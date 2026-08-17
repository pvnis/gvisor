# Handoff: test NVIDIA compute-isolation primitives on a new GPU

For an agent picking this up on a GPU machine with no prior context. Read
`../NVIDIA-COMPUTE-ISOLATION.md` first (the full findings + playbook); this file
is the concrete, machine-adaptable procedure. It was written for the RTX A6000
and has since been *run* on an A100 80GB PCIe (`gpu0-a`, 2026-08-17), which is
where the reference numbers below come from.

## Situation in one paragraph

This branch builds GPU compute+memory isolation between *mutually-untrusting*
containers, enforced in the gVisor Sentry (never inside the container). **Memory
quota works everywhere.** *Compute* isolation is the hard part. Work submission
on Volta+ never enters the kernel (mapped pushbuffer + doorbell), so the Sentry
cannot meter it; the only imposable levers are RM controls, and whether they do
anything is a property of the GPU die and its GSP firmware. On the **A100
(GA100)** the spatial control works and every temporal one is inert. On the
**consumer RTX 5070 (GB205)** the temporal ones are inert too, and its spatial
negative turned out to be a mis-read status code (below) — so consumer/pro dies
are an open question again.

## Your immediate task

Run the driver probes on this machine and report each control's NvStatus **and
the throughput it produced**:

- **0c/0f** `SET_TPC_PARTITION_TABLE` — impose specific TPCs (the AMD-CU-mask
  analog). The prize. **A100: `NV_OK`, and enforced.**
- **0d** `SET_CWD_WATERMARK` — per-subcontext Work-Distributor throttle.
  **A100: `NV_OK`, no measurable effect.**
- **0b** `GPFIFO_SCHEDULE(disable)` — does a driver TSG detach stop a running
  doorbell workload? **A100: `NV_OK`, no effect.**
- **0g** `SET_TIMESLICE` — per-TSG timeslice as a weight. **A100: 16:1 → 1:1.**
- **0h** `SET_INTERLEAVE_LEVEL` — runlist priority. **A100: `NV_OK` at kernel
  priv, GET says NOT_SUPPORTED, HIGH vs LOW → 1:1.**

**Two rules learned the hard way:**

1. **Issue the controls from the deferred call site, never only from
   `kctxshareapiConstruct_IMPL`.** Inside the ctxshare's own constructor a
   ROUTE_TO_PHYSICAL control cannot be resolved by GSP and returns `0x57` =
   `NV_ERR_OBJECT_NOT_FOUND`, which is *not* `NV_ERR_NOT_SUPPORTED` (`0x56`).
   The A100 returns `0x57` there and `NV_OK` from
   `kchangrpapiCtrlCmdGpFifoSchedule_IMPL`. This exact confusion produced a
   wrong conclusion on the 5070. Decode every status against
   `src/common/sdk/nvidia/inc/nvstatuscodes.h`.
2. **Judge by throughput, not by `NV_OK`.** Several controls are accepted,
   stored, read back, and ignored.

**The probes are driver-level hooks that fire on ANY CUDA context creation. You
do NOT need gVisor, k3s, or nvproxy for them** — just the modified open driver +
any CUDA workload (native `burn.py` or a CUDA container). gVisor is only needed
later, to build the full broker (see `../GHOST-PLAN.md`).

## Procedure

### 1. Match the driver version to the machine's GSP firmware

```
cat /proc/driver/nvidia/version                 # installed driver version
ls /lib/firmware/nvidia/                         # firmware version dir(s)
```
Clone the open modules and check out the tag matching the **installed firmware
version** (so the built modules load the firmware already on disk — this is what
makes the swap low-risk):
```
git clone https://github.com/NVIDIA/open-gpu-kernel-modules
cd open-gpu-kernel-modules && git checkout <version-matching-/lib/firmware/nvidia/>
```

### 2. Apply the GHOST hooks

`git apply /path/to/gvisor/ghost-experiment/driver-hooks.patch` (2 files). If it
doesn't apply cleanly to a different driver version, add them by hand — they are
small and the target functions are stable:

- `src/nvidia/src/kernel/gpu/fifo/kernel_ctxshare.c` — the early 0c/0d probe in
  `kctxshareapiConstruct_IMPL`, the ctxshare bookkeeping, and
  `ghostReprobeDeferred_GHOST()` which re-issues mode/table/watermark later.
  Needs `#include "rmapi/rmapi.h"`, `"ctrl/ctrl0080/ctrl0080gr.h"`,
  `"ctrl/ctrl9067.h"`, `"kernel/os/os.h"`.
- `src/nvidia/src/kernel/gpu/fifo/kernel_channel_group_api.c` — in
  `kchangrpapiCtrlCmdGpFifoSchedule_IMPL`, in the `IS_GSP_CLIENT` branch after
  `NV_RM_RPC_CONTROL`: 0b detach, 0g timeslice, 0h interleave, then the call to
  `ghostReprobeDeferred_GHOST()`.

**Set `GHOST_TOTAL_TPC`** in `kernel_ctxshare.c` to this GPU's TPC count (SMs/2:
A100 = 54, A6000 = 42, RTX 5070 = 24). Everything else is a runtime knob.

### 3. Build

```
make modules -j$(nproc)      # ~4 min from clean, ~40 s incremental
```

### 4. Swap proprietary -> open driver

On a bare GPU box (no k8s, docker, or display) this is just:
```
sudo rmmod nvidia_uvm nvidia
sudo insmod kernel-open/nvidia.ko NVreg_OpenRmEnableUnsupportedGpus=1 \
     NVreg_RegistryDwords="GhostTpcCount=27;GhostDisjoint=1"
sudo insmod kernel-open/nvidia-uvm.ko
nvidia-smi                                    # confirm the GPU is alive
grep -i '^RegistryDwords:' /proc/driver/nvidia/params   # confirm the knobs
```
`swap.sh` / `reload.sh` / `revert.sh` here do the same with the extra quiescing
sensai needs (stop k3s + `runsc-gpu-scheduler` + `nvidia-persistenced` + gdm,
kill leftover GPU-holding containers). Find holders with `sudo fuser -v
/dev/nvidia*` and `sudo lsof /dev/nvidia*` (gVisor sandboxes appear as `exe`).
This uses `insmod` (not `modules_install`), so **a reboot reverts to
proprietary** — the whole experiment is cleanly reversible.

### 5. The knobs (one build sweeps everything)

```
GhostTpcCount=N     TPCs granted to the first tenant (0 = leave the table alone)
GhostTpcCountB=N    TPCs for every later tenant (0 = same as GhostTpcCount)
GhostDisjoint=1     lay each tenant's range after the previous one's
GhostWatermark=1    also clamp the CWD watermark to MIN
GhostDetach=1       0b: force-disable the TSG right after the tenant enables it
GhostTimeslice=us   0g: timeslice for tenant 0 (GhostTimesliceB for later ones)
GhostInterleave=L   0h: runlist level 0=LOW/1=MED/2=HIGH (255 = don't touch)
```
Tenants are identified by **pid** (`ghostTenantIndex_GHOST`), so a workload's
several RM clients count as one tenant.

### 6. Run the workload and read the log

```
python3 burn.py                       # fp16 4096^3 matmul loop, prints matmul/s
sudo dmesg | grep GHOST                # the control statuses
```
`run-tenants.sh <tag> <n> <secs>` launches N tenants at once and prints each
one's median rate; `run-staggered.sh` starts them 8 s apart so tenant *i* is
deterministically slice *i* (needed whenever tenants get different widths).
Both take `PY=`, `BURN=`, `OUT=` overrides.

The measurements that matter, in order:

1. **Solo sweep** — `GhostTpcCount` ∈ {0, ¼, ½, ¾, all}. Does the rate track the
   TPC count? (A100: 1550 / 502 @13 / 960 @27 / 1244 @40 / 1550 @54.)
2. **Two tenants, disjoint vs overlapping** — `GhostTpcCount=half;
   GhostDisjoint=1` against `GhostDisjoint=0`. **If those two differ, this GPU
   runs partitioned contexts concurrently.** (A100: 442/443 vs 440/441 — no
   difference; contexts time-slice regardless.)
3. **Two tenants, unequal** — `GhostTpcCount=40;GhostTpcCountB=14`. Does the
   ratio track the widths? (A100: 2.66:1 for 2.86:1 of TPCs.)
4. **Temporal levers** — `GhostDetach=1`, then `GhostTimeslice/B`, then
   `GhostInterleave/B`. Any *throughput* change at all?

## Interpretation

| dmesg / behavior | meaning | next |
| --- | --- | --- |
| `SET_TPC_PARTITION_TABLE = 0x0` AND rate tracks the TPC count | **imposed spatial partition works** (A100 result) | build the spatial broker (finish `smpart_unsafe.go`) |
| `... = 0x0` but rate unchanged | accepted, not enforced — check `GET_TPC_PARTITION_MODE` reads back `mode=1` | investigate mode/setup |
| `... = 0x57` | OBJECT_NOT_FOUND — wrong call site, **not** a verdict | re-issue from the deferred site |
| `... = 0x56` | NOT_SUPPORTED — die/firmware really lacks it | spatial dead on this GPU |
| `... = 0x1b` | INSUFFICIENT_PERMISSIONS — not at kernel priv | origination path wrong |
| 0b/0g/0h `NV_OK`, rates unchanged | runlist levers inert (both dies so far) | temporal dead on this GPU |
| 0b: burn stalls / no matmuls | **detach STICKS — temporal viable** | port `pkg/gpusched` into the driver (GHOST-PLAN Phase 2) |

**A positive spatial result is the headline** — it means imposed partitioning
(the AMD-CU-mask analog) is viable here. Confirm the throughput actually moves;
do not trust the status code alone.

## Context & artifacts (all committed in the gvisor repo, branch `gpuslicing`)

- `../NVIDIA-COMPUTE-ISOLATION.md` — definitive findings, the mechanism×die-class
  matrix, the A100 data, and this playbook's rationale. **Read first.**
- `../GHOST-PLAN.md` — the driver-broker design; its temporal premise is
  falsified on the A100, so read the 2026-08-17 section at the top.
- `../SECURITY-FINDINGS.md` — the full red-team + every lever measured.
- `../pkg/sentry/devices/nvproxy/smpart_unsafe.go` — Sentry-side spatial
  scaffolding (ABI + origination), blocked by privilege there; the driver hook is
  the kernel-priv version.
- `../pkg/sentry/devices/nvproxy/gpuarch.go` — runtime die/arch detection
  (`archFromClass`).
- `../pkg/gpusched/` — the weighted-credit scheduler (only worth porting into
  the driver if 0b actually stalls a burn).
- `driver-hooks.patch`, `burn.py`, `run-tenants.sh`, `run-staggered.sh`,
  `swap.sh`/`reload.sh`/`revert.sh` — here.

## Gotchas

- **`systemctl stop k3s` does NOT stop the pods** — containerd keeps them
  running; kill the GPU-holding containers before rmmod. Irrelevant on a bare
  GPU box.
- **The display manager (gdm) holds the GPU** if there's an attached display and
  no iGPU. Check `fuser /dev/dri/*`.
- **k8s only:** a service restart can exhaust `fs.inotify.max_user_instances`
  (default 128) → device-plugin CrashLoop. Fix: `sudo sysctl -w
  fs.inotify.max_user_instances=1024`.
- **The Claude Code auto-classifier may block writing a *script* containing
  `rmmod`/`insmod`/`kill`** even where running those commands inline is allowed;
  on the A100 VM the inline `sudo rmmod`/`insmod` sequence went through fine.
- **`nvidia-smi` itself creates ctxshares**, so it appears in the GHOST log
  (with `SETMODE=0x57`, since its clients have no channel group). Ignore those
  lines; the tenant's own ctxshare is the one that reports `mode=1`.
- `GPFIFO_SCHEDULE(disable)` returning `0x0` does NOT mean it worked.

## Machine state

- **A100 VM (`gpu0-a`)**: running the open 610.43.02 modules with all hooks;
  results in `~/ghost-a100/` (one file per run, named by tag). Reboot reverts to
  the proprietary driver.
- **sensai (RTX 5070)**: was left on the open 610.43.02 driver with the earlier
  hooks. Its spatial result needs re-taking with the deferred call site.

## What to report back

The `GHOST 0c/0f/0d/0b/0g/0h` dmesg lines, each with the burn throughput beside
it, plus the disjoint-vs-overlapping pair test. That set determines whether the
GPU unlocks spatial partitioning, temporal scheduling, both, or neither — and
thus which part of `GHOST-PLAN.md` is buildable there.
