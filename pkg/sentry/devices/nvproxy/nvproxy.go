// Copyright 2023 The gVisor Authors.
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

// Package nvproxy implements proxying for the Nvidia GPU Linux kernel driver:
// https://github.com/NVIDIA/open-gpu-kernel-modules.
//
// Supported Nvidia GPUs: T4, L4, A100, A10G and H100.
//
// Lock ordering:
//
// - nvproxy.fdsMu
// - rootClient.objsMu
//   - nvproxy.clientsMu
package nvproxy

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/devutil"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/marshal"
	"gvisor.dev/gvisor/pkg/sentry/devices/nvproxy/nvconf"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
)

// Options holds arguments to Register.
type Options struct {
	// DriverVersion is the Nvidia GPU driver version.
	DriverVersion nvconf.DriverVersion

	// AllowUnsupportedDriver allows nvproxy to be initialized with an
	// unsupported driver version.
	AllowUnsupportedDriver bool

	// DriverCaps is the set of driver capabilities exposed to applications.
	DriverCaps nvconf.DriverCaps

	// GPUMemoryLimit is the maximum number of bytes of GPU memory that the
	// sandbox may allocate. Zero means no limit.
	GPUMemoryLimit uint64

	// MaxTimesliceUs is the longest GPU scheduler timeslice, in microseconds,
	// that the sandbox may request. Zero means no limit.
	MaxTimesliceUs uint64

	// MinComputePreemption is the least preemptible GPU compute
	// context-switch mode the sandbox may select. WFI means no limit, since
	// WFI is already the least preemptible mode there is.
	MinComputePreemption nvconf.ComputePreemption

	// ComputePercent is the percentage of wall-clock time during which the
	// sandbox may submit work to the GPU. Zero means no limit.
	ComputePercent uint64

	// Preempt is true if the sandbox's channel groups should be preempted when
	// its GPU window closes, evicting work already running rather than only
	// preventing more from being submitted.
	Preempt bool

	// SchedulerFD is an open connection to a GPU scheduler that decides this
	// sandbox's share, or -1 if it has none. The Sentry cannot connect to
	// anything itself, so the connection is made outside and donated.
	SchedulerFD int

	// SchedulerWeight is this sandbox's share relative to the others the
	// scheduler is dividing the GPU between.
	SchedulerWeight uint64

	// ContainerID identifies this sandbox to the scheduler.
	ContainerID string

	HostSettings *nvconf.HostSettings

	// If UseDevGofer is true, open device files via gofer.
	UseDevGofer bool
}

// Register registers all devices implemented by this package, and specified by
// opts, in vfsObj. If it succeeds, it returns information about registered
// devices; the returned DeviceInfo must not be mutated.
func Register(vfsObj *vfs.VirtualFilesystem, opts *Options) (*DeviceInfo, error) {
	// The kernel driver's interface is unstable, so only allow versions of the
	// driver that are known to be supported.
	log.Infof("NVIDIA driver version: %s", opts.DriverVersion)
	abiEntry, ok := abis[opts.DriverVersion]
	if !ok {
		return nil, fmt.Errorf("unsupported Nvidia driver version: %s", opts.DriverVersion)
	}
	if !abiEntry.supported {
		if !opts.AllowUnsupportedDriver {
			return nil, fmt.Errorf("unsupported Nvidia driver version: %s", opts.DriverVersion)
		}
		log.Warningf("NVIDIA driver version %s is not officially supported", opts.DriverVersion)
	}
	if opts.DriverCaps == 0 {
		log.Warningf("nvproxy: NVIDIA driver capability set is empty; all GPU operations will fail")
	}
	nvp := &nvproxy{
		abi:                    abiEntry.cons(),
		version:                opts.DriverVersion,
		capsEnabled:            opts.DriverCaps,
		useDevGofer:            opts.UseDevGofer,
		procDriverNvidiaParams: opts.HostSettings.ProcDriverNvidiaParams,
		frontendFDs:            make(map[*frontendFD]struct{}),
		clients:                make(map[nvgpu.Handle]*rootClient),
	}
	nvp.memAcct.gpuLimit = opts.GPUMemoryLimit
	nvp.maxTimesliceUs = opts.MaxTimesliceUs
	nvp.minComputePreemption = opts.MinComputePreemption
	scheduled := opts.SchedulerFD >= 0
	nvp.computeGate.init(opts.ComputePercent, scheduled, opts.Preempt)
	if scheduled {
		log.Infof("nvproxy: GPU share for %q decided by the scheduler, weight %d", opts.ContainerID, opts.SchedulerWeight)
		nvp.computeGate.follow(opts.SchedulerFD, opts.ContainerID, opts.SchedulerWeight, opts.ComputePercent)
	}
	if nvp.computeGate.enabled() {
		log.Infof("nvproxy: GPU compute limited to %d%% of wall-clock time", opts.ComputePercent)
		if opts.Preempt {
			log.Infof("nvproxy: GPU channel groups will be preempted when the window closes")
		}
		go nvp.computeGate.run()
	}
	if opts.MaxTimesliceUs != 0 {
		log.Infof("nvproxy: GPU scheduler timeslice limited to %d us", opts.MaxTimesliceUs)
	}
	if opts.MinComputePreemption != nvconf.ComputePreemptionWFI {
		log.Infof("nvproxy: GPU compute preemption mode required to be at least %s", opts.MinComputePreemption)
	}
	if opts.GPUMemoryLimit != 0 {
		log.Infof("nvproxy: GPU memory limited to %d bytes", opts.GPUMemoryLimit)
	}
	// Force ModifyDeviceFiles in /proc/driver/nvidia/params to 0. This is
	// consistent with libnvidia-container's src/nvc_mount.c:mount_procfs().
	nvp.procDriverNvidiaParams = strings.Replace(nvp.procDriverNvidiaParams, "ModifyDeviceFiles: 1", "ModifyDeviceFiles: 0", 1)

	for minor := uint32(0); minor <= nvgpu.NV_MINOR_DEVICE_NUMBER_REGULAR_MAX; minor++ {
		dev := &frontendDevice{
			nvp:   nvp,
			minor: minor,
		}
		nvp.regularDevs[minor] = dev
		if err := vfsObj.RegisterDevice(vfs.CharDevice, nvgpu.NV_MAJOR_DEVICE_NUMBER, minor, dev, &vfs.RegisterDeviceOptions{
			GroupName: "nvidia",
		}); err != nil {
			return nil, err
		}
	}
	if err := vfsObj.RegisterDevice(vfs.CharDevice, nvgpu.NV_MAJOR_DEVICE_NUMBER, nvgpu.NV_MINOR_DEVICE_NUMBER_CONTROL_DEVICE, &frontendDevice{
		nvp:   nvp,
		minor: nvgpu.NV_MINOR_DEVICE_NUMBER_CONTROL_DEVICE,
	}, &vfs.RegisterDeviceOptions{
		GroupName: "nvidiactl",
	}); err != nil {
		return nil, err
	}

	uvmDevMajor, err := vfsObj.GetDynamicCharDevMajor()
	if err != nil {
		return nil, fmt.Errorf("allocating device major number for nvidia-uvm: %w", err)
	}
	nvp.devInfo.UVMDevMajor = uvmDevMajor
	if err := vfsObj.RegisterDevice(vfs.CharDevice, uvmDevMajor, nvgpu.NVIDIA_UVM_PRIMARY_MINOR_NUMBER, &uvmDevice{
		nvp: nvp,
	}, &vfs.RegisterDeviceOptions{
		GroupName: "nvidia-uvm",
	}); err != nil {
		return nil, err
	}

	if opts.DriverCaps&nvconf.CapFabricIMEXManagement != 0 {
		if !opts.HostSettings.HaveFabricIMEXManagement {
			return nil, fmt.Errorf("driver capability %s is enabled, but fabric-imex-mgmt device minor number is unavailable", nvconf.CapFabricIMEXManagement)
		}
		capsDevMajor, err := vfsObj.GetDynamicCharDevMajor()
		if err != nil {
			return nil, fmt.Errorf("allocating device major number for nvidia-caps: %w", err)
		}
		nvp.devInfo.CapsDevMajor = capsDevMajor
		if err := vfsObj.RegisterDevice(vfs.CharDevice, capsDevMajor, opts.HostSettings.FabricIMEXManagementDevMinor, &openOnlyDevice{
			nvp:     nvp,
			relpath: fmt.Sprintf("nvidia-caps/nvidia-cap%d", opts.HostSettings.FabricIMEXManagementDevMinor),
		}, &vfs.RegisterDeviceOptions{
			GroupName: "nvidia-caps",
		}); err != nil {
			return nil, err
		}
		nvp.devInfo.HaveFabricIMEXManagement = true
		nvp.devInfo.FabricIMEXManagementDevMinor = opts.HostSettings.FabricIMEXManagementDevMinor
	}

	if imexChannelCount := opts.HostSettings.IMEXChannelCount(); imexChannelCount != 0 {
		capsIMEXChannelsDevMajor, err := vfsObj.GetDynamicCharDevMajor()
		if err != nil {
			return nil, fmt.Errorf("allocating device major number for nvidia-caps-imex-channels: %w", err)
		}
		nvp.devInfo.CapsIMEXChannelsDevMajor = capsIMEXChannelsDevMajor
		for minor := range imexChannelCount {
			if err := vfsObj.RegisterDevice(vfs.CharDevice, capsIMEXChannelsDevMajor, minor, &openOnlyDevice{
				nvp:     nvp,
				relpath: fmt.Sprintf("nvidia-caps-imex-channels/channel%d", minor),
			}, &vfs.RegisterDeviceOptions{
				GroupName: "nvidia-caps-imex-channels",
			}); err != nil {
				return nil, err
			}
		}
	}

	return &nvp.devInfo, nil
}

// DeviceInfo contains information on registered nvproxy devices.
//
// +stateify savable
type DeviceInfo struct {
	// CapsDevMajor is nvidia-caps' device major number. If CapsDevMajor is 0,
	// nvidia-caps is not enabled.
	CapsDevMajor uint32

	// If HaveFabricIMEXManagement is true, FabricIMEXManagementDevMinor is the
	// fabric-imex-mgmt capability's device minor number, which matches the
	// value on the host. (Its device major number is CapsDevMajor, which must
	// be non-zero and might not match the host's value.)
	HaveFabricIMEXManagement     bool
	FabricIMEXManagementDevMinor uint32

	// CapsIMEXChannelsDevMajor is nvidia-caps-imex-channels's device major
	// number. If CapsIMEXChannelsDevMajor is 0, nvidia-caps-imex-channels is
	// not enabled.
	CapsIMEXChannelsDevMajor uint32

	// UVMDevMajor is nvidia-uvm's device major number. If UVMDevMajor is 0,
	// nvidia-uvm is enabled.
	UVMDevMajor uint32
}

// DeviceInfoFromVFS returns device information for nvproxy devices registered
// in vfsObj. The returned DeviceInfo must not be mutated. If DeviceInfoFromVFS
// returns nil, nvproxy.Register(vfsObj) has not been called.
func DeviceInfoFromVFS(vfsObj *vfs.VirtualFilesystem) *DeviceInfo {
	if nvp := nvproxyFromVFS(vfsObj); nvp != nil {
		return &nvp.devInfo
	}
	return nil
}

// +stateify savable
type nvproxy struct {
	abi                    *driverABI `state:"nosave"`
	version                nvconf.DriverVersion
	capsEnabled            nvconf.DriverCaps
	maxTimesliceUs         uint64
	minComputePreemption   nvconf.ComputePreemption
	computeGate            computeGate
	useDevGofer            bool
	procDriverNvidiaParams string
	devInfo                DeviceInfo
	regularDevs            [nvgpu.NV_MINOR_DEVICE_NUMBER_REGULAR_MAX + 1]*frontendDevice

	// memAcct tracks GPU memory charged to this sandbox. It is shared by every
	// frontendDevice and frontendFD in the sandbox, so it accounts the
	// sandbox's aggregate usage across all of its processes.
	memAcct memAccount

	fdsMu       fdsMutex `state:"nosave"`
	frontendFDs map[*frontendFD]struct{}

	clientsMu sync.RWMutex `state:"nosave"`
	clients   map[nvgpu.Handle]*rootClient
}

func nvproxyFromVFS(vfsObj *vfs.VirtualFilesystem) *nvproxy {
	ctlDeviceAny := vfsObj.GetRegisteredDevice(vfs.CharDevice, nvgpu.NV_MAJOR_DEVICE_NUMBER, nvgpu.NV_MINOR_DEVICE_NUMBER_CONTROL_DEVICE)
	if ctlDeviceAny == nil {
		// nvproxy.Register() was never called.
		return nil
	}
	ctlDevice, ok := ctlDeviceAny.(*frontendDevice)
	if !ok {
		// Something else took nvidiactl's device number, which is surprising
		// but also implies that nvproxy.Register() was never called.
		return nil
	}
	return ctlDevice.nvp
}

type marshalPtr[T any] interface {
	*T
	marshal.Marshallable
}

func addrFromP64(p nvgpu.P64) hostarch.Addr {
	return hostarch.Addr(uintptr(uint64(p)))
}

type hasFrontendFDPtr[T any] interface {
	marshalPtr[T]
	nvgpu.HasFrontendFD
}

type hasStatusPtr[T any] interface {
	marshalPtr[T]
	nvgpu.HasStatus
}

type hasFBInfoListPtr[T any] interface {
	marshalPtr[T]
	nvgpu.HasFBInfoList
}

type hasFrontendFDAndStatusPtr[T any] interface {
	marshalPtr[T]
	nvgpu.HasFrontendFD
	nvgpu.HasStatus
}

type hasCtrlInfoListPtr[T any] interface {
	marshalPtr[T]
	nvgpu.HasCtrlInfoList
}

type hasPOsEventPtr[T any] interface {
	marshalPtr[T]
	nvgpu.HasPOsEvent
}

// NvidiaDeviceFD is an interface that should be implemented by all
// vfs.FileDescriptionImpl of Nvidia devices.
type NvidiaDeviceFD interface {
	IsNvidiaDeviceFD()
}

func openHostDevFile(ctx context.Context, relpath string, useDevGofer bool, openFlags uint32) (int32, string, error) {
	if useDevGofer {
		devClient := devutil.GoferClientFromContext(ctx)
		if devClient == nil {
			ctx.Warningf("nvproxy: failed to open device gofer %s: devutil.CtxDevGoferClient is not set", relpath)
			return -1, "", linuxerr.ENOENT
		}
		containerName := devClient.ContainerName()
		hostFD, err := devClient.OpenAt(ctx, relpath, openFlags)
		if err != nil {
			ctx.Warningf("nvproxy: failed to open device gofer %s: %v", relpath, err)
			return -1, "", err
		}
		return int32(hostFD), containerName, nil
	}
	abspath := filepath.Join("/dev", relpath)
	hostFD, err := unix.Openat(-1, abspath, int(openFlags&unix.O_ACCMODE|unix.O_NOFOLLOW), 0)
	if err != nil {
		ctx.Warningf("nvproxy: failed to open host %s: %v", abspath, err)
		return -1, "", err
	}
	return int32(hostFD), "", nil
}
