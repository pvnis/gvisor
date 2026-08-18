# gpu0-a: an A100 node running the whole stack

A single-node k3s cluster on the A100 80GB PCIe VM (`gpu0-a`), with gVisor
enforcing per-pod GPU limits, HAMi placing pods, `runsc gpu-scheduler`
dividing GPU time, and two vClusters standing in for tenants. Built
2026-08-18 from a bare Ubuntu 24.04 image with nothing installed —
no docker, no containerd, no toolchain — following
`g3doc/user_guide/gpu.md`'s Kubernetes section. This file records what that
section does not say, and what this node measured.

    OS            Ubuntu 24.04.3, kernel 6.8.0-138-generic
    GPU           A100 80GB PCIe (GA100), MIG disabled
    driver        610.43.02, the open modules built in ~/open-gpu-kernel-modules
    k3s           v1.36.3+k3s1, flannel, containerd 2.3.2
    runsc         built from this branch (d213a471c), /usr/local/bin/runsc
    HAMi          2.9.0 via helm, upstream chart, libvgpu preload dropped
    vCluster      0.36.1, tenants tenant-nv-a and tenant-nv-b

## The driver on this host is not a package

There is no DKMS and no NVIDIA driver under `/lib/modules` as shipped: the
running driver is the GHOST-instrumented open build in
`~/open-gpu-kernel-modules`, loaded with `insmod`, which meant **a reboot would
have left the machine with no GPU driver at all**. The modules are now copied
to `/lib/modules/$(uname -r)/updates/` with `depmod -a` run and
`/etc/modules-load.d/nvidia.conf` listing `nvidia` and `nvidia_uvm`, so a
reboot brings the GPU back on its own.

**As of 2026-08-18 these are clean, unhooked 610.43.02 modules** built from the
base tag `57130a27` in `~/ogkm-610-clean`, installed both live and under
`/lib/modules/.../updates`. An earlier point in the tree used the
GHOST-instrumented build, whose deferred re-probe defaults to a **27-TPC
partition even with no registry knobs** — which silently caps every CUDA context
at ~960 matmul/s of the ~1550 the card gives. If you ever reload the hooked
build (`~/open-gpu-kernel-modules`, for a compute-isolation probe), it is *not*
inert by default; pass `GhostTpcCount=54` to neutralise it, and restore the
clean modules before running the k8s stack again. The GHOST hooks and their
per-driver findings live in `NVIDIA-COMPUTE-ISOLATION.md` and
`ghost-experiment/HANDOFF.md`.

## Building runsc on a bare Ubuntu 24.04

`make build` wants docker; bazel directly does not. `bazelisk` installed as
`/usr/local/bin/bazel` picks up `.bazelversion` (8.3.1) by itself. Four
packages beyond `gcc`/`make`/`git` are needed, and three of them only announce
themselves as a genrule failing halfway through:

    clang                        //runsc/sandbox/bpf:af_xdp_ebpf
    gcc-aarch64-linux-gnu        //vdso:vdso  (built for both arches)
    libc6-dev-i386               //tools/xdp/cmd/bpf  (gnu/stubs-32.h)

**`gcc-multilib` and `gcc-aarch64-linux-gnu` conflict**: apt removes the cross
compiler to install multilib, without saying so, and the next build fails on
the aarch64 VDSO again. Install `libc6-dev-i386` alone — it carries the 32-bit
header the eBPF compile wants, and nothing needs multilib's compiler.

`//shim:containerd-shim-runsc-v1` must be built and installed separately;
k3s does not auto-detect runsc the way it detects `nvidia-container-runtime`,
so nothing appears in containerd's config without the template below.

## Four things the documented setup does not cover

**Driver 610.43.02 is unsupported, not unknown.** It is in nvproxy's table
(`v610_43_02 := addUnsupportedDriverABI(...)`) but not in
`runsc nvproxy list-supported-drivers`, which jumps 590.48.01 → 615.15.00.
Without `nvproxy-allow-unsupported-driver = "true"` in `/etc/containerd/runsc.toml`
every GPU sandbox refuses to start.

**Under CRI, something still has to inject the driver libraries.** HAMi's
device plugin sets `NVIDIA_VISIBLE_DEVICES` and (with `PASS_DEVICE_SPECS=true`)
the device nodes, but nothing mounts `libcuda.so` or `nvidia-smi`: the first
pod came up with no `/dev/nvidia*` and no libraries, and `nvidia-smi: command
not found` is what that looks like from inside. `nvproxy-docker = "true"` makes
runsc add `nvidia-container-runtime-hook` as a prestart hook, which is what
does the mounting. The flag is named for docker and is documented as legacy,
but it is what makes a CRI pod work.

**The HAMi chart's value keys are not what the guide says.** On 2.9.0 it is
`devicePlugin.passDeviceSpecsEnabled=true`, not
`devices.nvidia.passDeviceSpecsEnabled`; the wrong key is accepted silently and
`PASS_DEVICE_SPECS` stays `"false"`. Set `devicePlugin.runtimeClassName=nvidia`
through helm too — a `kubectl patch` of the daemonset is undone by the next
`helm upgrade`, and the `ld.so.preload` ConfigMap patch has to be re-applied
after every upgrade, followed by deleting the plugin pod (the key is mounted
with `subPath`, so it never updates in place).

**`hami-scheduler` cannot roll over on a single node.** Its deployment uses
RollingUpdate with pod anti-affinity, so the new replica sits `Pending` on
`didn't match pod anti-affinity rules` forever while the old one runs. Patch
the strategy to `Recreate`.

## The node's config, in full

`/etc/containerd/runsc.toml`:

    [runsc_config]
      nvproxy = "true"
      nvproxy-docker = "true"
      nvproxy-allow-unsupported-driver = "true"
      nvproxy-gpu-scheduler-socket = "/run/runsc-gpu-scheduler.sock"
      nvproxy-gpu-weight = "100"
      nvproxy-gpu-memory-limit = "42949672960"
      debug-log = "/var/log/runsc/%ID%/"

`/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl` extends k3s's
own template rather than replacing it:

    {{ template "base" . }}

    [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
      runtime_type = "io.containerd.runsc.v1"
      pod_annotations = ["dev.gvisor.*"]
      container_annotations = ["dev.gvisor.*"]

    [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc.options]
      TypeUrl = "io.containerd.runsc.v1.options"
      ConfigPath = "/etc/containerd/runsc.toml"

`runsc-gpu-scheduler.service` is the unit from the guide, with
`--measure-usage=false`. `RuntimeClass gvisor` → handler `runsc`; the node is
labelled `gpu=on` and `gpu.vendor=nvidia`.

**The weight ceiling is 100, so a pod may not ask for 300.** The reference
3:1 measurement used weights 300 and 100; with the runtime ceiling at 100 the
sandbox fails to start (`exceeds the weight of 100 configured on the runtime`),
which is the lower-only validator working as designed. Use 75 and 25 for the
same ratio, or raise the ceiling. Separately, `nvidia.com/gpucores` is a
placement budget summing to 100 per device: a pod asking 75 while another holds
50 stays `Pending` on `CardInsufficientCore`.

## vCluster

`vcluster create tenant-nv-a -n tenant-nv-a -f tenant-values.yaml --add=false`,
values in `~/vcluster-nv/tenant-values.yaml`: nodes synced from the host
filtered by `gpu.vendor: nvidia`, plus runtimeClasses and priorityClasses.

Two traps, both new since the sensai setup: **embedded etcd is a paid feature
in 0.36.1** (`embedded etcd is not enabled for this license`) — omit the
`controlPlane.backingStore` block and take the default — and **`--add=false` is
required** on a host with no platform login, or `vcluster create` aborts before
deploying anything.

Pods created inside a tenant are synced to the host namespace, admitted by
HAMi's webhook, placed by `hami-scheduler`, and run by runsc with the tenant's
annotations intact. Nothing about the tenancy layer had to know about the GPU.

## What this node measured

A `pytorch/pytorch:2.5.1-cuda12.4` pod inside `tenant-nv-a`, `runtimeClassName:
gvisor`, 4 GiB quota:

- **Sandboxing costs nothing measurable on the A100.** fp16 4096² matmul:
  **1547 matmul/s in the sandbox** against 1550 measured natively on the same
  GPU the day before — through gVisor, nvproxy, a vCluster and CRI.
- **The memory quota is what the pod sees.** `nvidia-smi` inside reports
  `0MiB / 2000MiB` for a 2000 MiB request; `torch.cuda.mem_get_info()` returns
  `total=4096 MiB` for a 4 GiB one. The device's real 81920 MiB is never
  visible.
- **The scheduler grants the right windows.** Two tenants at weights 75 and 25
  logged `GPU window is now 75ms of every 100ms at phase 25ms` and
  `25ms ... at phase 0s` — end to end from a pod annotation inside a vCluster
  to the Sentry's gate.
- **Whether those windows bind depends on the workload, and this reproduces
  the branch's Volta+ finding on datacenter silicon.** Two cuBLAS matmul
  tenants at 75/25 measured **706 and 706 matmul/s** — an even split, exactly
  half of solo, with the correct windows assigned throughout. A plain kernel
  launch loop under the same weights measured **81,099 and 22,270 launches/s,
  3.64:1** against a 3:1 request, so the gate does bind work that keeps
  entering the driver. This is the same split already documented on the
  RTX 5070 in `SECURITY-FINDINGS.md`, now on GA100: the gate stops submission
  it can see, and cuBLAS's doorbell/graph-replay path is not that.

Aggregate throughput of the gated pair was ~103k launches/s against ~130k solo
(79%); the cuBLAS pair kept 1412 of 1547 (91%).

## The admission webhook

`~/gpu-quota-webhook` — not gVisor's own `webhook/`, which is a different tool —
derives both annotations from the pod's resource request, so a tenant no longer
writes its own quota. It is deployed here as of 2026-08-18.

There is no docker on this host, and the image is `FROM scratch` with one
static binary in it, so the image was assembled by hand as a docker-archive
(a `layer.tar` holding the binary, a config naming its `diff_id`, a
`manifest.json`) and imported with `sudo k3s ctr -n k8s.io images import`. That
is worth knowing generally: **any single-binary image can be built on a host
with no container tooling at all**, in about twenty lines of Python.

The `MutatingWebhookConfiguration` is applied *last*, after both replicas are
Ready. It is `failurePolicy: Fail` by design — a GPU pod admitted while the
webhook is down would get the node-wide ceiling — so registering it before the
backend serves would wedge pod creation everywhere outside `kube-system`.

Verified here after deploying, each case run as a real pod on the A100:

| case | requested | pod annotated itself | result |
| --- | --- | --- | --- |
| suffixed quantity | `gpumem: "2k"` | — | 2000 MiB (**40960 MiB before**) |
| raise while `Pending` | `gpumem: 2000` | 1 GiB, then patched to 32 GiB | rewritten back; came up at 2000 MiB (**32768 MiB before**) |
| self-restriction | `gpumem: 2000` | 512 MiB | 512 MiB, honoured |
| through a vCluster tenant | `gpumem: "2Ki"` | 32 GiB | 2048 MiB |
| untranslatable request | `gpumem-percentage: 50` | — | refused at admission, with the reason |
| non-GPU pod | — | — | untouched, no annotations |
| `kube-system` pod | — | — | created normally, webhook not consulted |

The first two are holes that were measured open on this node before the fixes
that closed them: a quantity written as `2k` parsed as unreadable and derived
no limit at all, and the rules covered only `CREATE` while runsc reads the
annotations when the *sandbox* starts. Neither needed a race — HAMi's 100-core
budget will hold a pod `Pending` for as long as a tenant needs to edit it.
