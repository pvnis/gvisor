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

package sys

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/amdsysfs"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
)

// This file builds the sysfs surface that ROCm reads to discover AMD GPUs.
//
// The ROCm runtime checks the Kernel Fusion Driver's topology before it
// opens /dev/kfd; without it, it reports that the driver is not loaded and
// stops, whatever the sandbox's device files say. The topology lives under
// /sys/devices/virtual/kfd/kfd/topology and is reached both directly and
// through the /sys/class/kfd symlink, so both are reproduced, with the class
// entry a symlink into the canonical subtree exactly as the kernel lays it
// out.

// amdGPUSysfsDirs is the output of newAMDGPUSysfs: subtrees for
// GetFilesystem to graft into the overall /sys hierarchy.
type amdGPUSysfsDirs struct {
	// kfd is the "kfd" directory to add under /sys/devices/virtual.
	kfd kernfs.Inode
	// class is the "kfd" directory to add under /sys/class.
	class kernfs.Inode
	// module is the "amdgpu" directory to add under /sys/module, or nil.
	// The ROCm runtime reads its initstate to decide whether the driver is
	// loaded, and reports that there is no GPU if it is missing.
	module kernfs.Inode
}

// newAMDGPUSysfs builds the KFD sysfs subtrees from a host snapshot.
func (fs *filesystem) newAMDGPUSysfs(ctx context.Context, creds *auth.Credentials, snap *amdsysfs.Snapshot) *amdGPUSysfsDirs {
	if snap == nil || snap.Topology == nil {
		return nil
	}
	// /sys/devices/virtual/kfd/kfd/topology/...
	kfdDev := fs.newDir(ctx, creds, defaultSysDirMode, map[string]kernfs.Inode{
		"kfd": fs.newDir(ctx, creds, defaultSysDirMode, map[string]kernfs.Inode{
			"topology": fs.newSnapshotDir(ctx, creds, snap.Topology),
		}),
	})
	// /sys/class/kfd/kfd -> ../../devices/virtual/kfd/kfd, matching the
	// kernel's own relative target so that callers which resolve it and then
	// navigate from the result land in the canonical subtree.
	class := fs.newDir(ctx, creds, defaultSysDirMode, map[string]kernfs.Inode{
		"kfd": kernfs.NewStaticSymlink(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), "../../devices/virtual/kfd/kfd"),
	})
	dirs := &amdGPUSysfsDirs{kfd: kfdDev, class: class}
	if snap.Module != nil {
		dirs.module = fs.newSnapshotDir(ctx, creds, snap.Module)
	}
	return dirs
}

// newSnapshotDir recursively builds a read-only directory tree mirroring a
// snapshotted sysfs directory.
func (fs *filesystem) newSnapshotDir(ctx context.Context, creds *auth.Credentials, d *amdsysfs.Dir) kernfs.Inode {
	contents := make(map[string]kernfs.Inode, len(d.Files)+len(d.Dirs))
	for name, data := range d.Files {
		if !amdsysfs.SafeName(name) {
			continue
		}
		contents[name] = fs.newStaticFile(ctx, creds, defaultSysMode, data)
	}
	for name, sub := range d.Dirs {
		if !amdsysfs.SafeName(name) || sub == nil {
			continue
		}
		contents[name] = fs.newSnapshotDir(ctx, creds, sub)
	}
	return fs.newDir(ctx, creds, defaultSysDirMode, contents)
}
