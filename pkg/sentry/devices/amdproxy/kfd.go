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

package amdproxy

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fdnotifier"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

// kfdDevice implements vfs.Device for /dev/kfd.
//
// +stateify savable
type kfdDevice struct {
	amdp *amdproxy
}

// Open implements vfs.Device.Open.
func (dev *kfdDevice) Open(ctx context.Context, mnt *vfs.Mount, d *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	hostFD, containerName, err := openHostDevFile(ctx, "kfd", dev.amdp.useDevGofer, opts.Flags)
	if err != nil {
		return nil, err
	}
	fd := &kfdFD{
		hostFD:        hostFD,
		containerName: containerName,
		dev:           dev,
	}
	if err := fd.vfsfd.Init(fd, opts.Flags, auth.CredentialsFromContext(ctx), mnt, d, &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
		SpecialFile:       true,
	}); err != nil {
		unix.Close(int(hostFD))
		return nil, err
	}
	if err := fdnotifier.AddFD(hostFD, &fd.queue); err != nil {
		unix.Close(int(hostFD))
		return nil, err
	}
	fd.memmapFile.SetFD(int(hostFD))
	// Everything reachable by mmap()ing /dev/kfd is device registers: the
	// doorbells a queue is rung through, the MMIO remap page, and the event
	// page. Mapping those write-back would be wrong.
	fd.memmapFile.SetMemType(hostarch.MemoryTypeUncached)
	dev.amdp.trackFD(fd)
	return &fd.vfsfd, nil
}

// kfdFD implements vfs.FileDescriptionImpl for /dev/kfd.
//
// +stateify savable
type kfdFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD
	memmap.MappableNoTrackMappings

	hostFD        int32
	containerName string

	dev        *kfdDevice
	queue      waiter.Queue
	memmapFile fsutil.MmapPreciseFile
}

func (fd *kfdFD) isRestored() bool {
	return fd.hostFD == -1
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *kfdFD) Release(context.Context) {
	defer fd.dev.amdp.untrackFD(fd)
	if fd.isRestored() {
		return
	}
	fdnotifier.RemoveFD(fd.hostFD)
	fd.queue.Notify(waiter.EventHUp)
	fd.memmapFile.MappableRelease() // eventually closes fd.hostFD
}

// EventRegister implements waiter.Waitable.EventRegister.
func (fd *kfdFD) EventRegister(e *waiter.Entry) error {
	if fd.isRestored() {
		return nil
	}
	fd.queue.EventRegister(e)
	if err := fdnotifier.UpdateFD(fd.hostFD); err != nil {
		fd.queue.EventUnregister(e)
		return err
	}
	return nil
}

// EventUnregister implements waiter.Waitable.EventUnregister.
func (fd *kfdFD) EventUnregister(e *waiter.Entry) {
	if fd.isRestored() {
		return
	}
	fd.queue.EventUnregister(e)
	if err := fdnotifier.UpdateFD(fd.hostFD); err != nil {
		panic(fmt.Sprint("UpdateFD:", err))
	}
}

// Readiness implements waiter.Waitable.Readiness.
func (fd *kfdFD) Readiness(mask waiter.EventMask) waiter.EventMask {
	if fd.isRestored() {
		return 0
	}
	return fdnotifier.NonBlockingPoll(fd.hostFD, mask)
}

// Epollable implements vfs.FileDescriptionImpl.Epollable.
func (fd *kfdFD) Epollable() bool {
	return true
}

// kfdIoctlState holds the state of a single KFD ioctl in progress.
type kfdIoctlState struct {
	fd      *kfdFD
	ctx     context.Context
	t       *kernel.Task
	cmd     uint32
	argAddr hostarch.Addr
}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl.
//
// The kfdIoctlSimple cases are ioctls whose parameter struct is fixed-size
// and contains no pointers into application memory and no file descriptors,
// so they can be copied in, forwarded, and copied back out unchanged. The
// cases below those need a dedicated handler, either to translate a file
// descriptor or to retarget a pointer at a Sentry buffer.
//
// Every other KFD ioctl is denied, and each needs handling this package does
// not yet implement: ioctls carrying pointers to application buffers
// (GET_PROCESS_APERTURES, GET_QUEUE_WAVE_STATE, GET_DMABUF_INFO);
// ioctls carrying file descriptors (IMPORT_DMABUF, EXPORT_DMABUF, SMI_EVENTS);
// and ioctls with variable-length or otherwise unvalidated parameters
// (SVM, CRIU_OP, DBG_TRAP).
func (fd *kfdFD) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	if fd.isRestored() {
		return 0, linuxerr.EBADF
	}
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		panic("Ioctl should be called from a task context")
	}
	ki := &kfdIoctlState{
		fd:      fd,
		ctx:     ctx,
		t:       t,
		cmd:     args[1].Uint(),
		argAddr: args[2].Pointer(),
	}
	switch amdgpu.KFDIoctl(ki.cmd) {
	case amdgpu.AMDKFD_IOC_GET_VERSION:
		return kfdIoctlSimple[amdgpu.KFDIoctlGetVersionArgs](ki)
	case amdgpu.AMDKFD_IOC_DESTROY_QUEUE:
		return kfdIoctlSimple[amdgpu.KFDIoctlDestroyQueueArgs](ki)
	case amdgpu.AMDKFD_IOC_UPDATE_QUEUE:
		return kfdIoctlSimple[amdgpu.KFDIoctlUpdateQueueArgs](ki)
	case amdgpu.AMDKFD_IOC_SET_MEMORY_POLICY:
		return kfdIoctlSimple[amdgpu.KFDIoctlSetMemoryPolicyArgs](ki)
	case amdgpu.AMDKFD_IOC_GET_CLOCK_COUNTERS:
		return kfdIoctlSimple[amdgpu.KFDIoctlGetClockCountersArgs](ki)
	case amdgpu.AMDKFD_IOC_CREATE_EVENT:
		return kfdIoctlSimple[amdgpu.KFDIoctlCreateEventArgs](ki)
	case amdgpu.AMDKFD_IOC_DESTROY_EVENT:
		return kfdIoctlSimple[amdgpu.KFDIoctlDestroyEventArgs](ki)
	case amdgpu.AMDKFD_IOC_SET_EVENT:
		return kfdIoctlSimple[amdgpu.KFDIoctlSetEventArgs](ki)
	case amdgpu.AMDKFD_IOC_RESET_EVENT:
		return kfdIoctlSimple[amdgpu.KFDIoctlResetEventArgs](ki)
	case amdgpu.AMDKFD_IOC_SET_SCRATCH_BACKING_VA:
		return kfdIoctlSimple[amdgpu.KFDIoctlSetScratchBackingVAArgs](ki)
	case amdgpu.AMDKFD_IOC_SET_TRAP_HANDLER:
		return kfdIoctlSimple[amdgpu.KFDIoctlSetTrapHandlerArgs](ki)
	case amdgpu.AMDKFD_IOC_ALLOC_QUEUE_GWS:
		return kfdIoctlSimple[amdgpu.KFDIoctlAllocQueueGWSArgs](ki)
	case amdgpu.AMDKFD_IOC_SET_XNACK_MODE:
		return kfdIoctlSimple[amdgpu.KFDIoctlSetXNACKModeArgs](ki)
	case amdgpu.AMDKFD_IOC_RUNTIME_ENABLE:
		return kfdIoctlSimple[amdgpu.KFDIoctlRuntimeEnableArgs](ki)
	case amdgpu.AMDKFD_IOC_GET_TILE_CONFIG:
		return kfdGetTileConfig(ki)
	case amdgpu.AMDKFD_IOC_WAIT_EVENTS:
		return kfdWaitEvents(ki)
	case amdgpu.AMDKFD_IOC_ACQUIRE_VM:
		return kfdAcquireVM(ki)
	case amdgpu.AMDKFD_IOC_GET_PROCESS_APERTURES_NEW:
		return kfdGetProcessAperturesNew(ki)
	case amdgpu.AMDKFD_IOC_MAP_MEMORY_TO_GPU, amdgpu.AMDKFD_IOC_UNMAP_MEMORY_FROM_GPU:
		return kfdMapMemoryToGPU(ki)
	case amdgpu.AMDKFD_IOC_SET_CU_MASK:
		return kfdSetCUMask(ki)
	case amdgpu.AMDKFD_IOC_CREATE_QUEUE:
		return kfdCreateQueue(ki)
	case amdgpu.AMDKFD_IOC_SVM:
		return kfdSVM(ki)
	case amdgpu.AMDKFD_IOC_ALLOC_MEMORY_OF_GPU:
		return kfdAllocMemoryOfGPU(ki)
	case amdgpu.AMDKFD_IOC_FREE_MEMORY_OF_GPU:
		return kfdFreeMemoryOfGPU(ki)
	case amdgpu.AMDKFD_IOC_AVAILABLE_MEMORY:
		return kfdAvailableMemory(ki)
	}
	ctx.Warningf("amdproxy: unsupported KFD ioctl %s (%#x)", amdgpu.KFDIoctl(ki.cmd), ki.cmd)
	return 0, linuxerr.EINVAL
}

// kfdIoctlSimple implements a KFD ioctl whose parameters contain no pointers
// requiring translation, no file descriptors, and no special cases or
// effects.
func kfdIoctlSimple[Params any, PtrParams marshalPtr[Params]](ki *kfdIoctlState) (uintptr, error) {
	var paramsValue Params
	params := PtrParams(&paramsValue)
	if _, err := params.CopyIn(ki.t, ki.argAddr); err != nil {
		return 0, err
	}
	n, err := kfdIoctlInvoke(ki, params)
	if err != nil {
		return n, err
	}
	if _, err := params.CopyOut(ki.t, ki.argAddr); err != nil {
		return n, err
	}
	return n, nil
}
