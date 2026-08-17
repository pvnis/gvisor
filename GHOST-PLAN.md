# Implementing the key parts of Ghost in the open GPU driver

Plan for building a privileged, driver-level GPU resource broker — the thing this
branch's whole investigation concluded is *necessary* to enforce what the
deliberately-unprivileged gVisor Sentry cannot. Driver clone is at
`/home/dmd/open-gpu-kernel-modules` (open modules **610.57.04**, a minor delta
from the deployed proprietary **610.43.02**). Reference paper: Ghost, "Breaking
the Tradeoff: Elastic and Isolated GPU Sharing" (see memory `ghost-gvm`).

## PHASE 0 RESULTS (2026-08-14): both enforcement primitives are dead on consumer Blackwell

Executed on the RTX 5070 after swapping to the open 610.43.02 driver and adding
hooks that originate the controls at kernel privilege (the whole point of the
driver approach). Both negative:

- **0c (spatial, NV9067 TPC table):** the `0x1b` INSUFFICIENT_PERMISSIONS wall is
  **bypassed at kernel priv** — proven — but the table then returns
  **NV_ERR_NOT_SUPPORTED (0x57)**; `SET_TPC_PARTITION_MODE(STATIC)` is a no-op
  success. It's an SMC/MIG feature GB205's GSP firmware doesn't implement. Burn
  rate unchanged.
- **0b (temporal, TSG detach):** a driver-issued `GPFIFO_SCHEDULE(disable)`
  returns **NV_OK** but the doorbell burn keeps running at the **full 458
  matmul/s** — the detach doesn't stick, identical to userspace. The disable only
  takes effect when a channel idles, which a saturating workload never does.

- **0d (WDU/CWD credit path):** the core CWD partition/credit code isn't in the
  open driver (it's GSP firmware); the one driver-reachable ctxshare throttle,
  `SET_CWD_WATERMARK`, returns **NV_ERR_NOT_SUPPORTED (0x57)** too. Another
  SMC/datacenter feature GB205 lacks.

**Bottom line:** the driver-broker DOES unlock privilege (the one thing we set out
to prove), but consumer Blackwell's signed GSP firmware honors none of the
enforcement primitives — temporal detach/timeslice are ineffective on doorbell
workloads, and every spatial control (TPC table, CWD watermark) is NOT_SUPPORTED —
and the driver can't change GSP firmware. Ghost works on A100 because datacenter
GSP firmware honors these and supports SMC partitioning — which is also where MIG
exists. **On this consumer GPU, no driver-imposable compute isolation exists.**
The only thing that isolates (green contexts) is the per-TMD userspace/cooperative
path, below the ioctl boundary, so not imposable on an adversarial tenant. Revert
the open-driver swap when done (ghost-revert.sh / reboot). The design below stands
for a *datacenter* GPU; it does not rescue consumer Blackwell.

## Executing this on a datacenter GPU (the decided direction, 2026-08-14)

Phase 0 on the consumer RTX 5070 is a firm negative (above): the driver-broker
unlocks privilege but GB205's GSP firmware implements none of the primitives.
**The decision is to run Ghost on a datacenter-class GPU, where that firmware
support exists (Ghost was validated on A100).** Sequence:

1. **Pre-flight probe FIRST — do not build the broker until it passes.** Re-run
   the Phase 0 hooks (0b detach, 0c TPC table, 0d CWD watermark — already written
   in the open driver, files listed below) on the target card and read the
   NvStatus per `NVIDIA-COMPUTE-ISOLATION.md` Step 3. A true DC part (A100/H100/
   GB200/B200) is high-confidence; a **pro die (RTX 6000 Pro Blackwell = GB202)
   is UNKNOWN and may behave like the 5070** — probe, don't assume.
   - Green light for **temporal**: `0b` detach makes a burn pod stall.
   - Possible bonus for **spatial**: `0c` `SET_TPC_PARTITION_TABLE = NV_OK` *and*
     the burn confines to the TPC fraction. NOTE: this is **speculative** — we
     have no positive measurement of this control on any GPU, and it may be
     MIG-internal-only even on a DC part (see NVIDIA-COMPUTE-ISOLATION.md ‡). If
     it does light up it's the smaller win (finishes `smpart_unsafe.go`, the
     AMD-CU-mask analog); do not count on it.
2. **Then Phases 1-4 below** build the broker for whichever mechanism passed.
   Phase 2-temporal is a *port* of `pkg/gpusched` (Ghost converged on our exact
   weighted-credit scheduler), not a new design.

On a DC GPU the *evidenced* options are **MIG** (hard/static/zero-overhead,
shipping feature) and the **Ghost temporal broker** (elastic; measured on A100 in
the paper). The imposed spatial TPC partition is a *speculative* third option the
probe may or may not unlock. Ghost's value over MIG is elasticity +
work-conservation; choose per the tenancy needs, informed by what the probe shows
is actually live.

## The one thing to understand before building anything

**On a GSP GPU — which the RTX 5070 (GB205) is — the CPU driver does not manage
the runlist. It RPCs to GSP firmware, which is the actual scheduler.** Confirmed
in the clone: `kchangrpapiCtrlCmdGpFifoSchedule_IMPL` and the SetTimeslice path
both just `NV_RM_RPC_CONTROL(...)` to GSP (`kernel_channel_group_api.c:1177`);
`kfifoRunlistSubmit` resolves to the `_5baef9` (not-supported) stub on GSP parts,
with a real body only for `_GP102` (pre-GSP). So:

- **Ghost-in-the-driver reaches the same GSP over the same RPCs.** Moving into
  the driver is **not** a more forceful path to the scheduler, and GSP firmware
  is a signed blob we cannot modify. **But this is not a dead end, because A100
  is identical here:** the open driver Ghost used requires GSP-RM for every GPU
  including GA100, so on Ghost's A100 the CPU driver *also* RPCs the runlist to
  GSP. The A100↔Blackwell difference is not architectural — it is at most GSP
  *firmware behaviour*. Ghost's mechanism is therefore reachable on Blackwell via
  the identical interface where A100's GSP honours it.
- **Our earlier negatives did NOT test Ghost's actual mechanism.** We measured
  *static `SET_TIMESLICE` → proportional division* (inert, 16:1→1:1) — but Ghost
  does not use timeslice that way; it shrinks the timeslice to *force preemption*
  inside an active detach/attach credit scheduler. And the red-team's
  `GPFIFO_SCHEDULE(disable)` detach "did not stick" for reasons never pinned
  down. **Ghost's real primitive — actively detaching low-priority TSGs from the
  runlist — was never cleanly tested on Blackwell.** So the temporal path is
  genuinely open, on the same GSP interface where it works on A100.
- **What the driver *does* unlock is privilege.** Controls that returned
  `NV_ERR_INSUFFICIENT_PERMISSIONS` (0x1b) from the Sentry/root-helper —
  `SET_INTERLEAVE_LEVEL`, and crucially `SET_TPC_PARTITION_TABLE` — run at
  `RS_PRIV_LEVEL_KERNEL` from inside the driver, so the privilege check passes.

**Consequence for scope.** Two candidate enforcement mechanisms, both now worth
testing on Blackwell:
1. **Temporal (Ghost's core).** An active detach/attach + timeslice-preempt
   credit scheduler. Reachable via the same GSP RPCs A100 uses; never cleanly
   tested on Blackwell (our negatives tested a different thing). Genuinely open.
2. **Spatial (the driver's privilege unlock).** `SET_TPC_PARTITION_TABLE` at
   kernel priv — blocked *only* by the 0x1b gate from the Sentry, and we have
   already measured green-context SM partitioning *isolating* on this GPU (0.59×
   solo, 0.99× under a disjoint-half peer). High prior of working.

This plan implements Ghost's architecture (privileged broker + cgroup-like
control surface + per-sandbox enforcement); Phase 0 measures which mechanism
enforces on Blackwell's GSP, and we build that. Spatial has the stronger prior
(positive isolation data already), temporal has the bigger payoff if it holds
(work-conserving elastic sharing, exactly Ghost).

## How it composes with gVisor (the trust model stays intact)

Ghost trusts the host kernel + driver and treats tenants as untrusted userspace —
which is exactly our model: enforcement lives in the trusted host, nothing
depends on changes inside the container. The Sentry is **one host process per
sandbox**, so the driver naturally attributes GPU state per-sandbox = one Ghost
"container". nvproxy (in the Sentry) already intercepts every GPU ioctl, so it
replaces Ghost's in-container CUDA shim: nvproxy sets each sandbox's broker
parameters, the driver enforces. No in-container component, so the governing
constraint holds.

---

## Phase 0 — De-risk before writing a line of policy (make-or-break)

Everything hinges on what actually enforces on Blackwell's GSP. Do these first;
each is a small patch + the existing 2-tenant cuBLAS burn reproducer
(`~/.claude/jobs/*/tmp/redteam/burn*.sh`).

- **0a. Build + load the open driver on sensai, keep CUDA + nvproxy working.**
  Swap the RTX 5070 from proprietary 610.43.02 to the open 610.57.04 (brings its
  own matching GSP firmware). Re-verify: native CUDA runs; a gVisor pod runs
  (nvproxy already has `--nvproxy-allow-unsupported-driver`, update the pinned
  `--nvidia-driver-version`). Risk: open-vs-proprietary behavioural differences,
  and the KVM tail-page bug is kernel-version-gated not driver — recheck.

- **0b. Temporal enforceability probe (does detach/timeslice work from the
  driver on Blackwell?).** Add a debugfs trigger that, for a target TSG, (i)
  shrinks its timeslice and (ii) removes it from the runlist via *every*
  driver-reachable path — the `INTERNAL_GPFIFO_SCHEDULE` physical-RM control, and
  any lower-level runlist-membership RPC that is not the per-TSG schedule flag.
  Measure whether a saturating cuBLAS TSG actually stops/throttles. **If nothing
  enforces → Ghost's temporal core is not viable on this GPU; skip Phase 2-temporal.**

- **0c. Spatial enforceability probe (the high-value one).** From physical/kernel
  RM, originate `SET_TPC_PARTITION_MODE(STATIC)` + `SET_TPC_PARTITION_TABLE` on a
  target context share (the 0x1b that blocked the Sentry should not apply at
  kernel priv). Confirm it confines a workload to its TPC subset (we already know
  green contexts isolate; this tests *imposition* on an uncooperative context).
  **If yes → we have an imposed spatial partition, the AMD-CU-mask analog for
  NVIDIA, finally reachable.**

**Decision gate:** 0b and/or 0c tell us whether the broker enforces in *time*,
*space*, or not at all. Build only what enforces.

---

## Phase 1 — The gcgroup control surface (the interface)

A per-sandbox control channel the driver exposes and nvproxy drives. Mirror
Ghost's `gcgroup`: `gmem.limit`, a compute `weight`, and whichever enforcement
knob Phase 0 blessed (`compute.timeslice`/`freeze` for temporal, or `tpc.count`/
`tpc.mask` for spatial). Keyed on the calling process (the Sentry). Options,
cheapest first:
1. A **new physical-RM control** nvproxy already forwards — no new kernel surface,
   fits the existing ioctl path. Preferred.
2. **debugfs** file-per-container (Ghost's own fallback), simple, host-only.
3. Extend the Linux **cgroup** subsystem (Ghost's headline API) — most work,
   nicest for non-gVisor users; not needed for our integration.

Wire nvproxy to set these from the sandbox's existing weight/limit (the HAMi
`gpucores`→weight and `gpumem`→limit it already parses), narrow-only.

---

## Phase 2 — Compute enforcement (build the branch Phase 0 blessed)

- **2-spatial (likely the Blackwell-viable one).** Driver imposes a TPC partition
  per sandbox at context-share creation: weight → TPC count, disjoint ranges
  across co-resident sandboxes, set via `SET_TPC_PARTITION_TABLE` at kernel priv.
  This is what nvproxy's `smpart_unsafe.go` scaffolding tries and cannot (0x1b);
  the driver finishes it. Needs the TSG-count and co-tenancy bookkeeping the
  spatial partition requires (which TPCs are free), and the RDNA-style "whole
  workgroup-processor" rules if any.

- **2-temporal (only if 0b enforces).** Port our existing `pkg/gpusched`
  weighted-credit, work-conserving round-robin *into the driver* (Ghost arrived at
  the identical scheduler), driving the enforcing primitive from 0b
  (detach/attach + timeslice-shrink-to-preempt). The policy is done; the value
  was always the enforcement primitive, which 0b decides.

---

## Phase 3 — Memory (optional; hard quota already works, oversubscription is the new bit)

- **Hard quota:** we already enforce admit-before-forward per-sandbox limits in
  nvproxy (device + UVM counted together). Leave it there; no driver work needed.
- **Oversubscription / swap (Ghost's novel contribution):** driver-level UVM
  page-fault accounting + demand paging to host + `usermemfd` cooperative swap.
  Lives in `nvidia-uvm`; large. **Deferred:** for *mutually-untrusting* tenants,
  hard quotas are arguably safer than cross-tenant paging, whose interference is
  what Ghost spends most of its complexity mitigating. Revisit only if a
  bin-packing/elasticity requirement demands it.

---

## Phase 4 — Integration & hardening

- nvproxy sets per-sandbox broker params via Phase 1; driver enforces. Confirm
  the Sentry-per-sandbox = container mapping holds under Kubernetes (the pause
  container / registration-keys-off-root-spec issue, CLAUDE.md Next #7).
- Re-run the adversarial suite: a hostile sandbox must not be able to widen its
  own share (it has no kernel priv; the broker params come only from nvproxy).
- Measure against the existing baselines (weights honoured? isolation held?
  overhead?) on the 2- and 3-tenant reproducers.

---

## Risks & open questions (in priority order)

1. **Blackwell GSP firmware behaviour** — the whole temporal path, and unknown for
   the privileged controls' *efficacy* (0c may set the TPC table successfully yet
   find GSP under-honours it, as it did the timeslice). Phase 0 is entirely about
   this.
2. **Running the open driver on the RTX 5070** in place of proprietary — support,
   stability, nvproxy version pinning, KVM device-memory recheck.
3. **Forked-driver maintenance burden** — a patched GSP-era driver is a heavy
   ongoing cost; keep the patch minimal (a broker module + a few control hooks),
   not a rewrite.
4. **Signed GSP firmware is immutable** — we can only drive it via RPCs it
   honours; if neither temporal nor spatial enforces on Blackwell's GSP, the
   honest conclusion is that robust compute isolation for arbitrary CUDA needs
   MIG-class hardware this card lacks, and the driver work buys only memory
   accounting we already have.

## Bottom line

Ghost validates our architecture (privileged broker + weighted-credit scheduler)
and our diagnosis (enforcement must be privileged/driver-level). The plan builds
that broker, but **Phase 0 is non-negotiable and comes first**, because on
Blackwell the reachable temporal knobs are inert at the GSP and the driver does
not change that — the driver's genuine unlock is the *privileged spatial* TPC
partition, which we have independent evidence *isolates* on this GPU. Build the
broker; let Phase 0 pick temporal vs spatial; don't port memory oversubscription
until a requirement asks for it.
