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

// Package amdsysfs snapshots the host sysfs state that ROCm reads to
// discover AMD GPUs.
//
// The ROCm runtime does not learn about a GPU from ioctls. Before it opens
// /dev/kfd at all it reads the Kernel Fusion Driver's topology under
// /sys/devices/virtual/kfd/kfd/topology, and libdrm separately identifies a
// render node by walking sysfs. A sandbox whose sysfs lacks these sees no
// GPU, whatever its device files say.
//
// The topology is a tree of small text files, so it is snapshotted verbatim
// rather than modelled. It is collected on the host, before the sandbox is
// chrooted, and rebuilt inside the sandbox's synthetic sysfs.
package amdsysfs

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Path is where the snapshot is stored inside the chroot, for the sandbox
// process to read back after it has lost access to the host's sysfs.
const Path = "/sys-amdgpu-snapshot.json"

// TopologyPath is the host directory holding the KFD topology.
const TopologyPath = "devices/virtual/kfd/kfd/topology"

// ModulePath is the host directory describing the loaded amdgpu module. The
// ROCm runtime reads initstate from here to decide whether the driver is
// present, before it looks at anything else.
const ModulePath = "module/amdgpu"

// ClassDRMPath is the host directory of DRM device symlinks, from which the
// render nodes and their positions in the PCI hierarchy are discovered.
const ClassDRMPath = "class/drm"

// devicesPrefix is what a /sys/class symlink target starts with before the
// path relative to /sys/devices.
const devicesPrefix = "../../devices/"

// drmNodeRE matches a DRM node name: a render node, or the primary "card"
// node beside it.
//
// The card node is snapshotted for *enumeration* only. ROCr finds the GPU
// through KFD and needs nothing but the render node, which is why this used to
// match renderD<N> alone — but the AMD SMI library, and so every framework
// that uses it to detect a ROCm platform, globs /sys/class/drm for card<N>
// instead. Without one it reports AMDSMI_STATUS_NOT_INIT and vLLM concludes
// there is no GPU, on a sandbox where HIP kernels run perfectly well.
//
// Nothing needs /dev/dri/card<N> to be present in the container; only the
// sysfs entry is required. The anchors matter: connector directories such as
// "card1-DP-1" must not match, since they live under the card node rather
// than under devices/<pci path>/drm.
var drmNodeRE = regexp.MustCompile(`^(renderD|card)([0-9]+)$`)

// pciAttrs are the PCI attributes reproduced for a device and its ancestor
// bridges. This is an allowlist rather than everything in the directory:
// most of what a PCI directory holds is either irrelevant to identifying the
// device, or is a binary blob or a register window that reading has side
// effects on.
var pciAttrs = []string{
	"boot_vga",
	"class",
	"current_link_speed",
	"current_link_width",
	"device",
	"local_cpulist",
	"local_cpus",
	"max_link_speed",
	"max_link_width",
	"numa_node",
	"revision",
	"subsystem_device",
	"subsystem_vendor",
	"uevent",
	"vendor",
}

// Limits on what will be collected. The real topology is a few dozen small
// files per GPU; these bounds exist so that a surprising host cannot cause
// the sandbox to build an unbounded tree.
const (
	maxFileSize = 64 * 1024
	maxEntries  = 4096
	maxDepth    = 8
)

// safeName matches names that may be reproduced inside the sandbox or joined
// into host paths. Kernel-generated sysfs names satisfy this; anything else
// is dropped at collection time, so the construction side never sees a name
// containing a path separator or dot-dot.
var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:+-]*$`)

// SafeName reports whether name may be used as a sysfs entry name.
func SafeName(name string) bool {
	return safeName.MatchString(name) && !strings.Contains(name, "..")
}

// Dir is a snapshot of one sysfs directory.
type Dir struct {
	// Files maps a file's name to its contents.
	Files map[string]string `json:"files,omitempty"`
	// Dirs maps a subdirectory's name to its snapshot.
	Dirs map[string]*Dir `json:"dirs,omitempty"`
}

// PCIDir is a snapshot of one directory in the PCI hierarchy.
type PCIDir struct {
	// Path is the directory's path relative to /sys/devices, e.g.
	// "pci0000:bc/0000:bc:00.0".
	Path string `json:"path"`
	// Files maps an attribute's name to its contents.
	Files map[string]string `json:"files,omitempty"`
}

// DRMNode describes a DRM render node and where it sits in the PCI
// hierarchy.
type DRMNode struct {
	// Name is the node's directory name, e.g. "renderD128".
	Name string `json:"name"`
	// Major and Minor are the node's device numbers.
	Major uint32 `json:"major"`
	Minor uint32 `json:"minor"`
	// PCIPath is the owning PCI function's path relative to /sys/devices.
	PCIPath string `json:"pci_path"`
	// Files maps the node directory's attributes to their contents.
	Files map[string]string `json:"files,omitempty"`
}

// Snapshot is the host sysfs state needed to discover AMD GPUs.
type Snapshot struct {
	// Topology is /sys/devices/virtual/kfd/kfd/topology.
	Topology *Dir `json:"topology,omitempty"`
	// Module is /sys/module/amdgpu.
	Module *Dir `json:"module,omitempty"`
	// PCI holds every PCI directory in the closure: the GPU functions and
	// all their ancestor bridges and roots. Sorted by Path, which places
	// parents before children.
	PCI []PCIDir `json:"pci,omitempty"`
	// DRM holds the render nodes.
	DRM []DRMNode `json:"drm,omitempty"`
}

// Collect reads the KFD topology under sysRoot. It returns nil, nil if the
// host has no KFD topology, which is the ordinary case on a machine with no
// AMD GPU.
//
// TODO: The topology is currently reproduced whole, so a sandbox sees every
// GPU the host has rather than the ones assigned to it. Once a sandbox's
// GPUs are known here, the nodes should be filtered to those, and the
// per-node memory sizes reported against the sandbox's quota rather than the
// device's, for the same reason nvproxy rewrites its framebuffer-size query:
// otherwise the topology is an oracle for what the rest of the machine has
// and is doing.
func Collect(sysRoot string) (*Snapshot, error) {
	topoPath := filepath.Join(sysRoot, TopologyPath)
	fi, err := os.Stat(topoPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %q: %w", topoPath, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", topoPath)
	}
	budget := maxEntries
	topo, err := collectDir(topoPath, 0, &budget)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Topology: topo}
	// The module directory is expected to exist whenever the topology does,
	// but its absence is not worth failing over: the topology is what
	// describes the hardware.
	modPath := filepath.Join(sysRoot, ModulePath)
	if fi, err := os.Stat(modPath); err == nil && fi.IsDir() {
		if mod, err := collectDir(modPath, 0, &budget); err == nil {
			snap.Module = mod
		}
	}
	if err := snap.collectDRM(sysRoot); err != nil {
		return nil, err
	}
	return snap, nil
}

// collectDRM discovers the DRM nodes and the PCI directories leading to
// them. libdrm identifies a render node by resolving
// /sys/dev/char/<major>:<minor>/device/drm, so the node's position in the
// PCI hierarchy has to be reproduced, not just the node itself.
func (s *Snapshot) collectDRM(sysRoot string) error {
	classDRM := filepath.Join(sysRoot, ClassDRMPath)
	ents, err := os.ReadDir(classDRM)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %q: %w", classDRM, err)
	}
	pciPaths := make(map[string]bool)
	for _, ent := range ents {
		name := ent.Name()
		if !drmNodeRE.MatchString(name) || !SafeName(name) {
			continue
		}
		// The symlink target is the node's path relative to /sys/class, of
		// the form ../../devices/<pci path>/drm/<name>.
		target, err := os.Readlink(filepath.Join(classDRM, name))
		if err != nil {
			continue
		}
		rel, ok := strings.CutPrefix(target, devicesPrefix)
		if !ok {
			continue
		}
		// rel is "<pci path>/drm/<name>"; strip the trailing two components.
		pciPath := path.Dir(path.Dir(rel))
		if pciPath == "." || pciPath == "/" || !safePath(pciPath) {
			continue
		}
		nodeDir := filepath.Join(sysRoot, "devices", filepath.FromSlash(rel))
		files := collectAttrs(nodeDir, []string{"dev", "uevent"})
		major, minor, ok := parseDevFile(files["dev"])
		if !ok {
			continue
		}
		s.DRM = append(s.DRM, DRMNode{
			Name:    name,
			Major:   major,
			Minor:   minor,
			PCIPath: pciPath,
			Files:   files,
		})
		// Record the function and every ancestor up to the root complex.
		for p := pciPath; p != "." && p != "/"; p = path.Dir(p) {
			pciPaths[p] = true
		}
	}
	for p := range pciPaths {
		s.PCI = append(s.PCI, PCIDir{
			Path:  p,
			Files: collectAttrs(filepath.Join(sysRoot, "devices", filepath.FromSlash(p)), pciAttrs),
		})
	}
	// Sorting by path places parents before children, so construction can
	// create each directory knowing its parent exists.
	sort.Slice(s.PCI, func(i, j int) bool { return s.PCI[i].Path < s.PCI[j].Path })
	sort.Slice(s.DRM, func(i, j int) bool { return s.DRM[i].Name < s.DRM[j].Name })
	return nil
}

// safePath reports whether every component of a "/"-separated sysfs path is
// a plain sysfs name.
func safePath(p string) bool {
	for _, comp := range strings.Split(p, "/") {
		if !SafeName(comp) {
			return false
		}
	}
	return true
}

// collectAttrs reads the named attributes from dir, skipping any that are
// absent or unreadable.
func collectAttrs(dir string, names []string) map[string]string {
	var out map[string]string
	for _, name := range names {
		full := filepath.Join(dir, name)
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := readBounded(full)
		if err != nil {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[name] = data
	}
	return out
}

// parseDevFile parses a sysfs "dev" attribute, which holds "major:minor".
func parseDevFile(data string) (uint32, uint32, bool) {
	majStr, minStr, ok := strings.Cut(strings.TrimSpace(data), ":")
	if !ok {
		return 0, 0, false
	}
	major, err := strconv.ParseUint(majStr, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	minor, err := strconv.ParseUint(minStr, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	return uint32(major), uint32(minor), true
}

func collectDir(dirPath string, depth int, budget *int) (*Dir, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("sysfs tree deeper than %d at %q", maxDepth, dirPath)
	}
	ents, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", dirPath, err)
	}
	d := &Dir{}
	for _, ent := range ents {
		name := ent.Name()
		if !SafeName(name) {
			continue
		}
		if *budget <= 0 {
			return nil, fmt.Errorf("sysfs tree has more than %d entries", maxEntries)
		}
		*budget--
		full := filepath.Join(dirPath, name)
		// Do not follow symlinks: the topology's own contents are plain
		// files and directories, and anything else is not ours to reproduce.
		info, err := os.Lstat(full)
		if err != nil {
			continue
		}
		switch {
		case info.IsDir():
			sub, err := collectDir(full, depth+1, budget)
			if err != nil {
				return nil, err
			}
			if d.Dirs == nil {
				d.Dirs = make(map[string]*Dir)
			}
			d.Dirs[name] = sub
		case info.Mode().IsRegular():
			// sysfs reports size 0 for its attributes, so read and bound
			// rather than trusting the stat size.
			b, err := readBounded(full)
			if err != nil {
				// Some attributes fail to read depending on device state;
				// those are simply absent rather than fatal.
				continue
			}
			if d.Files == nil {
				d.Files = make(map[string]string)
			}
			d.Files[name] = b
		}
	}
	return d, nil
}

func readBounded(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, maxFileSize)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", err
	}
	return string(buf[:n]), nil
}

// Save serializes the snapshot to dst, creating parent directories.
func (s *Snapshot) Save(dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("creating %q: %w", filepath.Dir(dst), err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshaling AMD GPU sysfs snapshot: %w", err)
	}
	return os.WriteFile(dst, b, 0644)
}

// Load reads a snapshot written by Save. It returns nil, nil if src does not
// exist, which means no AMD GPU sysfs state was collected.
func Load(src string) (*Snapshot, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("unmarshaling AMD GPU sysfs snapshot: %w", err)
	}
	return &s, nil
}

// CUsPerComputeGroup returns the number of compute units the GPUs in this
// snapshot schedule as an indivisible group, or 0 if it cannot be determined.
//
// RDNA pairs compute units into workgroup processors, and KFD will not accept
// a queue CU mask that enables one half of a pair without the other: the
// SET_CU_MASK ioctl returns EINVAL. GCN and CDNA have no such pairing and take
// any mask. The distinction is worth making rather than assuming, because the
// consequence of getting it wrong is a container that appears to hang.
//
// gfx_target_version encodes the ISA as major*10000 + minor*100 + step, so
// gfx1101 reads 110001. Everything from gfx10 on is RDNA.
//
// When a node reports no usable version the node is skipped; when the GPUs
// disagree the largest group wins, since a mask has to satisfy every GPU it
// might be applied to.
func (s *Snapshot) CUsPerComputeGroup() int {
	if s == nil || s.Topology == nil {
		return 0
	}
	nodes := s.Topology.Dirs["nodes"]
	if nodes == nil {
		return 0
	}
	group := 0
	for _, node := range nodes.Dirs {
		if node == nil {
			return 0
		}
		props, ok := node.Files["properties"]
		if !ok {
			continue
		}
		// simd_count is 0 on CPU nodes, which have no compute units to mask.
		if propUint64(props, "simd_count") == 0 {
			continue
		}
		ver := propUint64(props, "gfx_target_version")
		if ver == 0 {
			continue
		}
		n := 1
		if ver/10000 >= 10 {
			n = 2
		}
		if n > group {
			group = n
		}
	}
	return group
}

// propUint64 returns the value of the named key in a sysfs properties string,
// or 0 if the key is absent or unparsable. Each line has the form "key value".
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
