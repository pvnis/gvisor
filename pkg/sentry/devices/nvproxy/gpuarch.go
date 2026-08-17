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

import "gvisor.dev/gvisor/pkg/abi/nvgpu"

// gpuArch is the NVIDIA GPU architecture generation nvproxy is running on.
//
// nvproxy serves many GPU generations through a single driver ABI -- one driver
// version spans Turing through Blackwell -- and their behaviour differs in ways
// nvproxy must account for, the work-submission model most of all. The ABI
// tables are keyed on driver version, which does not identify the GPU, and a
// sandbox never tells nvproxy which GPU it was given. So the architecture is
// detected at runtime from the classes the application allocates.
//
// The values are ordered oldest-to-newest so that generational predicates like
// submitsByDoorbell can be written as comparisons.
type gpuArch int32

const (
	archUnknown gpuArch = iota
	archKepler
	archMaxwell
	archPascal
	archVolta
	archTuring
	archAmpere
	archAda
	archHopper
	archBlackwell
)

// String returns the architecture's name.
func (a gpuArch) String() string {
	switch a {
	case archKepler:
		return "Kepler"
	case archMaxwell:
		return "Maxwell"
	case archPascal:
		return "Pascal"
	case archVolta:
		return "Volta"
	case archTuring:
		return "Turing"
	case archAmpere:
		return "Ampere"
	case archAda:
		return "Ada"
	case archHopper:
		return "Hopper"
	case archBlackwell:
		return "Blackwell"
	default:
		return "unknown"
	}
}

// submitsByDoorbell reports whether work is submitted by ringing a USERMODE
// doorbell on a persistent GPFIFO, which every architecture from Volta onward
// does.
//
// This is the property that decides whether the compute gate can enforce a
// limit: a doorbell ring writes device MMIO that the Sentry never faults on, so
// on these GPUs the gate cannot observe -- let alone bound -- a workload that
// submits without also rewriting a command buffer (measured with cuBLAS on
// Blackwell; see SECURITY-FINDINGS.md). Pre-Volta architectures submit through a
// path the gate can see, which is the regime the gate was designed for.
func (a gpuArch) submitsByDoorbell() bool {
	return a >= archVolta
}

// archFromClass maps a compute or channel-GPFIFO class to the architecture that
// introduced it, returning false for a class that identifies neither.
//
// The compute class is authoritative and is tried first: the channel-GPFIFO
// class cannot tell Ampere from Ada, which share AMPERE_CHANNEL_GPFIFO_A, and a
// USERMODE class is useless for this because NVIDIA reuses it across
// generations (a Blackwell GPU allocates HOPPER_USERMODE_A). These are the only
// classes whose number reliably encodes the generation.
func archFromClass(c nvgpu.ClassID) (gpuArch, bool) {
	switch c {
	case nvgpu.TURING_COMPUTE_A:
		return archTuring, true
	case nvgpu.AMPERE_COMPUTE_A, nvgpu.AMPERE_COMPUTE_B:
		return archAmpere, true
	case nvgpu.ADA_COMPUTE_A:
		return archAda, true
	case nvgpu.HOPPER_COMPUTE_A:
		return archHopper, true
	case nvgpu.BLACKWELL_COMPUTE_A, nvgpu.BLACKWELL_COMPUTE_B:
		return archBlackwell, true
	}
	// Fall back to the channel-GPFIFO class, which is present even for a
	// workload that never allocates a compute object. Ampere and Ada share a
	// class here; a workload doing compute will have resolved the ambiguity
	// above before this matters.
	switch c {
	case nvgpu.TURING_CHANNEL_GPFIFO_A:
		return archTuring, true
	case nvgpu.AMPERE_CHANNEL_GPFIFO_A:
		return archAmpere, true
	case nvgpu.HOPPER_CHANNEL_GPFIFO_A:
		return archHopper, true
	case nvgpu.BLACKWELL_CHANNEL_GPFIFO_A, nvgpu.BLACKWELL_CHANNEL_GPFIFO_B:
		return archBlackwell, true
	}
	return archUnknown, false
}
