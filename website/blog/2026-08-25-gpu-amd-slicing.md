# The AMD Side: Space, Time, and No Driver Broker

The [previous post](/blog/2026/08/19/making-the-gpu-enforce-its-own-compute-shares/)
ended by going *into* the NVIDIA driver. Submission on an NVIDIA GPU never enters
the kernel, so the Sentry could not meter it; imposing a compute share on a real
machine-learning workload meant a privileged, driver-resident broker driving the
GPU's own scheduler. The obvious next question is whether AMD needs the same
thing.

It does not — and the reason is the whole story. On AMD the controls that divide
the GPU, in *space* and in *time*, are ordinary ioctls on `/dev/kfd` that gVisor's
`amdproxy` already interprets. So AMD gets both a genuine spatial partition and a
work-conserving time-slicer, entirely from the Sentry, on the stock driver.

<!--/excerpt-->

## Why AMD is different

AMD submits work the same way NVIDIA does: a command written into a mapped ring
buffer, a doorbell rung through a mapped register. So the wall the first NVIDIA
post hit — you cannot intercept submission — is there on AMD too. You cannot
meter a doorbell from the syscall boundary on either vendor.

The difference is what the Sentry can reach *around* submission. On NVIDIA the
two things that actually divide the GPU — the spatial work-distributor partition
and the runlist schedule — live behind admin-gated resource-manager controls the
Sentry cannot issue, which is why they needed a driver broker. On AMD the
equivalents are in the ioctl path `amdproxy` already sees and allowlists:

- the **CU mask**, an argument to queue creation; and
- the **queue lifecycle** — suspend and resume — through the KFD debug interface.

`amdproxy` forwards ioctls on `/dev/kfd` and the amdgpu render nodes against a
per-call allowlist, denying everything it does not understand. Both controls
above are already inside that boundary. So AMD gets both mechanisms without the
Sentry reaching past its own privilege domain.

## Part 1: space — the CU mask

RDNA and CDNA GPUs let a queue be created with a **compute-unit mask**: a bitmap
of which CUs the queue's work may run on. It is an argument to the
queue-creation ioctl, so `amdproxy` simply narrows it. `--amdproxy-cu-mask` sets
the sandbox's ceiling; a queue asking for all CUs is silently clamped to the
mask, and a queue asking for CUs *outside* it gets `EINVAL`. The hardware's
command processor honors the mask thereafter, with no further decision from the
Sentry.

This is a **hard, concurrent** partition, and that word matters. Two tenants on
disjoint CU halves run *at the same time*, each on its own compute units. That is
the thing the NVIDIA side could only approximate: NVIDIA's imposed TPC partition
turned out to be a ceiling each tenant hits inside its own time-slice, not
side-by-side execution. AMD's CU mask is the real primitive. Measured with a
compute burner on a disjoint half of a Navi 32: a tenant's rate moved **−0.61%**
when a neighbour started underneath it and **+0.39%** when the neighbour left.
That near-perfect isolation is the strongest result on the whole project.

Two honest limits.

**Granularity is two CUs, not one.** RDNA pairs its compute units, and the driver
returns `EINVAL` for a queue mask that enables half a pair. So "half of 54" is 26
or 28, never 27. A mask like `0x7` that splits a pair makes every queue creation
fail — and, in a bug we hit the hard way, ROCr did not handle that failure and
died on a null dereference, so what the operator saw was a container that hung.
`amdproxy` now rejects such a mask at startup, naming the offending CU and
suggesting a valid one, rather than letting it fail deep in the runtime.

**It partitions compute, not memory bandwidth.** Equal disjoint shares are
exactly fair up to three tenants (Jain index 1.0000); beyond that the GPU
degrades unevenly — and native processes using `HSA_CU_MASK` degrade *identically*,
so this is the hardware faithfully reproduced, not a gVisor artifact. And CU masks
divide compute-pipeline occupancy, not the memory bus: three vLLM tenants, whose
decode is memory-bandwidth-bound, contend for VRAM bandwidth even on disjoint CUs.
A scheduler should cap concurrent GPU tenants rather than expect the mask to hold
service flat past three.

## Part 2: memory

Memory is the same tractable problem it was on NVIDIA, because AMD allocation
goes through ioctls the Sentry sees. `--amdproxy-gpu-memory-limit` is
admit-before-forward accounting on `ALLOC_MEMORY_OF_GPU`: a request that would
push the sandbox past its quota is refused before the ioctl reaches the driver.

And, as on NVIDIA, the Sentry makes the limit *visible*. `amdproxy` synthesises
the KFD topology the sandbox reads, and reports the sandbox's own quota as the
VRAM size — so `torch.cuda.mem_get_info()` inside a container holding 2 GiB reads
2 GiB total, not the card's 12. Getting that right needed one ground-truth detail:
RDNA reports its VRAM heap as `heap_type 2` where CDNA uses `1`, and an earlier
rewrite that matched only `1` left the sandbox seeing a quota-correct *free* next
to the whole device's *total*. Measured against the real header on the hardware,
the way this branch settles every ABI question.

## Part 3: time — the surprise

Here is the part that mirrors the NVIDIA post and inverts its conclusion. The
thing NVIDIA needed a kernel-resident broker for — imposing a weighted time
share on an uncooperative workload — AMD does from userspace, **with no driver
patch at all**.

The mechanism is the KFD debug interface: `KFD_IOC_DBG_TRAP` with
`SUSPEND_QUEUES` and `RESUME_QUEUES`. A debugger normally has to `PTRACE_ATTACH`
to its target — but the kernel skips that check when the target *is* the caller,
and under gVisor's KVM platform the Sentry is itself the KFD process for the
sandbox (KFD keys its process context on the calling `mm`, which is the Sentry's).
So the Sentry opens a debug session **on itself**, needs no ptrace and no separate
debugger process, and suspends and resumes the sandbox's own queues on a weighted
duty cycle.

The hardware does something the NVIDIA gate never could: it **preempts
mid-kernel**. When a queue is suspended, AMD's CWSR — compute wave save/restore —
checkpoints the in-flight waves and restores them on resume. So a `vecadd` kernel
stays `RESULT CORRECT` while being sliced at 25%. The NVIDIA compute gate revoked
a mapping and waited for the next submission to fault; AMD actually stops the
compute and picks it back up.

The division is proportional and work-conserving. Measured on the Navi 32, under
gVisor:

| Configuration | Result |
| --- | --- |
| two tenants, equal weights | Jain 1.0000 |
| weights 300:100 | **3.05:1** |
| weights 500:100 | 5.13:1 |
| a neighbour leaving | survivor reclaims the device, 1400 → 3440 → 6850 |
| `vecadd`, sliced at 25% | `RESULT CORRECT` |

Work-conservation — a lone tenant getting the whole GPU, an idle tenant's share
going to a busy one — is not free the way the mechanism is. A Sentry sees only
its own sandbox and cannot know how many others compete, so it needs a
coordinator outside all of them. That coordinator is the same `runsc gpu-scheduler`
the NVIDIA side uses, placing each tenant's slices end to end from the same
weighted policy in `pkg/gpusched`.

### What it costs, and when

The cost of holding the debug session open is **occupancy, not throughput**, and
it is *workload-shaped* — which is the whole point. Measured within a single
process, toggling the session on and off partway through a run so that clocks and
initialisation cannot explain the step:

| workload | session off | session on | cost |
| --- | --- | --- | --- |
| ALU-bound, register-heavy (`gpuburn`) | ~12000 iters/s | ~6900 | **−43%** |
| memory-bandwidth-bound (`memburn`) | 337.3 GB/s | 337.3 | **−0.003%** |

During the debug phase the clock is actually *higher* and the device still reads
100% busy, but power falls — fewer waves are resident. Only kernels that need many
resident waves to hide latency pay. And that is exactly the case that does *not*
matter for the workloads people run: LLM decode is memory-bandwidth-bound, so
time-slicing an inference tenant costs almost nothing. For two contending
bandwidth-bound tenants it even *beats* uncontrolled sharing, because serialising
their access to the saturated bus is more efficient than letting them collide.

## Part 4: one or the other, not both

On RDNA3 a queue can carry a CU mask **or** be time-sliced, **never both**. The
amdgpu driver refuses the debug session's CWSR workaround on a queue that carries
a user CU mask (`kfd_dbg_set_queue_workaround`, `-EBUSY`, guarded for GC versions
11.0.0–11.0.3 — every RDNA3 part), and a time slice is enforced through exactly
such a session. So the two are mutually exclusive per device — a driver rule, not
a policy choice — and `amdproxy` rejects a sandbox configured with both at
startup, read out of the source of the running kernel rather than inferred. Memory
quota composes with either.

So the operator chooses per device: **spatial** CU masks, for concurrent
execution on disjoint compute units, blind to memory bandwidth; or **temporal**
time-slicing, for weighted, work-conserving division that partitions the bus but
serialises the tenants.

## Part 5: Kubernetes, on the stock scheduler

The AMD spatial path needs a HAMi fork. Handing out *disjoint* CU masks requires a
node-scoped allocator that knows what masks every other tenant holds — that is a
scheduler's job, and it lives in the fork's `pkg/device/amd`.

Time-slicing needs no such thing, and that is its second surprise. A tenant's
weight is an **independent scalar**: weight 3 beside weight 1 is a 3:1 split, with
nothing to reconcile against anyone else. So it needs no allocator — it rides as
a plain pod annotation that the in-tree webhook restates as
`dev.gvisor.flag.amdproxy-gpu-weight` (narrow-only, so a pod can only lower its own
share), and **upstream, unmodified HAMi** does the placement. Dropping the fork is
the payoff: with time-slicing, the whole AMD path runs on stock HAMi — the same
shape the NVIDIA path always had.

Verified end to end: three honest vLLM tenants and one adversary, weighted, on
upstream HAMi v2.9.0, with no CU mask anywhere. The weighted division held
(**3.25:1** for a 3:1 request), the adversary — self-annotating the whole card and
the maximum weight — was clamped by the webhook and held to **9%** of the 10% its
weight entitles it to, the memory quota was still enforced in the Sentry, and
there were zero driver faults.

## Two traps worth keeping

The mechanism took two non-obvious findings to make solid, both the kind an
integration test surfaces and a unit test cannot.

- **`EC_QUEUE_NEW` blocks the first suspend, silently.** `SUSPEND_QUEUES` reports
  every queue invalid while KFD is holding a pending "new queue" exception; the
  exception must first be *consumed* by calling `QUERY_DEBUG_EVENT` until it
  answers `EAGAIN`. Nothing in the ioctl documents this, and without it nothing is
  ever suspended.
- **Sharing one KFD context across a sandbox's processes had two bugs** that only
  appeared once the full stack ran multi-process: a single signal page the driver
  settles before creating anything (so a second process lost its *event*, and
  ROCr null-dereferenced it during init, before any queue), and a debug session
  pinned to whichever descriptor created the first queue (so it could not be torn
  down when that process exited). Both are fixed; both were invisible until real
  workloads with many processes ran through the whole stack.

## The lesson

NVIDIA and AMD are mirror images of the same constraint. On NVIDIA, submission
hides from the kernel, so imposing a share meant reaching past the Sentry into a
driver broker — and even then the spatial control is a ceiling, not concurrency.
On AMD the queue lifecycle stays in ioctls the Sentry already interprets, so both
a genuinely concurrent spatial partition *and* a work-conserving temporal slicer
live in the Sentry, on the stock driver, with the enforcement — as the governing
constraint of this whole project demands — somewhere the workload can never reach.

AMD is the stricter vendor in exactly one place: you must pick space or time per
device, not both. It is the looser one in exactly the place that counts: you get
to pick.

Setup instructions for both vendors are in the
[GPU user guide](https://gvisor.dev/docs/user_guide/gpu/#kubernetes).
