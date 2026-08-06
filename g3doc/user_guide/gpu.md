# GPU Support

[TOC]

gVisor adds a layer of security to your AI/ML applications or other GPU
workloads while adding negligible overhead. By running these applications in a
sandboxed environment, you can isolate your host system from potential
vulnerabilities in AI code. This is crucial for handling sensitive data or
deploying untrusted AI workloads.

gVisor supports running most CUDA applications on preselected versions of
[NVIDIA's open source driver](https://github.com/NVIDIA/open-gpu-kernel-modules).
To achieve this, gVisor implements a proxy driver inside the sandbox, henceforth
referred to as `nvproxy`. `nvproxy` proxies the application's interactions with
NVIDIA's driver on the host. It provides access to NVIDIA GPU-specific devices
to the sandboxed application. The GPU application can run unmodified inside the
sandbox and interact transparently with these devices.

## Environments

The `runsc` flag `--nvproxy` must be specified to enable GPU support. gVisor
supports GPUs in the following environments.

### NVIDIA Container Runtime

The
[`nvidia-container-runtime`](https://github.com/NVIDIA/nvidia-container-toolkit/tree/main/cmd/nvidia-container-runtime)
is packaged as part of the
[NVIDIA GPU Container Stack](https://github.com/NVIDIA/nvidia-container-toolkit).
This runtime is just a shim and delegates all commands to the configured low
level runtime (which defaults to `runc`). To use gVisor, specify `runsc` as the
low level runtime in `/etc/nvidia-container-runtime/config.toml`
[via the `runtimes` option](https://github.com/NVIDIA/nvidia-container-toolkit/tree/main/cmd/nvidia-container-runtime#low-level-runtime-path)
and then run GPU containers with `nvidia-container-runtime`. The `runtimes`
option allows to specify an executable path or executable name that is
searchable in `$PATH`. To specify `runsc` with specific flags, the following
executable can be used:

```
# !/bin/bash

exec /path/to/runsc --nvproxy <other runsc flags> "$@"
```

NOTE: gVisor currently only supports
[legacy mode](https://github.com/NVIDIA/nvidia-container-toolkit/tree/main/cmd/nvidia-container-runtime#legacy-mode).
The alternative,
[csv mode](https://github.com/NVIDIA/nvidia-container-toolkit/tree/main/cmd/nvidia-container-runtime#csv-mode),
is not yet supported.

NOTE: The `nvidia-container-runtime` shim is a *legacy* GPU injection path. In
environments where the higher-level container runtime understands the
[Container Device Interface (CDI)](#cdi) (e.g. containerd ≥ 1.7 or CRI-O) GPUs
can be advertised via CDI specs and `runsc` can be invoked directly, without the
`nvidia-container-runtime` shim or its `prestart` hook. See the [CDI](#cdi)
section below. The `nvidia-container-toolkit` package is still required on the
host because CDI specs reference its `nvidia-ctk` hook binaries, but the runtime
*shim* is not.

### CDI

The
[Container Device Interface (CDI)](https://github.com/cncf-tags/container-device-interface)
is a vendor-neutral specification for how container runtimes inject devices into
containers. Instead of relying on a runtime shim, the host publishes CDI spec
files (typically under `/etc/cdi/` or `/var/run/cdi/`) that describe the device
nodes, bind mounts, environment variables, and hooks needed to expose a device.
CDI-aware container runtimes (containerd ≥ 1.7, CRI-O, recent Docker) merge
those specs into the OCI spec before invoking the low-level runtime.

`runsc` is fully CDI-compatible. In particular, it honors the `createContainer`
hooks emitted by NVIDIA's CDI specs, which is what allows the `nvidia-ctk hook`
invocations (symlink creation for client libraries, `update-ldcache`, etc.) to
run correctly inside the sandbox. Both
[NVIDIA's `k8s-device-plugin`](#nvidia-k8s-device-plugin) operating in
`DEVICE_LIST_STRATEGY=cdi-cri` mode and statically-generated CDI specs (via
`nvidia-ctk cdi generate`) are supported.

When using CDI, the `nvidia-container-runtime` shim is not required. `runsc` is
invoked directly as the low-level runtime, and the higher-level runtime applies
the CDI spec.

### Docker

The "legacy" mode of `nvidia-container-runtime` is directly compatible with the
`--gpus` flag implemented by the docker CLI. So with Docker, `runsc` can be used
directly (without having to go through `nvidia-container-runtime`).

```
$ docker run --runtime=runsc --gpus=all --rm -it nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda11.7.1-ubi8
[Vector addition of 50000 elements]
Copy input data from the host memory to the CUDA device
CUDA kernel launch with 196 blocks of 256 threads
Copy output data from the CUDA device to the host memory
Test PASSED
Done
```

### NVIDIA `k8s-device-plugin`

`nvproxy` is fully compatible with
[NVIDIA's `k8s-device-plugin`](https://github.com/NVIDIA/k8s-device-plugin),
including when it is configured to advertise GPUs via the
[Container Device Interface (CDI)](#cdi).

### GKE Device Plugin

[GKE](https://cloud.google.com/kubernetes-engine) uses a different GPU container
stack than NVIDIA's. GKE has
[its own device plugin](https://github.com/GoogleCloudPlatform/container-engine-accelerators/tree/master/cmd/nvidia_gpu)
(which is different from
[`k8s-device-plugin`](https://github.com/NVIDIA/k8s-device-plugin)). GKE's
plugin modifies the container spec in a different way than the above-mentioned
methods, and is also supported by `nvproxy`.

## Compatibility

gVisor supports a wide range of CUDA workloads, including PyTorch and various
generative models like LLMs. Check out
[this blog post about running Stable Diffusion with gVisor](/blog/2023/06/20/gpu-pytorch-stable-diffusion/).
gVisor also supports Vulkan and NVENC/NVDEC workloads. gVisor undergoes
continuous tests to ensure this functionality remains robust.
[Real-world usage](https://github.com/google/gvisor/issues?q=is%3Aissue+label%3A%22area%3A+gpu%22+)
of gVisor across different GPU workloads helps discover and address potential
compatibility or performance issues in `nvproxy`.

`nvproxy` is a passthrough driver that forwards `ioctl(2)` calls made to NVIDIA
devices by the containerized application directly to the host NVIDIA driver.
This forwarding is straightforward: `ioctl` parameters are copied from the
application's address space to the sentry's address space, and then a host
`ioctl` syscall is made. `ioctl`s are passed through with minimal intervention;
`nvproxy` does not emulate NVIDIA kernel-mode driver (KMD) logic. This design
translates to minimal overhead for GPU operations, ensuring that GPU bound
workloads experience negligible performance impact.

However, the presence of pointers and file descriptors within some `ioctl`
structs forces `nvproxy` to perform appropriate translations. This requires
`nvproxy` to be aware of the KMD's ABI, specifically the layout of `ioctl`
structs. The challenge is compounded by the lack of ABI stability guarantees in
NVIDIA's KMD, meaning `ioctl` definitions can change arbitrarily between
releases. While the NVIDIA installer ensures matching KMD and user-mode driver
(UMD) component versions, a single gVisor version might be used with multiple
NVIDIA drivers. As a result, `nvproxy` must understand the ABI for each
supported driver version, necessitating internal versioning logic for `ioctl`s.

As a result, `nvproxy` has the following limitations:

1.  Supports selected GPU models.
2.  Supports selected NVIDIA driver versions.
3.  Supports selected NVIDIA driver capabilities.
4.  Supports selected NVIDIA device files.
5.  Supports selected `ioctl`s on each device file.
6.  Supports selected platforms.

### Supported GPUs {#gpu-models}

gVisor currently supports NVIDIA GPUs:

*   **T4**, based on the
    [Turing microarchitecture](https://en.wikipedia.org/wiki/Turing_\(microarchitecture\))
*   **A100** and **A10G**, based on the
    [Ampere microarchitecture](https://en.wikipedia.org/wiki/Ampere_\(microarchitecture\))
*   **L4**, based on the
    [Ada Lovelace microarchitecture](https://en.wikipedia.org/wiki/Ada_Lovelace_\(microarchitecture\))
*   **H100**, based on the
    [Hopper microarchitecture](https://en.wikipedia.org/wiki/Hopper_\(microarchitecture\))

While not officially supported, other NVIDIA GPUs based on the same
microarchitectures as the above will likely work as well. This includes
consumer-oriented GPUs such as **RTX 3090** (Ampere) and **RTX 4090** (Ada
Lovelace).

Therefore, if you encounter an incompatible workload on a GPU on one of the
above microarchitectures, even if on an unsupported GPU, chances are that this
workload is also incompatible in the same manner on one of the officially
supported GPUs. Please
[open a GitHub issue](https://github.com/google/gvisor/issues/new?labels=type%3A+enhancement,area%3A+gpu&template=bug_report.yml)
with reproduction instructions so that it can be tested against an officially
supported GPU.

### Rolling Version Support Window {#driver-versions}

gVisor categorizes NVIDIA driver versions into three groups:

*   **Supported Drivers**: These are officially supported and qualified
    versions. We run continuous tests on them. They work by default.
*   **Unsupported Drivers**: These are "unsupported" (or unqualified) driver
    versions defined in the gVisor source code. They have not been fully tested.
    By default, `runsc` will fail to start on these versions, but you can allow
    them by passing the `--nvproxy-allow-unsupported-driver` flag. If allowed,
    `runsc` will log a warning and attempt to run, but this is *not* officially
    supported.
*   **Unknown Drivers**: These are drivers completely unknown to gVisor (not
    defined in the source code at all). They will always fail to start, even if
    `--nvproxy-allow-unsupported-driver` is specified.

Due to limited GPUs available to our testing infrastructure, it is currently not
feasible to test and qualify too many drivers continuously. Hence, we have a
policy that tries to keep the set of supported drivers limited.

The range of officially supported driver versions directly aligns with those
available within GKE. As GKE incorporates newer drivers, `nvproxy` will extend
support accordingly. Conversely, to manage versioning complexity, `nvproxy` will
shrink the window as drivers are removed from GKE. This strategy ensures a
streamlined process and avoids unbounded growth in `nvproxy`'s versioning.

To see what drivers a given `runsc` version officially supports, run:

```
$ runsc nvproxy list-supported-drivers
```

**NOTE**: `runsc`'s driver version is a strict version match because `runsc`
cannot assume ABI compatibility between driver versions. You may force `runsc`
to use a given supported ABI version with the `--nvproxy-driver-version` even
when running on a host that has an unknown driver version. However, doing so is
**not officially supported**, and running old drivers is generally not secure as
many driver updates address security bugs.

### Supported Driver Capabilities {#driver-capabilities}

The `NVIDIA_DRIVER_CAPABILITIES` environment variable defined in the container
spec controls which driver libraries/binaries will be mounted inside the
container. Different GPU workloads may have varying requirements. For instance,
Vulkan requires `graphics` capability, CUDA requires `compute`, while
NVENC/NVDEC requires `video`.

`nvproxy` supports the following driver capabilities: `compute`, `utility`,
`graphics` and `video`. By default, `nvproxy` only allows `compute` and
`utility`. If additional capabilities are required, then please set runsc flag
`--nvproxy-allowed-driver-capabilities` with a comma-separated list of
capabilities to allow. Allowing additional capabilities broadens the host driver
surface exposed to the sandbox, so provision this flag conservatively. Passing
"all" will allow all supported capabilities. If `NVIDIA_DRIVER_CAPABILITIES=all`
then all allowed capabilities will be used.

### Supported Device Files {#device-files}

gVisor only exposes `/dev/nvidiactl`, `/dev/nvidia-uvm` and `/dev/nvidia#`.

Some unsupported NVIDIA device files are:

-   `/dev/nvidia-caps/*`: Controls `nvidia-capabilities`, which is mainly used
    by Multi-instance GPUs (MIGs).
-   `/dev/nvidia-drm`: Plugs into Linux's Direct Rendering Manager (DRM)
    subsystem.
-   `/dev/nvidia-modeset`: Enables `DRIVER_MODESET` capability in `nvidia-drm`
    devices.

### Supported `ioctl` Set {#ioctls}

To minimize maintenance overhead across supported driver versions, the set of
supported NVIDIA device `ioctl`s is intentionally limited. This set was
generated by running a large number of GPU workloads in gVisor. As `nvproxy` is
adapted to more use cases, this set will continue to evolve.

Currently, `nvproxy` focuses on supporting compute, graphics and video workloads
(like CUDA, Vulkan and NVENC/NVDEC). If your GPU compute workload fails with
gVisor, it might be because some `ioctl` commands are still be unimplemented.
Please
[open a GitHub issue](https://github.com/google/gvisor/issues/new?labels=type%3A+bug,area%3A+gpu&template=bug_report.yml)
to describe about your use case. If a missing `ioctl` implementation is the
problem, then the [debug logs](/docs/user_guide/debugging/) will contain
warnings with prefix `nvproxy: handler is undefined *`. See below on how to run
the `ioctl_sniffer` tool.

### Supported Platforms {#platforms}

All nvproxy functionality is supported on systrap and ptrace platforms.
[cudaMallocManaged() is currently flaky on the KVM platform due to limitations
regarding virtual memory layout](https://github.com/google/gvisor/issues/11436);
all other nvproxy functionality is supported on the KVM platform.

### Debugging

There are a few methods to try when debugging GPU workloads. The first step to
try should be gVisor's
[ioctl_sniffer](https://github.com/google/gvisor/tree/master/tools/ioctl_sniffer)
tool; if your GPU workload fails due to unimplemented `ioctl` commands in
gVisor, this tool will provide a list of the specific ones.

Occasionally, you may also need to dig into the Nvidia GPU Driver itself. To do
so, you can install the OSS Driver repo and checkout the appropriate driver
version.

```bash
DRIVER_VERSION=550.54.15
git clone https://github.com/NVIDIA/open-gpu-kernel-modules.git
cd open-gpu-kernel-modules
git checkout tags/$DRIVER_VERSION
```

For `printk()` debugging, it is advised to use `portDbgPrintf()`. See more
discussion
[here](https://github.com/NVIDIA/open-gpu-kernel-modules/discussions/347). You
should be able to see the prints via `dmesg(1)`.

Then uninstall the existing Nvidia driver, build kernel module from local source
files and reinstall it.

```bash
sudo /usr/bin/nvidia-uninstall
make modules -j$(nproc)
sudo make modules_install -j$(nproc)
sudo insmod kernel-open/nvidia.ko
sudo insmod kernel-open/nvidia-uvm.ko
sudo insmod kernel-open/nvidia-drm.ko
sudo insmod kernel-open/nvidia-modeset.ko

# Install the user-space NVIDIA GPU driver components using the .run file.
sudo sh NVIDIA-Linux-x86_64-$DRIVER_VERSION.run --no-kernel-modules
```

### Host Configurations

<!---
TODO(b/324257702): Remove this section once this is fixed.
-->

When downloading large models within gVisor, you might encounter application
segmentation faults due to host VMA exhaustion. To workaround this, you can set
the value of `/proc/sys/vm/max_map_count` to a large number.

```bash
echo 1000000 | sudo tee /proc/sys/vm/max_map_count
```

Alternatively, you can also just pass the runsc flag `--host-settings=enforce`.

## GPU Memory Limits {#memory-limits}

`nvproxy` can limit how much GPU memory a sandbox is able to allocate. Because
the limit is applied in the Sentry, where the application's `ioctl`s are
interpreted, sandboxed code cannot observe or bypass it: there is no in-container
library to unset, preload around, or call past.

Set the limit in bytes with the runsc flag `--nvproxy-gpu-memory-limit`. The
default of `0` means no limit, and sandboxes without a limit behave exactly as
before.

A container may request a smaller limit for itself with an annotation:

```
"dev.gvisor.flag.nvproxy-gpu-memory-limit": "536870912"
```

The value configured on the runtime is a ceiling. An annotation may lower the
limit but not raise it, and a container asking for more than the ceiling fails
to start rather than being silently clamped. This matters because container
specs are frequently authored by the workload being limited; an annotation that
could raise the limit would be no limit at all.

When using Kubernetes, note that containerd drops pod annotations before they
reach the OCI spec unless the runtime declares them. Without the following in
the `runsc` runtime's containerd configuration, the annotation is ignored and no
error is reported:

```toml
pod_annotations = ["dev.gvisor.*"]
```

### Limiting the scheduler timeslice

The timeslice is how long the GPU scheduler runs a channel group before
switching to another, so a container that enlarges its own takes a greater
share of a GPU it shares with others. `--nvproxy-max-timeslice-us` caps the
value a sandbox may request; requests above it fail with
`NV_ERR_INSUFFICIENT_PERMISSIONS`. The default of `0` imposes no limit, leaving
behavior unchanged.

This is the only scheduling control nvproxy can police, for the reason given
under limitations below: everything else about scheduling happens without the
Sentry's involvement. It prevents a container enlarging its own share; it does
not let shares be assigned.

### Deriving the limit from a scheduler's resource

Schedulers that understand GPU memory, such as
[HAMi](https://project-hami.io)'s, have containers request it as an extended
resource (`nvidia.com/gpumem`, in mebibytes) and use it to decide which node and
GPU a pod is placed on. Placement alone does not stop a container from
allocating more than it asked for, so `webhook/pkg/gpushare` restates the
request as the annotation above, which has it enforced in the Sentry where the
container cannot reach it. The scheduler is unaffected and needs no changes,
since the resource it reads is left as it was. See
[GPU slicing under Kubernetes](#kubernetes) for how the rest of such a deployment
fits together.

The limit applies to the sandbox rather than to individual containers, so it is
the pod's peak demand: containers run together and their requests add up, while
init containers run before them and only the largest matters. A limit already
stated on the pod is left alone, since it may deliberately be lower than what
was requested.

Note that requesting an extended resource requires something on the node to
advertise it; the annotation is only derived, not invented, so a pod that
requests no GPU memory does not acquire a limit.

### What counts against the limit

The limit covers GPU device memory, and address space reserved on
`/dev/nvidia-uvm` for CUDA unified ("managed") memory. The two are counted
together because unified memory is committed into device memory; counting them
separately would let a sandbox obtain twice its limit.

Host memory pinned for GPU DMA is tracked but does not count against this limit.
It is host memory, and is bounded by the sandbox's memory limit instead.

Allocations are charged before being forwarded to the driver, so that concurrent
allocations cannot both be admitted against the same headroom. An allocation
that would exceed the limit is failed with `NV_ERR_NO_MEMORY`, which the CUDA
driver reports as `CUDA_ERROR_OUT_OF_MEMORY`, the same result an application
sees when the GPU is genuinely full.

`cuMemGetInfo()` reports the limit and its remaining headroom rather than the
whole device, so that applications which size their allocations as a fraction of
free memory, as PyTorch and TensorFlow do, size themselves to what they can
actually obtain. This also keeps a sandbox from observing how much memory the
device's other tenants are using.

### Limitations and future work

-   **Compute is not limited, and cannot be from here.** This bounds memory
    only. Once a channel has been set up, work is submitted by writing commands
    to a pushbuffer in mapped memory and ringing a doorbell through a mapped
    register. That never enters the kernel, so the Sentry does not see it: a
    sustained run of 13,000 kernel launches produced no `ioctl`s at all between
    context creation and teardown. There is consequently no point at which
    nvproxy could meter, delay or schedule GPU work, and no amount of `ioctl`
    handling would create one.

    This is a real difference from in-container approaches such as HAMi's
    `libvgpu.so`, which can limit compute precisely because it runs inside the
    container and intercepts CUDA calls before submission. That position is also
    what makes such limits removable by the workload being limited. The trade is
    structural rather than incidental: enforcement outside the container cannot
    see work submission, and enforcement that can see it is reachable by the
    container.

    A sandbox's share of GPU compute can be capped separately; see
    [Limiting GPU Compute](#compute-limits) below. Where compute must genuinely be
    partitioned rather than capped, MIG enforces it in hardware.

-   **Denying unified memory is reported poorly by CUDA.** An allocation
    refused by the limit fails the underlying `mmap` with `ENOMEM`, which is
    what Linux returns when a mapping exceeds a memory limit, but the CUDA
    driver reports it as `CUDA_ERROR_UNKNOWN` rather than
    `CUDA_ERROR_OUT_OF_MEMORY`. This is not a divergence from how a real
    exhaustion behaves: `cuMemAllocManaged()` performs no capacity check and
    does not fail for lack of memory, so there is no genuine out-of-memory
    result for this call to imitate. `CUDA_ERROR_OUT_OF_MEMORY` is not
    reachable from this path at all; of the errnos that reach the CUDA driver
    here, only `EINVAL` and `EPERM` map to anything more specific than
    "unknown", and neither describes a memory limit. Device memory allocations,
    which do have a real out-of-memory result, report
    `CUDA_ERROR_OUT_OF_MEMORY` as expected.

    Because the application-visible error is uninformative, the Sentry logs a
    warning the first time an allocation is denied, naming the limit and how
    much was in use. Only the first denial is reported, so that an application
    retrying in a loop does not flood the log. This appears wherever the
    sandbox's logs are directed: setting `--debug-log` alone is enough, since
    the message is logged at warning level and does not require `--debug`. With
    no log destination configured, runsc discards Sentry logs, and the denial is
    visible only as the application's error.

-   **Applications may not expect unified memory to fail.** Since
    `cuMemAllocManaged()` does not fail for capacity natively, workloads that
    rely on oversubscribing it -- reserving far more than the device holds and
    letting the driver migrate pages -- will fail under a limit that a
    device-memory workload of the same working set would not.

-   **Unified memory is charged at reservation, not commitment.** UVM commits
    device memory lazily, in response to GPU page faults serviced inside the
    host `nvidia-uvm` module, which reaches the resource manager through
    in-kernel calls rather than `ioctl`s. The Sentry cannot observe the
    commitment, so the address space reserved for it is charged instead. This is
    sound as a bound, since commitment cannot exceed the reservation, but it is
    not tight: a workload that deliberately oversubscribes, reserving far more
    than it backs, is charged for what it reserved.

-   **Fabric and EGM memory are not accounted.** `NV_MEMORY_FABRIC`,
    `NV_MEMORY_MULTICAST_FABRIC`, `NV_MEMORY_FABRIC_IMPORTED_REF`,
    `NV_MEMORY_EXPORT` and `NV_MEMORY_EXTENDED_USER` (EGM) are recorded as
    reviewed but deliberately uncharged in `memClassKinds`, because whether a
    given allocation is backed by local device memory or by memory imported from
    another node determines whether the sandbox should be charged for it at all,
    and charging the wrong ones would silently over- or under-count.

    Resolving this requires hardware that the accounting has not yet been
    validated against: an NVLink/NVSwitch fabric spanning more than one node for
    the fabric classes, and a platform with extended GPU memory (such as
    Grace-Hopper) for EGM. Until then, a sandbox on such a system can allocate
    fabric or EGM memory without it counting against its limit. Deployments that
    rely on this limit for isolation should not enable the
    `fabric-imex-mgmt` driver capability.

-   **A small fixed overhead is not charged.** Driver structures allocated
    alongside a CUDA context are not attributed to the sandbox. Measured against
    the GPU's own per-process accounting, this is a constant, independent of how
    much the workload allocates, so a sandbox can hold slightly more than its
    limit by a bounded amount rather than by a share of what it requests.

## Limiting GPU Compute {#compute-limits}

`--nvproxy-gpu-compute-percent` bounds the share of wall-clock time during
which a sandbox may submit work to the GPU, and a container may lower its own
share, but not raise it, with:

```
"dev.gvisor.flag.nvproxy-gpu-compute-percent": "25"
```

The default of `0` imposes no limit.

Submission cannot be intercepted directly. Once a channel exists, an application
writes commands to a ring buffer in memory it has mapped and rings a doorbell
through a mapped register, neither of which enters the kernel. What the Sentry
can do instead is revoke its own mapping of the ring buffer at the end of the
sandbox's share of each period, so that the next write to it faults, and hold
that fault until the share comes round again. The technique is taken from
[Krypton](https://www.usenix.org/conference/atc25/presentation/zhang-shulai),
which does the same from a kernel module.

Measured against an unlimited baseline of 654 kernel launches per second:

    configured   achieved   share of unlimited
        75%       498/s          76.2%
        50%       333/s          51.0%
        25%       169/s          25.8%

The small consistent overshoot is work already in flight when the mapping is
revoked, which runs to completion.

### What a limited sandbox may notice

While a sandbox is held, the task waiting to submit holds its address space's
lock, so other threads in the same sandbox stall on operations that need it:
mapping and unmapping memory, and faulting in pages that are not yet resident.
Threads computing over memory they have already touched are unaffected. Measured
with a limit of 25%, a thread doing continuous `mmap`/fault/`munmap` fell to
about 20% of its unlimited rate, while a thread doing arithmetic over a resident
buffer was unaffected.

This is a difference from Krypton, which delivers the fault to a handler inside
the container and so stalls only the submitting thread. gVisor handles the fault
in the Sentry, which is what removes the need for anything inside the container,
at the cost of holding that lock. Workloads that allocate heavily on other
threads while the GPU is throttled will feel it.

### Checkpointing is not yet supported

Checkpointing a sandbox that is using a GPU fails. The objects the driver
tracks on the application's behalf are recreated on restore by replaying their
creation, and only some of them can be: the OS events that every CUDA context
creates, memory allocated through the older allocation ioctls, duplicated
handles and pinned host descriptors have no such support, so the save is
refused rather than producing an image that could not be restored.

This is not specific to the limits described above; it applies to any sandbox
with a GPU.

### It is a cap, not a share

A percentage caps a sandbox against the clock, and nothing more. Time that one
sandbox does not use is not redistributed to another, because a Sentry sees only
its own sandbox; and because every sandbox measures its period from its own
start, two sandboxes each capped at 50% will as likely as not choose overlapping
halves and contend anyway. The cap is useful for holding a tenant to an agreed
fraction of a GPU, not for dividing a GPU between tenants.

The percentage is also only meaningful against a particular device. It is a
share of wall-clock time during which submission is permitted, which is not a
share of SMs, of memory bandwidth, or of anything else the hardware counts; two
GPUs given the same figure will do very different amounts of work. To divide a
GPU rather than cap its users, use the scheduler below.

## Dividing a GPU between sandboxes {#gpu-scheduler}

`runsc gpu-scheduler` is a coordinator that divides one GPU between the
sandboxes sharing it. It runs on the host, outside every sandbox, which is what
lets it do what a Sentry alone cannot: give a sandbox the time its neighbours
are not using, and place each sandbox's window so that the windows do not
overlap.

```
runsc gpu-scheduler --socket=/run/runsc-gpu-scheduler.sock
```

Sandboxes are pointed at it on the runtime, and each states its share:

```
--nvproxy-gpu-scheduler-socket=/run/runsc-gpu-scheduler.sock
--nvproxy-gpu-weight=100
```

A container may lower its own weight, but not raise it:

```
"dev.gvisor.flag.nvproxy-gpu-weight": "30"
```

The socket is connected by `runsc` before the sandbox starts and handed to the
Sentry as an already-open file descriptor, so the sandbox never needs the
ability to reach the host's filesystem or to open a connection of its own.

### Weights rather than percentages

A weight is relative, so it needs no reference to the hardware: a sandbox gets
its weight's share of what the *contending* sandboxes are actually asking for.
Two sandboxes weighted 300 and 100 divide a contended GPU 3:1 whatever the
device is, and either one gets all of it when the other is idle. Where the
weights happen to sum to 100 the reading coincides with a percentage, which is
what makes an existing `nvidia.com/gpucores` request usable as one directly.

Idle sandboxes yield their share and reclaim it on their next submission, so the
division is work-conserving. Measured with two sandboxes weighted 300 and 100:

    weight   achieved   share
      300     486.4/s   75.0%
      100     161.9/s   25.0%
      total   648.3/s   99.1% of the 654/s one sandbox reaches alone

The 3.00:1 ratio is the assigned one, and less than 1% of the GPU is lost to the
division.

### Charging for what was taken, not what was asked for

A sandbox cannot measure its own use of the GPU: work is submitted by writing to
mapped memory, and how long the device spends on it is not visible from inside.
Nor can the work be recalled once submitted, so a sandbox that launches one very
long kernel keeps the GPU past the end of its window, and from the inside looks
exactly like one that launched a short one.

With `--measure-usage` (the default), the scheduler reads per-process GPU
utilization from `nvidia-smi pmon` and charges each sandbox for what it actually
consumed, taking the excess out of its next windows. Against a workload
submitting kernels roughly 100 times longer than its equally-weighted neighbour:

    without measurement   67.6% / 32.4%   2.08:1
    with measurement      57.8% / 42.2%   1.37:1

The remaining imbalance is inherent: a kernel that outlasts the period cannot be
preempted by any software layer, `pmon` samples once a second against 100ms
windows, and repayment is deliberately capped at one period so that a single
long kernel cannot starve its author afterwards. A host without `nvidia-smi`
still divides the GPU, by the weaker signal of what was submitted.

## GPU slicing under Kubernetes {#kubernetes}

One GPU can be divided between several gVisor pods, each holding an agreed share
of its memory and its time, with nothing inside any of the pods responsible for
keeping them to it. This section sets that up end to end.

The work splits in two. Deciding *which* pods may share a GPU, and refusing to
put more on one than it can hold, is placement, and a GPU-aware scheduler such
as [HAMi](https://project-hami.io)'s already does it well; it runs on the
control plane, well away from the workload, and nothing about it needs to be
trusted by the sandbox. Holding each pod to what it was placed for is
enforcement, and that is nvproxy's job.

HAMi comes as four parts, of which three are used unchanged:

    hami-webhook          used     redirects GPU pods to HAMi's scheduler
    hami-scheduler        used     placement and per-device accounting
    hami-device-plugin    used     advertises the GPU to the kubelet
    libvgpu.so            dropped  enforcement, but inside the container

The last is dropped because a limiter living in the address space of the process
it limits is reachable by that process. On a pod requesting 512 MiB and
attempting 768: with the library enforcing, the workload was refused at 512 MiB;
with the single documented variable `CUDA_DISABLE_CONTROL=true` set, it saw the
whole 11.7 GiB device and took all 768 MiB. With the runsc limit in place
instead, the same attempt was refused at 320 MiB. See
[Security](#security) for why the position of the enforcement is the whole
point.

gVisor needs no device plugin and no scheduler of its own. Nothing a pod asks
for is rewritten, so HAMi places pods exactly as it did before.

### 1. Install HAMi

```
helm repo add hami-charts https://project-hami.github.io/HAMi/
helm install hami hami-charts/hami -n kube-system \
  --set devices.nvidia.passDeviceSpecsEnabled=true
kubectl label node <node> gpu=on
```

`passDeviceSpecsEnabled=true` is required for gVisor: it puts the `/dev/nvidia*`
nodes into `spec.Linux.Devices`, which is where nvproxy looks for them. Without
it the sandbox comes up with no GPU.

The device plugin itself calls NVML, so it must not run under gVisor:

```
kubectl -n kube-system patch daemonset hami-device-plugin --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/runtimeClassName","value":"nvidia"}]'
```

### 2. Drop the preload

```
kubectl -n kube-system patch cm hami-device-plugin --type merge \
  -p '{"data":{"ld.so.preload":""}}'
kubectl -n kube-system delete pod -l app.kubernetes.io/component=hami-device-plugin
```

The restart is required, and not optional in the way it usually is: that key is
mounted into the plugin with a `subPath`, and ConfigMap volumes mounted that way
never receive updates. Patching alone leaves the old value in place
indefinitely.

Once the plugin comes back, containers get an empty `/etc/ld.so.preload` and
`libvgpu.so` is never loaded. It is still bind-mounted into the container, along
with `/tmp/vgpulock`, and both are inert without the preload.

This is node-wide. On a node that also runs `runc` pods relying on HAMi to limit
them, skip this step and let the admission webhook in step 6 stand the library
down per-pod instead, which leaves it mapped but inert.

### 3. Run the coordinator

`runsc gpu-scheduler` divides the GPU between the sandboxes sharing it. It runs
on the host, outside every sandbox, which is what lets it give a sandbox the
time its neighbours are not using and place each sandbox's window so that the
windows do not overlap. One instance serves the node:

```
# /etc/systemd/system/runsc-gpu-scheduler.service
[Unit]
Description=runsc GPU scheduler
Before=k3s.service

[Service]
ExecStart=/usr/local/bin/runsc --debug --debug-log=/var/log/runsc-gpu-scheduler.log gpu-scheduler --socket=/run/runsc-gpu-scheduler.sock
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
```

It must be running before the first GPU sandbox starts, hence the ordering:
`runsc` connects to the socket while creating the sandbox, and a sandbox that
cannot reach it **fails to start** rather than running unscheduled. This is
deliberate -- a workload silently escaping its share is the failure worth
avoiding -- but it does mean the coordinator is on the critical path for every
GPU pod on the node.

The `--debug-log` is worth setting. Without it the scheduler writes nothing
anywhere, including to the journal, and there is no way to see that it started
or what it is doing.

### 4. Point the runtime at it

```
# /etc/containerd/runsc.toml
[runsc_config]
  nvproxy = "true"
  nvproxy-gpu-scheduler-socket = "/run/runsc-gpu-scheduler.sock"
  nvproxy-gpu-weight = "100"
  nvproxy-gpu-memory-limit = "8589934592"
  debug-log = "/var/log/runsc/%ID%/"
```

Both limits are ceilings: a pod may ask for less, never more. Setting the weight
to 100 lets a pod's `nvidia.com/gpucores` request be taken literally as a
percentage, and the memory ceiling should be at least the largest a single pod
may request -- here 8 GiB. A pod asking for more than either fails to start
rather than being quietly clamped, so a ceiling set too low is a confusing way
to discover this.

`debug-log` is what makes the check in step 8 possible; it is not otherwise
required.

`runsc` connects to the socket itself, before the sandbox starts, and hands the
Sentry an already-open file descriptor. The sandbox never needs the ability to
reach the host filesystem or to open a connection of its own.

### 5. Let the annotations through

```
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
  pod_annotations = ["dev.gvisor.*"]
  container_annotations = ["dev.gvisor.*"]

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc.options]
  TypeUrl = "io.containerd.runsc.v1.options"
  ConfigPath = "/etc/containerd/runsc.toml"
```

Without the two `*_annotations` lines, CRI drops the annotations before they
reach the OCI spec and every pod silently gets the runtime's defaults. This
fails quietly, so it is worth checking (step 8) rather than assuming.

### 6. Deploy the admission webhook

The webhook in `webhook/` reads what each pod was scheduled against and restates
it where the container cannot reach it:

    nvidia.com/gpumem: 512     ->  dev.gvisor.flag.nvproxy-gpu-memory-limit
    nvidia.com/gpucores: 30    ->  dev.gvisor.flag.nvproxy-gpu-weight

It also sets `CUDA_DISABLE_CONTROL=true`, which is what stands `libvgpu.so` down
on a node where step 2 was skipped. Losing that variable is safe in both
directions: a container that sets it back only re-enables a second limiter on
top of the Sentry's, which can restrict it further but never grant it more.

The step is optional. Its whole job is deriving the two annotations, so a pod
that writes them itself needs no webhook at all -- which is the simplest way to
try the rest of this out.

### 7. Request a slice

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: trainer
spec:
  runtimeClassName: gvisor
  containers:
    - name: trainer
      image: myrepo/trainer:latest
      resources:
        limits:
          nvidia.com/gpu: 1
          nvidia.com/gpumem: 2000    # MiB, enforced by the Sentry
          nvidia.com/gpucores: 30    # share of the GPU, relative
```

`nvidia.com/gpu: 1` asks for one of the plugin's slots rather than a whole
device; HAMi advertises `deviceSplitCount` (10 by default) of them per GPU, and
that is what allows several pods onto one device.

Without the webhook, state the two annotations directly:

```yaml
metadata:
  annotations:
    dev.gvisor.flag.nvproxy-gpu-memory-limit: "2097152000"
    dev.gvisor.flag.nvproxy-gpu-weight: "30"
```

Both may only lower what the runtime allows: 2000 MiB and a weight of 30 are
under the 8 GiB and 100 set in step 4. A pod asking for more than either fails
to start rather than being quietly clamped.

### 8. Check that it took

Check first that the annotations survived CRI, since step 5 is the one that
fails silently -- a missing `pod_annotations` line leaves every pod on the
runtime's defaults with nothing to indicate it:

```
crictl inspectp <pod-id> | grep dev.gvisor.flag
```

Then that the sandbox is being scheduled, which the Sentry records once per
change of window. This is the direct evidence, since it is the sandbox stating
the share it was actually given:

```
grep "GPU window is now" /var/log/runsc/<sandbox-id>/*
```

```
nvproxy: GPU window is now 40ms of every 100ms at phase 0s
nvproxy: gating GPU submission through command buffer 0x5c00004a
```

A window covering the whole period on a contended GPU means the sandbox is not
being scheduled; one that changes as other pods come and go means it is. The
`phase` is what keeps two sandboxes from being given the same 40ms.

The memory limit shows up in the pod's own view of the device: `nvidia-smi`
inside the pod reports the figure it asked for rather than the hardware's.

### What this does and does not divide

Memory is a hard limit: an allocation past it is refused. Time is a share rather
than a limit, and is work-conserving -- a sandbox gets its weight's share of
what the *contending* sandboxes are asking for, and all of the GPU when they are
idle. Two pods weighted 300 and 100 divide a contended GPU 3.00:1, with 99.1% of
single-sandbox throughput retained.

What is not divided is everything the hardware does not let software partition
from outside: SM occupancy within a window, memory bandwidth, cache. A kernel
that outlasts its window cannot be recalled, which is what bounds the accuracy
of the division; see
[Dividing a GPU between sandboxes](#gpu-scheduler) above. Where compute must be
partitioned rather than shared, MIG enforces it in hardware, and HAMi can place
pods onto MIG instances.

### One GPU per node, for now

The coordinator has no concept of a device: every sandbox that connects to it is
treated as contending with every other. On a node with one GPU that is correct.
On a node with several it is not -- HAMi will place pods across the devices, and
two pods on *different* GPUs would still be given disjoint windows and take a
fraction of the time each, though they never contend.

There is no per-pod way around this today, since the socket is named in the
runtime's configuration rather than in the pod's, so it cannot be varied per
GPU. Until the coordinator learns which device each sandbox was placed on, use
this on single-GPU nodes.

Checkpointing does not work either; see
[Checkpointing is not yet supported](#compute-limits) above. That applies to any
sandbox with a GPU, not just a scheduled one.

## Security

While GPU support enables important use cases for gVisor, it is important for
users to understand the security model around the use of GPUs in sandboxes. In
short, while gVisor will protect the host from the sandboxed application,
**NVIDIA driver updates must be part of any security plan with or without
gVisor**.

First, a short discussion on
[gvisor's security model](../architecture_guide/security.md). gVisor protects
the host from sandboxed applications by providing several layers of defense. The
layers most relevant to this discussion are the redirection of application
syscalls to the gVisor sandbox and use of
[seccomp-bpf](https://www.kernel.org/doc/html/v4.19/userspace-api/seccomp_filter.html)
on gVisor sandboxes.

gVisor uses a "platform" to tell the host kernel to reroute system calls to the
sandbox process, known as the sentry. The sentry implements a syscall table,
which services all application syscalls. The Sentry *may* make syscalls to the
host kernel if it needs them to fulfill the application syscall, but it doesn't
merely pass an application syscall to the host kernel.

On sandbox boot, seccomp filters are applied to the sandbox. Seccomp filters
applied to the sandbox constrain the set of syscalls that it can make to the
host kernel, blocking access to most host kernel vulnerabilities even if the
sandbox becomes compromised.

For example, [CVE-2022-0185](https://nvd.nist.gov/vuln/detail/CVE-2022-0185) is
mitigated because gVisor itself handles the syscalls required to use namespaces
and capabilities, so the application is using gVisor's implementation, not the
host kernel's. For a compromised sandbox, the syscalls required to exploit the
vulnerability are blocked by seccomp filters.

In addition, seccomp-bpf filters can filter by argument names allowing us to
allowlist granularly by `ioctl(2)` arguments. `ioctl(2)` is a source of many
bugs in any kernel due to the complexity of its implementation. As of writing,
gVisor does
[allowlist some `ioctl`s](https://github.com/google/gvisor/blob/ccc3c2cbd26d3514885bd665b0a110150a6e8c53/runsc/boot/filter/config/config_main.go#L111)
by argument for things like terminal support.

For example, [CVE-2024-21626](https://nvd.nist.gov/vuln/detail/CVE-2024-21626)
is mitigated by gVisor because the application would use gVisor's implementation
of `ioctl(2)`. For a compromised sentry, `ioctl(2)` calls with the needed
arguments are not in the seccomp filter allowlist, blocking the attacker from
making the call. gVisor also mitigates similar vulnerabilities that come with
device drivers
([CVE-2023-33107](https://nvd.nist.gov/vuln/detail/CVE-2023-33107)).

### nvproxy Security

Recall that "nvproxy" allows applications to directly interact with supported
ioctls defined in the NVIDIA driver.

gVisor's seccomp filter rules are modified such that `ioctl(2)` calls can be
made
[*only for supported ioctls*](https://github.com/google/gvisor/blob/be9169a6ce095a08b99940a97db3f58e5c5bd2ce/pkg/sentry/devices/nvproxy/seccomp_filters.go#L1).
The allowlisted rules aligned with each
[driver version](https://github.com/google/gvisor/blob/c087777e37a186e38206209c41178e92ef1bbe81/pkg/sentry/devices/nvproxy/version.go#L152).
This approach is similar to the allowlisted ioctls for terminal support
described above. This allows gVisor to retain the vast majority of its
protection for the host while allowing access to GPUs. All of the above CVEs
remain mitigated even when "nvproxy" is used.

However, gVisor is much less effective at mitigating vulnerabilities within the
NVIDIA GPU drivers themselves, *because* gVisor passes through calls to be
handled by the kernel module. If there is a vulnerability in a given driver for
a given GPU `ioctl` (read feature) that gVisor passes through, then gVisor will
also be vulnerable. If the vulnerability is in an unimplemented feature, gVisor
will block the required calls with seccomp filters.

In addition, gVisor doesn't introduce any additional hardware-level isolation
beyond that which is configured by by the NVIDIA kernel-mode driver. There is no
validation of things like DMA buffers. The only checks are done in seccomp-bpf
rules to ensure `ioctl(2)` calls are made on supported and allowlisted `ioctl`s.

Therefore, **it is imperative that users update NVIDIA drivers in a timely
manner with or without gVisor**. To see the latest drivers gVisor supports, you
can run the following with your runsc release:

```
$ runsc nvproxy list-supported-drivers
```

Alternatively you can view the
[source code](https://github.com/google/gvisor/blob/be9169a6ce095a08b99940a97db3f58e5c5bd2ce/pkg/sentry/devices/nvproxy/version.go#L1)
or download it and run:

```
$ make run TARGETS=runsc:runsc ARGS="nvproxy list-supported-drivers"
```

### So, if you don't protect against all the things, why even?

While gVisor doesn't protect against *all* NVIDIA driver vulnerabilities, it
*does* protect against a large set of general vulnerabilities in Linux.
Applications don't just use GPUs, they use them as a part of a larger
application that may include third party libraries. For example, Tensorflow
[suffers from the same kind of vulnerabilities](https://nvd.nist.gov/vuln/detail/CVE-2022-29216)
that every application does. Designing and implementing an application with
security in mind is hard and in the emerging AI space, security is often
overlooked in favor of getting to market fast. There are also many services that
allow users to run external users' code on the vendor's infrastructure. gVisor
is well suited as part of a larger security plan for these and other use cases.
