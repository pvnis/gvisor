# GPU memory overcommit — Phase 0 feasibility spike

Spike for the GVM virtual-memory / overcommit plan
(`~/.claude/plans/fuzzy-gathering-lamport.md`). Question S1: *does native UVM
oversubscription survive the gVisor/KVM path?* Answer: **it does now — a one-line
gVisor MM bug was found and fixed. Before the fix, managed memory worked up to
exactly 1 GiB and livelocked above it (both platforms). After the fix, gVisor
allocates 20 GiB of managed memory on the 12 GiB card and pages it, matching
native. The P0 prerequisite is cleared.**

## RESOLVED — root cause found and fixed (2026-08-21)

**Root cause:** `pkg/sentry/mm/address_space.go` `mapASLocked()` splits any
mapping longer than `singleMapThreshold = 1 << 30` (1 GiB) into 1 GiB-aligned
chunks, calling `AddressSpace.MapFile()` once per chunk — purely so it can check
`ctx.Killed()` between chunks. For an ordinary memory file that is harmless, but
for `/dev/nvidia-uvm` **each `MapFile` is a separate host `mmap(2)`, and the UVM
driver creates one `uvm_va_range` per mmap** (`uvm_api_validate_va_range`,
`kernel-open/nvidia-uvm/uvm_va_range.c:758`, returns `NV_OK` only if a single
va_range exactly covers `[base, base+length)`). So a >1 GiB managed region became
2+ fragmented va_ranges, `UVM_VALIDATE_VA_RANGE` returned `NV_ERR_INVALID_ADDRESS`
(0x1e) from the first call, and the CUDA runtime re-`MAP_FIXED`ed forever
(~2.5M times). It is in the platform-independent MM layer, which is why systrap
and KVM failed identically and native (one real `mmap` → one va_range) worked.

**Fix:** don't chunk when a non-default platform effect marks a range-sensitive
device mmap (the same signal the adjacent code already uses at the `mapAR = ar`
special-case). One added condition:
`if platformEffect != memmap.PlatformEffectDefault || pmaMapAR.Length() <= singleMapThreshold`.
Regular file/anon mappings keep the chunking (killability preserved); proxied
device mmaps (nvproxy/amdproxy/tpuproxy, which set `PlatformEffectPopulate` via
`GenericProxyDeviceConfigureMMap`) are mapped in a single call so the device sees
one contiguous mmap.

**Verified:** 1536 MiB `cudaMallocManaged` now `DONE ok` (was infinite hang);
20 GiB oversubscribed on the 12 GiB card pages at 2.9→6.8 GB/s (native: 2.0→6.7);
`//pkg/sentry/mm:mm_test` passes; gofmt clean. This is a generic gVisor bug (any
range-sensitive proxied device with a >1 GiB mmap), not GPU-slicing-specific —
candidate for `UPSTREAM-NOTES.md`.

---

_Everything below is the investigation record that led here; the "leading
hypotheses" in it were each tested and superseded by the root cause above._

## Original question (superseded by the resolution above)

Measured 2026-08-21 on **sensai** (RTX 5070 / Blackwell GB205, ghost open
**610.43.02**, kernel 6.8.0-117, `--platform=kvm`). Harness:
`/home/dmd/overcommit/uvm_oversub.cu` (managed-memory oversubscription probe,
compiled to compute_90 PTX and JIT-forwarded to Blackwell) +
`~/.claude/jobs/*/tmp/overcommit/`.

## The one-paragraph result

`cudaMallocManaged` **livelocks under gVisor+nvproxy on this stack** — it never
returns, spinning a tight `ioctl(/dev/nvidia-uvm, UVM_VALIDATE_VA_RANGE)=0` →
`mmap(MAP_FIXED, <same range>, /dev/nvidia-uvm)` → repeat loop (millions of
iterations, ~4.4M strace lines, 100% CPU). The identical binary run **natively
(runc)** allocates **20 GiB of managed memory on the 12 GiB card and runs to
completion**, paging at ~2 GB/s cold and ~6.7 GB/s warm. So the card + 610
driver support UVM oversubscription; gVisor is where it breaks, and it breaks at
allocation, before any oversubscription or compute is involved.

## What was ruled out (each with its own run)

| hypothesis | test | verdict |
| --- | --- | --- |
| oversubscription-specific | 8000 MiB (**under** the 12 GiB card) | still hangs — not it |
| the compute gate (submission revocation) | `iters=0`, alloc **only**, no kernel, no submission | still hangs — gate revokes only frontend maps; this path submits nothing |
| a missing UVM ioctl | nvproxy registers 62 UVM ioctls incl. `MIGRATE`, `MAP_EXTERNAL_ALLOCATION`, `VALIDATE_VA_RANGE` | surface is complete — not a missing handler |
| size / KVM tail-page | 2000 MiB also livelocks; native 20 GiB works | size-independent; not the `<6.13` tail-page `SIGBUS` bug |

The strace fingerprint (the loop that never converges):
```
ioctl(0x9 /dev/nvidia-uvm, 0x48 /*UVM_VALIDATE_VA_RANGE*/, …) = 0   (725ns)
mmap(0x7f7862000000, 0x7d000000, RW, SHARED|FIXED, /dev/nvidia-uvm, 0x7f7862000000) = 0x7f7862000000
…repeated forever on the same address and length…
```
The managed mmap "succeeds" (returns the fixed address) and the validate
"succeeds" (0), yet the CUDA runtime's populate loop never advances — it
re-validates and re-maps the same range indefinitely. The mapping nvproxy hands
back (via `uvm_mmap.go` `ConfigureMMap`/`AddMapping`/`Translate`) is evidently
not what the runtime treats as "range now backed," under KVM where the app's
guest VA and the host `nvidia-uvm` module's view of the range differ. Root cause
inside nvproxy's UVM mmap path, not yet pinned to a line.

## Native baseline (seeds S4)

One tenant, 20 GiB managed on the 12 GiB card, native runc:
- `cudaMallocManaged 20000MiB -> no error`; `mem_get_info` free=11603 total=11790 MiB.
- First sweep (cold, pages in ~8 GiB): 10.3 s → **2.0 GB/s**. Second: 13.0 s → 1.6 GB/s.
- Warm steady state: 3.06–3.12 s → **~6.7 GB/s**, stable across iters, `DONE ok`.

This reproduces GVM's motivating observation directly: native UVM paging is
**slow** (their measured 3.4 of 16 GB/s PCIe) — the exact inefficiency GVM's
huge-page + overlapped-swap work targets.

## Root cause, characterized (follow-up dig into `uvm_mmap.go`)

The livelock is **not** "managed memory is broken" — it is a clean, bisected
**1 GiB threshold**:

| managed alloc | result | placement |
| --- | --- | --- |
| ≤ **1024 MiB** (64/256/512/1024) | works end to end (alloc + kernel touch + `DONE ok`) | low VA, `0x2xx000000` |
| ≥ **1025 MiB** (1025/1152/1280/1536/2000) | livelocks in `cudaMallocManaged` | high VA, `0x7f…` |

The boundary is exactly 1 GiB (1024 MiB works, **1025 MiB** does not — off by one
MiB). 1 GiB is the GMMU's page-directory coverage with 2 MB big pages
(512 × 2 MB), and CUDA's allocator switches large managed allocations to their
own **high-VA region**: the 1025 MiB case maps `0x7f671e000000`, length
`0x40100000` (=1025 MiB), offset==addr, and re-`mmap`+`UVM_VALIDATE_VA_RANGE`es
that single region ~1.6M times without converging. Small allocations stay at low
VAs (`0x206c00000`) and validate once.

What the code does right, verified: the uvmFD sets `RequireAddrEqualsFileOffset`
(`uvm.go:66`) and `GenericProxyDeviceConfigureMMap` forces `PlatformEffectPopulate`
+ `RequirePlatformEffect`, so the host `/dev/nvidia-uvm` mapping is established
eagerly at host addr == file offset == the app VA (`mapInternalGap` uses
`MAP_FIXED_NOREPLACE` at `newRange.Start`). Setup ioctls all succeed
(`UVM_INITIALIZE`, `UVM_MM_INITIALIZE`, `UVM_ALLOC_SEMAPHORE_POOL`,
`UVM_MAP_EXTERNAL_ALLOCATION`), the app `mmap` returns the requested address, and
no `failed to map range`/`EEXIST` warning is logged.

**Collision hypothesis — TESTED AND KILLED (instrumented build, 2026-08-21).**
The first hypothesis was that `RequireAddrEqualsFileOffset` makes the Sentry map
the host uvm fd at host addr == the app's VA, and that a *high* VA (`0x7f…`)
collides with the Sentry's own address space so the `MAP_FIXED_NOREPLACE` in
`mapInternalGap` EEXISTs. I instrumented `mapInternalGap`
(`fsutil/mmap_precise_file.go`) and `uvmFD.AddMapping`/`Translate`
(`nvproxy/uvm_mmap.go`), rebuilt runsc, and ran the 1536 MiB case. Result:

- **`mapInternalGap` is called ZERO times.** The host `MAP_FIXED_NOREPLACE` at
  addr==offset is **not on the populate path at all** under `--platform=kvm`. So
  `RequireAddrEqualsFileOffset` and any Sentry address-space collision are
  irrelevant to this livelock — the hypothesis is disproven. (`MapInternal` is
  only used for Sentry buffered I/O, which this path never triggers.)
- **What the livelock actually is:** `uvmFD.AddMapping` + `Translate` for the
  managed region `0x7fa270000000-0x7fa2d0000000` (exactly 1536 MiB) are called
  **~2.5 million times**, each `AddMapping` reporting the **full 1536 MiB as
  newly-mapped** (`newlyMappedBytes=1610612736` every time). That is the guest
  application re-`MAP_FIXED`-ing the same high-VA range in a tight loop —
  RemoveMapping (release) + AddMapping (re-charge) churn — because
  `UVM_VALIDATE_VA_RANGE` never confirms the range. The small setup region
  (`0x206c00000`, 2 MB) is `AddMapping`'d once and proceeds.

**The platform is `systrap`, not KVM (corrected).** sensai's GPU sandboxes run
`--platform=systrap` — no `--platform` in `/etc/containerd/runsc.toml` (default),
and `dmd` is not in the `kvm` group; the failing 1536 MiB pod's boot log says
`Platform: systrap`. So this livelock is a **systrap** behavior, and my earlier
"KVM populate" phrasing was wrong.

**Revised leading hypothesis:** under systrap the application runs in a **stub
process**, and the Sentry services its `/dev/nvidia-uvm` mmap by mapping the host
uvm fd into the *stub's* address space — while the UVM context itself (the fd,
`UVM_INITIALIZE`, the ioctls) belongs to the **Sentry**. For small / low-VA
managed regions that split is tolerated and works; for a >1 GiB region CUDA places
at a high VA (`0x7f…`), the CPU mapping the stub holds and the UVM context the
Sentry holds evidently don't line up the way `UVM_VALIDATE_VA_RANGE` needs, so it
never confirms and the app re-`MAP_FIXED`es forever. This is the **same class** as
the already-recorded systrap limitation for AMD KFD — "`/dev/kfd` mappings are
impossible on systrap … KFD binds each mapping to the process holding the KFD
context, and systrap maps from a stub process, so `mmap` returns `EINVAL`; KVM is
the platform to use" (CLAUDE.md Next #6 / task #18) — except UVM degrades
*gradually* (small managed works, large livelocks) rather than failing outright.

**KVM does NOT avoid it — TESTED, hypothesis disproven (2026-08-21).** Enabled
`allow-flag-override` in `runsc.toml` (then reverted), forced
`dev.gvisor.flag.platform: "kvm"` on the pods, and confirmed `Platform: kvm` in
the boot log. Result: identical to systrap — **512 MiB works** (`DONE ok`, kernel
touch, 280 GB/s warm) but **1536 MiB livelocks** (no alloc return, GPU idle). The
1 GiB threshold is **platform-independent**. So the systrap stub-process reasoning
was wrong: the bug is *not* the platform's populate/mapping mechanism (systrap
stub vs KVM Sentry), because both fail identically at the same boundary.

**Where the bug actually is.** It is common to both platforms yet absent
natively, so it is in **nvproxy's UVM handling** (the platform-independent ioctl
interception) or the ghost 610 driver — triggered by the >1 GiB / high-VA managed
region. Native (runc) maps 20 GiB fine, so the driver itself handles large managed
regions; something nvproxy does or omits for the big region makes the forwarded
`UVM_VALIDATE_VA_RANGE` never confirm (it returns ioctl 0, but the range isn't
GPU-valid), so CUDA re-`MAP_FIXED`es forever on both platforms. The 1 GiB boundary
= GMMU PDE coverage (512 × 2 MB big pages), where CUDA switches large managed
allocations to a distinct high-VA arena and a different ioctl pattern (multiple /
larger `UVM_MAP_EXTERNAL_ALLOCATION` / `UVM_CREATE_EXTERNAL_RANGE` calls). The next
dig is that forwarded ioctl pattern for the >1 GiB region — compare the exact UVM
ioctl/param sequence a working ≤1 GiB alloc issues against a failing >1 GiB one,
in nvproxy — since platform is now ruled out.

## S3 — native UVM eviction is global, and it wrecks an innocent tenant (2026-08-21, post-fix)

With the fix in place, S3 is now measurable, and it is decisive. Two uncapped
gVisor sandboxes on the 12 GiB card:

- **A** — 4 GiB managed, hot (touched every sweep). Alone: **281 GB/s** steady,
  fully resident.
- **B** — 16 GiB managed, oversubscribed, hot. Pages at ~3.5 GB/s (PCIe-bound).

When B starts, A collapses **281 → 218 → 130 → 13.5 GB/s — a ~20x hit** — even
though A is 4 GiB on a 12 GiB card, well within any fair share. The driver's
native UVM eviction is a **global LRU**: B's 16 GiB working set evicts A's
resident pages, so A re-faults from host on every sweep. GPU 100% busy, 11789 MiB
resident, the rest paged.

**This is the justification and the specification for Phase 2.** Sentry-side
admission (Phase 1) bounds each tenant's *reservation*, but *residency* is the
driver's, and today it is unpartitioned. Per-tenant eviction (GVM's per-container
CLOCK: evict a tenant's own pages when it exceeds its device-resident share,
rather than the globally coldest page) is required to keep A's resident share
intact while B oversubscribes. Without it, overcommit is unsafe for latency-
sensitive tenants — one oversubscriber is a 20x noisy neighbour.

## S2 status

S2 (does `memquota.go:reserveUVMVA` refuse an oversubscribing reservation before
the driver) is subsumed: Phase 1 changed exactly that admission path, and the
end-to-end test above (5000 MiB admitted where the hard cap refused it, 7000 MiB
denied at gmem+hmem) confirms the Sentry is the admission authority.

## Implication for the plan — a prerequisite appears

The plan's managed-first Phase 1 assumed we could lean on the driver's **native
UVM eviction** and add only Sentry policy. That assumption fails at step 0:
**managed memory does not function under gVisor here.** So before any overcommit
policy work, there is a hard prerequisite:

- **P0 (new): make `cudaMallocManaged` work under gVisor+nvproxy on 610.** Root-
  cause the `UVM_VALIDATE_VA_RANGE`/`mmap` livelock in `uvm_mmap.go` (+ the UVM
  fault/validate path). Depth unknown — could be a small mmap-semantics fix, or a
  genuine gVisor/KVM UVM-fault incompatibility (the GPU raises replayable faults
  serviced by the host module against the Sentry's mm, not the guest app's).

Only after P0 do the original Phase-1 items (relax `admitLocked` for
oversubscription, split `gmem`/`hmem`, dynamic limit) become testable. And note
the strategic squeeze the spike sharpens: **neither** overcommit path is free
under gVisor today — managed memory is broken (P0), and non-managed `cudaMalloc`
is pinned and unpageable (the deep Phase-3 driver item). Overcommit needs one of
those two doors opened first.

**Go/no-go:** NO-GO on managed-first as written until P0 is understood. The cheap
spike did its job — it found the blocker before a line of overcommit code.
Recommended next step: a focused root-cause of the UVM mmap livelock (is it
nvproxy's `Translate`/`AddMapping` for MAP_FIXED UVM ranges, or the
validate/fault path under KVM?), which also tells us whether managed memory can
work under gVisor at all — a prerequisite worth knowing independent of overcommit.

## Reproduce

```
# build (nvcc via a k3s devel-image pod, compute_90 PTX)
nvcc -O3 -gencode arch=compute_90,code=compute_90 uvm_oversub.cu -o uvm_oversub
# native: works
runtimeClassName: nvidia ; command: /work/uvm_oversub 20000 8 nat   -> DONE ok
# gVisor: livelocks in cudaMallocManaged (any size), namespace unlabelled so nvproxy uncapped
runtimeClassName: gvisor ; command: /work/uvm_oversub 2000 0 strc   -> hangs; strace shows the loop
```
Spike pods: `~/.claude/jobs/*/tmp/overcommit/spikepod.sh` (gVisor, ns `overcommit`,
deliberately not `gvisor`-labelled so the webhook injects no cap). Enable
`dev.gvisor.flag.strace: "true"` + `strace-syscalls: "ioctl,mmap,futex"` to see
the loop.

## Phase 2 validated on hardware — per-tenant eviction protects residency (2026-08-21)

The eviction driver (driver branch `gpu-overcommit-evict`) + the Phase-2a Sentry
wiring were built, loaded on the RTX 5070, and run end to end. Repeat of S3, now
with a 4 GiB gmem cap on each tenant (nvproxy issues `UVM_SET_GMEM_LIMIT` from the
quota), a 3 GiB hot tenant A (under its cap) beside a 16 GiB oversubscriber B
(over its cap):

| eviction policy | A alone | A while B oversubscribes |
| --- | --- | --- |
| global LRU (before Phase 2) | 281 GB/s | **13.5 GB/s** — A evicted (~20x) |
| per-tenant (Phase 2) | 281 GB/s | **~131 GB/s** — A resident (~2x) |

Per-tenant eviction keeps A **resident** (stable ~131 GB/s, ≫ the 3.5 GB/s paging
speed, so not evicted) instead of collapsing to disk speed — a **~10x**
improvement in the innocent tenant's floor under an oversubscriber. The driver
evicted the over-budget tenant B, not A.

The residual drop from 281 to 131 is **memory-bandwidth contention** from B's
paging DMA, not eviction — a stable plateau, and A is clearly resident. Residency
isolation does not isolate HBM/PCIe bandwidth, the same limit CU masks hit on the
AMD side. So Phase 2 delivers what it targets (residency), and bandwidth
isolation under a paging neighbour remains a separate, open problem.

One bug found and fixed while loading it: the Sentry issues `UVM_SET_GMEM_LIMIT`
itself, and its own seccomp filter allow-lists UVM ioctls explicitly, so the new
op has to be added to `uvmIoctlFilters` or the Sentry is killed with SIGSYS on
UVM init (commit on `gpu-overcommit`). The eviction driver is a backward-
compatible superset of the working ghost driver: with no gmem cap set it uses the
original global eviction, so slicing workloads are unaffected while it is loaded.

## Adversarial: a memory PACKING attack defeats per-tenant eviction (2026-08-21)

The V4 compute-packing attack has a memory analog under overcommit, and it works.
The driver's gmem cap is set **per va_space**, but under gVisor each sandboxed
*process* opens its own UVM fd → its own va_space, and nvproxy issues
`UVM_SET_GMEM_LIMIT(gmem)` for **each** (in `uvmInitialize`). So a sandbox that
forks N processes gets **N va_spaces each capped at the full gmem** — N× the
protected residency the tenant is entitled to.

Measured on the RTX 5070 (gmem=4 GiB, hmem=16 GiB), a 3 GiB hot victim beside an
adversary making the **same ~11 GiB total** managed allocation:

| adversary shape | adversary resident | victim bw |
| --- | --- | --- |
| 1 process (11 GiB, over its 4 GiB cap) | capped at 4 GiB, rest paged | **281 GB/s** (protected) |
| 3 processes (3×3.7 GiB, each under 4 GiB) | ~11 GiB, none evicted | **~63 GB/s**, dips to 5.9 (starved) |

One process at 11 GiB is *over* its cap, so per-tenant eviction pages it down to
4 GiB and the victim is untouched. The same 11 GiB split across 3 processes is
three va_spaces each *under* their 4 GiB cap, so `get_over_budget_allocated_chunk`
finds no over-budget tenant, eviction falls back to the global LRU, and the
adversary holds ~11 GiB resident — squeezing the victim ~4.5x (real-time: the
victim recovered to 281 the instant the attack stopped).

**Root cause, same shape as V4:** the protection unit (va_space = process) is
finer than the tenant (sandbox), and packing multiplies the units. The Sentry's
admit-before-forward cap *does* bound the sandbox's total *reservation* at
gmem+hmem, but the driver-side *residency* protection is per-va_space, so packing
converts reservation headroom (hmem) into extra protected residency.

**Fix direction:** account residency per **tenant group**, not per va_space. The
Sentry already knows every va_space of a sandbox (it forwards each UVM_INITIALIZE
on the same nvproxy), so it can pass a per-sandbox group id with
`UVM_SET_GMEM_LIMIT`; the driver sums `gmem_resident_bytes` across the group and
treats the group as over budget when the sum exceeds the shared gmem cap. This is
the exact memory analog of the credit scheduler's per-tenant grouping that closed
V4 on the compute side. Until then, per-tenant eviction protects against a
single-process oversubscriber but not a packing one.

## Fix: per-tenant-GROUP accounting closes the packing attack (2026-08-21)

Implemented per-tenant-group residency in the driver (branch
`gpu-overcommit-evict`): `UVM_SET_GMEM_LIMIT` gains a `group` id, va_spaces
sharing an id share one resident total + cap, and nvproxy derives the id per
sandbox from the container ID (FNV-1a) so all of a sandbox's processes are one
group. Loaded on the RTX 5070 and re-ran the packing test (3 processes ×
3.7 GiB, each under the 4 GiB va_space cap):

| | victim under the packed adversary |
| --- | --- |
| per-va_space eviction | steady ~63 GB/s (attack succeeds) |
| per-tenant-group eviction | oscillates **281 ↔ 63**, peaks at full 281 |

The group is now summed (~11 GiB) against one 4 GiB cap, found over budget, and
its processes are the ones evicted (all paging at ~1.3 GB/s) — so packing no
longer multiplies protection, and the victim reaches full 281 GB/s, which is
impossible without the fix. But protection is **not yet steady**: with three
processes re-faulting their working sets, the group overshoots its cap faster
than the single-victim-per-call eviction drains it, so the victim swings between
protected (281) and contended (63), averaging ~134 vs the steady ~63 without the
fix. So the mechanism is correct (packing defeated) but the holding is loose.

Follow-up to make it steady: evict more aggressively from an over-budget group
(a per-group CLOCK list, or draining the group to its cap in one eviction pass
rather than one chunk per allocation-driven call), and tighten accounting
precision under churn. The seccomp lesson recurred: `UVM_SET_GMEM_LIMIT` must be
allowed under any capability set (like the other nvproxy-issued UVM ioctls), and
listed in the filter-count test's nvproxyOnlyUVMIoctls.

### Steady holding needs a proactive evictor, not a reactive tweak

Tried to steady the oscillation with eviction hysteresis (keep an over-budget
group as the eviction target until drained to 3/4 of its cap). It did not help —
the victim still swung 63 ↔ 282. The decisive comparison: a **single-process**
oversubscriber holds the victim steady at 281 (S3), while **three processes in
one group** oscillate. So this is not accounting drift (the one-process case
proves detection + accounting work) — it is **eviction rate**: three fault
streams re-fill the group's excess faster than the reactive,
one-root-chunk-per-allocation eviction can drain it, so the group overshoots its
cap in bursts and the victim's pages are caught in the global-LRU fallback during
those bursts.

The reactive PMM eviction (triggered only when PMA is full) is structurally
unable to pin a continuously-faulting oversubscriber at a tight cap. The fix is
the one GVM uses: a **proactive per-tenant background evictor** — a per-GPU
kthread that periodically scans groups and, for any group over its cap, evicts
its chunks down to the cap using the existing `evict_root_chunk` machinery,
independent of PMA pressure. That keeps over-cap groups pinned at their cap, so
the card always has free space for within-cap tenants and the victim stays
resident (steady 281). Scoped but non-trivial (kthread lifecycle + a drain loop
holding the right locks); left as the next step. The reactive group accounting
already lands the security-relevant result — a packed tenant is capped as one and
can no longer masquerade as N — so this is a stability refinement, not a hole.

### Proactive evictor: residency is held steadily; bandwidth contention is fundamental

Added a per-GPU background kthread (driver branch, evictor commit) that drains
over-cap tenant groups every 4 ms, independent of PMA pressure. Instrumented it
on the RTX 5070 (diagnostic since removed) to settle the oscillation's cause. The
data is decisive:

- Adversary group: counted resident **~8000 MiB** vs its **4096 MiB** cap — held
  there stably (down from filling the whole 12 GiB card without the evictor).
- Victim group: counted resident a **flat 3000 MiB** the whole run — **never
  evicted**.
- Evictor: `drained=16` every pass (its per-wake budget).

So **accounting is exact (no drift), and the victim's residency is protected
steadily** — the whole point. The evictor cannot force the adversary all the way
to 4 GiB because fault-in (CPU→GPU) and evict-out (GPU→CPU) are both
PCIe-bandwidth-limited at equal rates — a fundamental tug-of-war, not a bug — so
the adversary thrashes at ~8 GiB. The victim's remaining bandwidth dip (63↔279)
is therefore **not eviction** (it stays resident) but **memory-bus/PCIe
contention** from the oversubscriber's thrashing, which residency isolation does
not address — the same limit CU masks hit on the AMD side.

Reframed conclusion: overcommit's residency guarantee is delivered and steady
(a packed tenant is capped as one and cannot evict a neighbour); bandwidth
isolation from a thrashing oversubscriber is a distinct, out-of-scope problem
(it would need bandwidth partitioning or throttling the oversubscriber's fault
servicing, not more eviction).

## How much can we practically oversubscribe? (2026-08-21, RTX 5070, gVisor)

The one number everyone asks for, measured. One uncapped gVisor sandbox
(`--nvproxy-gpu-memory-limit=0`), `cudaMallocManaged` of TOTAL, whole buffer
populated once, then a timed loop touching only the first HOT MiB each pass
(the working set). Card is 12227 MiB. `uvm_oversub.cu` (`~/overcommit`) grew a
4th `HOT_MiB` arg for this.

**Regime A — working set = whole allocation (touch everything):**

| total (× card) | warm bandwidth |
| --- | --- |
| 8000 MiB (0.65×) | **275.2 GB/s** (fits, full HBM) |
| 12000 MiB (0.98×) | **6.0 GB/s** |
| 16000 MiB (1.31×) | 5.8 GB/s |
| 18000 MiB (1.47×) | 5.8 GB/s |

**Regime B — working set fixed at 8000 MiB (< card), allocation grows:**

| total (× card) | hot | warm bandwidth |
| --- | --- | --- |
| 12000 MiB (0.98×) | 8000 | **275.5 GB/s** |
| 16000 MiB (1.31×) | 8000 | 275.4 GB/s |
| 20000 MiB (1.64×) | 8000 | 275.1 GB/s |

**The limit is the working set, not the allocation.** The cliff sits exactly at
the card: the instant the *actively-touched* set exceeds VRAM, throughput falls
**~46×** (275 → ~6 GB/s, i.e. PCIe paging speed) and stays flat there no matter
how much further you go — you are simply paging every sweep. But as long as the
hot set fits in VRAM, the *total commitment* oversubscribes essentially for free:
**20 GiB committed on a 12 GiB card ran at full 275 GB/s** because the 12 GiB
cold tail sat on host RAM costing nothing while untouched. The practical ceiling
on total commitment is host RAM (30 GiB here), not the card.

**Practical rule.** Size `gmem` ≈ each tenant's hot working set and `hmem` = the
cold overflow you want to allow. Overcommit buys capacity for cold/rarely-touched
memory — idle models, large sparse tables, checkpoints, a paused tenant — at zero
throughput cost, not room to grow a hot working set (that thrashes at ~6 GB/s).
Two more bounds stand from the sections above: this is **UVM/managed memory
only** — `cudaMalloc` device memory (vLLM/PyTorch's default path) is still
hard-capped at `gmem` and cannot page (Phase 3) — and a tenant whose hot set
*does* exceed VRAM thrashes at PCIe speed; the per-tenant evictor keeps that
thrashing from stealing a well-behaved neighbour's residency, but not its
memory-bus bandwidth.
