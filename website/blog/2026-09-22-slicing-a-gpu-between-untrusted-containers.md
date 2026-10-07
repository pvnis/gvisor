# Slicing a GPU Between Untrusted Containers

GPUs are expensive and, for most workloads, mostly idle. Sharing one between
several tenants is the obvious way to make it pay for itself, and every existing
answer either requires particular hardware, gives up isolation, or puts the thing
doing the limiting inside the container being limited.

This post covers a set of changes to gVisor that divide a GPU's memory and
compute between mutually-untrusting sandboxes — on both NVIDIA and AMD — with all
of the enforcement somewhere the workload cannot reach. It covers what the
hardware makes easy, what it makes very hard, what the result costs, where the
two vendors turn out to be mirror images of each other, and the several
conclusions we got wrong along the way.

<!--/excerpt-->

## One constraint decides the design

**Nothing may depend on code inside the container.** Every limit is enforced in
the Sentry — where ioctls are interpreted — or in a trusted host component. A
hostile container participates in its own limiting nowhere.

That sounds like a stylistic preference until you measure the alternative.
[HAMi](https://project-hami.io) does the most complete job of GPU sharing in
Kubernetes today, and it splits cleanly in two. Its placement half is genuinely
good: a scheduler that understands `nvidia.com/gpumem` and `nvidia.com/gpucores`
and packs pods so they fit. Its enforcement half is `libvgpu.so`, `LD_PRELOAD`ed
into the container to intercept the CUDA API — living in the address space of the
process it is meant to constrain. Setting aside `dlsym`, patching the GOT, or
issuing ioctls directly, the library reads an environment variable,
`CUDA_DISABLE_CONTROL`, that turns it off.

Measured on a live cluster, the same pod three ways:

| libvgpu | annotation | device size seen | outcome |
| --- | --- | --- | --- |
| loaded | none | 512 / 512 MiB | refused at 512 MiB |
| dropped | none | 11347 / 11790 MiB | **allocated all 768 MiB** |
| dropped | 512 MiB | 350 / 512 MiB | refused at 320 MiB |

Row two is HAMi with its enforcement removed and nothing in its place: the
container sees the whole card and takes what it wants. Row three is the Sentry
doing the work — the container sees its quota and is held to it by something it
has no access to.

A control the workload can reach is not a control. So: keep HAMi's placement,
drop its enforcement, and rebuild that half where the container cannot follow.

## Memory is the tractable half

GPU memory allocation *does* go through ioctls, and both proxies already see
every one of them. The accounting is admit-before-forward: a request is charged
against the sandbox's quota *before* the ioctl reaches the driver, so two
concurrent allocations cannot both be admitted against the same headroom, and
the charge is returned if the driver refuses.

The details that took real work are the ones where the obvious approach is
subtly wrong:

- **Unified memory cannot be accounted where it is committed.** UVM populates
  device memory in response to GPU page faults serviced inside the host
  `nvidia-uvm` module, which calls the resource manager through in-kernel symbols
  rather than ioctls. The Sentry never observes the commitment — so it charges
  the *address-space reservation* instead. Sound as a bound, but not tight: a
  workload that deliberately oversubscribes is charged for what it reserved.
- **Charges are reference counted**, because `NV_ESC_RM_DUP_OBJECT` aliases an
  existing allocation rather than committing more memory, and they are released
  from `objFree()`'s cascade rather than the free handler, because freeing an
  object frees its dependents and only that loop sees all of them.
- **Every allocation class must make an explicit accounting decision.** A new
  class added to the allowlist without one fails a test rather than silently
  going uncharged.

Because the Sentry does the accounting, it can also make the limit *visible* the
way a real device is. Both proxies rewrite what the sandbox reads —
`cuMemGetInfo` and NVML on NVIDIA, the synthesised KFD topology on AMD — so an
unmodified PyTorch or vLLM sizes itself to what it can actually get. A container
holding 2 GiB of a 12 GiB card reads 2 GiB total, not 12.

One ground-truth detail worth naming, because it is the shape of most ABI bugs
here: RDNA reports its VRAM heap as `heap_type 2` where CDNA uses `1`. An earlier
rewrite matched only `1`, leaving the sandbox seeing a quota-correct *free* next
to the whole device's *total*. Settled by compiling against the real header on
the hardware, which is how this project settles every ABI question.

Memory quota works on every GPU we have tested, on both vendors, and is
independent of everything that follows.

## Compute: submission hides from the kernel

The natural next move is to meter compute at the same boundary. It cannot be made
to work, on either vendor.

Once a channel exists, submitting work to it does not enter the kernel at all.
The application writes commands into a ring buffer it has already mapped and
rings a doorbell through a mapped register. That is the whole submission path. A
sustained run of **13,000 kernel launches produced zero ioctls** between context
creation and teardown.

So there is nothing to intercept, and no amount of cleverness in ioctl handling
changes that. Everything below is an argument about what you can reach *around*
submission — and that is exactly where the two vendors diverge.

### The Sentry gate, and the workload that defeats it

The first mechanism we built takes its idea from
[Krypton (USENIX ATC'25)](https://www.usenix.org/conference/atc25): although
submission never enters the kernel, it requires the ring buffer to be *mapped*.
Take the mapping away and the next write faults. So the Sentry revokes its own
mapping of the command buffer at the end of the sandbox's share of each period,
and holds the faulting task until its turn comes round.

Against a kernel-launch loop this divides cleanly — 76.2% / 51.0% / 25.8%
achieved against 75/50/25 configured. And it rests on a quiet assumption: **that
the workload rewrites the gated buffer on every submission.**

Real machine-learning workloads do not. On Volta and later, cuBLAS GEMM and —
decisively — CUDA graph replay, which is how an inference server like vLLM spends
essentially all of its time, replay a pre-recorded command buffer and ring the
doorbell. The commands were written once, at capture time. Steady-state execution
never touches them again.

Measured against a real graph-replaying workload: **roughly zero faults per
period.** The sandbox runs at full rate with its share nominally set to a
fraction. The gate enforces nothing.

That is worth dwelling on as a *testing* failure. The synthetic kernel-launch
loop that validated the gate is the one workload shape the gate works against.
The benchmark and the mechanism shared a blind spot, and the clean 76/51/26 table
was measuring the exception rather than the rule.

## The wrong turn, and three stacked mistakes

So we went looking for anything that could impose a share on a doorbell workload.
The GPU has scheduling machinery of its own — a runlist scheduler and a spatial
work distributor — reachable through admin-gated resource-manager controls a
container cannot issue but a trusted host component can. We tried each one. Every
one appeared to fail: timeslice controls round-tripped `NV_OK` and changed
nothing, a driver-level TSG detach did likewise, and the spatial controls
returned `0x57` even at kernel privilege.

We wrote that down carefully: consumer hardware honors none of these primitives;
compute isolation for arbitrary CUDA is a property of MIG-capable datacenter
silicon. It was thorough, well-documented, and wrong — three of our own mistakes,
stacked:

- **A misread status code.** `0x56` is `NV_ERR_NOT_SUPPORTED`. `0x57`, which we
  got, is `NV_ERR_OBJECT_NOT_FOUND`. The control had not been refused as
  unsupported; the firmware could not *find the object* it named.
- **The wrong call site.** Those controls route to the GPU's GSP firmware, which
  can only act on an object after it is registered. We issued them from the
  context-share *constructor*, before registration. Issued from the deferred
  scheduling path, the identical control returns `NV_OK`.
- **A missing commit.** The temporal controls really did round-trip `NV_OK` and
  do nothing, because a companion RPC — `RESTART_RUNLIST` — is what tells the
  firmware to act on a staged runlist edit.

Each mistake alone produces a plausible negative that reproduces every time you
run it. Together they produced a hardware conclusion out of three software bugs.

## NVIDIA: let the GPU divide itself

The reframe: you cannot intercept submission, but you do not have to. **The GPU
already has a scheduler**, and it acts *below* the submission path — so a
doorbell workload cannot dodge it the way it dodged the mapping gate.

Enforcement therefore moves into a **driver-resident broker**: a small extension
to the host's open kernel modules that issues these controls on each sandbox's
objects. It is the shape GVM ("OS-Level GPU Virtualization", Berkeley/UCLA)
independently arrived at on the A100, and it satisfies the governing constraint
more firmly than the Sentry gate did — the limiter is now in a different
privilege domain entirely. Under gVisor's KVM platform the driver attributes a
sandbox's GPU objects to the Sentry's host process, which is exactly the
per-sandbox handle the broker keys on.

Both axes work, measured on an **RTX 5070 (consumer Blackwell, GB205)** — the
card we had written off:

| Temporal (`SET_TIMESLICE` + `RESTART_RUNLIST`) | Result |
| --- | --- |
| two tenants, equal weights | 220.7 / 220.7 (1:1) |
| weights 3:1 | **330 / 110 (3.00:1)**, total conserved |
| stop the smaller tenant | survivor rises 330 → 457 (= solo) |
| three tenants, 3000/2000/1000 | 3:2:1, conserved |
| 2:1 / 6:1 / 16:1 requested | 2.03:1 / 6.15:1 / 13.3:1 |

| Spatial (`SET_TPC_PARTITION_TABLE`) | Result |
| --- | --- |
| 13 of 24 TPCs | 272 matmul/s |
| 18 of 24 | 372 matmul/s |
| 24 (full) | 458 matmul/s (= solo) |

The temporal division is proportional, conserved, reclaims an idle tenant's share
automatically, and stays linear to about 6:1 before compressing. The spatial
control returns `NV_OK` **and** confines throughput — a real partition, not a
control accepted and ignored.

One caveat measured later on the A100: the spatial control is a *ceiling*, not
concurrency. Two tenants on disjoint TPC halves measure the same as two on the
same half, because their CUDA contexts time-slice the engine regardless.

## AMD: both levers, in the Sentry, on the stock driver

AMD submits work the same way, so the interception wall is identical. What
differs is what the Sentry can reach around it — and on AMD both dividing
controls are ordinary `/dev/kfd` ioctls `amdproxy` already allowlists.

**Space — the CU mask.** A queue can be created with a bitmap of which compute
units its work may run on. `--amdproxy-cu-mask` sets the sandbox's ceiling; a
queue asking for everything is silently clamped, one asking outside the mask gets
`EINVAL`, and the hardware's command processor honors it thereafter with no
further decision from the Sentry.

This is a **hard, concurrent** partition, and that word is the whole difference
from NVIDIA's TPC table. Two tenants on disjoint CU halves run *at the same
time*. Measured on a Navi 32: a tenant's rate moved **−0.61%** when a neighbour
started underneath it and **+0.39%** when it left. That near-perfect isolation is
the strongest result on the project.

Two honest limits: granularity is **two CUs, not one** (RDNA pairs them, and a
mask splitting a pair makes every queue creation fail), and it partitions
compute-pipeline occupancy, **not memory bandwidth** — three vLLM tenants, whose
decode is bandwidth-bound, contend for VRAM even on disjoint CUs.

**Time — the debug interface.** The thing NVIDIA needed a kernel broker for, AMD
does from userspace with no driver patch. `KFD_IOC_DBG_TRAP` with
`SUSPEND_QUEUES`/`RESUME_QUEUES` normally requires `PTRACE_ATTACH` — but the
kernel skips that check when the target *is* the caller, and under KVM the Sentry
is itself the KFD process for the sandbox. So the Sentry opens a debug session on
itself and slices its own queues on a weighted duty cycle.

The hardware does something the NVIDIA gate never could: it **preempts
mid-kernel**. CWSR checkpoints in-flight waves and restores them on resume, so a
`vecadd` kernel stays `RESULT CORRECT` while being sliced at 25%.

| Configuration | Result |
| --- | --- |
| two tenants, equal weights | Jain 1.0000 |
| weights 300:100 | **3.05:1** |
| weights 500:100 | 5.13:1 |
| a neighbour leaving | survivor reclaims: 1400 → 3440 → 6850 |

**But you must choose.** On RDNA3 a queue can carry a CU mask **or** be
time-sliced, never both: the amdgpu driver refuses the debug session's CWSR
workaround on a queue carrying a user CU mask. That is a driver rule, not a
policy choice, read out of the running kernel's source rather than inferred, and
`amdproxy` rejects a sandbox configured with both at startup. Memory quota
composes with either.

## The adversarial test that reshaped the design

A share mechanism is only interesting if it survives a tenant trying to cheat.
The attack that matters is **process packing**: the tenant simply forks.

The GPU's runlist timeslice binds a *channel group*, and one tenant owns many.
Encode weight as the per-group timeslice and a tenant's share tracks
`(groups × timeslice)` — so forking multiplies it. Measured on the full stack, a
tenant granted **one quarter** of the GPU ran four processes in its own pod and
took **more throughput than a peer weighted three times higher**: a requested 3:1
split delivered 0.78:1, aggregate conserved. Theft, not gap-filling.

The naive fix — divide a tenant's timeslice across its groups — cost **~42% of
aggregate throughput** to context switching and only blunted the attack. The fix
that works is the one GVM's paper specifies: **credit accounting at tenant
granularity**. Weight drives credit accrual per tenant, every tenant runs at one
large uniform quantum, consumed time is charged back, and a tenant that overdraws
has its whole set of channel groups detached until it recovers. A tenant with
fifteen groups burns one credit pool fifteen times faster and gains nothing.

gVisor makes this cleaner than it is natively: every process in a sandbox shares
one Sentry host PID, so **the tenant is unambiguous**. The same property that hid
the attack from a PID-keyed scheduler is what makes per-tenant accounting
natural. It needs one small driver change — the runlist reports each tenant's
channel-group count, so charge-back is weighted by it.

Measured after the change on GA102 and GB205: packing a weight-25 tenant from 3
to 12 channel groups buys it nothing, and the honest ~3:1 holds. The residual is
real and worth stating — the packer still lands at 27–31% against a granted 25%,
blunted rather than perfect — but taking more than a peer weighted 3× higher is
gone.

## What it costs

The honest answer to "what does enforcement cost?" is *it depends on whether the
GPU was already saturated*, which is a property of the workload and the die, not
of the scheduler.

- **A lone tenant pays nothing.** 139.3 matmul/s with the scheduler running
  against 139.4 without it.
- **On an RTX 5070, two tenants cost nothing either** — one tenant already
  saturates that card, so aggregate under enforcement equals the single-tenant
  baseline exactly.
- **On an A6000, two tenants under enforcement land ~20% below two tenants
  without it** — because unenforced tenants there *beat* the single-tenant rate
  by interleaving and filling each other's gaps. Enforcing a share serializes
  them and gives that bonus up.

So what you trade is not overhead; it is an **overlap bonus that exists only when
the device has slack**. The AMD side shows the same rule from the other
direction: holding the debug session open costs **−43%** on an ALU-bound,
register-heavy burner and **−0.003%** on a bandwidth-bound one, because the cost
is resident-wave occupancy rather than throughput. For two contending
bandwidth-bound tenants, slicing even *beats* uncontrolled sharing.

That framing took a wrong turn to reach: we first reported the A6000 number as
20% scheduler overhead, which it is not. Measuring the single-tenant baseline —
the run everyone skips — is what distinguishes the two.

Costs that are real and specific:

- **The Sentry gate's `activeMu` stall.** While a sandbox is held, the task
  waiting to submit holds its address space lock, so other threads stall on
  anything needing it. An mmap-heavy thread fell to ~20% of its unlimited rate
  under a 25% cap; a compute-bound thread over resident memory was unaffected.
- **`--measure-usage` misprices the ordinary case.** The `nvidia-smi pmon`
  sampler that defends against long kernels attributes most of the GPU to
  whichever pod is being throttled, and the debt mechanism then throttles it
  further. Two identical pods at equal weights land at 31 and 618 launches/s with
  it on, against 324 each with it off. It stays off by default until understood.
- **The scheduler is a fail-closed dependency.** A GPU pod cannot start while
  `runsc gpu-scheduler` is down. Deliberate — failing open would mean an
  unlimited sandbox — but it is a single point of failure for every GPU pod on
  the node.

## Kubernetes, without the injection

None of this is useful in a cluster unless something translates what a pod *asks
for* into what `runsc` *enforces*. An in-tree mutating webhook does exactly that,
at admission:

| request | annotation written |
| --- | --- |
| `nvidia.com/gpumem` (MiB) | `nvproxy-gpu-memory-limit` (bytes) |
| `nvidia.com/gpucores` (percent) | `nvproxy-gpu-weight` |
| `amd.com/gpu-vram-mib` | `amdproxy-gpu-memory-limit`, or `amdproxy-gpu-weight` |

Nothing a pod requests is changed, so HAMi's scheduler places pods exactly as
before and gVisor needs no device plugin of its own. The annotations ride
gVisor's existing override ratchet: the value configured on the runtime is a
**ceiling an annotation may lower but never raise**. That matters because pod
specs are usually authored by the workload being limited — a weight is relative,
so treating it as a floor would let a container grant itself as much of the GPU
as it liked.

Two deployment properties are load-bearing rather than incidental.
`failurePolicy: Fail` is a security control, not an availability preference: a
pod admitted *without* being mutated carries no limit and runs at the node-wide
ceiling, so relaxing it to `Ignore` turns an unreachable webhook into a silent
quota escape. And the `libvgpu.so` preload is not merely silenced but removed —
blanking the ConfigMap key that installs it takes the host copy to zero bytes.

With time-slicing rather than CU masks, the AMD path needs no HAMi fork either: a
weight is an independent scalar with nothing to reconcile against other tenants,
so **upstream, unmodified HAMi v2.9.0** does the placement for both vendors.
Verified end to end with three honest vLLM tenants and one adversary
self-annotating the whole card and the maximum weight: the weighted division held
at 3.25:1 for a 3:1 request, the adversary was clamped to 9% against the 10% its
weight entitles it to, and the memory quota was still enforced in the Sentry.

## Where it stands

Working and measured on hardware, both vendors: memory quota with truthful
reporting; weighted, work-conserving compute division that binds real ML
workloads; a genuinely concurrent spatial partition on AMD and a spatial ceiling
on NVIDIA; per-tenant isolation verified adversarially from inside a tenant
holding only its own API token, with quota escalation and sandbox-escape-by-
runtime-class both refused.

Not done, and worth being direct about:

- The residual packing edge above — blunted, not eliminated.
- Long kernels still overshoot. A GPU cannot be made to abandon work already
  submitted, so this is mitigated by charge-back rather than solved.
- The NVIDIA path needs the patched open kernel modules for the broker — a real
  deployment cost the AMD path does not have.
- Checkpoint/restore does not work for CUDA sandboxes; fabric memory and EGM are
  unaccounted pending hardware.
- Per-tenant compute isolation on NVIDIA is a property of the *die*, so each new
  GPU gets measured rather than predicted. Predicting from the die class is
  precisely the mistake that cost us above.

## The lesson

The two vendors are mirror images of one constraint. On NVIDIA, submission hides
from the kernel *and* the dividing controls are admin-gated, so imposing a share
meant reaching past the Sentry into a driver broker — and even then the spatial
control is a ceiling rather than concurrency. On AMD the queue lifecycle stays in
ioctls the Sentry already interprets, so both a concurrent spatial partition and
a work-conserving temporal slicer live in the Sentry on a stock driver. Where the
two proxies behave differently it is almost always the *driver* that differs, not
gVisor.

The most transferable part is methodological. Several conclusions on this project
were wrong in the same direction — a control returning an error read as "the
hardware cannot do this," when the probe had been issued from the wrong place, or
was missing a companion call, or the status code meant something else. Each was
settled the same way: by driving the complete mechanism and reading *throughput*,
not status codes. The thing that finally cracked the NVIDIA side was not a
cleverer argument; it was reading one status code correctly and moving one call
three functions later.

Setup instructions for both vendors are in the
[GPU user guide](https://gvisor.dev/docs/user_guide/gpu/#kubernetes).
