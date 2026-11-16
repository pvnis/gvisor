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

package amdsysfs

import (
	"fmt"
	"testing"
)

func link(from, to, typ int) *Dir {
	return &Dir{Files: map[string]string{
		"properties": fmt.Sprintf("type %d\nversion_major 0\nnode_from %d\nnode_to %d\nweight 20\n", typ, from, to),
	}}
}

func node(simd, minor int, links ...*Dir) *Dir {
	io := &Dir{Dirs: map[string]*Dir{}}
	for i, l := range links {
		io.Dirs[fmt.Sprint(i)] = l
	}
	return &Dir{
		Files: map[string]string{
			"properties": fmt.Sprintf("cpu_cores_count 0\nsimd_count %d\nio_links_count %d\np2p_links_count 0\ndrm_render_minor %d\n", simd, len(links), minor),
			"gpu_id":     fmt.Sprint(minor),
		},
		Dirs: map[string]*Dir{"io_links": io, "p2p_links": {}},
	}
}

// twoGPUHost is the topology of a host with one CPU node and two GPUs joined
// by XGMI (link type 11), as KFD reports a pair of MI355X.
func twoGPUHost() *Snapshot {
	return &Snapshot{
		Topology: &Dir{Dirs: map[string]*Dir{"nodes": {Dirs: map[string]*Dir{
			"0": node(0, 0, link(0, 1, 2), link(0, 2, 2)),
			"1": node(1024, 128, link(1, 0, 2), link(1, 2, 11)),
			"2": node(1024, 136, link(2, 0, 2), link(2, 1, 11)),
		}}}},
		PCI: []PCIDir{
			{Path: "pci0000:00"},
			{Path: "pci0000:00/0000:00:05.0"},
			{Path: "pci0000:00/0000:00:06.0"},
		},
		DRM: []DRMNode{
			{Name: "card0", Major: 226, Minor: 0, PCIPath: "pci0000:00/0000:00:05.0"},
			{Name: "card8", Major: 226, Minor: 8, PCIPath: "pci0000:00/0000:00:06.0"},
			{Name: "renderD128", Major: 226, Minor: 128, PCIPath: "pci0000:00/0000:00:05.0"},
			{Name: "renderD136", Major: 226, Minor: 136, PCIPath: "pci0000:00/0000:00:06.0"},
		},
	}
}

func nodesOf(s *Snapshot) map[string]*Dir { return s.Topology.Dirs["nodes"].Dirs }

func checkLinks(t *testing.T, n *Dir, want [][2]uint64) {
	t.Helper()
	links := n.Dirs["io_links"].Dirs
	if len(links) != len(want) {
		t.Fatalf("got %d io_links, want %d", len(links), len(want))
	}
	if got := propUint64(n.Files["properties"], "io_links_count"); got != uint64(len(want)) {
		t.Errorf("io_links_count = %d, want %d", got, len(want))
	}
	for i, w := range want {
		p := links[fmt.Sprint(i)].Files["properties"]
		if f, to := propUint64(p, "node_from"), propUint64(p, "node_to"); f != w[0] || to != w[1] {
			t.Errorf("io_links/%d = %d->%d, want %d->%d", i, f, to, w[0], w[1])
		}
	}
}

func TestRestrictToSecondGPU(t *testing.T) {
	s := twoGPUHost()
	if err := s.RestrictToRenderNodes(map[uint32]bool{136: true}); err != nil {
		t.Fatal(err)
	}
	nodes := nodesOf(s)
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 (CPU + one GPU)", len(nodes))
	}
	// The GPU that was node 2 is now node 1, with no gap for the thunk to
	// stop at.
	if got := propUint64(nodes["1"].Files["properties"], "drm_render_minor"); got != 136 {
		t.Errorf("node 1 drm_render_minor = %d, want 136", got)
	}
	// The CPU keeps only its link to the remaining GPU; the GPU keeps its
	// link to the CPU and loses the XGMI link to the dropped GPU.
	checkLinks(t, nodes["0"], [][2]uint64{{0, 1}})
	checkLinks(t, nodes["1"], [][2]uint64{{1, 0}})

	var drm []string
	for _, n := range s.DRM {
		drm = append(drm, n.Name)
	}
	if fmt.Sprint(drm) != "[card8 renderD136]" {
		t.Errorf("DRM nodes = %v, want [card8 renderD136]", drm)
	}
	var pci []string
	for _, d := range s.PCI {
		pci = append(pci, d.Path)
	}
	if fmt.Sprint(pci) != "[pci0000:00 pci0000:00/0000:00:06.0]" {
		t.Errorf("PCI dirs = %v", pci)
	}
}

func TestRestrictToBothGPUsIsIdentity(t *testing.T) {
	s := twoGPUHost()
	if err := s.RestrictToRenderNodes(map[uint32]bool{128: true, 136: true}); err != nil {
		t.Fatal(err)
	}
	nodes := nodesOf(s)
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want 3", len(nodes))
	}
	checkLinks(t, nodes["1"], [][2]uint64{{1, 0}, {1, 2}})
	checkLinks(t, nodes["2"], [][2]uint64{{2, 0}, {2, 1}})
	if len(s.DRM) != 4 || len(s.PCI) != 3 {
		t.Errorf("got %d DRM nodes and %d PCI dirs, want 4 and 3", len(s.DRM), len(s.PCI))
	}
}

func TestRestrictToNoGPU(t *testing.T) {
	s := twoGPUHost()
	if err := s.RestrictToRenderNodes(map[uint32]bool{}); err != nil {
		t.Fatal(err)
	}
	nodes := nodesOf(s)
	if len(nodes) != 1 || nodes["0"] == nil {
		t.Fatalf("got nodes %v, want only the CPU node 0", nodes)
	}
	checkLinks(t, nodes["0"], nil)
	if len(s.DRM) != 0 || len(s.PCI) != 0 {
		t.Errorf("got %d DRM nodes and %d PCI dirs, want none", len(s.DRM), len(s.PCI))
	}
}

func TestRestrictRejectsMalformedNodes(t *testing.T) {
	s := twoGPUHost()
	nodesOf(s)["x"] = &Dir{}
	if err := s.RestrictToRenderNodes(map[uint32]bool{128: true}); err == nil {
		t.Error("want an error for a non-numeric node directory")
	}
}

func TestForRenderNodesLeavesSharedSnapshot(t *testing.T) {
	s := twoGPUHost()
	c, err := s.ForRenderNodes(map[uint32]bool{128: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(nodesOf(c)); got != 2 {
		t.Errorf("copy has %d nodes, want 2", got)
	}
	if got := len(nodesOf(s)); got != 3 {
		t.Errorf("shared snapshot has %d nodes after restricting a copy, want 3", got)
	}
	checkLinks(t, nodesOf(s)["2"], [][2]uint64{{2, 0}, {2, 1}})
}

func TestRenderMinorGPUIDs(t *testing.T) {
	got := twoGPUHost().RenderMinorGPUIDs()
	if len(got) != 2 || got[128] != 128 || got[136] != 136 {
		t.Errorf("got %v, want the two GPUs keyed by render minor (the CPU node has none)", got)
	}
}
