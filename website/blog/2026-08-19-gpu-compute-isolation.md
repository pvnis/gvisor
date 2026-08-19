# Making the GPU Enforce Its Own Compute Shares

A [previous post](/blog/2026/08/07/slicing-a-gpu-between-untrusted-containers/)
described how gVisor's `nvproxy` divides an NVIDIA GPU's compute between
sandboxes by revoking, from the Sentry, the mapping through which a container
submits work — so its next submission faults and can be held until the
container's turn comes round. That mechanism is real, and it has a hole. The
hole opens under exactly the workloads people actually run on a shared GPU, it
took a chain of our own measurement mistakes to understand, and closing it meant
giving up on intercepting submission at all and letting the GPU's own scheduler
do the dividing — driven from a trusted component the container still cannot
reach.

This post is about that hole, the wrong conclusion we reached about it, and the
mechanism that actually imposes a compute share on an uncooperative CUDA
workload — measured on the very consumer GPU we had written off as incapable of
it.

<!--/excerpt-->

## Where the compute gate holds, and where it does not

To recap the mechanism: once a CUDA channel exists, submitting work to it does
not enter the kernel. The application writes commands into a ring buffer it has
already mapped and rings a doorbell through a mapped register. So the Sentry
cannot meter submission — but it can take the mapping away. The gate revokes its
own mapping of the command buffer at the end of a sandbox's share of each
period; the next write to that buffer faults into the Sentry, and the Sentry
holds the task there until the share comes round again.

The correctness of that depends on a quiet assumption: **that the workload
rewrites the gated buffer on every submission.** A kernel-launch loop does —
each launch builds a new command in the pushbuffer, hits the revoked mapping,
and faults. That is the workload the first post measured, and against it the
gate divides cleanly: 76%, 51%, 26% achieved against 75/50/25 configured.

Real machine-learning workloads do not submit that way. On Volta and later,
cuBLAS GEMM and — decisively — **CUDA graph replay**, which is how an inference
server like vLLM spends essentially all of its time, do not rebuild the
pushbuffer per launch. They replay a pre-recorded command buffer and ring the
doorbell. The commands were written once, at capture time; steady-state
execution never writes them again. So the mapping the gate revokes is one the
workload has no reason to touch, and the fault that the whole mechanism waits
for never arrives.

Measured on a real graph-replaying workload: **roughly zero faults per period.**
The sandbox runs on at full rate with its share nominally set to a fraction. The
gate enforces nothing.

This is worth dwelling on as a testing failure, not just a mechanism failure.
The synthetic kernel-launch loop that validated the gate in the first post is
the *one* workload shape the gate works against. The benchmark and the mechanism
shared a blind spot, and the clean 76/51/26 table was measuring the exception,
not the rule. "Measure, don't infer" is a rule this branch repeats — but you
have to measure the workload that will actually run.

## The wrong turn: "this hardware can't do it"

So we went looking for anything that *could* impose a share on a doorbell
workload. The GPU has scheduling machinery of its own — a runlist scheduler and
a spatial work distributor — reachable through resource-manager controls. Those
controls are admin-gated: a container cannot issue them, but a trusted host
component can. We tried each one. Every one appeared to fail:

- The static per-channel-group **timeslice** control round-tripped `NV_OK` but
  changed nothing: a 16:1 ratio produced a 1:1 division.
- A driver-level **TSG detach** returned `NV_OK` on every channel group, and the
  workload ran on at full rate anyway.
- The **TPC partition table** and the **CWD watermark** — the spatial controls,
  the direct analog of AMD's CU mask — returned `0x57`, which we decoded as
  `NV_ERR_NOT_SUPPORTED`, even after we swapped in the open kernel driver and
  issued them at kernel privilege to rule privilege out.

We wrote that down, carefully and at length: consumer Blackwell honors none of
these primitives; robust compute isolation for arbitrary CUDA is a property of
MIG-capable datacenter silicon this card is not; the temporal levers are inert
and the spatial ones are absent in the firmware. It was a thorough, well-
documented, confident conclusion.

It was three of our own mistakes, stacked.

**A misread status code.** The spatial controls returned `0x57`. We read it as
`NV_ERR_NOT_SUPPORTED`. But `0x56` is NOT_SUPPORTED; `0x57` is
`NV_ERR_OBJECT_NOT_FOUND`. The control had not been refused as unsupported — the
firmware could not *find the object* the control named.

**The wrong call site.** Those controls are routed to the GPU's GSP firmware,
which can only act on an object after that object has been registered with it.
We were issuing them from the context-share *constructor* — before registration
— so of course the firmware could not find the object. Issued instead from the
deferred scheduling path, after the object is live, the identical control
returns `NV_OK`.

**A missing commit.** The temporal controls really did round-trip `NV_OK` and
really did nothing, because a companion RPC — `RESTART_RUNLIST` — is what tells
the firmware to act on a staged runlist edit. Without it the driver stages a
change the scheduler never applies.

Each mistake, on its own, produces a plausible negative that reproduces every
time you run it. Together they produced a hardware conclusion out of three
software bugs. The reproducer that could have decided the question correctly
existed the whole time; we ran a broken version of it and trusted the numbers,
which is exactly the failure mode this branch warns about — reasoning
confidently about code that was never correctly run.

## The fix: stop intercepting; let the GPU divide itself

The lesson, once the errors were out, reframes the whole problem. You cannot
intercept submission — that much of the first post stands, and it is a genuine
hardware fact. But you do not have to. **The GPU already has a scheduler.** The
GSP-resident runlist scheduler divides engine time between channel groups, and
the work distributor partitions the SMs. Both are configurable — through exactly
the admin-gated controls above — and both act *below* the submission path, so a
doorbell workload cannot dodge them the way it dodged the mapping gate.

So enforcement moves out of the Sentry's mapping revocation and into a
**driver-resident broker**: a small extension to the host's open kernel modules
that issues these scheduling controls on each sandbox's objects. This is the
shape that Ghost ("Breaking the Tradeoff: Elastic and Isolated GPU Sharing")
independently arrived at on the A100 — a privileged, driver-level resource
broker — and it satisfies the
governing constraint of this whole project even more firmly than the Sentry gate
did: the thing doing the limiting is now in the host kernel, not merely outside
the container but in a different privilege domain entirely. Under gVisor's KVM
platform the driver attributes a sandbox's GPU objects to the Sentry's host
process, which is precisely the per-sandbox handle the broker keys its policy on.

Two axes of enforcement, both now working:

**Temporal — weighted runlist timeslices.** `SET_TIMESLICE` plus the
`RESTART_RUNLIST` commit, per channel group. A sandbox's weight maps to its
timeslice length, and the firmware scheduler divides engine time in proportion.
This is the same weighted, work-conserving policy `pkg/gpusched` already
implements — only now the GPU's own scheduler enforces it instead of a fault in
the Sentry. And it is work-conserving *for free*: an idle-but-attached tenant's
slice is yielded by the firmware to a busy neighbor automatically, with no
detach and re-attach needed to reclaim it.

**Spatial — an imposed TPC partition.** `SET_TPC_PARTITION_TABLE`, issued from
the deferred site, pins a sandbox to a specific set of texture/processing
clusters. This is the hard spatial partition — the direct analog of the CU mask
that the AMD side (`amdproxy`) has had all along, and the thing the first post's
NVIDIA half explicitly could not do. AMD got a spatial partition because the CU
mask is an argument to an ioctl the Sentry already interprets and the hardware
honors thereafter; NVIDIA's equivalent lives behind an admin-gated control the
Sentry cannot issue. The driver broker is what lets NVIDIA reach it — closing,
from the other direction, the "the two mechanisms are not the same shape" gap
between the two proxies.

## Measured, on the card we had written off

All figures on an **RTX 5070 (consumer Blackwell, GB205)** — the exact GPU we
had documented as incapable of imposable compute isolation — with two vLLM
cuBLAS tenants running under gVisor.

**Temporal.** Two equal tenants share the device at 220.7 / 220.7 matmul/s. Set
weights 3:1 (via `SET_TIMESLICE` + `RESTART_RUNLIST` on each):

| Configuration | Result |
| --- | --- |
| two tenants, equal weights | 220.7 / 220.7 (1:1) |
| weights 3:1 | 330 / 110 (**3.00:1**), total conserved |
| stop the smaller tenant | survivor rises 330 → 457 (= solo) |
| resume it | split restored |
| three tenants, equal | 1:1:1 |
| three tenants, 3000/2000/1000 | 3:2:1, conserved |
| 2:1 / 6:1 / 16:1 requested | 2.03:1 / 6.15:1 / 13.3:1 |

The division is proportional and conserved, it reclaims an idle tenant's share
automatically, it holds for three tenants, and it stays linear to about 6:1
before compressing — the same shape reported on the A100. This is a
graph-replaying cuBLAS workload, the one the mapping gate could not touch.

**Spatial.** From the deferred site, on the sandbox's primary compute context
share, imposing a TPC partition and measuring the burn rate:

| TPCs granted (of 24) | Result |
| --- | --- |
| 13 | 272 matmul/s |
| 18 | 372 matmul/s |
| 24 (full) | 458 matmul/s (= solo) |

About 20 matmul/s per TPC, roughly linear. `SET_TPC_PARTITION_TABLE` returns
`NV_OK` **and** confines throughput to the granted fraction — a real hardware
partition, not a control that is accepted and ignored. That is the AMD-CU-mask
analog, working on NVIDIA.

The die-class story we had written down — "only datacenter MIG silicon does
this" — was an artifact of the three measurement errors, not a fact about the
hardware. Both axes work on a consumer card with no MIG at all.

## What is honestly true, and what is not yet

- **This is a driver-resident broker, not a drop-in `nvproxy` flag.** It is a
  prototype extending the open kernel modules, deployed on the host. The
  enforcement axis is proven on hardware; the productization — `nvproxy` setting
  broker parameters per sandbox, mapping weight to timeslice and weight to TPC
  count — is the remaining engineering. The Sentry-side scaffolding
  (`smpart_unsafe.go`, `computegate_unsafe.go`, `pkg/gpusched`) is already in the
  tree; the broker finishes what privilege blocked it from doing there.
- **The trusted base is larger than the first post's.** That mechanism lived
  entirely in the Sentry. This one adds a trusted host *driver* component — still
  outside the container, so the governing constraint holds, but a heavier
  deployment story that requires running the patched open kernel modules.
- **The two mechanisms compose.** Where a workload genuinely does fault
  frequently, the Sentry mapping gate still works with no driver changes; the
  broker handles the doorbell case the gate could not. They are not competing
  answers.
- **This is measured on one GPU.** The same battery is being run on pro and
  datacenter dies (RTX A6000, A100, RTX 6000 Pro Blackwell). We are deliberately
  **not** predicting those results from the die class — predicting from the die
  class is precisely the mistake that cost us here. Each GPU gets measured, with
  the status codes read correctly and the controls issued from the right place.
- **Memory-quota isolation is unaffected and unchanged.** It was always a
  separate, solved problem — admit-before-forward accounting with
  `cuMemGetInfo`/NVML rewritten — and it works on every GPU regardless of any of
  this.

## The lesson

The first post ended on a hardware fact — work submitted to a GPU cannot be
recalled — and treated it as a ceiling. It is a ceiling for *interception*. It
is not a ceiling for *scheduling*: the GPU will divide its own time and its own
space, if you ask it correctly. "Correctly" turned out to be the entire
difficulty — the right control, on a live object, with the commit that makes the
firmware act — and for several days three small mistakes about *how to ask*
wore the costume of a fact about *what is possible*. The thing that finally
settled it was not a cleverer argument; it was reading one status code correctly
and moving one call three functions later.
