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
	"gvisor.dev/gvisor/pkg/marshal"
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
		kfdFDs:      make(map[*kfdFD]struct{}),
		renderFDs:   make(map[*renderFD]struct{}),
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

// +stateify savable
type amdproxy struct {
	useDevGofer bool
	devInfo     DeviceInfo

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
	defer amdp.fdsMu.Unlock()
	delete(amdp.kfdFDs, fd)
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
