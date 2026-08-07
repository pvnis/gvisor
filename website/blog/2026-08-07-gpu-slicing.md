# Slicing a GPU Between Untrusted Containers

GPUs are expensive and, for most workloads, mostly idle. Sharing one between
several tenants is the obvious way to make it pay for itself, and every existing
answer either requires particular hardware, gives up isolation, or puts the
thing doing the limiting inside the container being limited.

This post describes a set of changes to gVisor's `nvproxy` that divide an
NVIDIA GPU's memory and compute between sandboxes, with all of the enforcement
in the Sentry where the workload cannot reach it. It covers what the GPU makes
easy, what it makes very hard, what the resulting mechanism costs, and what it
still does not do.

<!--/excerpt-->

## The problem

gVisor can already run CUDA workloads. `nvproxy` forwards the ioctls a container
makes against `/dev/nvidiactl` and friends, after checking them against a
per-driver-version allowlist of structures it understands. That is a real
security boundary — it is what makes a GPU container in gVisor meaningfully
different from one with the device node bind-mounted in — but it is a boundary
about *shape*, not about *quantity*. Nothing in it stops a container from
allocating every byte of framebuffer on the card, or from keeping the SMs busy
100% of the time.

On a single-tenant node that does not matter. On a shared one it is the whole
game. A container that allocates all of a GPU's memory does not merely
inconvenience its neighbours; it makes them fail, because CUDA allocation
failures are usually fatal to the process. And a container that submits an
unbroken stream of kernels will take essentially the entire device, because the
GPU's own scheduler is round-robin between channels and knows nothing about
tenancy.

The existing options each give something up:

*   **MIG** partitions the hardware itself, with real memory and SM isolation.
    It is the strongest answer available, and it is also only on datacenter
    parts, only at a handful of fixed partition sizes, and requires draining
    and reconfiguring the device to change the layout.
*   **MPS** lets several processes submit to one GPU context concurrently. It
    improves utilization considerably, and it is not a security boundary: the
    active-thread-percentage and memory limits it offers are set by the client
    itself through environment variables, which is precisely the property we
    are trying to avoid.
*   **Kubernetes device-plugin time-slicing** simply advertises one GPU as
    several. There are no memory limits and no proportional shares — every
    replica is told it has the whole device, and they fight.
*   **[HAMi](https://project-hami.io)** does the most complete job. It has a
    scheduler that understands `nvidia.com/gpumem` and `nvidia.com/gpucores`
    and places pods so they fit, and it enforces those requests with
    `libvgpu.so`, which it `LD_PRELOAD`s into the container to intercept the
    CUDA API.

That last one is the interesting case, because the placement half is genuinely
good and the enforcement half is in the wrong place. `libvgpu.so` lives in the
address space of the process it is meant to constrain. Even setting aside
`dlsym`, patching the GOT, or issuing ioctls directly, the library reads an
environment variable, `CUDA_DISABLE_CONTROL`, that turns it off. A control the
workload can reach is not a control.

We do not assume a cooperative environment. The goal here is fair sharing such
that no workload can monopolize the GPU to the detriment of others, and that
means the enforcement has to be somewhere the workload cannot get to. In gVisor,
that place is the Sentry.

## Part 1: memory

Memory is the tractable half, because GPU memory allocation *does* go through
ioctls, and `nvproxy` already sees every one of them.

The accounting hooks into the allocation and free paths and tracks three kinds
of memory separately:

*   **Device memory** — the framebuffer.
*   **Host memory pinned for DMA** — reached through the same allocation ioctls
    but drawn from a different pool. `NV01_MEMORY_SYSTEM` in particular is host
    memory despite appearing alongside the device memory classes, and
    conflating the two would both reject legitimate allocations and leave
    others unaccounted.
*   **Address space reserved on `/dev/nvidia-uvm`**, which backs CUDA unified
    memory.

The limit, `--nvproxy-gpu-memory-limit`, covers device memory and reserved
unified-memory address space together, since unified memory is committed into
device memory. Pinned host memory is tracked but not limited by it; it is
already bounded by the sandbox's ordinary memory limit.

Three details are worth pulling out.

**Unified memory cannot be accounted where it is committed.** UVM populates
device memory in response to GPU page faults, which are serviced inside the
host `nvidia-uvm` module. That module calls into the resource manager through
in-kernel symbols rather than ioctls, so the Sentry never observes the
commitment. What it can observe is the address space reservation, so that is
what gets charged. This is sound as a bound — committed memory cannot exceed
the address space reserved for it — but it is not tight, and a workload that
deliberately oversubscribes is charged for what it reserved. Consequently
`uvmFD` tracks its mappings rather than using `MappableNoTrackMappings`, so
that a range mapped more than once, including across `fork()`, is charged once
and released only when the last mapping goes away.

**Charging happens before the ioctl is forwarded**, so that two concurrent
allocations cannot both be admitted against the same headroom, with the charge
returned if the driver rejects the allocation. Charges are reference counted,
because `NV_ESC_RM_DUP_OBJECT` aliases an existing allocation rather than
committing more memory. And they are released from `objFree()`'s cascade rather
than from the `NV_ESC_RM_FREE` handler, because freeing an object also frees its
dependents and only that loop observes all of them.

**Every allocation class must make an explicit accounting decision.** A new
class added to the allowlist without one fails a test rather than silently
going uncharged.

Because the Sentry is doing the accounting, it can also make the limit *visible*
in the way a real device is: `nvproxy` reports framebuffer sizes clamped by the
quota, so a container that asks the driver how much memory the card has is told
its own limit rather than the physical size. That is what makes an unmodified
CUDA application behave sensibly against a quota instead of confidently
allocating past it.

Two things remain unaccounted. Fabric memory and EGM are deliberately left out:
whether they are backed by local device memory or imported from another node
determines whether the sandbox should be charged at all, and settling that
requires hardware we do not have. That has to be resolved before the accounting
can be called complete.

## Part 2: compute, and why it is much harder

The natural next move is to meter compute at the same boundary, and it does not
work. It cannot be made to work.

Once a CUDA channel exists, submitting work to it does not enter the kernel at
all. The application writes commands into a ring buffer in memory it has already
mapped, and rings a doorbell through a mapped register. That is it. A sustained
run of **13,000 kernel launches produced zero ioctls** between context creation
and teardown.

So there is nothing to intercept. Compute cannot be metered or scheduled from
the syscall boundary at any price, and no amount of cleverness in `nvproxy`'s
ioctl handling changes that.

There is exactly one scheduling control that *is* reachable through ioctls, and
it is worth closing: `NVA06C_CTRL_CMD_SET_TIMESLICE` sets how long the GPU's own
scheduler runs a channel group before switching away from it. It was being
forwarded unexamined, and it is reachable with the compute capability every CUDA
container has, on channel groups the container allocates for itself — so a
container could enlarge its own timeslice and take a greater share of a GPU it
shares. `--nvproxy-max-timeslice-us` now rejects requests above a configured
bound with `NV_ERR_INSUFFICIENT_PERMISSIONS`. It defaults to no limit, because
whether this measurably increases a container's share has not been demonstrated,
and imposing a default cap on an unmeasured risk is more likely to break a
workload than protect one.

That closes the one door in the ioctl path. It does not solve the problem.

### Revoking the mapping

The technique that does work is taken from
[Krypton (USENIX ATC'25)](https://www.usenix.org/conference/atc25), which does
the same thing from a kernel module. The insight is that although submission
never enters the kernel, it does require the ring buffer to be *mapped*. Take
the mapping away and the next write to it faults.

So: the Sentry revokes its own mapping of the command buffer at the end of the
sandbox's share of each period. The application's next submission faults into
`memmap.Mappable.Translate`, and the Sentry holds it there until the share comes
round again.

Finding the command buffer takes a small dance. It is identified from the
channel allocation parameters, which name the memory object holding it — but
applications map that object *before* creating the channel that identifies it as
a command buffer. So the association is recorded from whichever side arrives
first (`noteMapping` or `addCommandBuffer`) and resolved when the other appears.

Two things about the implementation matter more than they look.

**Revocation must happen exactly at the end of the allowance.** An earlier
version revoked on a fixed interval without regard to phase. It throttled, but
weakly and not in proportion to the configured share, because an application
that faulted early in a period ran on unimpeded through the remainder of it. The
`untilWindowEnd` calculation exists to get this exactly right.

**The wait is against a deadline, not against a signal.** This is not a style
choice. `Translate()` runs with the address space's `activeMu` held for writing,
and the goroutine that revokes mappings needs that same lock to invalidate them.
A task that waited to be woken by the revoking goroutine would deadlock with it.
Waiting on a computed deadline leaves that goroutine merely *delayed* until the
task wakes and releases the lock. The sleep is uninterruptible, so a sandbox
cannot escape its limit by arranging to be signalled, and it is bounded by one
period, so a limited sandbox always makes progress.

Measured on an RTX 5070, against an unlimited baseline of 654 kernel launches
per second:

| Configured | Achieved |
| ---------- | -------- |
| 75%        | 76.2%    |
| 50%        | 51.0%    |
| 25%        | 25.8%    |

The small consistent overshoot is work already in flight when the mapping is
revoked, which runs to completion.

### What it costs

Handling the fault in the Sentry is what removes the need for anything inside
the container. It is also what makes it expensive for the rest of the sandbox.

While a sandbox is held, the task waiting to submit is holding its address
space's `activeMu`, so other threads in the same sandbox stall on any operation
that needs it. Measured under a 25% limit: a thread doing continuous
`mmap`/fault/`munmap` fell to about **20% of its unlimited rate**, while a
thread computing over already-resident memory was **unaffected**.

This is a genuine difference from the scheme this implements. Krypton delivers
the fault to a handler inside the container, so only the submitting thread
stalls. Handling it in the Sentry is what buys the security property — nothing
in the container participates in its own limiting — and this is the bill.

## Part 3: a cap is not a share

At this point each sandbox can be held to a fraction of a GPU. That is useful
for holding a tenant to an agreed fraction, and it does not divide a GPU
between tenants, for two reasons.

**Nothing redistributes unused time.** A Sentry sees only its own sandbox. If
one sandbox is capped at 25% and idle, the other three do not get its share;
it is simply wasted.

**Every sandbox measures its window from the same instant.** All of them are
permitted at the start of each period and none of them at the end, so they
contend during the first part of the period and leave the GPU idle for the rest.

Both problems have the same root: the gate lives inside a sandbox, and a sandbox
cannot know how many others are competing with it or how much of the GPU any of
them is using. Fixing it requires a coordinator outside all of them.

That is `runsc gpu-scheduler`. One instance runs per GPU on the host, sandboxes
connect to it over a Unix socket, and it hands each of them a window.

### Weights, not percentages

Clients are described by **weight** rather than percentage. A percentage is only
meaningful against a particular device, it leaves the GPU idle whenever the
holder has nothing to submit, and it says nothing about what should happen when
the percentages sum to less than 100.

A weight needs no reference to the hardware: weight 200 gets twice the time of
weight 100, on any GPU and against any number of competitors. Clients that are
actually using the GPU divide each period in proportion to their weights, so one
running alone gets all of it and no time is reserved for clients that would not
use it. Where the requests do happen to sum to 100 — which is exactly what
HAMi's scheduler enforces at placement time — the two readings agree, and a pod
asking for 30 gets 30% of a contended GPU.

A client may still carry a ceiling, so the "never more than this" cap remains
available on top.

### Taking turns

Windows are placed **end to end** rather than all starting at zero. Given
adjacent windows, exactly one sandbox is permitted at every instant, rather than
all of them at some instants and none at others. This is the `Phase` field in a
`Grant`, and it is what turns a set of independent caps into a schedule.

A client that is registered but idle keeps a small floor (`MinAllowance`, 5ms).
It cannot be given nothing, or it would have no opportunity to resume and would
stay idle *because* it was idle. Its window overlaps an active client's, which
costs nothing while it stays idle, and lasts only until its first use is
observed.

Capping and debt repayment leave time unclaimed, and that slack is offered back
to clients that can take it, so that a limit on one sandbox benefits another
instead of idling the GPU.

### Getting the schedule into the sandbox

The Sentry's seccomp filter does not permit it to `connect()` to anything. So
`runsc` connects on its way to starting the sandbox and donates the open socket,
exactly as it already does for the control server. Inside, the connection is
read and written as a plain file descriptor rather than through the `net`
package, which keeps the exchange to the `read` and `write` the filter already
allows.

The exchange runs on its own goroutine and is driven by the scheduler: each
window it sends is applied, and a report of what the sandbox did with the
previous one goes back. **Nothing on the submission path waits for any of it**,
so a scheduler that stops responding leaves the sandbox with the window it last
had rather than stalling it. A configured compute percentage is passed along as
a ceiling, so being scheduled cannot grant a sandbox more than it was allowed.

Verified on hardware: two pods sharing one GPU with weights of 300 and 100
received **486 and 162 kernel launches per second** — a ratio of 3.00:1 against
the 3:1 requested — and together retained **99% of the throughput a single pod
achieves alone**. Two pods contending with no scheduler at all receive 324 each,
so the division is the scheduler's doing rather than the hardware's.

## Part 4: the long-kernel problem

There is a hole in the above, and it is the interesting one.

The scheduler initially credited a sandbox with its whole window whenever it
submitted anything. That cannot distinguish a sandbox that stays inside its
window from one that submits a single kernel outlasting it — and a GPU cannot be
made to abandon work already submitted to it. The second sandbox keeps the GPU
well past the end of its share while looking, from the inside, exactly like the
first. The overrun accounting existed, but nothing could ever trigger it.

This is a real attack, not a theoretical one: submit one very long kernel per
window and you take far more than your share.

The fix is to measure what sandboxes actually consumed, via a `Sampler` that
reads per-process GPU utilization from `nvidia-smi pmon` (`--measure-usage`,
on by default, and scheduling degrades gracefully to the weaker
did-it-submit-anything signal on hosts where `nvidia-smi` cannot be run).

That runs into an identity problem. The driver attributes work to the process
that opened the device — which is the Sentry — but the Sentry runs in a process
namespace where it is process 1, so every sandbox would report the same number.
The sandbox cannot report its own host PID because it cannot see it. So `runsc`
announces it instead, over a short separate connection made after the sandbox
starts, that being the first moment the process ID exists. The announcement and
the sandbox's own connection may arrive in either order, so the PID is recorded
apart from the connections and joined up later.

Time used beyond a window is then charged back against later ones as **debt**.
The amount recovered is capped at one period, so a single long kernel cannot
starve its author for many periods afterwards, and time recovered is offered to
*other* clients before being returned to the one that overran — otherwise
charging the overrun back would be meaningless.

Measured on two pods of equal weight, one submitting kernels roughly a hundred
times longer than the other:

| | Split | Ratio |
| --- | --- | --- |
| Without measurement | 67.6% / 32.4% | 2.08:1 |
| With measurement    | 57.8% / 42.2% | 1.37:1 |

The imbalance falls by 56% rather than vanishing, and it cannot vanish here. The
long kernel runs for about 150ms against a 100ms period and cannot be preempted
once submitted; `pmon` samples once a second while windows are 100ms; and
recovery is deliberately capped at one period. This mitigates the attack
substantially. It does not eliminate it.

It also, as it turns out, costs more than it should. Measured after the fact on
two pods running an *identical* kernel loop at equal weights, `pmon` attributes
most of the GPU to whichever pod is being throttled — and the debt mechanism
then throttles it further, which is self-reinforcing. The division collapses to
31 against 618 launches per second, where turning the measurement off gives 324
and 324. So the sampling that defends against long kernels currently misprices
the ordinary case badly, and `--measure-usage=false` is the better setting until
that is understood. This is a defect in how `sm` is being read, not in the debt
accounting it feeds.

## Part 5: Kubernetes, without the injection

None of the above is useful in a cluster unless something translates what a pod
*asks for* into what `runsc` *enforces*.

HAMi has four components. Three of them are worth keeping exactly as they are:

*   `hami-webhook`, which sets `schedulerName` on GPU pods.
*   `hami-scheduler`, which does placement and per-device accounting.
*   `hami-device-plugin`, which advertises the devices.

All three run on the control plane, well away from the workload, and nothing
about them needs to be trusted by the sandbox. They solve a real problem —
deciding which node and which device a pod runs on, and refusing to place more
work on a GPU than it can hold — and gVisor does not need a device plugin or a
scheduler of its own.

The fourth component is `libvgpu.so`, and that is the one to drop.

It is worth noting *why* the seam is here and not somewhere tidier. Substituting
the stock NVIDIA device plugin does not work: `hami-scheduler`'s accounting
depends on the node annotation `hami.io/node-nvidia-register`, which HAMi's own
device plugin writes. Scheduler and plugin are coupled, so they stay together.

A small mutating webhook does the translation, in `webhook/pkg/gpushare`:

*   `nvidia.com/gpumem` (MiB) → `dev.gvisor.flag.nvproxy-gpu-memory-limit`
    (bytes)
*   `nvidia.com/gpucores` (percent) → `dev.gvisor.flag.nvproxy-gpu-weight`
*   `CUDA_DISABLE_CONTROL=true` into every container, standing down the
    in-container limiter

Because the limit applies to the sandbox rather than to individual containers,
it is the pod's *peak* demand: containers run together so their requests add up,
while init containers run before them so only the largest matters. A limit
already stated on the pod is left alone, since it may deliberately be lower than
the request, and a pod requesting no GPU memory does not acquire a limit at all.

Nothing about what the pod asks for is changed, so HAMi's scheduler continues to
work unmodified and needs no knowledge of gVisor.

These annotations ride the existing gVisor override ratchet: the value
configured on the runtime is a **ceiling that an annotation may lower but never
raise**. This matters because container specs are frequently authored by the
workload being limited. A weight is relative, so raising one takes GPU time from
every other container on the device; treating it as a floor rather than a
ceiling would let a container grant itself as much of the GPU as it liked.

### Does dropping the preload actually work?

Setting `CUDA_DISABLE_CONTROL` leaves the library loaded but inert. Better is to
not load it at all — HAMi's device plugin installs the preload via a ConfigMap
key, and blanking that key removes it from the node entirely. (I predicted this
would not be enough, on the theory that the init script only copies files that
do not already exist. That was wrong: the host copy goes to zero bytes after the
plugin restarts. Worth stating, because it is the difference between "silenced"
and "gone".)

The three-way test, run on a live k3s cluster, confirms `nvproxy` carries the
enforcement alone:

| libvgpu | annotation | device size seen | outcome |
| --- | --- | --- | --- |
| loaded  | none    | 512 / 512 MiB     | refused at 512 MiB |
| dropped | none    | 11347 / 11790 MiB | allocated all 768 MiB |
| dropped | 512 MiB | 350 / 512 MiB     | refused at 320 MiB |

Row two is what HAMi looks like with its enforcement removed and nothing put in
its place: the container sees the whole card and takes what it wants. Row three
is the same cluster with the Sentry doing the work — the container sees its
quota, and is held to it by something it has no access to.

## Pros and cons

**What this gets you:**

*   Enforcement the workload cannot reach, disable, or observe. There is no
    environment variable, no library to patch, no ioctl to issue.
*   No special hardware. It works on any NVIDIA GPU `nvproxy` supports, at
    arbitrary ratios, without draining the device to reconfigure.
*   Work-conserving sharing. Idle tenants' time goes to busy ones, and the
    99%-of-solo-throughput figure says the division costs almost nothing in
    aggregate.
*   Unmodified schedulers. HAMi's placement logic — genuinely good, and a lot of
    work to reproduce — runs as shipped.
*   Some resistance to the long-kernel attack, which pure token-bucket schemes
    have none of.

**What it costs, and what it still does not do:**

*   **The `activeMu` stall.** A held sandbox's other threads block on
    address-space operations. Memory-mapping-heavy workloads inside a limited
    sandbox pay heavily (~20% of unlimited rate under a 25% cap); compute-bound
    ones over resident memory pay nothing.
*   **100ms granularity.** Latency-sensitive inference under a small share will
    feel the period.
*   **Long kernels still overshoot.** Mitigated, not solved, and bounded by
    what `pmon`'s 1Hz sampling can see.
*   **The usage measurement misprices the ordinary case.** With
    `--measure-usage=true`, two identical pods at equal weights end up at 31 and
    618 launches per second instead of 324 each. See below.
*   **A pod holding several GPUs gets one window.** Each device counts it, and
    it is held to the smallest window any of them granted — the only safe
    choice, since the gate covers every device it holds at once and anything
    wider would let it exceed its share of the most contended one. A pod that
    has one GPU to itself and shares another is therefore throttled on both,
    leaving the unshared device idle for the rest of each period.
*   **No checkpoint/restore for CUDA sandboxes.** Only `rmAllocObject` and
    `rootClient` implement `Restore`; `miscObject` and `osDescMem` do not, and
    those cover the OS events every CUDA context creates. In practice no
    sandbox running CUDA can be saved.
*   **Fabric memory and EGM are unaccounted**, pending hardware to validate
    against.
*   **UVM is charged at reservation, not commitment**, so deliberate
    oversubscription is charged for what it reserved.
*   **The coordinator is on the critical path.** A sandbox that cannot reach the
    scheduler socket fails to start rather than starting unscheduled. That is
    deliberate — failing open would mean an unlimited sandbox — but it is a new
    thing that has to be running.

## Results in one place

All figures from an RTX 5070.

| Measurement | Result |
| --- | --- |
| ioctls observed during 13,000 kernel launches | 0 |
| Cap: 75% / 50% / 25% configured | 76.2% / 51.0% / 25.8% achieved |
| Weights 300:100, two pods | 486 vs 162 launches/sec (3.00:1) |
| Same two pods, aggregate throughput | 99% of a single pod running alone |
| Two pods, no scheduler | 324 each |
| Long-kernel abuse, without measurement | 67.6% / 32.4% (2.08:1) |
| Long-kernel abuse, with measurement | 57.8% / 42.2% (1.37:1) |
| Two identical pods, equal weights, measurement off | 324 / 324 |
| Two identical pods, equal weights, measurement on | 31 / 618 |
| Two pods on separate GPUs, one coordinator | a whole device each |
| Cost to an mmap-heavy thread under a 25% cap | ~20% of unlimited rate |
| Cost to a compute-bound thread under a 25% cap | none measurable |

## Where this goes next

The measurement is the weak point. `nvidia-smi pmon`'s `sm` column is what the
overrun accounting is built on, and it does not appear to measure what the
scheduler assumes. Two pods running an identical kernel loop at equal weights
divide the GPU exactly in half with `--measure-usage=false` (324 and 324
launches per second, against 654 for a pod running alone) and 31 against 618
with it on:
`pmon` attributes the *larger* share to the pod achieving one twentieth of the
throughput, and the debt mechanism then pins it at the 5ms floor and keeps it
there. The long-kernel defence that measurement exists to provide is real, but
it is currently paid for with a worse division of the ordinary case, and the
sampling needs to be understood before it can be trusted.

Beyond that, the honest summary is that GPU sharing under an adversarial threat
model is bounded by a hardware fact — work submitted to a GPU cannot be
recalled. Everything above is an argument about how closely you can approximate
fair sharing given that constraint, from outside the container. The answer
appears to be: quite closely for well-behaved workloads, and closely enough to
be worth having for badly-behaved ones.

Setup instructions are in the
[GPU user guide](https://gvisor.dev/docs/user_guide/gpu/#kubernetes).
