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
	"sort"

	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
)

// gpuAllowlist is the set of GPUs the sandbox's containers were given.
//
// KFD keeps one device entry per GPU in the host for every KFD process, not
// only for the GPUs that process may use, and reports all of them through
// GET_PROCESS_APERTURES_NEW. ROCm's thunk binds to each GPU it is told about,
// and the driver refuses the ones outside the sandbox's device cgroup, after
// which the thunk discards every GPU: on a multi-GPU host a sandbox given one
// GPU would see none. The sysfs topology is already restricted to the
// container's GPUs (amdsysfs.Snapshot.ForRenderNodes); this restricts the
// ioctl that would otherwise contradict it.
//
// It is the union over the sandbox's containers, because they share the
// sandbox's single KFD context and the Sentry's device cgroup is the pod's.
//
// +stateify savable
type gpuAllowlist struct {
	mu sync.Mutex `state:"nosave"`

	// byMinor maps a render node minor to its GPU's gpu_id. Immutable after
	// Register.
	byMinor map[uint32]uint32

	// allowed holds the permitted gpu_ids. It is nil until a container
	// declares its GPUs, and until then nothing is filtered, which keeps the
	// previous behaviour for a sandbox whose devices are never declared.
	allowed map[uint32]struct{}
}

// allowRenderMinors adds the GPUs owning the given render node minors.
// Minors that are not a known GPU's render node are ignored.
func (g *gpuAllowlist) allowRenderMinors(minors map[uint32]bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.allowed == nil {
		g.allowed = make(map[uint32]struct{})
	}
	var ids []string
	for m := range minors {
		if id, ok := g.byMinor[m]; ok {
			g.allowed[id] = struct{}{}
			ids = append(ids, fmt.Sprintf("%#x (renderD%d)", id, m))
		}
	}
	sort.Strings(ids)
	log.Infof("amdproxy: container given GPUs %v; the sandbox may now use %d of the host's %d", ids, len(g.allowed), len(g.byMinor))
}

// filterApertures returns the entries for permitted GPUs, in their original
// order. It reuses aps's backing array.
func (g *gpuAllowlist) filterApertures(aps []amdgpu.KFDProcessDeviceApertures) []amdgpu.KFDProcessDeviceApertures {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.allowed == nil {
		return aps
	}
	out := aps[:0]
	for _, ap := range aps {
		if _, ok := g.allowed[ap.GPUID]; ok {
			out = append(out, ap)
		}
	}
	return out
}

// gpuOfMinor returns the gpu_id of the GPU whose render node has the given
// minor number.
func (g *gpuAllowlist) gpuOfMinor(minor uint32) (uint32, bool) {
	id, ok := g.byMinor[minor]
	return id, ok
}

// AllowRenderNodes permits the sandbox to use the GPUs owning the given
// render node minors. It is called for each container as its devices are
// created. It changes the sandbox's amdproxy state, not the DeviceInfo.
func (d *DeviceInfo) AllowRenderNodes(minors map[uint32]bool) {
	if d != nil && d.gpus != nil {
		d.gpus.allowRenderMinors(minors)
	}
}
