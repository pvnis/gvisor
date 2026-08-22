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

package nvproxy

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fdnotifier"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

// uvmDevice implements vfs.Device for /dev/nvidia-uvm.
//
// +stateify savable
type uvmDevice struct {
	nvp *nvproxy
}

// Open implements vfs.Device.Open.
func (dev *uvmDevice) Open(ctx context.Context, mnt *vfs.Mount, vfsd *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	fd := &uvmFD{
		dev: dev,
	}
	var err error
	fd.hostFD, fd.containerName, err = openHostDevFile(ctx, "nvidia-uvm", dev.nvp.useDevGofer, opts.Flags)
	if err != nil {
		return nil, err
	}
	if err := fd.vfsfd.Init(fd, opts.Flags, auth.CredentialsFromContext(ctx), mnt, vfsd, &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
		SpecialFile:       true,
	}); err != nil {
		unix.Close(int(fd.hostFD))
		return nil, err
	}
	if err := fdnotifier.AddFD(fd.hostFD, &fd.queue); err != nil {
		unix.Close(int(fd.hostFD))
		return nil, err
	}
	fd.memmapFile.SetFD(int(fd.hostFD))
	fd.memmapFile.RequireAddrEqualsFileOffset()
	return &fd.vfsfd, nil
}

// uvmFD implements vfs.FileDescriptionImpl for /dev/nvidia-uvm.
//
// +stateify savable
type uvmFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD

	dev           *uvmDevice
	containerName string
	hostFD        int32
	memmapFile    uvmFDMemmapFile

	// mappings tracks application mappings of this file, which reserve the
	// address space that CUDA unified memory is committed into. It is used to
	// account that reservation; see memKindUVMVA. Mappings are not tracked for
	// invalidation, which this file does not require.
	//
	// mappings is protected by mappingsMu.
	mappingsMu sync.Mutex `state:"nosave"`
	mappings   memmap.MappingSet

	queue waiter.Queue

	// monitorStop, when closed, stops the gmem thrash-detection monitor
	// goroutine started for this fd by setUVMGmemLimit (see gmemMonitor). nil
	// when no monitor runs (no gmem quota configured, or a driver without the
	// counters). Not saved: a GPU sandbox cannot be checkpointed anyway.
	monitorStop chan struct{} `state:"nosave"`
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *uvmFD) Release(context.Context) {
	if fd.monitorStop != nil {
		close(fd.monitorStop)
	}
	fdnotifier.RemoveFD(fd.hostFD)
	fd.queue.Notify(waiter.EventHUp)
	fd.memmapFile.MappableRelease()
}

// EventRegister implements waiter.Waitable.EventRegister.
func (fd *uvmFD) EventRegister(e *waiter.Entry) error {
	fd.queue.EventRegister(e)
	if err := fdnotifier.UpdateFD(fd.hostFD); err != nil {
		fd.queue.EventUnregister(e)
		return err
	}
	return nil
}

// EventUnregister implements waiter.Waitable.EventUnregister.
func (fd *uvmFD) EventUnregister(e *waiter.Entry) {
	fd.queue.EventUnregister(e)
	if err := fdnotifier.UpdateFD(fd.hostFD); err != nil {
		panic(fmt.Sprint("UpdateFD:", err))
	}
}

// Readiness implements waiter.Waitable.Readiness.
func (fd *uvmFD) Readiness(mask waiter.EventMask) waiter.EventMask {
	return fdnotifier.NonBlockingPoll(fd.hostFD, mask)
}

// Epollable implements vfs.FileDescriptionImpl.Epollable.
func (fd *uvmFD) Epollable() bool {
	return true
}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl.
func (fd *uvmFD) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	cmd := args[1].Uint()
	argPtr := args[2].Pointer()

	t := kernel.TaskFromContext(ctx)
	if t == nil {
		panic("Ioctl should be called from a task context")
	}

	if ctx.IsLogging(log.Debug) {
		ctx.Debugf("nvproxy: uvm ioctl %d = %#x", cmd, cmd)
	}

	ui := uvmIoctlState{
		fd:              fd,
		ctx:             ctx,
		t:               t,
		cmd:             cmd,
		ioctlParamsAddr: argPtr,
	}
	result, err := fd.dev.nvp.abi.uvmIoctl[cmd].handle(&ui)
	if err != nil {
		if handleErr, ok := err.(*errHandler); ok {
			ctx.Warningf("nvproxy: %v for uvm ioctl %d = %#x", handleErr, cmd, cmd)
			return 0, linuxerr.EINVAL
		}
	}
	return result, err
}

// IsNvidiaDeviceFD implements NvidiaDeviceFD.IsNvidiaDeviceFD.
func (fd *uvmFD) IsNvidiaDeviceFD() {}

// uvmIoctlState holds the state of a call to uvmFD.Ioctl().
type uvmIoctlState struct {
	fd              *uvmFD
	ctx             context.Context
	t               *kernel.Task
	cmd             uint32
	ioctlParamsAddr hostarch.Addr
}

func uvmIoctlNoParams(ui *uvmIoctlState) (uintptr, error) {
	n, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(ui.fd.hostFD), uintptr(ui.cmd), 0 /* params */)
	if errno != 0 {
		return n, errno
	}
	return n, nil
}

func uvmIoctlSimple[Params any, PtrParams hasStatusPtr[Params]](ui *uvmIoctlState) (uintptr, error) {
	var ioctlParamsValue Params
	ioctlParams := PtrParams(&ioctlParamsValue)
	if _, err := ioctlParams.CopyIn(ui.t, ui.ioctlParamsAddr); err != nil {
		return 0, err
	}
	n, err := uvmIoctlInvoke(ui, ioctlParams)
	if err != nil {
		return n, err
	}
	if _, err := ioctlParams.CopyOut(ui.t, ui.ioctlParamsAddr); err != nil {
		return n, err
	}
	return n, nil
}

func uvmInitialize(ui *uvmIoctlState) (uintptr, error) {
	var ioctlParams nvgpu.UVM_INITIALIZE_PARAMS
	if _, err := ioctlParams.CopyIn(ui.t, ui.ioctlParamsAddr); err != nil {
		return 0, err
	}
	origFlags := ioctlParams.Flags
	// This is necessary to share the host UVM FD between sentry and
	// application processes.
	ioctlParams.Flags = ioctlParams.Flags | nvgpu.UVM_INIT_FLAGS_MULTI_PROCESS_SHARING_MODE
	n, err := uvmIoctlInvoke(ui, &ioctlParams)
	// Only expose the MULTI_PROCESS_SHARING_MODE flag if it was already present.
	ioctlParams.Flags &^= ^origFlags & nvgpu.UVM_INIT_FLAGS_MULTI_PROCESS_SHARING_MODE
	if err != nil {
		return n, err
	}
	// The va_space now exists on the host UVM fd; program its per-tenant
	// device-resident cap from this sandbox's gmem quota.
	setUVMGmemLimit(ui)
	if _, err := ioctlParams.CopyOut(ui.t, ui.ioctlParamsAddr); err != nil {
		return n, err
	}
	return n, nil
}

// setUVMGmemLimit programs the driver's per-va_space device-resident cap
// (UVM_SET_GMEM_LIMIT) from the sandbox's gmem quota, so the driver's per-tenant
// eviction keeps this sandbox's device residency within its share while its
// unified memory oversubscribes into host swap. Best-effort and non-fatal: on a
// driver without the ioctl (or with no quota configured) the sandbox keeps the
// driver's default global eviction, exactly the prior behaviour.
func setUVMGmemLimit(ui *uvmIoctlState) {
	limit := ui.fd.dev.nvp.memAcct.residentLimit()
	if limit == 0 {
		return
	}
	ioctlParams := nvgpu.UVM_SET_GMEM_LIMIT_PARAMS{
		Limit: limit,
		Group: ui.fd.dev.nvp.gmemGroupID,
	}
	sub := &uvmIoctlState{
		fd:  ui.fd,
		ctx: ui.ctx,
		t:   ui.t,
		cmd: nvgpu.UVM_SET_GMEM_LIMIT,
	}
	if _, err := uvmIoctlInvoke(sub, &ioctlParams); err != nil {
		ui.ctx.Warningf("nvproxy: failed to set UVM gmem limit to %d bytes: %v", limit, err)
		return
	}
	// The driver accepted the cap and reported this tenant's counters back, so
	// the thrash-detection counters are present. Start a background monitor that
	// samples them and logs the eviction rate (the detection half of the
	// thrash-policy mechanism). One per uvm fd; harmless if several of a
	// sandbox's fds each start one, since they observe the same shared group.
	if ui.fd.monitorStop == nil {
		ui.fd.monitorStop = make(chan struct{})
		go gmemMonitor(ui.fd, limit, ui.fd.dev.nvp.gmemGroupID)
	}
}

// Thresholds for tagging a sample as thrashing. These are fixed for now; the
// configurable thrash *policy* (per-node default + narrow-only per-tenant
// override, and the throttle/detach/kill actions) is layered on in a later step.
const (
	// A tenant counts as "at cap" when its device-resident bytes are within this
	// fraction of its gmem limit — the precondition for thrashing (a tenant well
	// under its cap is not paging under pressure).
	gmemAtCapFraction = 0.9
	// Sustained eviction above this rate while at cap is the thrash signature.
	// A healthy overcommit (large but cold working set) evicts once at warmup
	// then settles to ~0; a thrasher pages every access and stays high.
	gmemThrashRateMiBps = 100.0
	// How often the monitor samples the driver counters.
	gmemMonitorInterval = 2 * time.Second
)

// gmemMonitor periodically reads this tenant's per-group device-resident and
// cumulative-evicted byte counters from the driver (via an idempotent
// UVM_SET_GMEM_LIMIT that returns them) and logs the eviction rate. A tenant
// pinned at its gmem cap with a sustained nonzero eviction rate is a thrashing
// oversubscriber — paging every access at PCIe speed and saturating the memory
// bus its neighbours share. Read-only and best-effort: it observes, never acts,
// and exits when the fd is released or the driver ioctl starts failing.
func gmemMonitor(fd *uvmFD, limit, group uint64) {
	ticker := time.NewTicker(gmemMonitorInterval)
	defer ticker.Stop()
	var prevEvicted uint64
	havePrev := false
	for {
		select {
		case <-fd.monitorStop:
			return
		case <-ticker.C:
		}
		resident, evicted, err := uvmQueryGmem(fd.hostFD, limit, group)
		if err != nil {
			// The fd is likely closing; stop quietly.
			return
		}
		if havePrev {
			deltaMiB := float64(int64(evicted-prevEvicted)) / (1 << 20)
			rateMiBps := deltaMiB / gmemMonitorInterval.Seconds()
			atCap := limit > 0 && float64(resident) >= gmemAtCapFraction*float64(limit)
			tag := ""
			if atCap && rateMiBps >= gmemThrashRateMiBps {
				tag = " THRASH"
			}
			log.Infof("nvproxy: gmem group=%#x resident=%dMiB/%dMiB evict_rate=%.0fMiB/s%s",
				group, resident>>20, limit>>20, rateMiBps, tag)
		}
		prevEvicted = evicted
		havePrev = true
	}
}

// gmemGroupIDFromContainerID derives the driver-side tenant group id for a
// sandbox from its container ID, via FNV-1a. All processes of the sandbox share
// this id so the driver accounts their UVM device residency together; different
// sandboxes get different ids. Non-zero, since the driver reads zero as "no
// group".
func gmemGroupIDFromContainerID(containerID string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(containerID); i++ {
		h ^= uint64(containerID[i])
		h *= prime64
	}
	if h == 0 {
		h = 1
	}
	return h
}

func uvmMMInitialize(ui *uvmIoctlState) (uintptr, error) {
	var ioctlParams nvgpu.UVM_MM_INITIALIZE_PARAMS
	if _, err := ioctlParams.CopyIn(ui.t, ui.ioctlParamsAddr); err != nil {
		return 0, err
	}

	uvmFileGeneric, _ := ui.t.FDTable().Get(ioctlParams.UvmFD)
	if uvmFileGeneric == nil {
		return 0, uvmFailWithStatus(ui, &ioctlParams, nvgpu.NV_ERR_INVALID_ARGUMENT)
	}
	defer uvmFileGeneric.DecRef(ui.ctx)
	uvmFile, ok := uvmFileGeneric.Impl().(*uvmFD)
	if !ok {
		return 0, uvmFailWithStatus(ui, &ioctlParams, nvgpu.NV_ERR_INVALID_ARGUMENT)
	}

	origFD := ioctlParams.UvmFD
	ioctlParams.UvmFD = uvmFile.hostFD
	n, err := uvmIoctlInvoke(ui, &ioctlParams)
	ioctlParams.UvmFD = origFD
	if err != nil {
		return n, err
	}
	if _, err := ioctlParams.CopyOut(ui.t, ui.ioctlParamsAddr); err != nil {
		return n, err
	}
	return n, nil
}

func uvmIoctlHasFrontendFD[Params any, PtrParams hasFrontendFDAndStatusPtr[Params]](ui *uvmIoctlState) (uintptr, error) {
	var ioctlParamsValue Params
	ioctlParams := PtrParams(&ioctlParamsValue)
	if _, err := ioctlParams.CopyIn(ui.t, ui.ioctlParamsAddr); err != nil {
		return 0, err
	}

	origFD := ioctlParams.GetFrontendFD()
	if origFD < 0 {
		n, err := uvmIoctlInvoke(ui, ioctlParams)
		if err != nil {
			return n, err
		}
		if _, err := ioctlParams.CopyOut(ui.t, ui.ioctlParamsAddr); err != nil {
			return n, err
		}
		return n, nil
	}

	ctlFileGeneric, _ := ui.t.FDTable().Get(origFD)
	if ctlFileGeneric == nil {
		return 0, linuxerr.EINVAL
	}
	defer ctlFileGeneric.DecRef(ui.ctx)
	ctlFile, ok := ctlFileGeneric.Impl().(*frontendFD)
	if !ok {
		return 0, linuxerr.EINVAL
	}

	ioctlParams.SetFrontendFD(ctlFile.hostFD)
	n, err := uvmIoctlInvoke(ui, ioctlParams)
	ioctlParams.SetFrontendFD(origFD)
	if err != nil {
		return n, err
	}
	if _, err := ioctlParams.CopyOut(ui.t, ui.ioctlParamsAddr); err != nil {
		return n, err
	}
	return n, nil
}

func uvmFailWithStatus[Params any, PtrParams hasStatusPtr[Params]](ui *uvmIoctlState, ioctlParams PtrParams, status uint32) error {
	return failWithStatus(ui.ctx, ui.t, ui.ioctlParamsAddr, ioctlParams, status)
}
