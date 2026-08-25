# Multi-tenant GPU slicing: full stack setup

How to stand up the whole stack that divides a single GPU between
mutually-untrusting containers — NVIDIA (`nvproxy`) and AMD (`amdproxy`), with
every limit enforced in the gVisor Sentry (or a trusted host component), never
inside the container.

This is the reference recipe for the two-node, two-vendor cluster (`sensai` =
NVIDIA, `sens1` = AMD, one k3s control plane). A single-vendor setup is a strict
subset: build the same gVisor, skip the other vendor's driver/device-plugin, and
follow the matching column below.

> **Scope of what works today.** All deployed and verified: memory-quota
> isolation (both vendors); AMD spatial CU-mask partitioning **and** AMD
> work-conserving time-slicing (mutually exclusive per device on RDNA3 — Part 8b);
> and NVIDIA compute scheduling by weight — the Sentry gate for kernel-launch
> workloads, and the **driver-runlist broker** for doorbell/cuBLAS/ML workloads
> (Part 8a), validated on GA100, GA102, and GB205. The runlist broker needs the
> open-driver fork (Part 4) and `runsc gpu-scheduler --runlist-control`; memory
> quota alone needs none of the compute machinery. A single-vendor stack skips
> the other vendor's driver/plugin.
>
> **Companion:** [`../vcluster-multitenant/SETUP.md`](../vcluster-multitenant/SETUP.md)
> is the same recipe from the cluster repo's side; keep the two in sync.

---

## 1. Components and repositories

Every repo below is a **fork** under `github.com/pvnis`; the upstream it tracks
is named so you can see the delta. Clone the fork, not upstream.

| Component | Repo (fork) | Branch | Upstream | What it provides |
| --- | --- | --- | --- | --- |
| **Modified gVisor** | `github.com/pvnis/gvisor` | `gpuslicing` | `google/gvisor` | `runsc` with `nvproxy`, `amdproxy`, `pkg/gpusched`, the `runsc gpu-scheduler` daemon, and the in-tree `webhook/`. The core of everything. |
| **NVIDIA open driver fork** *(Part 4; NVIDIA compute isolation)* | `github.com/pvnis/open-gpu-kernel-modules` | `gpuslicing` | `NVIDIA/open-gpu-kernel-modules` | The driver broker: `/proc/driver/nvidia/gpusched` (runlist detach/attach + per-TSG timeslice + `RESTART_RUNLIST` + the per-tenant `tsgs` report) and per-tenant UVM eviction. Required for NVIDIA *compute* isolation (Part 8a); memory quota alone runs on a stock 610.43.02 driver. See `../open-gpu-kernel-modules/DRIVER-CHANGES.md`. |
| **HAMi** | **upstream `projecthami/hami:v2.9.0`** | — | — | `hami-scheduler` (placement), `hami-device-plugin` (advertises `nvidia.com/gpu`), `hami-webhook`. Runs the whole NVIDIA path and the AMD *time-slicing* path unmodified. |
| **HAMi fork** *(AMD spatial CU masks only)* | `github.com/pvnis/HAMi` | `gvisor-amd-timeslice` | `Project-HAMi/HAMi` | Adds automatic **disjoint AMD CU-mask** assignment (`pkg/device/amd/cualloc.go`, `cusForRequest`); emits a weight **or** a mask, never both. The only reason to run the fork instead of upstream — needed only for AMD spatial partitioning. |
| **Quota-translation webhook** | in-tree: `github.com/pvnis/gvisor` → `webhook/` | `gpuslicing` | — | Mutating admission webhook (`webhook/pkg/gpushare`) that restates a pod's GPU request — **both vendors** — as `dev.gvisor.flag.*` annotations (narrow-only) so the sandbox quota comes from what the scheduler admitted, and drops HAMi's `libvgpu.so`. Built from the gVisor tree; no separate repo. (The standalone `pvnis/gpu-quota-webhook` was the predecessor and is retired.) |
| **AMD device plugin** *(AMD node only)* | `/home/dmd/amdgpu-device-plugin` | `master` | `ROCm/k8s-device-plugin` | Advertises `amd.com/gpu-vram-mib`. Its `Allocate()` env vars are **not** used for enforcement — gVisor reads annotations. Topology code treats VRAM `heap_type` 1 and 2 alike (needed for RDNA). |
| **Cluster config / vCluster** | `/home/dmd/vcluster-multitenant` | — | — | k3s config, Cilium values, containerd drop-ins, vCluster tenant values + network policies, and the reference manifests. `TWO-VENDOR-CLUSTER.md` + `CILIUM-DESIGN.md` are the authoritative runbooks. |

Third-party components installed as-is (no fork):

| Component | Version used | Role |
| --- | --- | --- |
| **k3s** | v1.36.2 (pin agent ≤ server) | The Kubernetes distribution. sensai = control plane, sens1 = agent. |
| **Cilium** | (Helm) VXLAN tunnel, `kubeProxyReplacement` | CNI; provides pod-to-pod and cross-tenant network isolation. |
| **vCluster** | current | Cluster-level multi-tenancy (each tenant its own API server). Optional but part of the reference. |
| **containerd** | k3s-bundled | Runtime; hosts the `runsc` runtime handler. |
| **Tailscale** | current | The only path between the two nodes (sensai is NAT'd). Both nodes use their Tailscale IP as the k3s `node-ip`. |

Test harnesses / reproducers (not part of the runtime, but referenced
throughout): `~/amdtest/` (AMD), `~/nvproxy-quota-test/` (NVIDIA memory),
`~/vllm-overhead/` (vLLM tenancy), `ghost-experiment/` in this repo (the driver
broker).

---

## 2. Node prerequisites

| | sensai (NVIDIA) | sens1 (AMD) |
| --- | --- | --- |
| OS / kernel | Ubuntu 22.04, 6.8.x | Ubuntu 24.04, **7.0.0-28+** (KVM device-memory fix needs ≥ 6.13) |
| GPU | RTX 5070, driver **610.43.02**, 12 GiB | Navi 32 (gfx1101), 54 CUs, 12 GiB; **ROCm 7.2** |
| gVisor platform | `systrap` (or `kvm`) | **`kvm`** (required — see note) |
| container runtime | containerd only (no docker) | docker + containerd |
| GPU scheduler role | `hami-scheduler` | `hami-scheduler` (or `default-scheduler` for bare-annotation spatial) |
| `runsc gpu-scheduler` | required (runlist enforcement) | required (time-slicing coordination) |

**Why AMD requires `--platform=kvm`.** `/dev/kfd` binds each mapping to the
process holding the KFD context; systrap maps from a stub process, so the mapping
`mmap` returns `EINVAL`. Only KVM presents the Sentry as the mapping process. On
kernels **< 6.13**, KVM also has a device-memory bug (tail pages of a compound
amdgpu allocation `SIGBUS`); sens1 runs 7.0.0-28 to avoid it. See
`UPSTREAM-NOTES.md`.

---

## 3. Build and install modified gVisor

Clone the fork and build `runsc` (which contains both proxies **and** the
`gpu-scheduler` subcommand):

```sh
git clone git@github.com:pvnis/gvisor.git && cd gvisor
git checkout gpuslicing
```

**On sensai (no docker, containerd only)** — build with bazel directly; `make
build` needs docker:

```sh
bazel build //runsc:runsc
```

**On sens1 (docker via sudo, dmd not in the docker group)** — put a
`sudo docker` shim first on `PATH`:

```sh
PATH=/home/dmd/amdtest/bin:$PATH make build TARGETS=//runsc:runsc \
    DOCKER_CLI_PATH=/home/dmd/amdtest/bin/docker
```

**Verify the build actually changed** — bazel reports "0 actions" for a build
whose output did change, and its symlink can be stale:

```sh
sha256sum bazel-bin/runsc/runsc_/runsc          # compare to the deployed copy
strings bazel-bin/runsc/runsc_/runsc | grep <a string you just added>
```

Install to **both** locations on each node — Docker and Kubernetes both run
`/usr/local/bin/runsc`:

```sh
sudo cp bazel-bin/runsc/runsc_/runsc /usr/local/bin/runsc
cp bazel-bin/runsc/runsc_/runsc ~/amdtest/runsc      # (sens1 convenience copy)
```

### The compute scheduler daemon

`runsc gpu-scheduler` is one host daemon per GPU node; sandboxes connect to it
over a Unix socket and it hands each a weighted time window. It is required on
**both** GPU nodes — the NVIDIA node for runlist enforcement, and the AMD node to
coordinate work-conserving time-slicing (a Sentry cannot see how many other
tenants compete, so the coordinator must). Run it as a systemd unit on the socket
the runsc config names (`/run/runsc-gpu-scheduler.sock`):

```ini
# /etc/systemd/system/runsc-gpu-scheduler.service   (on EVERY GPU node)
[Service]
# NVIDIA node — drive the hardware runlist:
ExecStart=/usr/local/bin/runsc gpu-scheduler --socket /run/runsc-gpu-scheduler.sock \
    --measure-usage=false --runlist-control=/proc/driver/nvidia/gpusched
# AMD node — same daemon WITHOUT --runlist-control (amdproxy drives DBG_TRAP itself):
# ExecStart=/usr/local/bin/runsc gpu-scheduler --socket /run/runsc-gpu-scheduler.sock --measure-usage=false
Restart=always
```

`--measure-usage` defaults **on** and currently misprices the ordinary
two-tenant case (see `SECURITY-FINDINGS.md` / the GPU blog); the documented setup
passes `--measure-usage=false` until that is fixed.

**`--runlist-control` (NVIDIA, needs the Part-4 open-driver fork).** Empty, the
scheduler enforces via the Sentry compute gate — which on Volta+ **cannot bind a
doorbell/cuBLAS workload at all**, so a real ML tenant is unisolated. Set to the
fork's `/proc/driver/nvidia/gpusched`, it drives the hardware runlist (the credit
scheduler: per-tenant credit accrual weighted by the driver's `tsgs` report,
whole-tenant detach/attach), which does bind cuBLAS. This is the only mode that
actually divides compute between adversarial NVIDIA tenants; see Part 8a. Measured
to close the process-packing share-theft on GA100, GA102, and GB205
(`SECURITY-FINDINGS.md` V4). On the **AMD** node, leave `--runlist-control` unset:
amdproxy performs the DBG_TRAP suspend/resume itself and the scheduler only places
the windows.

> **Operational SPOF — the scheduler is a hard, fail-closed dependency.** With
> `nvproxy-gpu-scheduler-socket` set in the runsc config, a GPU pod **cannot
> start at all** while `runsc-gpu-scheduler` is down — sandbox creation fails with
> `connecting to the GPU scheduler … connection refused`. That is deliberate
> (failing open would mean an unlimited sandbox), but it makes this service a
> single point of failure for every GPU pod on the node. To take a
> *no-scheduler* baseline, comment the socket out of the runsc config — the shim
> re-reads it per sandbox, so a fresh pod picks it up **without** a k3s restart.

---

## 4. NVIDIA driver

For **memory quota only**, the stock NVIDIA **610.43.02** driver (proprietary or
open) is all you need — install it normally and confirm `nvidia-smi` sees the card.

For **NVIDIA compute isolation** (Part 8a — the only thing that binds a
doorbell/cuBLAS ML tenant) and for GPU-memory overcommit, install the open-driver
fork:

```sh
git clone git@github.com:pvnis/open-gpu-kernel-modules.git
cd open-gpu-kernel-modules && git checkout gpuslicing
# per-GPU spatial knob (TPCs = SMs / 2): RTX 5070 = 24, A6000 = 42, A100 = 54
#   src/nvidia/src/kernel/gpu/fifo/kernel_ctxshare.c: #define GHOST_TOTAL_TPC <N>
make modules -j$(nproc)
sudo bash ../gvisor/ghost-experiment/reload.sh   # unload + insmod, restart k3s + the scheduler
```

The fork is *unpackaged* — it reverts on reboot unless the modules are placed in
`/lib/modules/.../updates`. Sandboxes then run with
`--nvproxy-allow-unsupported-driver`. Confirm it loaded
(`cat /sys/module/nvidia_uvm/srcversion`; count `GHOST` lines in `dmesg`, 0 = hook
absent). Details: `../open-gpu-kernel-modules/DRIVER-CHANGES.md`.

---

## 5. containerd + runsc runtime config (per node)

Two files per node, and both traps below are silent if you get them wrong.

**(a) The runsc config** — decoded into `pkg/shim/v1/runsc.Options`; runsc flags
**must** live under a `[runsc_config]` table or they are dropped without a word.

`sensai:/etc/runsc/config.toml` (NVIDIA):

```toml
log_level = "info"
[runsc_config]
  nvproxy = "true"
  nvproxy-allow-unsupported-driver = "true"
  nvproxy-docker = "true"
  nvproxy-gpu-scheduler-socket = "/run/runsc-gpu-scheduler.sock"
  nvproxy-gpu-weight = "100"                 # node-wide ceiling; webhook lowers per-pod
  debug-log = "/var/log/runsc/%ID%/"
```

`sens1:/etc/runsc/config.toml` (AMD) — same shape. The AMD side now also carries a
weight + scheduler socket for time-slicing; a CU mask is the mutually-exclusive
spatial alternative (Part 8b), never both:

```toml
[runsc_config]
  platform = "kvm"
  amdproxy = "true"
  amdproxy-gpu-memory-limit = "..."          # node ceiling; webhook lowers per-pod
  amdproxy-gpu-scheduler-socket = "/run/runsc-gpu-scheduler.sock"   # time-slicing
  amdproxy-gpu-weight = "100"                 # node ceiling; webhook lowers per-pod
  # amdproxy-cu-mask = "0xfff"                # spatial INSTEAD of time-slicing (not both)
```

**(b) The containerd runtime handler** — register `runsc` and, critically,
forward the annotations. Use a k3s drop-in so it survives upgrades:
`/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d/10-runsc.toml`:

```toml
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
  pod_annotations = ["dev.gvisor.*"]         # WITHOUT THIS every pod silently
  container_annotations = ["dev.gvisor.*"]   # runs at the node-wide ceiling
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc.options]
  TypeUrl = "io.containerd.runsc.v1.options"
  ConfigPath = "/etc/runsc/config.toml"
```

> **Trap 1 — `pod_annotations`.** CRI copies only its own
> `io.kubernetes.cri.*` annotations by default. Without the two lines above,
> every `dev.gvisor.flag.*` annotation is dropped and each pod runs with the
> node-wide ceiling. Measured on AMD: a pod asking for 4 × 512 MiB got 12032 MiB.
>
> **Trap 2 — `[runsc_config]` table.** The Kubernetes config file is *not*
> runsc's flat config file. Flags at the top level are dropped silently; a pod
> then runs with no `--amdproxy`, no `--platform=kvm`, and `open("/dev/kfd")`
> returns `ENXIO` with **no proxy log at all** (the node exists but nothing is
> registered behind it). Absence of a log line is evidence the subsystem was
> never reached.

**Verify the flags actually arrived** (do not infer from a plausible number):

```sh
sudo grep -m1 Args: $(sudo ls -t /var/log/runsc/*.boot.txt | head -1)
# the --*proxy-gpu-memory-limit on that line must be the POD's value
```

The `RuntimeClass`:

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: gvisor }
handler: runsc
```

One `RuntimeClass gvisor` serves both vendors — each node resolves `runsc`
through its own `ConfigPath`, so the same class yields nvproxy flags on sensai
and amdproxy flags on sens1 with nothing vendor-specific in the pod spec.

---

## 6. k3s cluster + Cilium

Full runbook: `vcluster-multitenant/TWO-VENDOR-CLUSTER.md` and
`CILIUM-DESIGN.md`. The load-bearing points:

- **Both nodes use their Tailscale address as `node-ip`.** sensai is NAT'd and
  unreachable from sens1 over LAN; Cilium's VXLAN tunnel needs two-way
  reachability. Do not "fix" a node IP back to a LAN address. The apiserver cert
  needs the Tailscale IP in `tls-san` (delete the old
  `serving-kube-apiserver.{crt,key}` so k3s regenerates), and Cilium's
  `k8sServiceHost` must be the Tailscale IP too.
- **Label the nodes** so device plugins and schedulers land correctly:
  `gpu.vendor=nvidia,gpu=on` on sensai; `gpu.vendor=amd` on sens1.
- **CNI paths (agent node).** With `flannel-backend: none`, k3s omits the
  containerd CNI block, so containerd falls back to empty upstream defaults and
  the node stays `NotReady` with `cni plugin not initialized` *while Cilium is
  healthy*. The `10-runsc.toml` drop-in points containerd at the k3s CNI paths
  (`/var/lib/rancher/k3s/data/cni`, `/var/lib/rancher/k3s/agent/etc/cni/net.d`).

Config files: `manifests/twovendor/sensai-k3s-config.yaml`,
`sens1-k3s-config.yaml`.

---

## 7. HAMi, the AMD device plugin, and the quota webhook

### 7a. HAMi (control plane)

Deploy **upstream** `projecthami/hami:v2.9.0` — `hami-scheduler`,
`hami-device-plugin`, `hami-webhook`. It runs the NVIDIA path and the AMD
time-slicing path unmodified. Keep the scheduler, plugin, and webhook — they do
placement and run far from the workload. **Remove the `libvgpu.so` preload:** blank
the device-plugin ConfigMap key that installs it, so it is gone from the node
entirely rather than merely silenced (the webhook also stands it down with
`CUDA_DISABLE_CONTROL`).

- NVIDIA (and AMD, for uniform placement) pods set `schedulerName: hami-scheduler`.
- **AMD spatial CU masks only:** run the **fork** instead
  (`github.com/pvnis/HAMi`, `gvisor-amd-timeslice`) — it adds automatic disjoint
  CU-mask assignment (`cusForRequest`, proportional to the VRAM request) and
  overrides a manual `amd.com/cu-mask`. AMD **time-slicing** needs no fork.
- Switching fork↔upstream is a shared-cluster change: snapshot first and keep a
  `runsc gpu-scheduler` up throughout, or GPU pods fail to start. (On upstream,
  HAMi does no AMD-level accounting — the AMD plugin advertises only
  `amd.com/gpu-vram-mib`, so ordinary Kubernetes extended-resource accounting
  refuses oversubscription and the Sentry enforces the real limit.)

### 7b. AMD device plugin (AMD node only)

Deploy from `manifests/twovendor/amd-device-plugin.yaml`
(`nodeSelector: gpu.vendor=amd`). It advertises `amd.com/gpu-vram-mib`; AMD pods
use the default scheduler and are placed by that request alone.

> **After a kubelet/k3s restart the plugin does not re-register** (no
> re-register loop). The node then advertises `amd.com/gpu-vram-mib: 0` and GPU
> pods sit `Pending`. Delete the plugin pod to re-register.

### 7c. Quota webhook (the piece that makes limits mandatory)

Neither HAMi nor the AMD plugin writes the `dev.gvisor.flag.*` annotations, so
without this webhook a pod that omits them runs at the node ceiling — i.e. the
whole GPU. Build and deploy the **in-tree** webhook, `webhook/pkg/gpushare` from
this gVisor tree (no separate repo). It covers both vendors, is narrow-only, and
also performs the `libvgpu.so` stand-down (`CUDA_DISABLE_CONTROL`). It
translates, at admission:

| request | annotation written |
| --- | --- |
| `amd.com/gpu-vram-mib: N` | `amdproxy-gpu-memory-limit = N × 512 MiB` — the AMD plugin advertises VRAM in **512-MiB units** (~23 for a 12 GiB card), not MiB |
| `amd.com/gpu-vram-mib: N` (time-slicing) | `amdproxy-gpu-weight = N` — the requested unit count *is* the ratio (admission runs before placement, so no device size is known and a ratio is all the scheduler needs) |
| `nvidia.com/gpumem: M` (MiB) | `nvproxy-gpu-memory-limit = M × 1 MiB` |
| `nvidia.com/gpucores: C` | `nvproxy-gpu-weight = min(C, 100)` |

Requests are summed across a pod's containers (the flags are per-**sandbox**),
init containers taken with `max()`. **Narrow-only:** a pod may lower its own
limit but an annotation higher than the computed value is overwritten — verified
from inside a vCluster tenant, an 8 GiB self-annotation was rewritten to the
1 GiB requested. It deliberately does **not** assign disjoint CU masks (that
needs placement state admission cannot see — it lives in the HAMi scheduler,
7a).

> **`failurePolicy: Fail` is a security property here, not an availability
> preference.** The webhook is what derives a pod's quota from the request the
> scheduler admitted, so a pod admitted *without* being mutated carries no
> `dev.gvisor.flag.*-gpu-memory-limit` and runs at the node-wide ceiling in
> `/etc/runsc/config.toml` — the whole device. Registered with `Fail`, an
> unreachable webhook blocks pod creation (fails closed, loudly); relaxing it to
> `Ignore` turns an unreachable webhook into a **silent quota escape**. Keep it
> `Fail`.

**Deployment notes** — each blocks the webhook silently if wrong:

- Pass **`--port=8443`**; it defaults to 0 while the Service targets 8443. The
  `--log-level=debug` from the old upstream manifest is not a valid flag (the
  binary exits 1).
- The webhook mints a fresh CA on every start and **reconciles the
  `MutatingWebhookConfiguration`'s `caBundle`** to match, so a pod restart no
  longer wedges admission. This needs **`get` and `update` on
  `mutatingwebhookconfigurations`** in its ClusterRole, alongside `create`.
- Namespace scoping is via `--pod-namespace-labels=<label>`; an *empty* selector
  is rejected by the apiserver, so label the target namespaces explicitly.

---

## 8. Compute isolation

Memory quota (Parts 5, 7) is independent of this. This part is how a *compute*
share is imposed on an adversarial tenant, per vendor.

### 8a. NVIDIA — the driver-runlist broker (production)

The Sentry compute gate holds a kernel-launch workload to a wall-clock fraction,
but on Volta+ it **cannot bind a doorbell/cuBLAS/graph-replay workload** — those
never re-fault the gated buffer, i.e. a real ML tenant is unisolated. The
driver-runlist broker does bind them: with the Part-4 open-driver fork installed
and `runsc gpu-scheduler --runlist-control=/proc/driver/nvidia/gpusched` (Part 3),
the **credit scheduler** (`pkg/gpusched/credit.go`) charges per-tenant credit
weighted by the driver's per-tenant TSG count and detaches/attaches whole tenants
from the runlist. This closes the process-*packing* share-theft (a low-weight
tenant forking N processes to multiply its channel groups), verified on GA100,
GA102, GB205. Nothing pod-side changes — it is all host scheduler + driver.

Manual runlist control (for probing):

```sh
echo poll            | sudo tee /proc/driver/nvidia/gpusched   # list tenant PIDs + tsgs
echo "ts <pid> 3000" | sudo tee /proc/driver/nvidia/gpusched   # weight → timeslice µs
echo "detach <pid>"  | sudo tee /proc/driver/nvidia/gpusched   # evict / restore with attach
```

Under gVisor+KVM the driver attributes a sandbox's objects to the Sentry PID
(which appears as `exe`), so that PID is the per-sandbox handle the broker keys
on. Full record: `NVIDIA-COMPUTE-ISOLATION.md`,
`../open-gpu-kernel-modules/DRIVER-CHANGES.md`.

### 8b. AMD — spatial CU masks XOR temporal time-slicing

On RDNA3 a sandbox may carry a CU mask **or** a time slice, **never both** — the
amdgpu driver refuses a debug session's CWSR workaround on a CU-masked queue
(`kfd_dbg_set_queue_workaround`, `-EBUSY`, GC 11.0.0–11.0.3), and a time slice is
enforced through exactly such a session. runsc rejects a sandbox configured with
both at startup. Memory quota composes with either.

- **Spatial (CU masks):** `amdproxy-cu-mask` (Part 5); tenants run concurrently on
  disjoint CUs, 2-CU granularity. Automatic disjoint assignment needs the HAMi
  fork (Part 7a); otherwise supply `amd.com/cu-mask` manually and the Sentry
  narrows to it.
- **Temporal (time-slice):** `amdproxy-gpu-weight` + `amdproxy-gpu-scheduler-socket`
  (Parts 3, 5); tenants take turns on the whole device by weighted duty cycle,
  work-conserving via `runsc gpu-scheduler`, on **upstream** HAMi. Measured
  300:100 → 3.05:1, Jain 1.0000 at equal weights; `vecadd` stays correct while
  sliced (CWSR preempts mid-kernel).

See `GPU-ISOLATION.md` and `CLAUDE.md` ("Time-slicing AMD from userspace").

---

## 9. Multi-tenant isolation layer (vCluster) — optional

For cluster-level tenancy on top of the host-level slicing, each tenant gets its
own API server and NetworkPolicy boundary via vCluster. This is orthogonal to
the GPU enforcement (verified: neither weakens the other). Recipe in
`TWO-VENDOR-CLUSTER.md`; the essentials:

```sh
vcluster create tenant-amd-a -n tenant-amd-a \
  --values values/tenant.yaml --values values/amd-tenant.yaml \
  --values values/tenant-amd-a.yaml --connect=false
# then apply manifests/tenant-netpol-amd.yaml AND manifests/tenant-cnp-amd.yaml
```

- **Scope node sync to the vendor node** (`sync.fromHost.nodes.selector.labels:
  {gpu.vendor: amd}`) so a GPU tenant's scheduler sees the real allocatable
  without learning about other nodes/tenants.
- **Cross-node vCluster netpol needs Cilium `fromEntities: [remote-node]`.** A
  control plane on the agent node crosses Cilium's overlay, which resolves that
  traffic to the reserved `remote-node` identity *before* any `ipBlock` CIDR is
  checked — so no plain `NetworkPolicy` CIDR ever matches it. Only a
  `CiliumNetworkPolicy` with `fromEntities: [remote-node]` allows it
  (`tenant-cnp-amd.yaml`). Diagnose with `hubble observe --pod <ns>/<pod>`.

---

## 10. Verification

Run one pod per vendor simultaneously and confirm each sees only its own quota:

```sh
kubectl apply -f manifests/twovendor/both-vendors-quota.yaml
kubectl apply -f manifests/twovendor/both-vendors-compute.yaml
kubectl logs -n gpu-test amd-mem        # reports its quota (e.g. 2048 MiB), not 12272
kubectl logs -n gpu-test nv-compute     # cuMemGetInfo total = the limit, not 12227
```

Reference results (RTX 5070 + Navi 32): the NVIDIA kernel loop scored 652.45
launches/s while the AMD probe saturated its own quota on the other node, against
a 652.32 solo baseline — no cross-vendor interference. Two AMD tenants on
disjoint CU-mask halves (`0x03f` / `0xfc0`) each held exactly their 2048 MiB
ceiling. Two NVIDIA pods at weights 300/100 divided 486/162 launches/s (3.00:1)
at 99% of solo aggregate throughput.

---

## 11. Consolidated gotchas

Collected here because each one costs real time and reads like a different bug
than it is:

- **A wedged GPU sandbox does not clean up.** `kubectl delete --force` and
  `runsc delete --force` both return while the Sentry keeps `/dev/kfd` and the
  VRAM. It appears as `exe` (not `runsc-sandbox`); find it with
  `sudo lsof /dev/kfd` / `sudo fuser -v /dev/nvidia*` and kill it, or the next
  pod fails with a "free memory" error that looks like a quota bug.
- **containerd's `k8s.io` image store is separate from docker's.** A stale image
  invisible to `docker images` can be missing libraries and fail *after* the
  sandbox enumerates the GPU. Compare both stores.
- **`gpu_id` and the `/dev/kfd` major are dynamic** — they change on reboot.
  Never hardcode them; read from `/sys/class/kfd/kfd/topology/nodes/*/gpu_id`
  (`~/amdtest/gputest.sh` does).
- **Sentry warnings go to `/var/log/runsc/`, not the pod's `gvisor.log`** (which
  is compat events only). No recent `*.boot.txt` there means debug logging never
  reached runsc.
- **`--measure-usage` (NVIDIA scheduler) defaults on and misprices the ordinary
  case** — two identical equal-weight pods end up 31/618 instead of 324/324. Run
  with `--measure-usage=false` until fixed.
- **AMD CU masks must select whole workgroup processors** on RDNA — an odd mask
  like `0x7` is rejected at startup with a suggested valid mask; granularity is
  2 CUs. `AMDKFD_IOC_SVM` is denied deliberately (forwarding it crashes ROCr).
- **AMD CU mask and time slice are mutually exclusive on RDNA3** (Part 8b) — the
  driver refuses the combination; runsc rejects a sandbox that requests both.
- **`runsc gpu-scheduler` is a fail-closed SPOF on every GPU node** — no gVisor
  GPU pod starts while it is down. Never `pgrep -f gpu-scheduler | kill` (it
  matches and kills your own shell); kill by explicit pid.

---

## Pointers

- `GPU-ISOLATION.md` — the top-level design overview (start here).
- `CLAUDE.md` — the project's own running record of what is done and verified.
- `NVIDIA-COMPUTE-ISOLATION.md` — compute-isolation findings + per-GPU playbook.
- `../open-gpu-kernel-modules/DRIVER-CHANGES.md` — the driver-broker mechanisms.
- `SECURITY-FINDINGS.md` — the red-team and every compute lever measured.
- `UPSTREAM-NOTES.md` — the two gVisor bugs to send upstream + the KVM issue.
- `../vcluster-multitenant/SETUP.md` — the same recipe from the cluster repo (companion; keep in sync).
- `../vcluster-multitenant/{TWO-VENDOR-CLUSTER,CILIUM-DESIGN}.md` — the cluster.
- The GPU sharing blog posts under `website/blog/2026-08-*-gpu-*.md`.
