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

// renderDevice implements vfs.Device for /dev/dri/renderD*.
//
// +stateify savable
type renderDevice struct {
	amdp *amdproxy

	// minor is the device minor number, which matches the host's.
	minor uint32
}

// Open implements vfs.Device.Open.
func (dev *renderDevice) Open(ctx context.Context, mnt *vfs.Mount, d *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	relpath := fmt.Sprintf("dri/renderD%d", dev.minor)
	// With address space sharing on, every process in the sandbox must end up
	// on the same open file, or it will be refused the buffers the first one
	// allocated; see renderShare.
	hostFD, containerName, err := dev.amdp.renderShare.open(dev.minor, func() (int32, string, error) {
		return openHostDevFile(ctx, relpath, dev.amdp.useDevGofer, opts.Flags)
	})
	if err != nil {
		return nil, err
	}
	fd := &renderFD{
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
	dev.amdp.trackRenderFD(fd)
	return &fd.vfsfd, nil
}

// renderFD implements vfs.FileDescriptionImpl for /dev/dri/renderD*.
//
// A render node is the second interface to the same physical GPU that
// /dev/kfd manages. ROCm opens one per device and hands it to
// AMDKFD_IOC_ACQUIRE_VM, which is why amdproxy must implement it: the
// address space KFD binds a process to is owned by this file description.
// It is also a second path to device memory, via the amdgpu GEM ioctls, so
// any accounting that only watched KFD would be trivially bypassed by
// allocating here instead.
//
// +stateify savable
type renderFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD
	memmap.MappableNoTrackMappings

	hostFD        int32
	containerName string

	dev        *renderDevice
	queue      waiter.Queue
	memmapFile fsutil.MmapPreciseFile
}

func (fd *renderFD) isRestored() bool {
	return fd.hostFD == -1
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *renderFD) Release(context.Context) {
	defer fd.dev.amdp.untrackRenderFD(fd)
	if fd.isRestored() {
		return
	}
	fdnotifier.RemoveFD(fd.hostFD)
	fd.queue.Notify(waiter.EventHUp)
	fd.memmapFile.MappableRelease() // eventually closes fd.hostFD
}

// EventRegister implements waiter.Waitable.EventRegister.
func (fd *renderFD) EventRegister(e *waiter.Entry) error {
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
func (fd *renderFD) EventUnregister(e *waiter.Entry) {
	if fd.isRestored() {
		return
	}
	fd.queue.EventUnregister(e)
	if err := fdnotifier.UpdateFD(fd.hostFD); err != nil {
		panic(fmt.Sprint("UpdateFD:", err))
	}
}

// Readiness implements waiter.Waitable.Readiness.
func (fd *renderFD) Readiness(mask waiter.EventMask) waiter.EventMask {
	if fd.isRestored() {
		return 0
	}
	return fdnotifier.NonBlockingPoll(fd.hostFD, mask)
}

// Epollable implements vfs.FileDescriptionImpl.Epollable.
func (fd *renderFD) Epollable() bool {
	return true
}

// renderIoctlState holds the state of a single render node ioctl in progress.
type renderIoctlState struct {
	fd      *renderFD
	ctx     context.Context
	t       *kernel.Task
	cmd     uint32
	argAddr hostarch.Addr
}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl.
//
// Only the ioctls needed to identify the device are forwarded. The GEM
// allocation, address-binding and command-submission ioctls
// (AMDGPU_GEM_CREATE, AMDGPU_GEM_VA, AMDGPU_CS and friends) are denied:
// allocation here is a second path to device memory that must be charged
// against the same budget as KFD's, and command submission is the point at
// which work reaches the GPU. Neither should be opened up before the
// accounting that governs them exists.
func (fd *renderFD) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	if fd.isRestored() {
		return 0, linuxerr.EBADF
	}
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		panic("Ioctl should be called from a task context")
	}
	ri := &renderIoctlState{
		fd:      fd,
		ctx:     ctx,
		t:       t,
		cmd:     args[1].Uint(),
		argAddr: args[2].Pointer(),
	}
	switch amdgpu.DRMIoctl(ri.cmd) {
	case amdgpu.DRM_IOCTL_GET_CLIENT:
		return renderIoctlSimple[amdgpu.DRMClient](ri)
	case amdgpu.DRM_IOCTL_VERSION:
		return drmVersion(ri)
	case amdgpu.DRM_IOCTL_AMDGPU_INFO:
		return drmAMDGPUInfo(ri)
	}
	ctx.Warningf("amdproxy: unsupported DRM render node ioctl %s (%#x)", amdgpu.DRMIoctl(ri.cmd), ri.cmd)
	return 0, linuxerr.EINVAL
}
