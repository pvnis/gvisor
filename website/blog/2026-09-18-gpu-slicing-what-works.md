# One GPU, Many Tenants, No Trust

Three earlier posts covered pieces of this: [dividing an NVIDIA GPU from the
Sentry](/blog/2026/08/07/slicing-a-gpu-between-untrusted-containers/), [making
the GPU enforce its own compute
shares](/blog/2026/08/19/making-the-gpu-enforce-its-own-compute-shares/), and
[the AMD side](/blog/2026/08/25/the-amd-side-space-time-and-no-driver-broker/).
This one is the short version of the whole thing: what it takes to give two
mutually-untrusting containers a guaranteed slice of one GPU, what that
actually costs, and which parts are still open.

<!--/excerpt-->

## One constraint decides the design

**Nothing may depend on code inside the container.** Every limit is enforced in
the Sentry, where ioctls are interpreted, and a hostile container cannot reach
it.

That sounds like a stylistic preference until you measure the alternative. The
standard way to share an NVIDIA GPU in Kubernetes puts a limiter library inside
the container via `LD_PRELOAD`. A limiter living in the address space of the
process it limits is reachable by that process — and this one switches off with
a documented environment variable. Measured: a pod requesting 512 MiB was held
to 512 MiB with the library enforcing, and took **768 MiB of a 12 GiB device**
with `CUDA_DISABLE_CONTROL=true` set in its own pod spec.

So the container-side limiter was useful as a statement of *what* to enforce,
and never as a mechanism. Everything below runs in the Sentry or in a trusted
host component.

## Memory is the solved half

Both proxies admit-before-forward on the allocation path: the Sentry accounts a
request against the sandbox's quota and only then passes the ioctl to the
driver. Over-limit allocations fail the way a genuinely full GPU fails —
`NV_ERR_NO_MEMORY`, surfacing as `CUDA_ERROR_OUT_OF_MEMORY`.

The part that matters for real frameworks is that the sandbox is also *told* the
truth: `cuMemGetInfo` and the AMD topology are rewritten to report the quota
rather than the device, so PyTorch and vLLM size themselves to what they can
actually get. A pod holding 2 GiB of a 46 GiB A6000 sees 2048 MiB in
`nvidia-smi`, not 46068.

This works on every GPU we have tested, on both vendors, and is independent of
everything that follows.

## Compute is the hard half, and the vendors diverge

On both vendors, submitting work to an existing channel **never enters the
kernel** — a command written into a mapped ring buffer, a doorbell rung through
a mapped register. The Sentry sees no ioctl per kernel launch, so it cannot
meter submission on either side.

What differs is what the Sentry can reach *around* submission.

**AMD: the levers are ioctls the Sentry already interprets.** A queue's CU mask
is an argument to queue creation, and the hardware honours it thereafter — a
genuine spatial partition, set at the ioctl boundary. Time-slicing is
`KFD_IOC_DBG_TRAP_SUSPEND_QUEUES`, equally an ordinary `/dev/kfd` ioctl. Both
work on the **stock driver**, with no vendor patch. Two tenants asking 75/25 of
the device measure **3.05:1**, and CWSR means AMD can preempt mid-kernel, which
NVIDIA cannot.

**NVIDIA: the levers are admin-gated, so they need a trusted host component.**
The controls that actually divide an NVIDIA GPU — the work-distributor partition
and the hardware runlist — are resource-manager controls the Sentry may not
issue. Enforcement therefore lives in a host daemon (`runsc gpu-scheduler`)
driving the GPU's own runlist scheduler through the driver. The container still
cannot reach it; it simply is not the Sentry.

The general lesson: the two proxies share nearly all of the gVisor machinery,
and where they behave differently it is almost always the *driver* that differs,
not gVisor.

## The adversarial result that shaped the NVIDIA design

A share mechanism is only interesting if it survives a tenant trying to cheat.
The attack that matters is **process packing**: a tenant simply forks.

The GPU's runlist timeslice binds a *channel group*, and one tenant owns many.
Encode a tenant's weight as its per-group timeslice, and a tenant's share tracks
`(groups × timeslice)` — so forking multiplies it. Measured on the full stack, a
tenant granted **one quarter** of the GPU ran four processes in its own pod and
took **more throughput than a peer weighted three times higher**: a requested
3:1 split delivered 0.78:1, with the aggregate conserved. That is theft, not
gap-filling.

The fix is not to divide the timeslice — we tried, and it cost 42% of aggregate
throughput for an incomplete fix. It is to stop encoding weight in the timeslice
at all. Following the design in the [GVM
paper](https://github.com/ovg-project/GVM), weight instead drives **credit
accrual per tenant**: every tenant runs at the same large quantum, consumed time
is charged back to the tenant, and a tenant that overdraws is detached from the
runlist until it recovers. A tenant with fifteen channel groups burns one credit
pool fifteen times faster and gains nothing by forking.

gVisor makes this cleaner than it is natively: every process in a sandbox shares
one Sentry host PID, so *the tenant is unambiguous*. The same property that hid
the attack from a PID-keyed scheduler is what makes per-tenant accounting
natural.

Measured, same attack, after the change:

| | attacker's share | granted |
| --- | --- | --- |
| weight-as-timeslice | **more than its victim** | 25% |
| credit scheduling, A6000 | 31% | 25% |
| credit scheduling, RTX 5070 | 27% | 25% |

Blunted rather than perfect — the residual edge is real and we say so — but the
theft is gone, verified on three dies (GA100, GA102, GB205).

## What it costs, and why there is no single number

The honest answer to "what does enforcement cost?" is *it depends on whether the
GPU was already saturated*, and that is a property of the workload and the die,
not of the scheduler.

- **A lone tenant pays nothing.** 139.3 matmul/s with the scheduler running
  against 139.4 without it.
- **On an RTX 5070, two tenants cost nothing either** — one tenant already
  saturates that GPU, so aggregate under enforcement equals the single-tenant
  baseline exactly.
- **On an A6000, two tenants under enforcement land ~20% below two tenants
  without it** — because unenforced tenants there *beat* the single-tenant rate
  by interleaving and filling each other's gaps. Enforcing a share serializes
  them and gives that bonus up.

So what you trade is not overhead; it is an **overlap bonus that only exists
when the device has slack**. The same rule appeared independently on the AMD
side, where time-slicing *gained* 34% aggregate on bandwidth-saturated tenants
and *cost* 28% on two vLLM tenants that were complementing each other.

That framing took a wrong turn to reach: we first reported the A6000 number as
20% scheduler overhead, which it is not. Measuring the single-tenant baseline —
the run everyone skips — is what distinguishes the two.

## Where it stands

Working and measured on hardware, both vendors: memory quota with truthful
reporting; weighted compute division that binds real ML workloads; per-tenant
isolation verified adversarially from inside a tenant holding only its own API
token — quota escalation refused, sandbox-escape-by-runtime-class refused, no
peer visibility. Kubernetes integration uses **upstream HAMi** for placement,
with an admission webhook deriving each sandbox's quota from the request the
scheduler admitted, so the limit is not optional.

Still open, and worth being direct about: the residual packing edge above;
`--measure-usage` remains defective and off by default; the NVIDIA path needs
the open-driver fork for the runlist broker, which is a real deployment cost the
AMD path does not have; and per-tenant compute isolation on NVIDIA is a property
of the *die*, so it must be re-measured on each new GPU rather than assumed.

The most transferable lesson is methodological. Four separate conclusions on
this project were wrong in the same direction — a control returning an error
code read as "the hardware cannot do this," when the probe had been issued from
the wrong place or was missing a companion call. Each was settled the same way:
by driving the complete mechanism and reading *throughput*, not status codes. If
a claim about a GPU matters, there is almost always a reproducer that can decide
it.
