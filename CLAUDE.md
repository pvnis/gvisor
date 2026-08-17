# GPU slicing in gVisor

Dividing one GPU between mutually untrusting containers, for both vendors:
`nvproxy` for NVIDIA and `amdproxy` for AMD, alongside `tpuproxy`.

**The governing constraint, and the reason this work exists:** *nothing may
depend on changes inside the container.* Every limit is enforced in the Sentry,
where ioctls are interpreted, and a hostile container cannot lift it. The
in-container shim that HAMi's vGPU design relies on was a reference for *what*
to enforce, never for *how* — a limiter living in the address space of the
process it limits is reachable by that process, and HAMi's switches off from
inside with a documented environment variable. Measured: a pod requesting
512 MiB was refused at 512 MiB with the library enforcing, and allocated
768 MiB against the whole 12 GiB device with `CUDA_DISABLE_CONTROL=true` set.

Work happens directly in the working directory, not a worktree.

## The two mechanisms are not the same shape

This is the thing to understand before reading anything else. Both proxies
enforce in the Sentry, but they partition different resources, and almost every
difference in their behaviour follows from that.

| | AMD (`amdproxy`) | NVIDIA (`nvproxy`) |
| --- | --- | --- |
| compute is divided in | **space** — CU masks | **time** — submission windows |
| enforced by | the GPU's command processor | the Sentry, gating submission |
| set at | queue creation, per ioctl | continuously, per period |
| unused share goes to | nobody; it idles | whoever is asking (with the scheduler) |
| granularity | 2 CUs on RDNA, fixed at start | a weight, re-divided every period |
| memory quota | admit-before-forward on `ALLOC_MEMORY_OF_GPU` | admit-before-forward on the RM/VMM paths |

**Why AMD gets a spatial partition and NVIDIA cannot.** Once an NVIDIA channel
is set up, work is submitted by writing to a pushbuffer in mapped memory and
ringing a doorbell through a mapped register. That never enters the kernel — a
sustained run of 13,000 kernel launches produced *no ioctls at all* between
context creation and teardown. So there is no point at which nvproxy could meter
the work itself; it can only decide whether the sandbox is allowed to submit
right now. AMD's CU mask, by contrast, is an argument to an ioctl the Sentry
already interprets, and the hardware honours it thereafter.

**What each is consequently good at.** The CU mask is a hard partition that
holds without any ongoing decision, and it isolates beautifully — but it cannot
give an idle tenant's share to a busy one, and it cannot be changed once queues
exist. The NVIDIA scheduler *can* reassign unused time and adjusts as tenants
come and go, but it is a policy running every period, and it costs more to be
right.

## Where the work runs

**Since 2026-08-11, `sensai` and `sens1` are one k3s cluster serving both
vendors** — NVIDIA slices on sensai, AMD slices on sens1, a single
`RuntimeClass gvisor` resolving to each node's own runsc config. sens1's
standalone k3s and its vCluster were destroyed to join it.
`/home/dmd/vcluster-multitenant/TWO-VENDOR-CLUSTER.md` is the reference; read
it before touching either node's Kubernetes setup.

| | sensai | sens1 | sensnucbox2 |
| --- | --- | --- | --- |
| OS / kernel | Ubuntu 22.04.5, 6.8.0-117 | Ubuntu 24.04.4, **7.0.0-28-generic** | Ubuntu 26.04, 7.0.0-generic |
| GPU | **RTX 5070, driver 610.43.02**, 12227 MiB | **Navi 32 discrete, gfx1101, 54 CUs, 12272 MiB** | **Phoenix1 APU**, gfx1103, 12 CUs, ~14170 MiB |
| ROCm | — | **7.2** | 7.1 |
| `gpu_id` | — | **63860** (changes on reboot) | 40786 |
| `/dev/kfd` major | — | **234** (dynamic) | **511** (dynamic) |
| docker | **no** (containerd only) | yes | yes |
| kubernetes | **k3s control-plane**, Cilium, HAMi, vCluster | **k3s agent** + AMD device plugin | **yes, with the AMD device plugin** |
| `sudo` | passwordless | passwordless | **passwordless (`(ALL) NOPASSWD: ALL`)** |

The RTX 5070 is on sensai, so nvproxy measurements can be re-run there;
`~/nvproxy-quota-test/` holds that harness. sensai has **no docker** and the
user is not in the `kvm` group, so `//pkg/sentry/platform/kvm:kvm_cgo_test`
fails there on `/dev/kvm` permission for reasons that have nothing to do with
this branch. Build with bazel directly rather than `make build`.

The two nodes reach each other **only over Tailscale** — sensai is NAT'd behind
192.168.0.0/24 and is unreachable from sens1 — so both use their Tailscale
address as the k3s `node-ip`. Do not "fix" a node IP back to a LAN address.

sens1 was rebooted into kernel **7.0.0-28** on 2026-08-10, so the KVM bug below
no longer applies on either host.

Nothing should hardcode a `gpu_id` or a KFD major: the major is dynamically
allocated and genuinely differs. `~/amdtest/gputest.sh` reads both from the host.

`~/amdtest/` holds every AMD reproducer, a `Makefile`, `gputest.sh`, and the
baselines measured on sens1. Read `~/amdtest/README.md` first. `~/amdtest/k8s/`
holds the Kubernetes pod specs and shim config — read its README before touching
anything Kubernetes-side.

### Building

**Check the hash, not bazel's summary.** Bazel reports "0 processes, 0 total
actions" for a build whose output did change, and its `bazel-bin` symlink can
point at a stale tree; `bazel clean` did *not* force a rebuild here. Confirm
with `sha256sum` against the deployed binary, and confirm a specific change
landed with `strings runsc | grep <a string you just added>`.

On **sens1**, `dmd` is not in the `docker` group, but `sudo docker` works, so
put a shim first on `PATH` — `~/amdtest/bin/docker`, which is
`exec sudo /usr/bin/docker "$@"`. It covers both the bare `docker` calls in
`images.mk` and `$(DOCKER_CLI_PATH)` in `bazel.mk`:

```
PATH=/home/dmd/amdtest/bin:$PATH make build TARGETS=//runsc:runsc \
    DOCKER_CLI_PATH=/home/dmd/amdtest/bin/docker
```

On **sensnucbox2**, `dmd` *is* in the `docker` group (added 2026-08-07), so the
group can be named directly:

```
sudo -u dmd -g docker make build TARGETS=//runsc:runsc \
    DOCKER_CLI_PATH=/home/dmd/amdtest/sudodocker
```

If a new shell already has the docker group active (`id` shows `docker`), plain
`make build` works. After building, copy `bazel-bin/runsc/runsc_/runsc` to both
`~/amdtest/runsc` and `/usr/local/bin/runsc` — Docker and Kubernetes both run
the latter.

## AMD: done, and verified against hardware

- `pkg/abi/amdgpu/{kfd,drm}.go` — KFD and DRM ABI. Every struct size and ioctl
  number was ground-truthed by compiling C against the real
  `linux/kfd_ioctl.h`, not read off documentation.
- `pkg/sentry/devices/amdproxy/` — the proxy. `/dev/kfd` plus amdgpu render
  nodes; a per-ioctl allowlist, with everything unhandled denied rather than
  passed through.
- **CU-mask enforcement.** `--amdproxy-cu-mask=0x3f`: a request for all CUs is
  silently narrowed to the mask, and a disjoint request gets `EINVAL`.
- **GPU memory quota.** `--amdproxy-gpu-memory-limit`: admit-before-forward
  accounting on `ALLOC_MEMORY_OF_GPU`. 2 GiB reports 2048 MiB, cuts off at
  exactly 2048 MiB, 0 after, 1024 MiB after freeing half.
- `pkg/sentry/devices/amdproxy/amdconf` — leaf package for the `CUMask` type, so
  `runsc/config` does not import sentry code. Mirrors `nvconf`.
- `pkg/amdsysfs` + `pkg/sentry/fsimpl/sys/amdgpu.go` — bounded host sysfs
  snapshot and the synthetic KFD topology / PCI / DRM tree. The topology reports
  the *sandbox's quota* as the VRAM size, not the device's: a pod holding 2 GiB
  reads 2147483648 where the host reads 14859169792. It also synthesises
  `/sys/devices/system/node/node0/{cpumap,cpulist,distance}` so ROCr can resolve
  the nearest CPU agent without a host bind mount; without this, NEAREST_CPU,
  MEMORY_PROPERTIES, SCRATCH_LIMIT_MAX, SCRATCH_LIMIT_CURRENT and CLOCK_COUNTERS
  all return `HSA_STATUS_ERROR_INVALID_ARGUMENT` (`14e55b2dc`).
- runsc plumbing, including annotation overrides with narrow-only / lower-only
  validators, so a container cannot widen its own mask or raise its own limit.
- **`vecadd` runs end to end** — a real HIP kernel, correct results, on all
  three paths: `hiptest.sh`'s OCI bundle, Docker, and Kubernetes. Under
  Kubernetes the AMD device plugin supplies the per-pod quota and the Sentry
  enforces it: a pod asking for 4 × 512 MiB with `amd.com/cu-mask: "0x3f"` gets
  exactly 2 GiB and 6 CUs, narrowed from the node's 0xfff ceiling.
- **Two containers sharing the GPU simultaneously, verified end to end.**
  `vecadd-a` (cu-mask `0x03f`, CUs 0–5, 2 GiB) and `vecadd-b` (cu-mask `0xfc0`,
  CUs 6–11, 2 GiB) ran at the same time under both Docker (`hiptest.sh -f`) and
  Kubernetes (`~/amdtest/k8s/vecadd-{a,b}.yaml`); both produced `RESULT CORRECT`.
  Concurrent `memprobe` confirmed each sandbox sees exactly its own 2048 MiB
  ceiling regardless of what the other allocates.

### A sandbox gets one KFD process context, and what follows from it

KFD keys its `kfd_process` on the calling process's `mm`. Under `--platform=kvm`
the caller is **the Sentry**, one host process for the whole sandbox, so every
sandboxed process that opens `/dev/kfd` lands on one shared `kfd_process`.
Measured: the same command run twice in one sandbox gave INIT OK then `EBUSY`,
while the identical pod under runc gave INIT OK twice.

**Consequently only one process per sandbox could use the GPU at all**, which
ruled out every multi-process runtime. `--amdproxy-share-kfd-vm` (off by
default, `781f822ec`) shares the one context instead. It needed three pieces,
each found only after the previous one moved the failure:

| | failure removed |
| --- | --- |
| ACQUIRE_VM answered locally for later processes | `EBUSY` on ACQUIRE_VM |
| render node shared by `dup`, so one `drm_file` | `EACCES` on a GEM mmap offset |
| RUNTIME_ENABLE made idempotent, released by the last holder | `EBUSY` on nr `0x25` |

The second is the non-obvious one: DRM checks mmap permission per *file*
(`drm_vma_node_is_allowed`), so sharing the address space without sharing the
file leaves the second process unable to map what the first allocated.

The cost is that those processes allocate from one address space while each
believes it has its own, so `vaGuard` refuses an allocation overlapping another
process's rather than aliasing it. **Nothing crosses a sandbox boundary** —
each sandbox has its own Sentry and so its own `kfd_process` — so this is not
an isolation regression; it is a correctness trade confined to the container
that opts in, which is why it is off by default.

This is the same root as the KFD-mmap process binding and SVM's EFAULT: the
driver seeing the Sentry where it expects the application.

### The signal page, and why sharing needs a polling fallback

A KFD process has exactly **one signal page** — where the driver records that
an event fired — so in a shared sandbox only the first process gets one and the
rest are refused EINVAL:

```
tgid=6   page_in=0xf97400000001  err=<nil>
tgid=18  page_in=0xf97400000002  err=invalid argument
```

Their events still get distinct slots, so nothing collides; what they lose is
the *wakeup*, and they waited forever for one that was not coming. The signal
itself is never lost — an HSA signal is a value the GPU writes into the buffer,
and the event only saves a waiter from polling. So refused processes get their
waits capped, re-read the signal from their own memory, and proceed
(`4196b648d`).

**The cap must catch long finite waits, not just `KFD_EVENT_TIMEOUT_INFINITE`.**
ROCr asks for `0xFFFFFFFE`, one less than the sentinel. A first version matched
the sentinel exactly and changed nothing; that off-by-one was the whole
difference between a hang and a working server.

**vLLM 0.16.0 now serves inference under gVisor on AMD**, and costs **9.3%**:
186.9 tok/s native against 169.5 tok/s sandboxed, both within 0.5% run to run.
See `~/vllm-overhead/PLAN.md`; that is one model at concurrency 1, not a sweep.

### A sandbox's own VRAM total was leaking the real device's

Two independent gaps, both letting a sandbox see the whole card instead of its
quota, found while sizing a smaller-than-full-device slice for a two-tenant
test (`a3d7937ed`):

- `DRM_IOCTL_AMDGPU_INFO` forwarded every query unchanged — `AMDGPU_INFO_MEMORY`,
  `VRAM_GTT`, `VRAM_USAGE`, `VIS_VRAM_USAGE`, `GTT_USAGE` all report
  device-wide capacity and use. Ground-truthed against the real header on
  sens1 the way this branch always does; a small C probe against the actual
  render node confirmed struct layout before writing any Go. Now clamped and
  rewritten to the sandbox's own accounted usage, the same shape as
  `AMDKFD_IOC_AVAILABLE_MEMORY`.
- The actual leak for the case that mattered: the KFD-topology sysfs rewrite
  in `pkg/sentry/fsimpl/sys/amdgpu.go` only matched `heap_type == 1`. Measured
  on sens1's Navi 32, the real value is **`heap_type 2`** — RDNA reports VRAM
  differently from CDNA — so `size_in_bytes` was never rewritten and
  `torch.cuda.mem_get_info()` returned quota-correct `free` next to the whole
  device's `total`. The AMD device plugin's own topology code
  (`~/amdgpu-device-plugin/topology.go`) already treated `1` and `2` alike;
  the gVisor-side rewrite had never been brought into agreement with it.

Symptom before the fix: a small slice (e.g. 6 of 23) failed
`--gpu-memory-utilization` outright, because vLLM computed its budget against
the real 12272 MiB card rather than the sandbox's quota. Fixed, an ordinary
utilization value works regardless of slice size.

### Two vLLM tenants, two different models, concurrently: it works

Qwen2.5-0.5B and TinyLlama-1.1B-Chat, disjoint 26-CU halves
(`0x3ffffff`/`0x3ffffff0000000`), each with its own quota, both under
`--amdproxy-share-kfd-vm`. Both served the full request load: A 77.1→68.7
tok/s (−10.9%) under B's load, B 63.4→62.2 tok/s (−1.9%) under A's — the same
shape already measured with `gpuburn` on disjoint masks, now with two real
models actually serving. `torch.cuda.mem_get_info()` in each pod showed only
its own quota throughout, holding under concurrent load. First
concurrent-workload result on the sharing branch; see `~/vllm-overhead/PLAN.md`.

A benchmark-harness trap worth naming: TinyLlama-Chat given a raw
`/v1/completions` prompt emitted EOS immediately (`completion_tokens: 1`),
producing a plausible-looking but wrong tok/s number. Chat-tuned models need
`/v1/chat/completions`; a benchmark can silently measure the wrong thing.

### Disjoint CU masks isolate compute, not memory bandwidth

Extended the two-tenant test to three real models (Qwen2.5-0.5B, TinyLlama-1.1B,
SmolLM2-360M on the leftover CU pair, `0xc000000`). Two tenants cost each
other little (−10.9%/−1.9%); a third roughly **tripled** the hit on the first
two (−35.0%/−32.2%/−9.2%), reproduced exactly across two runs and fully
recovered to solo the instant load stopped — real-time contention, not
throttling. Host CPU ruled out (`mpstat`: 50-98% idle throughout, `%steal` 0).

The reading: CU masks partition compute pipeline occupancy, not the memory
bus, and LLM decode is memory-bandwidth-bound. Two tenants' more modest
overlap didn't yet expose it; three did. This sharpens the existing "past 3
tenants the GPU throttles" note from the pure `gpuburn` sweep — that was
compute-bound and degraded only *past* 3 with 8 CUs each; real serving hits
comparable degradation *at* 3, because it's bottlenecked on a different shared
resource CU-mask partitioning was never going to isolate. VRAM bandwidth is
the leading explanation, not confirmed with memory-controller counters — see
`~/vllm-overhead/PLAN.md`.

### Three tenants, real vCluster isolation, sustained load

`tenant-amd-a/b/c` — three new vClusters, node-sync scoped to sens1 via
`sync.fromHost.nodes.selector.labels: {gpu.vendor: amd}`
(`~/vcluster-multitenant/values/amd-tenant.yaml`), rather than mutating
`tenant-a`/`tenant-b`, which deliberately keep nodes faked for other tests.
First AMD workload run through vCluster on this branch.

Two things worth knowing before touching this again:

- **A netpol gotcha specific to cross-node vClusters.** `tenant-netpol.yaml`'s
  admin-access rule works for tenant-a/b because their control planes run on
  sensai, where local traffic reaches them under sensai's own LAN IP. A
  control plane on sens1 crosses Cilium's overlay, and Cilium resolves that
  traffic to the reserved identity `remote-node` *before* checking any
  `ipBlock` — so no CIDR guess, correct or not, ever matches it (two guesses
  were tried and both silently dropped with a hung handshake, no RST).
  `hubble observe --pod <ns>/<pod>` named it in one line; plain NetworkPolicy
  cannot express "allow this identity" at all, only `CiliumNetworkPolicy`'s
  `fromEntities: [remote-node]` can (`tenant-cnp-amd.yaml`).
- **Pods scheduled via the real `hami-scheduler` path get automatic disjoint
  CU-mask assignment** from the HAMi-gvisor fork's own allocator
  (`cusForRequest`, proportional to the VRAM request) — and it **overrides**
  a manually supplied `amd.com/cu-mask` rather than trusting it. The earlier
  bare-namespace three-tenant test bypassed `hami-scheduler` entirely (AMD
  pods there use the default scheduler), which is why it needed manual masks.
  Three tenants requesting only their own VRAM, no CU hint given, got
  genuinely disjoint masks (14/22/8 CUs, zero overlap) with no coordination.

150 requests × 64 tokens per tenant, three tenants, three models, concurrent,
~3 minutes: 60.7/57.0/50.1 tok/s, GPU at 100% throughout, no restarts,
`torch.cuda.mem_get_info()` still showed only each tenant's own quota after
450 real requests. See `~/vllm-overhead/PLAN.md`.

### The two mechanisms tested together: vCluster tenancy + host-level slicing, NVIDIA

Cluster-level tenancy (vCluster) and host-level GPU slicing (gVisor/nvproxy)
are orthogonal in what they *guarantee* -- confirmed, not assumed. Three
genuinely separate vClusters (`tenant-nv-a`, `tenant-nv-b`, `tenant-c`),
weights 100/50/25, all reporting the identical `80,944`-token KV cache
(comparable setups). `nvidia-smi` inside each still reports the 3072 MiB
quota, not the real device, and a cross-tenant pod probe timed out rather
than connecting. Neither mechanism weakened the other.

They are **not** orthogonal in what they *deliver*: throughput ordered
correctly (2763.96/1936.79/1660.84 tok/s, A>B>C) but compressed toward equal
much more than the bare-namespace reference test at the same weights
(-13.7/+1.8/+11.8 points vs -2.6/+0.1/+2.6). Not yet explained; every tenant
here runs its own control plane and synced networking path, any of which
could interact with the scheduler's granting, but that's a hypothesis. See
`~/vllm-overhead/PLAN.md`.

**A real, unresolved finding along the way, worth knowing before touching
tenant-a/b again**: reusing them (as asked) hit two of their own deliberate
hardening measures — PodSecurity `baseline` blocking `hostPath`, and the
Cilium tenant floor blocking egress *even to the cluster's own excepted
pod/service CIDR*, in a way a scoped `NetworkPolicy` egress allow did not
fix. `hubble observe` showed `Policy denied DROPPED` for traffic to a
ClusterIP inside the supposedly-excepted range. Root cause not settled —
possibly `toCIDRSet`+`except` never applies to a destination that already
resolves to a known Cilium identity (a real pod) rather than a genuinely
external one. Left open rather than bypassed; needs `cilium policy trace` or
direct map inspection, not more guessing. Worked around for this test with
two fresh tenants matching the AMD side's simpler `tenant-amd-a/b/c` recipe.

### SVM: denying it is better than forwarding it

Do not "fix" SVM by making it reach the driver. That was tried (`95a5fa7e1`)
and reverted (`cc8d83036`) on an A/B of that single change: forwarded, vLLM's
engine died with exit 128 during RCCL init; denied, the identical pod reached
an 8.66 GiB KV cache. Denial gives ROCr an error it falls back from, and under
KVM a driver acting on a guest VA is acting on the wrong memory.

Note SVM is variable-length — a header plus `NAttr` attributes — so its command
number encodes a different size per call and the exact-match dispatch never saw
it. That is why the log said `UNKNOWN KFD COMMAND 0xc0284b20` rather than
naming it.

### A wedged GPU sandbox does not clean up

It holds `/dev/kfd` and its VRAM. `kubectl delete --force` returns while the
Sentry keeps running, and `runsc delete --force` returns 0 without reaping it.
`pgrep runsc-sandbox` finds nothing because the Sentry appears as **`exe`**;
`sudo lsof /dev/kfd` names it. Until it is gone `rocm-smi` shows the VRAM still
held and the next pod fails with "Free memory on device cuda:0 (1.38/11.98
GiB)", which reads like a quota bug and is not one.

### A CU mask must select whole workgroup processors

RDNA pairs compute units, and KFD returns `EINVAL` for a queue mask that enables
half a pair. amdproxy applies the sandbox's mask to every queue at creation, so
a mask like `0x7` made every queue creation fail; the proxy destroyed the queue
as designed, ROCr did not handle that and died on a null dereference, and what
the operator saw was **a container that hung**. `034d71a64` rejects such a mask
at startup, naming the offending compute unit and suggesting a valid mask. The
rule is driven by `gfx_target_version` from the host topology, so GCN and CDNA —
which do not pair — are unaffected.

Measured: `0x3`, `0xc`, `0xf`, `0x33`, `0x3ffffff` run; `0x1`, `0x5`, `0x7`,
`0xaa`, `0x1fff`, `0x7ffffff` all hung before and are now refused in about a
second. **The slicing granularity on RDNA is 2 CUs, not 1** — "half of 54" is 26
or 28, never 27. Native ROCr does the opposite with such a mask: it silently
ignores it and runs on the whole device, which matters when comparing against
native.

## AMD: what concurrent sharing actually delivers

Measured on sens1's Navi 32 with `gpuburn` (ALU-bound) and the harnesses in
`~/amdtest/`: `fairness.sh`, `fairness2.sh`, `tenants.sh`, `nativevs.sh`.
`~/amdtest/README.md` has the method and the traps.

- **The proxy costs nothing measurable.** Solo full device: native 11833–11924
  vs sandbox 11945–11987 iters/s. Solo 8 CUs: native 1586.5 vs sandbox 1591.0.
- **Throughput tracks the mask.** 54 CUs 11987, 26 CUs 5849, 12 CUs 3154, 6 CUs
  1609, 2 CUs 544. Per-CU rises as the slice shrinks (222/CU at 54, 272/CU at 2)
  because a smaller slice clocks higher.
- **Isolation is the strongest result.** A on a disjoint half saw its rate move
  **−0.61%** when B started underneath it and **+0.39%** when B left.
- **Equal disjoint shares are exactly fair up to 3 tenants.** Two halves: Jain
  1.0000. Three thirds: 4424.2 / 4423.2 / 4425.7, Jain 1.0000. Two sandboxes
  deliberately oversubscribed onto the *same* 26 CUs split 3349/3352, Jain
  1.0000 — neither starved.
- **Memory quotas hold under concurrency.** Three sandboxes allocating at once
  under 2 GiB / 1 GiB / 512 MiB each stopped at exactly its own ceiling and each
  got exactly half back after freeing half. No cross-talk, and none saw the
  device's 12272 MiB.
- **Proportionality is approximate, not exact.** 2:1 in CUs gave 1.84:1 in
  throughput (−8%), 3:1 gave 2.67:1 (−6.6%), 5:1 gave 5.75:1 (+31%). Fine for
  capacity planning, not a precise dial.
- **Past 3 tenants the GPU throttles, and that is not gVisor's doing.** With
  8 CUs each, 2 and 3 tenants get 100% of solo; from 4 on, service goes bimodal
  — some tenants at ~100%, the rest at ~58%. **Native processes using
  `HSA_CU_MASK` behave identically**: at 6 tenants native scored Jain 0.9385
  with aggregate 6805 and gVisor Jain 0.9387 with 6790, a 0.2% difference. So
  this is the GPU and its driver, faithfully reproduced. Placement is not the
  cause — 8 CUs at six different offsets all measure 1580–1588 solo, a 0.5%
  spread. A plausible cause not yet confirmed is hardware queue slots
  (`num_cp_queues 8`) being oversubscribed once each tenant's runtime brings its
  own queues; that is a hypothesis, not a measurement.

The practical reading: **this GPU slices cleanly for two or three tenants and
degrades unevenly beyond that**, and the degradation is the hardware's, so a
scheduler should cap concurrent GPU tenants rather than expect the CU mask to
hold service flat.

## NVIDIA: done, and verified against hardware

- **GPU memory quota.** `--nvproxy-gpu-memory-limit`, admit-before-forward.
  Device memory and UVM address space are counted together, since unified memory
  commits into device memory and counting them apart would yield twice the
  limit. Over-limit allocations fail with `NV_ERR_NO_MEMORY`, which CUDA reports
  as `CUDA_ERROR_OUT_OF_MEMORY` — the same thing a genuinely full GPU gives.
  `cuMemGetInfo()` is rewritten to report the limit and its headroom, so PyTorch
  and TensorFlow size themselves to what they can actually get, and a sandbox
  cannot observe its neighbours' usage.
- **Compute gating.** `--nvproxy-gpu-compute-percent` holds a sandbox to a
  fraction of wall-clock submission time. Since submission never enters the
  kernel, the gate works by revoking the mappings through which work is
  submitted, outside the window.
- **`pkg/gpusched`** — the scheduler, kept free of I/O so it is testable without
  a GPU. Clients carry a **weight**, not a percentage: weight is meaningful
  without naming a particular GPU, and says something sensible when the shares
  do not sum to 100. Active clients divide each period in proportion to their
  weights; windows are placed **end to end by phase** so tenants take turns
  rather than all contending at the start of every period. An idle-but-
  registered client keeps a 5 ms floor, or it could never resume. Overrun is
  charged back against later windows, bounded so one long kernel cannot starve
  its author for many periods.
- **`runsc gpu-scheduler`** — one instance per node, serving windows over a
  socket. The Sentry's syscall filter forbids it to connect to anything, so
  runsc connects on its way to starting the sandbox and donates the open fd, as
  it already does for the control server; inside, it is read and written as a
  plain fd.
- **Per-device scheduling.** One `Scheduler` per GPU: sandboxes on different
  devices do not contend, and holding them to shares of each other left both
  GPUs idle most of every period. `DeviceTable` resolves the many names a GPU
  arrives under (device-plugin UUID, `docker --gpus` index, CRI device node)
  onto the index `nvidia-smi` reports. A sandbox on several GPUs registers with
  each and is held to the smallest window any granted.
- **The webhook reads the pod and leaves the injection behind.** HAMi's webhook,
  scheduler and device plugin are used unchanged; only the `libvgpu.so` preload
  is dropped. `nvidia.com/gpumem` becomes a memory limit and `nvidia.com/gpucores`
  becomes a *weight*, restated where the container cannot reach it, and
  `CUDA_DISABLE_CONTROL` stands the redundant in-container copy down. Nothing a
  pod requests is changed, so HAMi places pods exactly as before and gVisor
  needs no device plugin of its own.
- **Measured on hardware.** Two pods at weights 300 and 100 received 486 and 162
  kernel launches/s — **3.00:1 against the 3:1 requested** — and together kept
  99% of the throughput one pod gets alone. Two pods contending *without* the
  scheduler get 324 each against 654 solo, so the division is the scheduler's
  doing and not the hardware's. A sandbox that stops submitting drops to the
  5 ms floor and its neighbour expands to the whole period; one that disconnects
  releases its share entirely.

### Three NVIDIA-side caveats that matter

- **`--measure-usage` is on by default and is defective.** It reads per-process
  use from `nvidia-smi pmon` to charge sandboxes for what they took rather than
  what they asked for, which is the right idea — but on an *ordinary* workload
  it divides badly. Two pods running an identical kernel loop at equal weights
  get 324/324 with it off and **31/618 with it on**, because `pmon` credits most
  of the device to whichever pod is being throttled and the repayment throttles
  it further. Reproduced on the previous binary, so it is not a consequence of
  per-device scheduling. **The documented setup turns it off.** Where it does
  help — one pod submitting kernels ~100× longer than the other — it cuts the
  imbalance from 2.08:1 to 1.37:1, not to nothing: a 150 ms kernel cannot be
  preempted inside a 100 ms period, and `pmon` samples once a second.
- **The gate is a cap, not a share, without the scheduler.** Time one sandbox
  leaves unused is not given to another, because a Sentry sees only its own
  sandbox. Useful for holding a tenant to an agreed fraction; not for dividing a
  GPU efficiently. The scheduler exists precisely to fix this.
- **Gating costs more than the submitting thread.** While a sandbox is held, the
  task waiting to submit holds its address space's lock, so other threads in
  that sandbox stall on anything needing it. A thread doing continuous
  mmap/fault/munmap fell to ~20% of its unlimited rate under a 25% limit, while
  a thread computing over resident memory was unaffected. This is the price of
  handling the fault in the Sentry rather than in an in-container handler — that
  is, the price of the governing constraint.

### Checkpointing a GPU sandbox does not work

`ffffc6e3d` removed the first obstacle: mappings of `/dev/nvidia#` and
`/dev/nvidia-uvm` are backed by host device memory and cannot be serialized, and
both inherited a no-op `InvalidateUnsavable`, so any sandbox that had touched a
GPU failed to save. Dropping them costs nothing — the application faults on next
access after restore, which already happens whenever the compute gate revokes
them.

It is still blocked. Only `rmAllocObject` and `rootClient` implement `Restore`;
objects recorded as `miscObject` or `osDescMem` do not, and those cover the OS
events every CUDA context creates. Both obstacles predate this branch.

## Two fixes that belong upstream, not here

Both are gVisor bugs with nothing vendor-specific about them; they affect any
proxied device. See `UPSTREAM-NOTES.md`. **Not yet sent.**

- `797e29b80` — `GenericConfigureMMap` rejected any mmap with offset+length >
  `MaxInt64`. Correct for a file, wrong for a device, where the offset is an
  opaque token with type bits in the high bits. Every KFD doorbell mapping was
  refused with `EOVERFLOW`.
- `6bfb3c267` — a container could panic the Sentry by mmaping a proxied device
  whose host mmap fails: the wrapped error reached `ExtractErrno`, matched no
  case, and hit the panic. A denial of service from inside the sandbox.

## Kubernetes traps, both vendors

**containerd does not pass pod annotations to the sandbox spec.** The CRI plugin
copies only its own `io.kubernetes.cri.*` annotations. Without
`pod_annotations = ["dev.gvisor.*"]` on the runtime handler, every
`dev.gvisor.flag.*` annotation is dropped and each pod silently runs with the
**node-wide ceiling** from the runtime config rather than its own quota.
Measured on AMD: a pod asking for 4 × 512 MiB allocated 12032 MiB. It is silent
— the flags simply arrive at their configured defaults. **This bites both
proxies identically**; it was hit first on the NVIDIA side and then again on
AMD, so check it before believing any per-pod limit.

**The Kubernetes config file is not runsc's config file.** `/etc/runsc/config.toml`
— what containerd's `ConfigPath` points at — is decoded into
`pkg/shim/v1/runsc.Options`. runsc flags belong under a `[runsc_config]` table;
unknown top-level keys are dropped without a word. It had been written in
runsc's own flat format, where only `root` happened to match a field, so every
pod silently ran with no `--amdproxy`, no `--platform=kvm`, and no debug log.

That failure mode is worth recognising by shape: `open("/dev/kfd")` returned
**ENXIO**, and amdproxy logged *nothing*, because the workload's spec still
lists `/dev/kfd` so gVisor still creates the node — with the host's major number
and nothing registered behind it. The `open` dies in VFS dispatch, before any
driver code runs. **Absence of a log line from the subsystem you suspect is
evidence the subsystem was never reached, not evidence it is quiet.**
`createDeviceFile` now warns in exactly this case.

**A device plugin may not survive a kubelet restart.** The AMD one registers
once and has no re-register loop, so after `systemctl restart k3s` the pod stays
`Running` while the node advertises `amd.com/gpu-vram-mib: 0` and every GPU pod
sits `Pending` with `Insufficient amd.com/gpu-vram-mib`. Delete the plugin pod to
re-register.

Two smaller ones, both of which cost real time:
- `/var/log/pods/<pod>/gvisor.log` is the **user** log — compat events only.
  Sentry warnings go to `/var/log/runsc/`, and if that directory has no recent
  `*.boot.txt`, debug logging never reached runsc at all.
- containerd's `k8s.io` image store is **separate from docker's**. A stale
  `vecadd` there, invisible to `docker images`, was missing `libamd_comgr.so.3`
  and reported "no ROCm-capable device is detected" — which reads like a proxy
  failure but arrives *after* the sandbox has already enumerated the GPU.
  `AMD_LOG_LEVEL=4` plus `LD_DEBUG=libs` in the pod env named it in one run.
  Compare both stores before believing any K8s-only bug.

## Context worth having

**The KVM device-memory bug is not a gVisor bug.** Under `--platform=kvm` on a
kernel < 6.13, only the head page of each compound allocation behind an amdgpu
mapping is usable; every tail page gets `SIGBUS`. `hva_to_pfn_remapped()` calls
`kvm_try_get_pfn()` => `get_page_unless_zero()`, a tail page's refcount lives on
its head, so it returns `-EFAULT`, and gVisor turns the unservicable fault into
`SIGBUS`. TTM allocates GTT in high orders, so 511 of every 512 pages are tail
pages. Fixed upstream for 6.13 by "KVM: Stop grabbing references to PFNMAP'd
pages" — a series whose own motivation cites exposing GPU TTM buffers to KVM
guests. `UPSTREAM-NOTES.md` has the full trace, the ruled-out hypotheses, and
the affected-kernel table. **6.12 is the LTS and is one release short.**

**nvproxy escapes that by luck, not design.** Identical gVisor code path; the
difference is that Nvidia's driver maps BAR pfns (no `struct page`) and order-0
system pages, both of which KVM accepts. This is the clearest example of a
general lesson on this branch: the two proxies share almost all of the gVisor
machinery, and where they behave differently it is usually the *driver* that
differs, not gVisor.

**Measure, do not infer.** The KVM bug got attributed wrongly twice — first to a
KVM limitation, then over-corrected to purely gVisor's fault — both times from
plausible reasoning about code that was never run. What settled it was a
tracepoint and a probe that printed *which* pages worked. The same thing nearly
happened with the AMD 4-tenant degradation, which looked like a gVisor fairness
failure until native `HSA_CU_MASK` processes were measured doing the identical
thing. **If a claim about this codebase matters, there is almost always a
reproducer that can decide it**, and `~/amdtest/` and `~/nvproxy-quota-test/`
are where they live.

**Conventions.** Hand-written Go needs gofmt's alignment; use bazel's gofmt
binary rather than eyeballing struct tags. New ABI structs need `+marshal` and
the matching BUILD deps; BUILD deps are sorted. On sens1 git has no configured
identity, so commits need `-c user.name=dmd -c user.email=dmd17@cornell.edu`.

## Next

1. **Document the AMD half in `g3doc/user_guide/gpu.md`.** That file is
   1054 lines and entirely NVIDIA; amdproxy has no user-facing documentation at
   all. This is the largest remaining gap in making the branch genuinely
   two-vendor.
2. **Fix or default off `--measure-usage`.** It is on by default and makes an
   ordinary two-pod split *worse* than no scheduler at all (31/618 against
   324/324). The documented setup works around it; the code should not need the
   workaround. Related, still open: the compute gate assigns *correct weighted
   windows* once the scheduler knows a sandbox is active, but on Volta+ GPUs it
   cannot *enforce* them against a doorbell-submission workload (cuBLAS) — see
   `SECURITY-FINDINGS.md`, where three mechanisms (command-buffer revocation,
   TSG preempt/unschedule, doorbell/USERMODE revocation) are each measured to
   fail. A `pkg/gpusched/server.go` change that uses the sampler for *activity
   detection* (not the divergent debt path) is in the tree and fixes the
   scheduling half.
3. **nvproxy must handle multiple NVIDIA GPU architectures, with runtime family
   detection.** One driver ABI spans Turing→Blackwell, and behaviour differs by
   generation — e.g. cuBLAS on a *Blackwell* GPU allocates a *Hopper* USERMODE
   class, and whether the compute gate can enforce at all is a per-family
   property (all Volta+ submit by doorbell). Detecting the GPU from the driver
   version alone is wrong. First piece landed (`pkg/sentry/devices/nvproxy/
   gpuarch.go`): a `gpuArch` enum + `archFromClass` (compute class is
   authoritative; channel class is the fallback; USERMODE is unreliable) +
   `submitsByDoorbell()`, detected at runtime from the classes the sandbox
   allocates and logged once (verified: reports "Blackwell" on the RTX 5070).
   Extend this to drive family-specific behaviour wherever the code currently
   assumes one architecture, and to gate/deny features a given family does not
   support. **`NVIDIA-COMPUTE-ISOLATION.md` is the definitive record + per-GPU
   test playbook:** on the consumer RTX 5070 (Blackwell GB205), *no* Sentry- or
   driver-reachable mechanism enforces a compute share against a doorbell
   workload — every temporal lever is ineffective and every spatial control
   (`SET_TPC_PARTITION_TABLE`, `SET_CWD_WATERMARK`) is `NV_ERR_NOT_SUPPORTED`,
   even at kernel privilege via the open driver (privilege was not the wall; die/
   firmware feature-support is). Compute isolation for arbitrary CUDA is a
   property of the GPU *die class*, not gVisor. **Immediate next work: run that
   doc's Step-3 playbook on the DC/pro nodes (RTX A6000 GA102, RTX 6000 Pro
   Blackwell GB202)** to find whether they enable those controls where GB205 did
   not. (Memory-quota isolation is separate and works everywhere.)
4. **Send the two upstream fixes** (`797e29b80`, `6bfb3c267`) and file the KVM
   issue.
5. **SVM (`AMDKFD_IOC_SVM`) is denied, deliberately.** See the SVM section
   above: forwarding it crashes ROCr's initialisation, and denial is what lets
   ROCr fall back. `hipMallocManaged` will not work; hipMalloc workloads
   (vecadd, and vLLM up to graph capture) are unaffected.
6. **`/dev/kfd` mappings are impossible on systrap** (task #18). Not a bug: KFD
   binds each mapping to the process holding the KFD context, and systrap maps
   from a stub process, so `mmap` returns `EINVAL`. Only the KFD mapping is
   process-bound; the render node's is not. KVM is the platform to use.
7. **Registration keys off the root container's spec.** `registerFilesystems`
   takes `&l.root`, so under Kubernetes — where the root is the pause container
   and carries no devices — spec-based detection never fires and a pod depends
   on `--amdproxy` being set for the whole sandbox. nvproxy has the same shape,
   so this is upstream's design rather than a local bug, but it means the flag
   is mandatory in Kubernetes. The GPU scheduler works around the same problem
   on the NVIDIA side by re-announcing devices from `StartSubcontainer`; the
   same trick may apply here.
