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
	"strconv"
	"strings"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/amdsysfs"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
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
	// node is the /sys/devices/system/node subtree. ROCr reads this to
	// locate the nearest CPU agent (HSA_AMD_AGENT_INFO_NEAREST_CPU) and
	// to initialise several other GPU agent attributes that depend on
	// having resolved a CPU neighbourhood.
	node kernfs.Inode
}

// newAMDGPUSysfs builds the KFD sysfs subtrees from a host snapshot.
// memLimitBytes, when non-zero, is the container's GPU memory quota; the
// topology's memory-bank sizes are rewritten to this value.
func (fs *filesystem) newAMDGPUSysfs(ctx context.Context, creds *auth.Credentials, snap *amdsysfs.Snapshot, memLimitBytes uint64) *amdGPUSysfsDirs {
	if snap == nil || snap.Topology == nil {
		return nil
	}
	topo := snap.Topology
	if memLimitBytes > 0 {
		topo = applyMemoryLimit(topo, memLimitBytes)
	}
	// /sys/devices/virtual/kfd/kfd/topology/...
	kfdDev := fs.newDir(ctx, creds, defaultSysDirMode, map[string]kernfs.Inode{
		"kfd": fs.newDir(ctx, creds, defaultSysDirMode, map[string]kernfs.Inode{
			"topology": fs.newSnapshotDir(ctx, creds, topo),
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
	// ROCr reads /sys/devices/system/node to resolve the nearest CPU agent.
	// Without it, HSA_AMD_AGENT_INFO_NEAREST_CPU and several dependent
	// attributes (SCRATCH_LIMIT_MAX, CLOCK_COUNTERS, ...) return
	// HSA_STATUS_ERROR_INVALID_ARGUMENT, and hipGetDeviceCount fails.
	k := kernel.KernelFromContext(ctx)
	cores := uint(k.ApplicationCores())
	dirs.node = fs.newDir(ctx, creds, defaultSysDirMode, map[string]kernfs.Inode{
		"node0": fs.newDir(ctx, creds, defaultSysDirMode, map[string]kernfs.Inode{
			"cpumap":   fs.newStaticFile(ctx, creds, defaultSysMode, fullCPUMask(cores)+"\n"),
			"cpulist":  fs.newStaticFile(ctx, creds, defaultSysMode, cpuListString(cores)),
			"distance": fs.newStaticFile(ctx, creds, defaultSysMode, "10\n"),
		}),
	})
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
		hwmon    map[string]map[string]string
		// depth is how many components the node's path has under
		// /sys/devices, used to aim the "subsystem" symlink back at /sys.
		depth int
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
		n := getNode(d.Path)
		n.files = attrs[d.Path]
		n.hwmon = d.Hwmon
		n.depth = len(strings.Split(d.Path, "/"))
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
		if n.depth > 0 {
			// libdrm reads this to learn which bus the device is on. Its
			// absence is not handled as an error: readlink returns ENOENT and
			// libdrm then corrupts its heap, which surfaces as "double free
			// or corruption" and a SIGABRT in anything using it - amdsmi, and
			// so every ROCm framework that probes for a GPU that way.
			//
			// Only the link's text is read, never followed, so it does not
			// matter that /sys/bus is not otherwise synthesised; what libdrm
			// wants is the final component, "pci". The depth is the node's
			// own path components plus one for "devices", to climb back to
			// /sys.
			contents["subsystem"] = kernfs.NewStaticSymlink(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(),
				strings.Repeat("../", n.depth+1)+"bus/pci")
		}
		if len(n.hwmon) > 0 {
			// The AMD SMI library reads the card's sensor names and labels
			// while probing it, and crashes rather than degrading when the
			// directory is missing - a segfault, not a clean failure.
			hwmonContents := make(map[string]kernfs.Inode, len(n.hwmon))
			for inst, files := range n.hwmon {
				if !amdsysfs.SafeName(inst) {
					continue
				}
				instContents := make(map[string]kernfs.Inode, len(files))
				for name, data := range files {
					if amdsysfs.SafeName(name) {
						instContents[name] = fs.newStaticFile(ctx, creds, defaultSysMode, data)
					}
				}
				hwmonContents[inst] = fs.newDir(ctx, creds, defaultSysDirMode, instContents)
			}
			contents["hwmon"] = fs.newDir(ctx, creds, defaultSysDirMode, hwmonContents)
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

// applyMemoryLimit returns a shallow copy of topo with GPU memory-bank
// size_in_bytes fields rewritten to limitBytes. Only banks with heap_type 1
// (local/VRAM) are rewritten; system-RAM banks (heap_type 0) are left alone.
// local_mem_size in the per-node properties is also rewritten, covering
// discrete GPUs where that field is non-zero.
//
// Only the Dir nodes that contain memory size fields are cloned; the rest of
// the tree is shared with the original snapshot.
func applyMemoryLimit(topo *amdsysfs.Dir, limitBytes uint64) *amdsysfs.Dir {
	nodesDir := topo.Dirs["nodes"]
	if nodesDir == nil {
		return topo
	}
	newNodes := &amdsysfs.Dir{
		Files: nodesDir.Files,
		Dirs:  make(map[string]*amdsysfs.Dir, len(nodesDir.Dirs)),
	}
	for name, node := range nodesDir.Dirs {
		newNodes.Dirs[name] = rewriteNodeMemory(node, limitBytes)
	}
	newTopo := &amdsysfs.Dir{
		Files: topo.Files,
		Dirs:  make(map[string]*amdsysfs.Dir, len(topo.Dirs)),
	}
	for name, d := range topo.Dirs {
		newTopo.Dirs[name] = d
	}
	newTopo.Dirs["nodes"] = newNodes
	return newTopo
}

// rewriteNodeMemory returns a shallow copy of node with memory sizes rewritten.
func rewriteNodeMemory(node *amdsysfs.Dir, limitBytes uint64) *amdsysfs.Dir {
	if node == nil {
		return nil
	}
	newNode := &amdsysfs.Dir{
		Files: make(map[string]string, len(node.Files)),
		Dirs:  make(map[string]*amdsysfs.Dir, len(node.Dirs)),
	}
	for k, v := range node.Files {
		newNode.Files[k] = v
	}
	for k, v := range node.Dirs {
		newNode.Dirs[k] = v
	}
	// Rewrite local_mem_size only when the kernel reports a non-zero value
	// (discrete GPU). APU nodes report local_mem_size 0 to indicate that the
	// GPU uses the system NUMA memory rather than dedicated VRAM; writing the
	// quota there would misrepresent the memory topology.
	if props, ok := newNode.Files["properties"]; ok {
		if propUint64(props, "local_mem_size") > 0 {
			newNode.Files["properties"] = rewriteProp(props, "local_mem_size", limitBytes)
		}
	}
	if memBanks := node.Dirs["mem_banks"]; memBanks != nil {
		newBanks := &amdsysfs.Dir{
			Files: memBanks.Files,
			Dirs:  make(map[string]*amdsysfs.Dir, len(memBanks.Dirs)),
		}
		for name, bank := range memBanks.Dirs {
			if bank != nil {
				if props, ok := bank.Files["properties"]; ok && memBankHeapType(props) == 1 {
					newBank := &amdsysfs.Dir{
						Files: make(map[string]string, len(bank.Files)),
						Dirs:  bank.Dirs,
					}
					for k, v := range bank.Files {
						newBank.Files[k] = v
					}
					newBank.Files["properties"] = rewriteProp(props, "size_in_bytes", limitBytes)
					newBanks.Dirs[name] = newBank
					continue
				}
			}
			newBanks.Dirs[name] = bank
		}
		newNode.Dirs["mem_banks"] = newBanks
	}
	return newNode
}

// rewriteProp replaces the value of the named key in a sysfs properties string.
// Each line has the form "key value\n"; if key is found, its value is replaced
// with the decimal representation of newVal.
func rewriteProp(data, key string, newVal uint64) string {
	lines := strings.SplitAfter(data, "\n")
	for i, line := range lines {
		k, _, ok := strings.Cut(line, " ")
		if ok && k == key {
			lines[i] = key + " " + strconv.FormatUint(newVal, 10) + "\n"
		}
	}
	return strings.Join(lines, "")
}

// memBankHeapType returns the heap_type value from a mem_banks properties
// string, or -1 if it cannot be parsed.
func memBankHeapType(data string) int {
	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && k == "heap_type" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	return -1
}

// propUint64 returns the value of the named key in a sysfs properties string,
// or 0 if the key is absent or cannot be parsed.
func propUint64(data, key string) uint64 {
	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && k == key {
			if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}
