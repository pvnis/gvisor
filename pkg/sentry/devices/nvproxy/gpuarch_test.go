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

package nvproxy

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/nvgpu"
)

func TestArchFromClass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		class nvgpu.ClassID
		want  gpuArch
		ok    bool
	}{
		// Compute classes are authoritative.
		{"turing-compute", nvgpu.TURING_COMPUTE_A, archTuring, true},
		{"ampere-compute-a", nvgpu.AMPERE_COMPUTE_A, archAmpere, true},
		{"ampere-compute-b", nvgpu.AMPERE_COMPUTE_B, archAmpere, true},
		{"ada-compute", nvgpu.ADA_COMPUTE_A, archAda, true},
		{"hopper-compute", nvgpu.HOPPER_COMPUTE_A, archHopper, true},
		{"blackwell-compute-a", nvgpu.BLACKWELL_COMPUTE_A, archBlackwell, true},
		{"blackwell-compute-b", nvgpu.BLACKWELL_COMPUTE_B, archBlackwell, true},
		// Channel classes are the fallback. Note Ada is measured as Ampere here
		// because they share AMPERE_CHANNEL_GPFIFO_A; a compute alloc
		// disambiguates.
		{"turing-channel", nvgpu.TURING_CHANNEL_GPFIFO_A, archTuring, true},
		{"ampere-channel", nvgpu.AMPERE_CHANNEL_GPFIFO_A, archAmpere, true},
		{"hopper-channel", nvgpu.HOPPER_CHANNEL_GPFIFO_A, archHopper, true},
		{"blackwell-channel-a", nvgpu.BLACKWELL_CHANNEL_GPFIFO_A, archBlackwell, true},
		{"blackwell-channel-b", nvgpu.BLACKWELL_CHANNEL_GPFIFO_B, archBlackwell, true},
		// A USERMODE class must NOT identify a generation: NVIDIA reuses it, so a
		// Blackwell GPU allocates HOPPER_USERMODE_A. Relying on it would misread
		// the architecture, which is the bug this whole mechanism avoids.
		{"hopper-usermode-unreliable", nvgpu.HOPPER_USERMODE_A, archUnknown, false},
		{"blackwell-usermode-unreliable", nvgpu.BLACKWELL_USERMODE_A, archUnknown, false},
		// Unrelated classes.
		{"channel-group", nvgpu.KEPLER_CHANNEL_GROUP_A, archUnknown, false},
		{"root", nvgpu.NV01_ROOT, archUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := archFromClass(tc.class)
			if got != tc.want || ok != tc.ok {
				t.Errorf("archFromClass(%#x) = (%v, %v), want (%v, %v)", tc.class, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSubmitsByDoorbell(t *testing.T) {
	// Every architecture nvproxy resolves from a class is Volta or later, so all
	// of them submit by doorbell; this is why the compute gate's fault-based
	// enforcement is best-effort on all currently-supported GPUs.
	for _, a := range []gpuArch{archTuring, archAmpere, archAda, archHopper, archBlackwell} {
		if !a.submitsByDoorbell() {
			t.Errorf("%v.submitsByDoorbell() = false, want true", a)
		}
	}
	// Pre-Volta and unknown do not (unknown conservatively does not claim the
	// doorbell property).
	for _, a := range []gpuArch{archUnknown, archKepler, archMaxwell, archPascal} {
		if a.submitsByDoorbell() {
			t.Errorf("%v.submitsByDoorbell() = true, want false", a)
		}
	}
}
