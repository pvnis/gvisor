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

package nvconf

import "fmt"

// ComputePreemption is a GPU compute context-switch preemption mode: what the
// hardware does to work already running when it is told to switch a channel
// group off an engine.
//
// The values mirror NV2080_CTRL_SET_CTXSW_PREEMPTION_MODE_COMPUTE_* in the
// driver ABI, and are deliberately ordered by increasing preemptibility so that
// a configured minimum can be enforced by comparison.
type ComputePreemption uint32

// Possible values of ComputePreemption.
const (
	// ComputePreemptionWFI waits for the running work to finish on its own.
	// Nothing takes the GPU away from a sandbox before then, which is what
	// makes it the mode an adversarial sandbox would choose.
	ComputePreemptionWFI ComputePreemption = 0

	// ComputePreemptionCTA preempts at thread block boundaries.
	ComputePreemptionCTA ComputePreemption = 1

	// ComputePreemptionCILP preempts within an instruction. Finer than CTA,
	// but the context it must save is correspondingly larger.
	ComputePreemptionCILP ComputePreemption = 2
)

// String implements fmt.Stringer.String.
func (p ComputePreemption) String() string {
	switch p {
	case ComputePreemptionWFI:
		return "wfi"
	case ComputePreemptionCTA:
		return "cta"
	case ComputePreemptionCILP:
		return "cilp"
	default:
		return fmt.Sprintf("unknown(%d)", uint32(p))
	}
}

// ParseComputePreemption returns the ComputePreemption named by s. The empty
// string means WFI, which imposes no requirement, since WFI is already the
// least preemptible mode there is.
func ParseComputePreemption(s string) (ComputePreemption, error) {
	switch s {
	case "", "wfi":
		return ComputePreemptionWFI, nil
	case "cta":
		return ComputePreemptionCTA, nil
	case "cilp":
		return ComputePreemptionCILP, nil
	default:
		return ComputePreemptionWFI, fmt.Errorf("invalid compute preemption mode %q: want one of wfi, cta, cilp", s)
	}
}
