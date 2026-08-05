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
	"fmt"
	"path"
	"strings"

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
	// devices maps PCI root-complex names to their subtrees, to be added
	// under /sys/devices.
	devices map[string]kernfs.Inode
	// classDRM is the "drm" symlink directory for /sys/class.
	classDRM kernfs.Inode
	// devChar holds the /sys/dev/char symlinks naming the render nodes.
	devChar map[string]kernfs.Inode
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
	fs.newAMDGPUPCI(ctx, creds, snap, dirs)
	return dirs
}

// newAMDGPUPCI builds the PCI hierarchy holding the render nodes, plus the
// /sys/class/drm and /sys/dev/char symlinks that point into it.
//
// libdrm decides whether a file descriptor names a render node by resolving
// /sys/dev/char/<major>:<minor>/device/drm, so the chain of symlinks and
// directories that path walks through has to exist with the kernel's own
// shape: the node directory holds a "device" symlink to its PCI function,
// and that function holds the "drm" directory the node lives in.
func (fs *filesystem) newAMDGPUPCI(ctx context.Context, creds *auth.Credentials, snap *amdsysfs.Snapshot, dirs *amdGPUSysfsDirs) {
	if len(snap.DRM) == 0 {
		return
	}
	// Collect each PCI function's attributes, and the render nodes it owns.
	attrs := make(map[string]map[string]string, len(snap.PCI))
	for _, d := range snap.PCI {
		attrs[d.Path] = d.Files
	}
	nodesByPCI := make(map[string][]amdsysfs.DRMNode)
	for _, n := range snap.DRM {
		nodesByPCI[n.PCIPath] = append(nodesByPCI[n.PCIPath], n)
	}

	// Build the tree bottom-up into a nested map keyed by path component,
	// then convert to inodes, since a kernfs directory is sealed once made.
	type node struct {
		children map[string]*node
		files    map[string]string
		drm      []amdsysfs.DRMNode
	}
	roots := make(map[string]*node)
	getNode := func(p string) *node {
		comps := strings.Split(p, "/")
		root, ok := roots[comps[0]]
		if !ok {
			root = &node{children: make(map[string]*node)}
			roots[comps[0]] = root
		}
		cur := root
		for _, c := range comps[1:] {
			next, ok := cur.children[c]
			if !ok {
				next = &node{children: make(map[string]*node)}
				cur.children[c] = next
			}
			cur = next
		}
		return cur
	}
	for _, d := range snap.PCI {
		getNode(d.Path).files = attrs[d.Path]
	}
	for pciPath, nodes := range nodesByPCI {
		getNode(pciPath).drm = nodes
	}

	var build func(n *node, leafName string) kernfs.Inode
	build = func(n *node, leafName string) kernfs.Inode {
		contents := make(map[string]kernfs.Inode)
		for name, data := range n.files {
			if amdsysfs.SafeName(name) {
				contents[name] = fs.newStaticFile(ctx, creds, defaultSysMode, data)
			}
		}
		for name, child := range n.children {
			if amdsysfs.SafeName(name) {
				contents[name] = build(child, name)
			}
		}
		if len(n.drm) > 0 {
			drmContents := make(map[string]kernfs.Inode, len(n.drm))
			for _, dn := range n.drm {
				nodeContents := make(map[string]kernfs.Inode, len(dn.Files)+1)
				for name, data := range dn.Files {
					if amdsysfs.SafeName(name) {
						nodeContents[name] = fs.newStaticFile(ctx, creds, defaultSysMode, data)
					}
				}
				// <function>/drm/<node>/device -> ../../../<function>, the
				// kernel's own relative target.
				nodeContents["device"] = kernfs.NewStaticSymlink(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), "../../../"+leafName)
				drmContents[dn.Name] = fs.newDir(ctx, creds, defaultSysDirMode, nodeContents)
			}
			contents["drm"] = fs.newDir(ctx, creds, defaultSysDirMode, drmContents)
		}
		return fs.newDir(ctx, creds, defaultSysDirMode, contents)
	}

	dirs.devices = make(map[string]kernfs.Inode, len(roots))
	for name, n := range roots {
		dirs.devices[name] = build(n, name)
	}

	classDRMContents := make(map[string]kernfs.Inode, len(snap.DRM))
	dirs.devChar = make(map[string]kernfs.Inode, len(snap.DRM))
	for _, n := range snap.DRM {
		target := path.Join("devices", n.PCIPath, "drm", n.Name)
		classDRMContents[n.Name] = kernfs.NewStaticSymlink(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), "../../"+target)
		dirs.devChar[fmt.Sprintf("%d:%d", n.Major, n.Minor)] = kernfs.NewStaticSymlink(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), "../../"+target)
	}
	dirs.classDRM = fs.newDir(ctx, creds, defaultSysDirMode, classDRMContents)
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
