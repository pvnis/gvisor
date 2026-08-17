# Red-team findings: Sentry-enforced GPU slicing

Results of an adversarial red-team run (2026-08-13) against the gVisor + nvproxy
GPU sandbox, conducted as a malicious tenant holding **only** a vCluster API
token (tenant-nv-a on sensai's RTX 5070), every attack launched from inside a
`runtimeClassName: gvisor` pod. Method and raw log:
`~/.claude/jobs/*/tmp/redteam/` while it exists; this file is the durable record.

The bar for a finding: a break had to be *demonstrated* from inside the sandbox,
not inferred. Reading peer/host state was allowed only to verify a claimed
in-sandbox result, never to perform the attack.

## OPEN — priority

### V1+V2. Compute gate fails to intercept the workload's submission channel, so it does not enforce at all (BROKEN, root-caused)
**The gate gates the wrong command buffer, is therefore mostly bypassed, and the
scheduler consequently mis-classifies an active sandbox as idle and grants it the
full period — turning the gate off. Weight is ignored and any number of
concurrent submitters run unconstrained.** "Window-packing" (below) is just the
most dramatic way to exploit the already-open gate, not a separate bug.

Measured, weights A=25 / B=100 / C=100, identical fp16 4096³ matmul, three
tenants in separate vClusters on one RTX 5070, GPU saturated (total conserved
~437 matmul/s throughout):

| A config | A | B | C |
| --- | --- | --- | --- |
| honest, 1 process | 145.5 | 145.5 | 145.5 |
| A packs 5 processes | **312** | **62** | **62** |

Equal 145/145/145 despite 4:4:1 weights (V2); packing then takes A to 312 while
B/C fall to 62, total conserved ⇒ theft not gap-filling (V1). Clean repeatable
A/B: killing the extra processes returned A to 146 and B/C to 146.

**Root cause, from the logs (not inferred):**
- The attacker sandbox's own gate log: `computeGate: 3000 revocations, 88
  submissions, 1 gated file descriptions`. Over ~3000 periods it revoked its
  mappings every period but the workload faulted back through the gated mapping
  only **88 times (~3%)**, and it ever found only **1** command buffer to gate.
  So the cuBLAS compute submission goes through a channel the gate **never
  gated**; revocation hits a cold mapping, the hot path is never blocked.
- The `runsc gpu-scheduler` journal (`gpusched: … weight=… idle=… allowance=…`)
  shows the burners `idle=true` ~90% of ticks, and — whether idle or active —
  granted `allowance=100ms` (the whole period). With ~0 submissions reported
  (because faults are rare), the scheduler's activity signal reads idle, so
  `Grants()` never puts the sandbox in the weighted division; it oscillates
  between the 5ms idle floor and a full-period grant. `waitUntil` returns 0
  whenever `allowance >= period`, so the gate never delays anything. Weights
  (25 vs 100) were correctly received — confirmed in the log — and simply never
  used, because no client is ever in the "active" weighted set at the same time.

**Why the earlier clean 3:1 held and this didn't:** that test used a tight
kernel-launch loop that submitted through the one gated channel every period, so
faults were frequent, the sandbox read as active, and the weighted division
applied. A cuBLAS matmul submits through a channel the gate misses, so the whole
mechanism collapses. The gate's correctness depends on catching *every* compute
submission channel, and it doesn't.

**Instrumented follow-up (2026-08-13, throwaway `gatediag` build) — the root
cause is deeper than "wrong buffer":** a diagnostic that revoked *every* frontend
mapping each period and counted per-fd write-faults showed, for a solo weight-25
cuBLAS burner at 456 matmul/s:
- `1 command buffers known, 26 mapped-memory fds, 1 gated, 0 known-but-unmapped`
  — no linking bug; the gate correctly gated the one command buffer the driver
  told it about.
- **Max write-faults on *any* mapping: 5 per 3s**, versus ~1,368 matmuls
  submitted per 3s. Only three cold mappings ever faulted (~3/interval),
  including the gated one. **Submission bypasses every revocable mapping by
  ~270×.** cuBLAS rewrites the pushbuffer ~1/s and submits ~456/s by ringing a
  doorbell (GP_PUT) on a persistent GPFIFO — a path the memory-revocation gate
  cannot catch.
- **Preemption was enabled and still never enforced**, because it fires only at
  *window close*, and the window never closes: no faults → scheduler marks the
  sandbox idle → grants the full period → the weighted boundary never arrives.

So the gate's founding assumption — "writes to a channel's command buffer submit
work, so revoking it catches submission" — is **false for cuBLAS**. Neither a
targeted fix (gate a different buffer) nor a broad one (gate all command buffers)
can work: submission is not a revocable memory write. The whole fault-based
activity signal is therefore unreliable, which is what feeds the scheduler the
"idle" mis-read that turns the gate off.

**Fix direction (design change, not a patch):** enforcement has to stop
depending on submission faults. The mechanism that *can* hold a doorbell-driven
workload already exists — **preemption** (evict the sandbox's channel groups at
window close) — but it only runs if the window actually closes. Two viable
shapes, to decide next:
1. **Default-deny weighted windows + preemption as primary.** The scheduler
   holds every registered sandbox to its weighted window rather than granting the
   full period on a fault-based "idle" guess; preemption evicts running work each
   period at the boundary. True idleness is detected from an
   submission-independent signal (e.g. whether the periodic preempt actually
   evicted work, or a GPU-usage sample) so work-conservation still redistributes.
2. **Fix the GPU-usage sampler** (`--measure-usage`, currently defective and off)
   to give the scheduler real per-sandbox utilisation, so active-but-not-faulting
   sandboxes are seen as active and get their weighted window (then preemption
   enforces as in 1).
Both make preemption the enforcer and demote memory-revocation to a best-effort
first line. Code: `pkg/gpusched/{gpusched,server,usage}.go`,
`pkg/sentry/devices/nvproxy/computegate*.go`. A throwaway diagnostic
(`writeFaults` counter + `diagnose()` goroutine, TODO-marked) is in the tree from
this investigation; delete it when the real fix lands.

**Progress (2026-08-13) — sampler fix landed and VERIFIED at the scheduling
layer; a second, deeper enforcement gap now isolated.** Implemented the chosen
direction (fix the GPU-usage sampler): `pkg/gpusched/server.go` now consults the
`nvidia-smi pmon` sampler for **activity detection** (a sandbox the driver
reports as using the GPU is active regardless of its fault count), and drops the
old `used = measured` debt path that diverged (31/618). Verified on the RTX 5070
3-tenant reproducer (weights 25/100/100): `pmon` attributes SM% to the three
Sentry PIDs, and the scheduler now hands out **correct, disjoint, weighted
windows** — C `[0–44.4ms]`, A(w25) `[44.4–55.5ms]`, B `[55.5–100ms]` — instead of
the previous full-period-for-everyone. This fixes the idle-misdetection root of
V2 at the scheduling layer.

**But throughput stayed equal (143/147/147, 1:1 not 1:4).** The windows are now
correct and still not enforced, because the enforcement actions are no-ops: the
burner sandbox logs show **zero** `preempted GPU channel group` / `off the
runlist` / `failed to …` lines despite preempt **and** unschedule being enabled
and `allowance < period`. That means `computeGate.tsgs` is empty — the sandbox's
channel groups are never tracked, so `preemptAll()` and `setScheduled()` have
nothing to act on. `addChannelGroup` (frontend.go, from `rmAllocChannelGroup`)
is apparently not firing for this cuBLAS/driver-610.43.02 path, or the channel
groups are allocated by a route the proxy doesn't intercept. Net: revocation
misses cuBLAS (doorbell), and preempt/unschedule are inert, so nothing confines
the sandbox to its (now-correct) window.

**Update 2 (2026-08-13, instrumented `enforcediag` build) — `tsgs` was empty
only because preempt was off; enabling the full toolkit STILL does not confine
cuBLAS.** The channel-group tracking is fine: with `enforcediag` logging,
`addChannelGroup` fires and channels sit under channel groups
(`parent=0x5c0000{13,38,46}`), but the guard `if !g.preempt` discarded them
because the node's `/etc/containerd/runsc.toml` never set
`nvproxy-gpu-preempt`/`unschedule`. So enforcement had simply never been engaged.

Enabling `nvproxy-gpu-preempt=true` + `nvproxy-gpu-unschedule=true` node-wide,
with the sampler on, and re-running the 3-tenant test:
- `tsgs` = **3**; preemption fires every window close — **8448 preempts, mean
  450µs, max 5.2ms, none over 10ms** — and unschedule is active.
- The scheduler hands out **correct disjoint weighted windows** (w25 → 11.1ms @
  phase 44.4, w100 → 44.4ms each), confirmed live.
- **Throughput was still exactly 1:1:1 (147/145/147).** Preempt +
  `GPFIFO_SCHEDULE(disable)` succeed (no failures logged) yet have *zero* net
  effect on the division.

So on Blackwell (RTX 5070, driver 610.43.02), the full enforcement toolkit —
mapping revocation, `NVA06C_CTRL_CMD_PREEMPT`, and
`NVA06C_CTRL_CMD_GPFIFO_SCHEDULE(disable)` on every compute TSG — does **not**
hold a doorbell-driven workload off the GPU. The most likely reason: the
sandbox's continuing doorbell rings (which the Sentry cannot intercept — the
whole original problem) re-make the channel runnable faster than descheduling
removes it, and `GPFIFO_SCHEDULE(disable)` does not evict a TSG the application
keeps poking. Preemption evicts the *running* work for 450µs but the next
doorbell resubmits within the same window.

**Update 3 (2026-08-13) — doorbell gating tried and it does NOT work either;
three independent enforcement mechanisms now ruled out by measurement.** Gated
the work-submission doorbell region directly (new `rmAllocUserMode` handler
registering the USERMODE object with the gate so its mapping is revoked outside
the window; `version.go` overrides for the 610 ABI). Found first that cuBLAS on
Blackwell allocates **HOPPER_USERMODE_A (0xc661)**, not `BLACKWELL_USERMODE_A`,
so both are now gated. Confirmed the gate then tracks **3 gated fds** (command
buffer + 2 USERMODE). Result on the 3-tenant test: **still exactly 1:1:1**, and
the sandbox's submission count stayed at ~6.5% (92/1400) — gating the USERMODE
added *zero* faults. From the earlier `gatediag` sweep, the USERMODE mapping
(`0x5c000006`) was written only ~3×/3s while the doorbell fires ~1350×/3s, so
**the doorbell write does not go through any revocable frontend mapping** (it is
device MMIO whose writes the Sentry never faults on under systrap, the default
platform here). Memory revocation cannot see the doorbell.

**Update 4 (2026-08-13) — retried under `--platform=kvm`; same result.** The
hypothesis was that systrap simply doesn't trap MMIO writes, but KVM would
(EPT-fault on an unmapped BAR). Added per-pod platform selection to the override
allowlist (KVM-only), ran the same burn under KVM, and re-ran the `gatediag`
diagnostic (revoke every mapping each period, count per-fd write-faults).
Confirmed the sandbox was on KVM and CUDA worked (457 matmul/s). The write-fault
counts were **identical to systrap: max 5 per 3s interval on any mapping**,
versus ~1370 matmuls/3s. So KVM was never the issue. The real reason so few
writes are trappable is that **cuBLAS submits in large batches** — one doorbell
ring per ~150 matmuls — so the events the Sentry could gate arrive at ~0.1/period
regardless of platform, far too coarse to bound per-matmul throughput (and each
batch runs well past the window anyway). Trapping the doorbell, on either
platform, cannot rate-limit this workload. (Both the diagnostic and the
platform-override allowlist entry were reverted after measuring.)

So three enforcement mechanisms have now each been measured to fail against a
doorbell-driven cuBLAS workload on this Blackwell GPU:
1. **Command-buffer revocation** (the original gate) — misses; cuBLAS rewrites
   the pushbuffer ~1/s, not per submission.
2. **TSG preempt + unschedule** — fire correctly (8448 preempts, ~450µs;
   `GPFIFO_SCHEDULE(disable)` succeeds) but do not hold the channel off the GPU;
   throughput stays 1:1:1.
3. **Doorbell / USERMODE revocation** — the MMIO doorbell write never faults, so
   revoking its mapping adds no gating.

**Honest conclusion.** nvproxy cannot time-divide a doorbell-driven CUDA
workload (cuBLAS, and by extension steady-state CUDA-graph replay) on this GPU
with the mechanisms available. This sharpens the branch's own thesis: nvproxy
chose *time* division precisely because submission never enters the kernel — and
this result shows that for the workloads that submit purely by ringing a doorbell
on a persistent GPFIFO, time division cannot be *enforced* at all, because the
Sentry can neither intercept the submission (no fault) nor make the GPU stop
honouring it (preempt/unschedule don't stick). The compute gate works only for
workloads that fault frequently (those that rewrite a command buffer per
submission), a narrower class than assumed. Real compute isolation for arbitrary
CUDA on this hardware needs a spatial/hardware mechanism (MIG), which this GPU
does not have — the same shape as the AMD side, where compute is partitioned in
space (CU masks) rather than time.

The sampler fix (correct weighted windows) remains valid and useful for the
workloads the gate *can* enforce; it is not sufficient on its own.

### Runlist timeslice as a doorbell-proof time-division lever — MEASURED INERT on Blackwell (2026-08-14)

The most promising escape from the doorbell wall: instead of intercepting
submission (impossible) or revoking mappings (misses doorbell), configure the
GPU's *own* runlist scheduler at the one ioctl nvproxy does see — the per-TSG
timeslice (`NVA06C_CTRL_CMD_SET_TIMESLICE`). Set each sandbox's channel-group
timeslice proportional to its weight and let the hardware round-robin enforce the
division, regardless of how work is submitted. nvproxy already tracks every
channel group and already originates sibling A06C controls (preempt, gpfifo
schedule), and already *clamps* a container's own SET_TIMESLICE (`ctrlSetTimeslice`)
— so the machinery and the defensive half existed; only proactive origination was
missing.

Built it behind an operator-only flag (`--nvproxy-set-timeslice-us`) and ran the
smoke test on the RTX 5070: two saturating cuBLAS burners, equal scheduler weight
(100/100, so the gate stays neutral), different timeslices.

- **4:1 ratio (4000 us vs 1000 us): 218.6 vs 218.3 matmul/s — exactly 1:1.**
- **16:1 ratio (8000 us vs 500 us): 218.2 vs 218.2 — exactly 1:1.**
- The value is genuinely live: `GET_TIMESLICE` reads back exactly what was set
  (4000/1000, 8000/500), and it survives `GPFIFO_SCHEDULE` unchanged
  (before-reimpose == after == set value). So this is not "reset after
  scheduling" — the driver stores and honours the value *as state*; the runlist
  simply does not apportion engine time by it.

**Conclusion: on Blackwell (GSP-managed scheduling), the per-TSG software
timeslice does not weight the compute-engine division.** It is accepted, stored,
and inert. This is the same per-architecture story as everywhere else on this
branch: the mechanism is real in the runlist (Bakita & Anderson ECRTS'25 Fig. 4
measured it dividing on GA100), but the newest GPU's GSP scheduler ignores it.
Kept off-by-default and documented in `computegate_unsafe.go`
(`setChannelGroupTimeslice`); the operator flag is not container-overridable (a
container enlarging its own timeslice is exactly what `ctrlSetTimeslice` guards
against). The adversarial version is moot until a GPU is found where the lever
divides at all.

The sibling knob `SET_INTERLEAVE_LEVEL` (LOW/MED/HIGH runlist priority) was then
tried the same way (`setChannelGroupInterleaveLevel`, flag
`--nvproxy-set-interleave-level`). **It is admin-gated: originating it from the
Sentry returns `NV_ERR_INSUFFICIENT_PERMISSIONS` (0x1b) for every level
(LOW and HIGH alike), on every channel group.** Same wall as
`SET_TPC_PARTITION_TABLE` — the deliberately-unprivileged Sentry (no
CAP_SYS_ADMIN) cannot set it, so its efficacy for weighting the division is
untestable here. (Note the driver's own inconsistency: `SET_TIMESLICE` is
non-privileged, `SET_INTERLEAVE_LEVEL` is privileged.) Kept off-by-default,
documented, not container-overridable.

**Net for the runlist-scheduler avenue: closed on Blackwell.** The one runlist
knob nvproxy *can* set (timeslice) is inert; the one that might weight
(interleave) is admin-gated and unreachable. Combined with the earlier temporal
result (submission uninterceptable, preempt/unschedule don't stick) and the
spatial result (TPC table admin-gated, green contexts cooperative-only), every
ioctl-reachable NVIDIA scheduling lever is either inert or privileged on this
GPU. Time-division via the compute gate remains the only working mechanism, and
only for command-buffer-faulting workloads.

--- superseded exploratory note (kept for the record) ---
The earlier-hoped direction was to **gate the work-submission doorbell / USERD
mapping itself** — revoke it outside the window
so the first doorbell ring faults into `wait()` and blocks there until the
window opens (each thread/stream blocks on its own first ring, so all submitters
are held). This is the true choke point the current gate misses (it revokes only
the pushbuffer command buffer, which cuBLAS rewrites ~1/s). Open work: identify
the doorbell/USERD mapping (likely a `VOLTA_USERMODE_A`-class mapping or a
specific mmap offset), confirm writes to it fault after `Invalidate` on this
write-combined MMIO region, and add it to the gate's `gated` set. If MMIO writes
to a revoked mapping do not fault, memory-revocation gating cannot work here at
all and enforcement would need a hardware feature this driver/GPU may not expose
to a proxy — in which case the honest conclusion is that time-division
enforcement of doorbell workloads is not achievable on this GPU via nvproxy, and
the spatial-partition story (as on AMD) or MIG is the only real isolation.

Code touched this session (all uncommitted, in the working tree):
`pkg/gpusched/server.go` (sampler-for-activity fix — KEEP), plus throwaway
diagnostics `gatediag` (nvproxy `computegate.go`/`frontend*.go` `writeFaults` +
`diagnose()`) and `enforcediag` (the channel-group/preempt logs) — DELETE both
when the real fix lands.

Not weight-multiplication by pod count — two separate weight-25 pods did not
double the share (291→312, not →582).

## OPEN — low severity

### V3. GPU power-draw side-channel (disclosure, low)
`nvidia-smi` / NVML inside the sandbox report live **power draw** (8–13 W idle),
a real physical signal that tracks the host device and would move under a
co-tenant's compute — a covert activity/timing channel across tenants. No data
content. Everything else in the disclosure surface is correctly rewritten to the
tenant's own quota (total memory, process list, serial, PCI id all masked).
Consider clamping/oncealing power (and any other live physical telemetry) in the
NVML/smi rewrite the same way memory already is.

## Operational note (not a vulnerability)
Two `runsc gpu-scheduler` daemons were bound to the same socket path
`/run/runsc-gpu-scheduler.sock`; the newer holds all client connections, the
older is orphaned. Harmless here but worth a single-instance guard.

## HELD — verified contained (2026-08-13)
Every other surface held; recording so they are not re-litigated without cause.
- **Host / filesystem escape** — HELD. `4.19.0-gvisor`; only 3 nvidia device
  nodes + null/zero/etc; synthetic /proc & /sys; no host mounts; no magic-symlink
  leak. `/etc/ld.so.preload` empty — HAMi `libvgpu.so` mounted but not preloaded,
  so limits are the Sentry's, not the in-container shim's.
- **GPU memory quota** — HELD. Caps at the slice then `OUT_OF_MEMORY`; UVM
  counted with device memory; 4-process barrier-synced race summed correctly, no
  overshoot, no TOCTOU.
- **Disclosure** — HELD except V3 above.
- **Self-escalation** — HELD. `nvproxy-gpu-weight` annotation is lower-only
  (100000 refused at startup); `nvproxy-gpu-memory-limit=12000` raise collapses
  the effective limit to ~1 MiB (grants nothing) while a normal pod gets its full
  slice at the same instant; HAMi independently refuses an oversized `gpumem`.
- **Sentry DoS / panic (mmap+ioctl)** — HELD. Overflowing offsets, huge lengths,
  arbitrary tokens on all three nodes returned clean errors; 0 restarts. The
  historical offset-overflow panic class appears fixed.
- **Runtime-selection bypass** — HELD (strong). `runtimeClassName: nvidia`,
  `crun`, and the default/no-runtimeClass **all** still ran under gVisor with the
  quota enforced. A tenant cannot pick a raw-driver runtime to bypass nvproxy.
- **Cross-tenant GPU memory read/write** — HELD both ways. Against a live victim
  holding a secret sentinel in 512 MiB of VRAM on the same card: every allocation
  (device, reused, UVM) came back zeroed (marker never seen); out-of-bounds
  reads/writes faulted `CUDA_ERROR_ILLEGAL_ADDRESS` at the per-context GPU MMU;
  an 11-offset raw mmap sweep of the device nodes opened no VRAM window. The
  victim logged zero corruption events. Two barriers: pages arrive zeroed, and no
  tenant-reachable primitive addresses memory outside its own allocations.

## Spatial SM/TPC partition as the NVIDIA analog of AMD's CU mask (investigated 2026-08-13)

Motivated by the enforcement wall above: time-division can't bound a doorbell
workload, so a *spatial* partition (bind a sandbox to a fixed subset of SMs,
like AMD's CU mask) would enforce without gating submission at all. Investigated
empirically on the RTX 5070 (Blackwell, driver 610.43.02) under gVisor with
CUDA-driver-API probes (ctypes, no nvcc). MPS itself is out (cooperative-trust
model, shared server/context — see the MPS discussion), but its SM-fraction knob
proves spatial partitioning is a hardware capability; the question was whether
nvproxy can reach and impose it directly.

**Result — the primitive exists and works through gVisor, but is not trivially
Sentry-imposable.**

- **Exec affinity (`cuCtxCreate_v3` SM_COUNT) is unsupported on this GPU**:
  `cuDeviceGetExecAffinitySupport(SM_COUNT)` returns 0 (it's the older,
  MPS-gated path). Dead end here.
- **Green contexts (CUDA 12.4+) WORK under nvproxy.** A probe split the 48 SMs,
  created a green context on 24 of them, materialised it (`cuCtxFromGreenCtx` +
  a real compute kernel launch), and `cuGreenCtxGetDevResource` confirmed
  **24/48 SMs**. Every ioctl it needs already passes nvproxy's allowlist — no
  new support required for a *cooperative* tenant to run spatially partitioned.
- **But the TPC/SM partition is not a distinct RM control or object nvproxy
  dispatches on.** Diffing a green-context materialisation against a normal one
  (logging every `NV_ESC_RM_CONTROL` cmd and `NV_ESC_RM_ALLOC` class): the
  control-command set is *identical* (59 vs 59) and the alloc-class set is
  *identical*; the **only** difference is the green path allocates **one extra
  `NV01_MEMORY_LOCAL_USER` (class 0x40)** buffer. So the mask is carried in
  parameters / that memory buffer, bound lazily at first compute use — not as an
  ioctl argument the way AMD's CU mask is an argument to `CREATE_QUEUE`.

**Why this makes imposition hard (unlike AMD).** Two obstacles:
1. **The default context uses all SMs.** Green contexts are opt-in userspace; a
   hostile container just uses the normal full-device context (no partition) and
   runs on all 48 SMs. Enforcement can't rely on the container creating green
   contexts — nvproxy would have to *impose* a TPC partition on every context,
   which the default path never sets up.
2. **The mask isn't a narrowable ioctl arg.** amdproxy enforces by narrowing the
   CU-mask argument on every `CREATE_QUEUE` (one field, one interception). Here
   there is no such field on a dispatched call; the partition lives in opaque
   params / a descriptor buffer, so nvproxy can neither read nor clamp it
   without reverse-engineering that layout (and it may be GSP-mediated and
   driver-version-fragile).

**Where that leaves it.**
- *Cooperative* spatial partitioning already works: if the platform arranges for
  workloads to use green contexts, nvproxy passes them through and the GPU
  isolates by SM. But "arrange for the container to do X" violates the governing
  "nothing in the container" constraint unless nvproxy can force it.
- *Imposed* spatial partitioning — the real goal — needs (a) a way to bind an
  SM/TPC subset onto the sandbox's default context, and (b) parsing/injecting
  the carrier (the extra `NV01_MEMORY_LOCAL_USER` descriptor or the params of an
  existing graphics-context call). That is a substantial, driver-fragile RE
  effort, and is the open question.
- **Measured (the "is it real" test) — it isolates.** A compute-bound FMA
  kernel timed under gVisor (reproduced across two runs, <0.5% spread):
  - full 48 SMs: **21,918 GFLOP/s**
  - green-24 solo: **12,921 GFLOP/s = 0.59× of full** — the partition genuinely
    caps compute; ~0.59 not 0.50 because a smaller slice clocks higher, the same
    per-unit-efficiency behaviour seen with AMD CU masks.
  - green-24 with a peer green-24 on the *disjoint* half, both running
    concurrently: **12,840 GFLOP/s each = 0.99× of solo** — a co-tenant on the
    other half costs **1%**. That is the NVIDIA equivalent of the AMD result
    "isolation is the strongest result" (−0.61% when the peer starts). Two
    tenants on disjoint SM halves each get ~half the GPU and do not interfere.

  So the primitive is not cosmetic: green-context SM partitioning **proportionally
  limits and cleanly isolates compute** on this Blackwell GPU under gVisor. This
  is the spatial enforcement the time-division gate cannot provide for doorbell
  workloads. What remains is purely the *imposition* problem (obstacles 1–2
  above): making this apply to an uncooperative container, since the primitive
  itself now demonstrably works and isolates.

- **Imposition attempt — the mask is not reachable through any ioctl (measured).**
  Tried to find what nvproxy would clamp/inject. Instrumented the channel alloc,
  every RM control's raw parameters, and the alloc classes, diffing a 24-SM
  green-context materialisation against a normal full-device one:
  - The channel alloc has a `TPCConfigID` field (looked like the AMD-CU-mask
    analog) but **green's channels all set `TPCConfigID=0`** — it is not the
    carrier.
  - **`SET_TPC_PARTITION_MODE` (0x801108) is never issued**, and the control
    *set* is identical between green and normal.
  - Every differing control **parameter** is a pointer or an object handle
    (`...7f0000` addresses, per-run handles) — **no control param carries a mask
    value** (no 24, no 12-TPC mask, no half-vs-full pattern).
  - The only structural difference remains the **one extra
    `NV01_MEMORY_LOCAL_USER` buffer**, whose *contents* are written by userspace
    to mapped memory, not through an ioctl.

  **Conclusion: nvproxy cannot impose the SM/TPC partition.** The restriction
  that demonstrably works is applied by the userspace CUDA driver — via the
  pushbuffer command stream at kernel launch (the doorbell path) and/or userspace
  writes into that buffer — entirely below the ioctl boundary the Sentry
  mediates. This is the *same root* as the compute-gate failure: the GPU
  programming that matters happens in userspace/command-stream, not in
  interceptable kernel calls. Green contexts are therefore a *cooperative*
  feature: a tenant can partition itself and gVisor passes it through cleanly,
  but a hostile tenant cannot be forced onto a subset.

  **Imposition via originated RM calls — attempted, half-works, blocked on
  privilege (2026-08-13).** Rather than the green-context path (userspace,
  unreachable), used the *explicit* TPC-partition controls the driver exposes:
  `SET_TPC_PARTITION_MODE` (device/channel-group) + `SET_TPC_PARTITION_TABLE`
  (context-share, `NV9067_CTRL_CMD_SET_TPC_PARTITION_TABLE = 0x90670102`). Exact
  structs pulled from NVIDIA open-gpu-kernel-modules (ctrl0080gr.h, ctrl9067.h)
  and added to the ABI; nvproxy originates the controls on the host fd on its own
  behalf (same machinery as `preemptChannelGroup`), hooked at channel-group and
  context-share allocation. Code: `pkg/sentry/devices/nvproxy/smpart_unsafe.go`
  (off by default, `smPartitionTPCs = nil`).

  Measured on the RTX 5070 with a normal (non-green) matmul:
  - **`SET_TPC_PARTITION_MODE(STATIC)` SUCCEEDS** on every channel group when
    originated by nvproxy — the device handle (the channel group's parent) is
    correct and static mode is accepted. This is *not* MIG-gated.
  - **`SET_TPC_PARTITION_TABLE` fails `NV_ERR_INSUFFICIENT_PERMISSIONS` (0x1b)**
    — assigning specific TPCs is a *privileged* control, and the container's RM
    client is non-privileged (CUDA allocates `NV01_ROOT_CLIENT`, not the admin
    `NV01_ROOT`). Originating the call doesn't help, because it carries the
    container's client handle.
  - Net: the normal context still ran at full throughput (ratio 1.00) — mode
    without a table imposes nothing.

  **Privileged-client + DUP_OBJECT built and tested — the imposition is blocked
  by gVisor's own capability drop (2026-08-13, decisive).** Implemented and
  measured, in order:
  - Originating `SET_TPC_PARTITION_TABLE` on a fresh **privileged `NV01_ROOT`
    client** nvproxy allocates: the client *allocates* fine, but the control
    still fails `NV_ERR_INSUFFICIENT_PERMISSIONS`, and duplicating the context
    share into it first fails — a device/channel-group/context-share hierarchy
    is not `NV_ESC_RM_DUP_OBJECT`-able across clients this way (device dup →
    `NV_ERR_INVALID_ARGUMENT`).
  - Making the container's **own** client privileged: a no-op, because CUDA
    *already* allocates `NV01_ROOT` (the nominally-privileged class), and the
    table still fails. So the class is not the check.
  - **The check is the process's `CAP_SYS_ADMIN`.** The Sentry runs as uid 0 but
    `CapEff = 0x8001f` — **bit 21 (`CAP_SYS_ADMIN`) is not set**; gVisor
    deliberately drops it. RM's `osIsAdministrator()` therefore returns false for
    every client the Sentry allocates, so the admin-gated table control can never
    succeed from inside the sandbox, by any client.

  **Conclusion: nvproxy cannot impose the TPC partition.** Not for lack of a
  mechanism — `SET_TPC_PARTITION_MODE(STATIC)` works and the table control is
  right there — but because assigning TPCs requires `CAP_SYS_ADMIN`, which the
  Sentry (correctly) does not hold. The very isolation that makes gVisor secure
  is what puts this control out of reach. This is the same shape as every other
  wall in this document: the thing that would enforce lives on the far side of
  the boundary the Sentry sits behind.

  **Privileged-helper delegation built and tested — it does NOT work either
  (2026-08-13, decisive).** Validated the helper idea directly: a root process
  holding `CAP_SYS_ADMIN` (which the Sentry lacks) grabbed the container's own
  `/dev/nvidiactl` fd via `pidfd_getfd` (same RM session, so the container's
  client/context handles are valid) and issued `SET_TPC_PARTITION_TABLE` on the
  container's context share. Result: `ioctl_ret=0, errno=0`, **`NvStatus=0x1b`
  (`INSUFFICIENT_PERMISSIONS`) again** — a fully-privileged admin process *still*
  cannot set the table on the container's context.

  So the admin check is **not on the calling process**; it is bound to the RM
  **client at allocation time** (`RS_PRIV_LEVEL`, set from the allocating
  process's `osIsAdministrator()`). The container's client was allocated by the
  capless Sentry, so it is permanently `RS_PRIV_LEVEL_USER`, and *any* caller —
  Sentry or external root helper — issuing an admin-gated control on it fails.
  Delegating just the *control* to a privileged helper cannot work, because the
  object lives in a non-privileged client and privilege cannot be conferred after
  the fact.

  **What that leaves (both unattractive):**
  1. **Delegate the client *allocation*** to a privileged helper, so the
     container's root client is born `RS_PRIV_LEVEL_ADMIN`. Then the table
     control works from within. But this gives the *container* a privileged RM
     client — it could then issue any admin-gated control nvproxy forwards,
     expanding the sandbox's attack surface. It trades the isolation property the
     whole design exists to protect, and would demand a hard audit of the control
     allowlist. Not obviously acceptable.
  2. **Give the Sentry `CAP_SYS_ADMIN`** so its clients are privileged — directly
     defeats gVisor's host isolation. No.

  **Honest conclusion for the whole spatial-partition thread.** The RTX 5070 can
  be spatially partitioned and it isolates cleanly (measured 0.59× / 0.99%), the
  explicit control path exists and its non-privileged half works — but the piece
  that assigns TPCs is admin-gated *at the client that owns the object*, and every
  client inside a gVisor sandbox is non-privileged by construction. There is no
  way to impose it on an uncooperative container without either privileging the
  container's RM client (a security regression) or privileging the Sentry
  (defeating the sandbox). Green contexts remain available for *cooperative*
  tenants; imposed spatial isolation on this hardware is, like time-division for
  doorbell workloads, blocked by the same boundary that makes gVisor gVisor.
  Scaffolding (ABI structs, mode/table origination) is in `smpart_unsafe.go` (off
  by default), documenting exactly what works and what is blocked.

  **Can a client be downgraded to non-privileged after the fact? No.** Checked
  the driver binary: there is no `SetClientPrivLevel` / downgrade / `CLIENT_SET`
  privilege control of any kind. A client's privilege is fixed at allocation from
  the allocating process's admin capability (`pSecInfo->privLevel`) and read
  thereafter via `rmclientIsAdmin(pRmClient)` against that cached, immutable flag.
  This kills the "allocate the client as admin, set the table, then drop
  privilege" mitigation for option 1: a helper-allocated admin client would stay
  admin *permanently*, so the container would keep a privileged RM client for its
  whole life, not just during setup. The privilege is baked in and one-way; there
  is no transient-elevation escape hatch.

  **Separate-admin-client + DUP path built and tested — also blocked, and the
  privilege model pinned down from driver source (2026-08-14).** The remaining
  untried variant of option 1 was to *not* privilege the container's client at
  all: have a privileged helper allocate its *own* admin `NV01_ROOT` client, DUP
  the container's context share into it, and set the table via that admin client
  — leaving the container non-privileged, no downgrade needed. Built the
  diagnostic (`redteam/helper2.py`, `helper3.py`, `helper4.py`) and measured:
  - **Privilege is caller-based, not (only) client-cached, and userspace root is
    one rung too low.** Driver source (`nvidia/os-interface.c`,
    `common/inc/nv-linux.h`): `os_is_administrator() == NV_IS_SUSER() ==
    capable(CAP_SYS_ADMIN)`, evaluated on the *current* caller. RM checks
    `pCallContext->secInfo.privLevel` against an ordered ladder whose top rung is
    `RS_PRIV_LEVEL_KERNEL` — *above* a root process's `RS_PRIV_LEVEL_USER_ROOT`
    and reachable only by the driver/GSP itself. Since a root+CAP_SYS_ADMIN
    helper (USER_ROOT) still gets `0x1b` on the table (measured, `helper.py`),
    the control requires either KERNEL level or an admin-*owning*-client — and no
    userspace actor can supply the former.
  - **A client the root helper allocates is not usably admin through the Sentry's
    fd.** Allocating a fresh `NV01_ROOT` on the grabbed fd succeeds, but a
    `NV01_DEVICE_0` under it returns `INSUFFICIENT_PERMISSIONS` (0x1b) and the
    cross-client ctxshare DUP returns `0x1b`. (Correcting an earlier read: the
    own-fresh-fd client alloc *does* work — the `0x19`/`INSERT_DUPLICATE_NAME`
    seen once was a **global** client-handle btree collision from a leaked handle,
    not an fd-init failure. RM client handles are process-global.)
  - **The DUP refusal is an `RS_ACCESS` share-policy denial, not a hard "can't."**
    The binary shows RM itself dups `hKernelGraphicsContext` across clients — but
    only from its own (KERNEL-priv) channel-group-duplication path — and exposes
    `cliresCtrlCmdClientSetInheritedSharePolicy`. The container's context simply
    is not shared with an outside client, and a USER_ROOT caller cannot override
    that.

  **Net:** every userspace path (Sentry, root helper on the container's client,
  separate admin client + DUP, downgrade-after-the-fact) is blocked. A *hard,
  imposed* SM/TPC partition needs an `RS_PRIV_LEVEL_KERNEL` actor — a kernel-mode
  RM component / GSP — exactly as the temporal side needs hardware it doesn't
  have for doorbell submission. Two large, unproven levers remain, recorded but
  not pursued: (a) build a *full* admin context on a helper-owned fd and set the
  table on a ctxshare that client legitimately owns — this would resolve whether
  USER_ROOT-on-an-admin-owning-client suffices (KERNEL-only ⇒ dead; client-admin
  ⇒ a "privileged broker owns the context from creation" design becomes
  *theoretically* possible, though its isolation properties need separate
  analysis); (b) the `SetInheritedSharePolicy` path, whether a sufficiently
  privileged broker can be granted legitimate cross-client access. Both are
  design-scale, not a probe.

Ties into CLAUDE.md "Next" #3 (per-architecture behaviour): green-context and
TPC-partition support vary by generation, so this belongs behind the `gpuArch`
detection just added.

## Cross-cluster tenancy (lives outside this repo)
The one unverified avenue — a vCluster-authored `privileged`/`hostPath` pod
escaping to the host via a permissive syncer — is a Kubernetes/vCluster concern,
not a gVisor one, and per project convention is recorded under
`~/vcluster-multitenant/SECURITY-FINDINGS.md`.
