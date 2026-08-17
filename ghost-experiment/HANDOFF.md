# Handoff: test NVIDIA compute-isolation primitives on the RTX A6000

For an agent picking this up on the A6000 machine with no prior context. Read
`../NVIDIA-COMPUTE-ISOLATION.md` first (the full findings + playbook); this file
is the concrete, machine-adaptable procedure.

## Situation in one paragraph

This branch builds GPU compute+memory isolation between *mutually-untrusting*
containers, enforced in the gVisor Sentry (never inside the container). **Memory
quota works everywhere.** *Compute* isolation is the hard part, and we proved —
exhaustively, down to a kernel-privileged driver hook — that on the **consumer
RTX 5070 (Blackwell GB205)** no mechanism can enforce a compute share against an
arbitrary (doorbell-driven) CUDA workload: temporal levers (compute gate,
timeslice, TSG detach) are ineffective, and the spatial control-plane controls
(`SET_TPC_PARTITION_TABLE`, `SET_CWD_WATERMARK`) return `NV_ERR_NOT_SUPPORTED`.
The wall was **not privilege** (we bypassed that by moving into the open driver);
it is **GPU die/firmware feature support**. Datacenter dies (A100/H100) are known
to support the primitives (Ghost paper). **The open question this handoff resolves:
does the RTX A6000 (Ampere GA102 — a *pro/workstation* die, no MIG) support them,
or does it behave like the consumer 5070?** We honestly do not know; measure it.

## Your immediate task

Run the three driver probes on the A6000 and report the NvStatus + throughput:

- **0c** `SET_TPC_PARTITION_TABLE` — impose a specific-TPC partition (AMD-CU-mask analog). The prize.
- **0d** `SET_CWD_WATERMARK` — per-subcontext Work-Distributor throttle.
- **0b** `GPFIFO_SCHEDULE(disable)` — does a driver TSG detach stop a running doorbell workload?

**Key simplification: the probes are driver-level hooks that fire on ANY CUDA
context creation. You do NOT need gVisor, k3s, or nvproxy for them** — just the
modified open driver + any CUDA workload (native `burn.py` or a CUDA container).
gVisor is only needed later, to build the full broker (see `../GHOST-PLAN.md`).

## Procedure

### 1. Match the driver version to the machine's GSP firmware

```
cat /proc/driver/nvidia/version                 # installed driver version, e.g. 570.x
ls /lib/firmware/nvidia/                         # firmware version dir(s), e.g. 570.86.15
```
Clone the open modules and check out the tag matching the **installed firmware
version** (so the built modules load the firmware already on disk — this is what
made the swap low-risk on sensai):
```
git clone https://github.com/NVIDIA/open-gpu-kernel-modules
cd open-gpu-kernel-modules && git checkout <version-matching-/lib/firmware/nvidia/>
```

### 2. Apply the GHOST hooks

`git apply /path/to/gvisor/ghost-experiment/driver-hooks.patch` (2 files, ~60
lines). If it doesn't apply cleanly to a different driver version, add them by
hand — they are small and the target functions are stable:

- `src/nvidia/src/kernel/gpu/fifo/kernel_ctxshare.c`, in
  `kctxshareapiConstruct_IMPL`, just before the `failed:` label: originate
  `SET_TPC_PARTITION_MODE(STATIC)` + `SET_TPC_PARTITION_TABLE` (0c) and
  `SET_CWD_WATERMARK` (0d) via `rmapiGetInterface(RMAPI_GPU_LOCK_INTERNAL)->Control`,
  logging each NvStatus. Needs `#include "rmapi/rmapi.h"`,
  `"ctrl/ctrl0080/ctrl0080gr.h"`, `"ctrl/ctrl9067.h"`.
- `src/nvidia/src/kernel/gpu/fifo/kernel_channel_group_api.c`, in
  `kchangrpapiCtrlCmdGpFifoSchedule_IMPL`, in the `IS_GSP_CLIENT` branch after
  `NV_RM_RPC_CONTROL`: the 0b force-disable, gated by `bGhostDetach` (default
  `NV_FALSE`).

**Adjust `GHOST_TPC_COUNT`** in the ctxshare hook to ~half the A6000's TPC count.
The A6000 (full GA102) has **84 SMs = 42 TPCs**, so use **21** for a half-partition
test (the hook ships with 12 for the 5070's 24). If 0c returns NV_OK, a ~0.5x
throughput drop is the confirmation the partition is real.

### 3. Build

```
make modules -j$(nproc)
```

### 4. Swap proprietary -> open driver

The scripts `swap.sh` / `reload.sh` / `revert.sh` here are **tuned for sensai**
(they stop k3s + `runsc-gpu-scheduler` + `nvidia-persistenced` + gdm, kill
leftover GPU-holding containers, then rmmod/insmod). **Adapt them to the A6000
machine's setup** — the essential sequence is:
```
# stop everything using the GPU (display manager, any CUDA procs/containers,
#   persistenced); find holders with: sudo fuser -v /dev/nvidia*  and
#   sudo lsof /dev/nvidia*  (gVisor sandboxes appear as 'exe')
sudo rmmod nvidia_drm nvidia_modeset nvidia_uvm nvidia_peermem nvidia
sudo insmod .../kernel-open/nvidia.ko NVreg_OpenRmEnableUnsupportedGpus=1
sudo insmod .../kernel-open/nvidia-uvm.ko
nvidia-smi   # confirm the GPU is alive on the open driver
```
This uses `insmod` (not `modules_install`), so **a reboot reverts to
proprietary** — the whole experiment is cleanly reversible.

### 5. Run a CUDA workload and read the probe results

```
python3 burn.py &          # or run any CUDA container; prints matmul/s
sudo dmesg | grep GHOST     # the control statuses
```

### 6. For the 0b temporal test (separate run)

0b force-disables the TSG, which (if it works) stalls the workload — so it can't
share a run with a throughput measurement. Enable it by setting
`bGhostDetach = NV_TRUE` in the channel-group hook, rebuild, reload, run burn.py.
If burn makes no progress / stalls, detach sticks (temporal viable). If it runs
at full rate with `disable = 0x0` in dmesg, detach is ineffective (like the 5070).

## Interpretation (per NVIDIA-COMPUTE-ISOLATION.md)

| dmesg / behavior | meaning | next |
| --- | --- | --- |
| `SET_TPC_PARTITION_TABLE = 0x0` AND rate drops to the TPC fraction | **imposed spatial partition WORKS on A6000** | build the spatial broker (finish `smpart_unsafe.go`) |
| `... = 0x0` but rate unchanged | accepted, not enforced | investigate mode/setup |
| `... = 0x57` (NOT_SUPPORTED) | die/firmware lacks it (5070 result) | spatial dead on A6000 |
| `... = 0x1b` (INSUFFICIENT_PERMISSIONS) | not at kernel priv | origination path wrong |
| 0b: `disable = 0x0`, burn stalls / no matmuls | **detach STICKS — temporal viable** | port `pkg/gpusched` into the driver (GHOST-PLAN Phase 2) |
| 0b: `disable = 0x0`, burn at full rate | detach ineffective (5070 result) | temporal dead on A6000 |

**A positive 0c is the headline result** — it means imposed spatial partitioning
(the AMD-CU-mask analog) is viable on the A6000, and the whole `GHOST-PLAN.md`
design goes live there. Caveat from `NVIDIA-COMPUTE-ISOLATION.md ‡`: even a
positive result may turn out MIG-internal-only — confirm the throughput actually
drops, don't trust the status code alone.

## Context & artifacts (all committed in the gvisor repo, branch `gpuslicing`)

- `../NVIDIA-COMPUTE-ISOLATION.md` — definitive findings + the mechanism×die-class
  matrix + this playbook's rationale. **Read first.**
- `../GHOST-PLAN.md` — the driver-broker design (what to build if a probe passes);
  Phase-2-temporal is a port of `pkg/gpusched`.
- `../SECURITY-FINDINGS.md` — the full red-team + every lever measured.
- `../pkg/sentry/devices/nvproxy/smpart_unsafe.go` — Sentry-side spatial
  scaffolding (ABI + origination), blocked by privilege there; the driver hook is
  the kernel-priv version.
- `../pkg/sentry/devices/nvproxy/gpuarch.go` — runtime die/arch detection
  (`archFromClass`); on the A6000 it will report "Ampere" (AMPERE_COMPUTE_A/B).
- `../pkg/gpusched/` — the weighted-credit scheduler (Ghost's policy, ready to
  port into the driver if temporal works).
- `driver-hooks.patch`, `burn.py`, `swap.sh`/`reload.sh`/`revert.sh` — here.

## Gotchas (learned the hard way on sensai)

- **`systemctl stop k3s` does NOT stop the pods** — containerd keeps them running;
  kill the GPU-holding containers (device-plugin, etc.) before rmmod. Not relevant
  if the A6000 box isn't running k8s.
- **The display manager (gdm) holds the GPU** if there's an attached display and
  no iGPU. Stopping it kills the desktop session. Check `fuser /dev/dri/*`.
- **k8s only:** a service restart can exhaust `fs.inotify.max_user_instances`
  (default 128) → device-plugin CrashLoop. Fix: `sudo sysctl -w
  fs.inotify.max_user_instances=1024`.
- **The Claude Code auto-classifier blocks rmmod/insmod/`systemctl stop`/`kill`
  from the agent.** Either have the human run the swap script (`! sudo bash
  swap.sh`) or add a Bash permission rule to pre-authorize.
- `GPFIFO_SCHEDULE(disable)` returning `0x0` (NV_OK) does NOT mean it worked —
  it's accepted but only takes effect when the channel idles, which a saturating
  workload never does. Judge by throughput, not the status code.

## Source-machine (sensai) state

sensai (RTX 5070) is currently on the **open 610.43.02 driver** with these hooks
loaded (an earlier build; the committed source is the consolidated version).
Revert with `revert.sh` or a reboot. sensai is now just the code source-of-truth;
the live experiment moves to the A6000.

## What to report back

The `GHOST 0c` / `GHOST 0d` dmesg lines, the `GHOST 0b` behavior, and the burn
throughput vs a solo baseline. That triple determines whether the A6000 unlocks
spatial partitioning, temporal scheduling, both, or neither — and thus which part
of `GHOST-PLAN.md` becomes buildable.
