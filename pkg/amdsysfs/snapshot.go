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
	"path/filepath"
	"regexp"
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

// Snapshot is the host sysfs state needed to discover AMD GPUs.
type Snapshot struct {
	// Topology is /sys/devices/virtual/kfd/kfd/topology.
	Topology *Dir `json:"topology,omitempty"`
	// Module is /sys/module/amdgpu.
	Module *Dir `json:"module,omitempty"`
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
	return snap, nil
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
