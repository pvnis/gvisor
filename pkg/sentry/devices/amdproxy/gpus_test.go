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
	"testing"

	"gvisor.dev/gvisor/pkg/abi/amdgpu"
)

// The gpu_ids and render minors of the two MI355X the filter was written for.
const (
	gpuA = 0xa657 // renderD128
	gpuB = 0x5717 // renderD136
)

// hostApertures is the driver's reply: every GPU in the host, in its order,
// which is not the topology's.
func hostApertures() []amdgpu.KFDProcessDeviceApertures {
	return []amdgpu.KFDProcessDeviceApertures{{GPUID: gpuB}, {GPUID: gpuA}}
}

func ids(aps []amdgpu.KFDProcessDeviceApertures) []uint32 {
	var out []uint32
	for _, ap := range aps {
		out = append(out, ap.GPUID)
	}
	return out
}

func newAllowlist() *gpuAllowlist {
	return &gpuAllowlist{byMinor: map[uint32]uint32{128: gpuA, 136: gpuB}}
}

func TestAperturesUnfilteredUntilDeclared(t *testing.T) {
	if got := ids(newAllowlist().filterApertures(hostApertures())); len(got) != 2 {
		t.Errorf("got %x, want both GPUs before any container declares its own", got)
	}
}

func TestAperturesFilteredToContainerGPU(t *testing.T) {
	g := newAllowlist()
	g.allowRenderMinors(map[uint32]bool{128: true})
	got := ids(g.filterApertures(hostApertures()))
	if len(got) != 1 || got[0] != gpuA {
		t.Errorf("got %x, want only %x", got, gpuA)
	}
}

func TestAperturesUnionOfContainers(t *testing.T) {
	g := newAllowlist()
	g.allowRenderMinors(map[uint32]bool{136: true})
	g.allowRenderMinors(map[uint32]bool{128: true})
	got := ids(g.filterApertures(hostApertures()))
	if len(got) != 2 || got[0] != gpuB || got[1] != gpuA {
		t.Errorf("got %x, want both in the driver's order", got)
	}
}

func TestAperturesNoGPUDeclared(t *testing.T) {
	g := newAllowlist()
	// A container with /dev/kfd but no render node, or only nodes that are
	// not a GPU's, is given no GPU.
	g.allowRenderMinors(map[uint32]bool{200: true})
	if got := g.filterApertures(hostApertures()); len(got) != 0 {
		t.Errorf("got %x, want none", ids(got))
	}
}

func TestAllowRenderNodesNilSafe(t *testing.T) {
	var d *DeviceInfo
	d.AllowRenderNodes(map[uint32]bool{128: true})
	(&DeviceInfo{}).AllowRenderNodes(map[uint32]bool{128: true})
}
