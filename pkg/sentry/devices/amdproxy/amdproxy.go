// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package amdproxy implements proxying for the AMD GPU Linux kernel drivers:
// the Kernel Fusion Driver (KFD, /dev/kfd) and amdgpu's DRM render nodes
// (/dev/dri/renderD*).
//
// Unlike nvproxy, whose ioctl ABI is defined by an out-of-tree driver that
// changes shape between releases, KFD's ABI is upstream and versioned: it
// promises compatibility within a major version, and the kernel zero-pads
// parameter structs that are smaller than it expects. amdproxy therefore
// dispatches on the exact ioctl command number, which encodes the parameter
// struct's size, rather than maintaining nvproxy's per-driver-version ABI
// tree. A command whose size does not match this package's definition does
// not match any case and is denied, so an ABI change fails loudly instead of
// being silently misinterpreted.
package amdproxy

import (
	"fmt"
	"path"
	"path/filepath"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/devutil"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/marshal"
	"gvisor.dev/gvisor/pkg/sentry/devices/amdproxy/amdconf"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
)

// kfdMinor is /dev/kfd's device minor number. KFD registers a dynamically
// allocated char device major, so only the minor number is fixed.
const kfdMinor = 0

// Options holds arguments to Register.
type Options struct {
	// If UseDevGofer is true, open device files via gofer.
	UseDevGofer bool

	// GPUMemoryLimit is the maximum number of bytes of device memory the
	// sandbox may allocate at once. Zero means no limit.
	GPUMemoryLimit uint64

	// CUMask is the set of GPU compute units the sandbox may run on. A nil
	// mask means every compute unit.
	//
	// Which units a sandbox is given has to be decided outside it, by
	// whatever assigns GPUs to sandboxes: masks are only a partition if they
	// do not overlap, and a Sentry sees nothing but its own sandbox.
	CUMask amdconf.CUMask

	// CUsPerComputeGroup is the number of compute units the GPU schedules as
	// an indivisible group: 2 on RDNA, where units are paired into workgroup
	// processors, and 1 where they are independent. Zero means unknown, and
	// disables the check that CUMask selects only whole groups.
	CUsPerComputeGroup int

	// ShareKFDVM allows the processes of this sandbox to share one GPU address
	// space, so that more than one of them can use the GPU. The driver binds a
	// single address space per process and the Sentry is one process, so
	// without this only the first process to initialise the GPU succeeds.
	//
	// It is a correctness trade, confined to this sandbox; sharedvm.go states
	// the terms.
	ShareKFDVM bool

	// SchedulerFD is an open connection to the GPU scheduler that divides the
	// device between the sandboxes on this host, or -1 if this sandbox is not
	// being time-sliced. The Sentry may not connect to anything itself, so the
	// descriptor is opened by runsc and donated.
	SchedulerFD int

	// SchedulerWeight is this sandbox's share of the GPU relative to the
	// others the scheduler is dividing it between. It is a weight rather than
	// a percentage because a percentage means nothing without naming a
	// particular device, and says nothing useful when the shares do not sum
	// to 100.
	SchedulerWeight uint64

	// ContainerID identifies this sandbox to the scheduler.
	ContainerID string
}

// DeviceInfo contains information on registered amdproxy devices. Device
// major numbers are allocated dynamically by the Sentry and need not match
// the host's.
//
// +stateify savable
type DeviceInfo struct {
	// KFDDevMajor is /dev/kfd's device major number.
	KFDDevMajor uint32
}

// Register registers all devices implemented by this package, and specified
// by opts, in vfsObj. If it succeeds, it returns information about registered
// devices; the returned DeviceInfo must not be mutated.
func Register(vfsObj *vfs.VirtualFilesystem, opts *Options) (*DeviceInfo, error) {
	amdp := &amdproxy{
		useDevGofer: opts.UseDevGofer,
		cuMask:      opts.CUMask,
		kfdFDs:      make(map[*kfdFD]struct{}),
		renderFDs:   make(map[*renderFD]struct{}),
	}
	amdp.memAcct.init(opts.GPUMemoryLimit)
	amdp.vmShare.init(opts.ShareKFDVM)
	amdp.vaGuard.init(opts.ShareKFDVM)
	amdp.renderShare.init(opts.ShareKFDVM)
	amdp.runtimeShare.init(opts.ShareKFDVM)
	amdp.eventShare.init(opts.ShareKFDVM)
	amdp.timeSlicer.init(opts.SchedulerWeight, opts.SchedulerFD, opts.ContainerID)
	if opts.GPUMemoryLimit != 0 {
		log.Infof("amdproxy: GPU memory limited to %d bytes", opts.GPUMemoryLimit)
	}
	if opts.ShareKFDVM {
		log.Infof("amdproxy: the processes of this sandbox will share one GPU address space; " +
			"allocations that would overlap between them are refused rather than aliased")
	}
	if len(opts.CUMask) > 0 {
		if opts.CUMask.Empty() {
			return nil, fmt.Errorf("amdproxy: CU mask selects no compute units")
		}
		// Refuse a mask the driver will not accept, here, rather than at the
		// first queue the container creates.
		//
		// KFD rejects a mask that enables half of a workgroup processor with
		// EINVAL. That failure surfaces at CREATE_QUEUE, where this package
		// applies the mask; it destroys the queue as it should, but ROCr does
		// not handle a failed queue creation and dies on a null dereference.
		// What the operator sees is a container that starts, prints its device
		// name, and then hangs, with the real reason one warning line deep in
		// the Sentry log. Failing here instead names the problem while there
		// is still someone to read it.
		if n := opts.CUsPerComputeGroup; n > 1 {
			if split := opts.CUMask.FirstSplitGroup(n); split >= 0 {
				aligned := opts.CUMask.AlignedDownTo(n)
				suggestion := fmt.Sprintf("%v (%d units)", aligned, aligned.Count())
				if aligned.Empty() {
					suggestion = "a mask selecting at least one whole group"
				}
				return nil, fmt.Errorf("amdproxy: CU mask %v selects compute unit %d but not the rest of its group of %d; "+
					"this GPU schedules compute units in groups of %d, and the driver rejects a mask that splits one, "+
					"so a container given this mask would fail at its first kernel launch. Select whole groups: %s",
					opts.CUMask, split, n, n, suggestion)
			}
		}
		log.Infof("amdproxy: GPU compute units limited to %v (%d units)", opts.CUMask, opts.CUMask.Count())
	}

	if err := checkSliceOrMask(opts); err != nil {
		return nil, err
	}

	kfdDevMajor, err := vfsObj.GetDynamicCharDevMajor()
	if err != nil {
		return nil, fmt.Errorf("allocating device major number for kfd: %w", err)
	}
	amdp.devInfo.KFDDevMajor = kfdDevMajor
	if err := vfsObj.RegisterDevice(vfs.CharDevice, kfdDevMajor, kfdMinor, &kfdDevice{
		amdp: amdp,
	}, &vfs.RegisterDeviceOptions{
		GroupName: "kfd",
	}); err != nil {
		return nil, err
	}

	// DRM's device major number is statically assigned, so unlike KFD the
	// Sentry's numbering matches the host's and render nodes keep their minor
	// numbers. Registering a device only determines which implementation
	// serves it if it is opened; whether the node exists in the sandbox at all
	// is decided by the container's device list.
	for minor := uint32(amdgpu.DRM_RENDER_MINOR_BASE); minor <= amdgpu.DRM_RENDER_MINOR_MAX; minor++ {
		if err := vfsObj.RegisterDevice(vfs.CharDevice, amdgpu.DRM_MAJOR, minor, &renderDevice{
			amdp:  amdp,
			minor: minor,
		}, &vfs.RegisterDeviceOptions{
			GroupName: "dri",
			Pathname:  path.Join("dri", fmt.Sprintf("renderD%d", minor)),
			FilePerms: 0666,
		}); err != nil {
			return nil, err
		}
	}

	return &amdp.devInfo, nil
}

// checkSliceOrMask reports whether opts asks for both a spatial and a temporal
// partition of the GPU, which cannot both be applied.
//
// A sandbox may be given a slice of the device in space or in time, and on
// this hardware not both.
//
// The driver forbids it. A time slice is enforced through a KFD debug
// session, and kfd_dbg_set_queue_workaround() in the amdkfd driver refuses
// to put that session's CWSR workaround on a queue carrying a user CU
// mask:
//
//	if (enable && q->properties.is_user_cu_masked)
//		return -EBUSY;
//
// guarded by kfd_dbg_has_cwsr_workaround(), which covers GC versions
// 11.0.0 to 11.0.3 -- every RDNA3 part, including the Navi 32 this was
// found on. Confirmed against the source of the running kernel, not
// inferred.
//
// Configuring both is refused here rather than left to fail later, because
// later is unrecoverable and unreadable: CREATE_QUEUE succeeds, the CU mask
// this package applies straight afterwards returns EBUSY, the queue is
// destroyed as designed, and ROCr -- which does not handle a failed queue
// creation -- dereferences null. What the operator sees is a workload that
// dies during GPU initialisation with a SIGSEGV and nothing in dmesg. It is
// the same ROCr fragility that makes a mask splitting a workgroup processor
// worth refusing above.
//
// Neither is silently preferred over the other. Which one a sandbox should
// get is a real choice -- a mask is a hard partition that idles when its
// tenant does, a weight is work-conserving and exactly proportional -- and
// it belongs to whoever is placing the sandbox, not here.
//
// This refuses on every ASIC, though the driver's rule is specific to
// gfx11. CDNA and gfx12 fall outside that version range and should permit
// both; nothing here can test that, and the safe direction for an untested
// combination that is known to crash the runtime elsewhere is to refuse.
func checkSliceOrMask(opts *Options) error {
	if len(opts.CUMask) > 0 && opts.SchedulerFD >= 0 {
		return fmt.Errorf("amdproxy: a sandbox may have a compute unit mask or a GPU time slice, not both: " +
			"--amdproxy-cu-mask partitions the device in space and --amdproxy-gpu-weight divides it in time, and the " +
			"amdgpu driver refuses a debug session's CWSR workaround on a CU-masked queue (EBUSY) on gfx11. " +
			"Unset one of them: clear --amdproxy-cu-mask (including any node-wide default) to time-slice this sandbox, " +
			"or clear --amdproxy-gpu-scheduler-socket to keep the spatial partition")
	}
	return nil
}


// +stateify savable
type amdproxy struct {
	useDevGofer bool
	devInfo     DeviceInfo

	// cuMask is the sandbox's compute unit ceiling, applied to every queue
	// it creates. Immutable after Register.
	cuMask amdconf.CUMask

	// memAcct tracks GPU memory charged to this sandbox. It is shared by
	// every device and file description in the sandbox, so it accounts the
	// sandbox's aggregate usage across all of its processes.
	memAcct memAccount

	// vmShare lets the sandbox's processes share the one GPU address space
	// the driver will bind to it, and vaGuard keeps their allocations from
	// overlapping once they do. Both are inert unless sharing is enabled.
	vmShare vmShare
	vaGuard vaGuard

	// renderShare keeps the sandbox's processes on one open render node, so
	// that the address space vmShare lets them share is one they can all
	// actually allocate and map through.
	renderShare renderShare

	// runtimeShare makes the debug runtime, which the driver keeps once per
	// KFD process, survive being enabled by more than one of them.
	runtimeShare runtimeShare

	// eventShare bounds the waits of processes the driver refused a signal
	// page, since an event can never wake them.
	eventShare eventShare

	// timeSlicer holds the sandbox to its share of the GPU by suspending the
	// queues it submits through. It is inert unless a scheduler connection was
	// donated.
	timeSlicer timeSlicer

	fdsMu     sync.Mutex `state:"nosave"`
	kfdFDs    map[*kfdFD]struct{}
	renderFDs map[*renderFD]struct{}
}

func (amdp *amdproxy) trackFD(fd *kfdFD) {
	amdp.fdsMu.Lock()
	defer amdp.fdsMu.Unlock()
	amdp.kfdFDs[fd] = struct{}{}
}

func (amdp *amdproxy) untrackFD(fd *kfdFD) {
	amdp.fdsMu.Lock()
	last := false
	successor := int32(-1)
	if _, ok := amdp.kfdFDs[fd]; ok {
		delete(amdp.kfdFDs, fd)
		last = len(amdp.kfdFDs) == 0
	}
	if !last {
		// Any descriptor still open reaches the same kfd_process, so any of
		// them can carry a debug session opened on the one going away.
		for other := range amdp.kfdFDs {
			if other.hostFD >= 0 {
				successor = other.hostFD
				break
			}
		}
	}
	amdp.fdsMu.Unlock()
	if !last {
		// This descriptor is going away but the sandbox is still using the
		// GPU. If the debug session was opened on it, it has to be moved: the
		// number is about to be reused.
		amdp.timeSlicer.forgetHostFD(fd.hostFD, successor)
		return
	}
	if last {
		// The sandbox has stopped using the GPU. Stop slicing it, and above
		// all leave its queues running: a suspended queue cannot be destroyed,
		// so a session torn down mid-suspend would strand them.
		amdp.timeSlicer.shutdown()
	}
}

func (amdp *amdproxy) trackRenderFD(fd *renderFD) {
	amdp.fdsMu.Lock()
	defer amdp.fdsMu.Unlock()
	amdp.renderFDs[fd] = struct{}{}
}

func (amdp *amdproxy) untrackRenderFD(fd *renderFD) {
	amdp.fdsMu.Lock()
	defer amdp.fdsMu.Unlock()
	delete(amdp.renderFDs, fd)
}

type marshalPtr[T any] interface {
	*T
	marshal.Marshallable
}

func openHostDevFile(ctx context.Context, relpath string, useDevGofer bool, openFlags uint32) (int32, string, error) {
	if useDevGofer {
		devClient := devutil.GoferClientFromContext(ctx)
		if devClient == nil {
			ctx.Warningf("amdproxy: failed to open device gofer %s: devutil.CtxDevGoferClient is not set", relpath)
			return -1, "", linuxerr.ENOENT
		}
		containerName := devClient.ContainerName()
		hostFD, err := devClient.OpenAt(ctx, relpath, openFlags)
		if err != nil {
			ctx.Warningf("amdproxy: failed to open device gofer %s: %v", relpath, err)
			return -1, "", err
		}
		return int32(hostFD), containerName, nil
	}
	abspath := filepath.Join("/dev", relpath)
	hostFD, err := unix.Openat(-1, abspath, int(openFlags&unix.O_ACCMODE|unix.O_NOFOLLOW), 0)
	if err != nil {
		ctx.Warningf("amdproxy: failed to open host %s: %v", abspath, err)
		return -1, "", err
	}
	return int32(hostFD), "", nil
}
