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
	"gvisor.dev/gvisor/pkg/sentry/devices/nvproxy/nvconf"
)

const cilpSet = nvgpu.NV2080_CTRL_GR_SET_CTXSW_PREEMPTION_MODE_FLAGS_CILP_SET

// TestMinComputePreemptionUnsetIsUnrestricted tests that a sandbox with no
// minimum configured is not subject to one, so that the limit is opt-in and
// existing sandboxes are unaffected.
func TestMinComputePreemptionUnsetIsUnrestricted(t *testing.T) {
	nvp := &nvproxy{}
	if nvp.minComputePreemption != nvconf.ComputePreemptionWFI {
		t.Errorf("minComputePreemption = %s by default, want wfi (unrestricted)", nvp.minComputePreemption)
	}
}

// TestComputePreemptionDenied tests the policy that decides whether a sandbox
// may make itself less preemptible than the host requires.
func TestComputePreemptionDenied(t *testing.T) {
	for _, test := range []struct {
		name     string
		flags    uint32
		mode     uint32
		min      nvconf.ComputePreemption
		wantDeny bool
	}{
		{
			// The attack: a sandbox picks the mode under which the GPU cannot
			// take work away from it until that work finishes on its own.
			name:     "wfi refused when cta required",
			flags:    cilpSet,
			mode:     nvgpu.NV2080_CTRL_SET_CTXSW_PREEMPTION_MODE_COMPUTE_WFI,
			min:      nvconf.ComputePreemptionCTA,
			wantDeny: true,
		},
		{
			name:     "cta allowed when cta required",
			flags:    cilpSet,
			mode:     nvgpu.NV2080_CTRL_SET_CTXSW_PREEMPTION_MODE_COMPUTE_CTA,
			min:      nvconf.ComputePreemptionCTA,
			wantDeny: false,
		},
		{
			// More preemptible than required is always fine.
			name:     "cilp allowed when cta required",
			flags:    cilpSet,
			mode:     nvgpu.NV2080_CTRL_SET_CTXSW_PREEMPTION_MODE_COMPUTE_CILP,
			min:      nvconf.ComputePreemptionCTA,
			wantDeny: false,
		},
		{
			name:     "cta refused when cilp required",
			flags:    cilpSet,
			mode:     nvgpu.NV2080_CTRL_SET_CTXSW_PREEMPTION_MODE_COMPUTE_CTA,
			min:      nvconf.ComputePreemptionCILP,
			wantDeny: true,
		},
		{
			// Without the flag bit the driver ignores the mode field, so a WFI
			// value there is not a request for WFI and must not be refused --
			// refusing it would break callers that only set the graphics mode.
			name:     "wfi value ignored without its flag bit",
			flags:    nvgpu.NV2080_CTRL_GR_SET_CTXSW_PREEMPTION_MODE_FLAGS_GFXP_SET,
			mode:     nvgpu.NV2080_CTRL_SET_CTXSW_PREEMPTION_MODE_COMPUTE_WFI,
			min:      nvconf.ComputePreemptionCILP,
			wantDeny: false,
		},
		{
			name:     "no minimum permits wfi",
			flags:    cilpSet,
			mode:     nvgpu.NV2080_CTRL_SET_CTXSW_PREEMPTION_MODE_COMPUTE_WFI,
			min:      nvconf.ComputePreemptionWFI,
			wantDeny: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			mode, deny := computePreemptionDenied(test.flags, test.mode, test.min)
			if deny != test.wantDeny {
				t.Errorf("computePreemptionDenied(%#x, %d, %s) deny = %t, want %t",
					test.flags, test.mode, test.min, deny, test.wantDeny)
			}
			if got := nvconf.ComputePreemption(test.mode); mode != got {
				t.Errorf("returned mode = %s, want %s", mode, got)
			}
		})
	}
}

// TestSetCtxswPreemptionModeParamsSize tests that the params struct matches the
// driver's layout. A mismatch would make the handler reject every call with
// EINVAL, silently disabling GPU preemption configuration rather than limiting
// it.
func TestSetCtxswPreemptionModeParamsSize(t *testing.T) {
	var p nvgpu.NV2080_CTRL_GR_SET_CTXSW_PREEMPTION_MODE_PARAMS
	// flags + hChannel + gfxpPreemptMode + cilpPreemptMode = 16, then
	// NV2080_CTRL_GR_ROUTE_INFO {flags, pad, route} = 16.
	if got, want := p.SizeBytes(), 32; got != want {
		t.Errorf("NV2080_CTRL_GR_SET_CTXSW_PREEMPTION_MODE_PARAMS.SizeBytes() = %d, want %d", got, want)
	}
}

// TestPreemptParamsSize tests the layout of the preempt control's parameters.
// The Sentry builds these itself rather than copying them from the sandbox, so
// a wrong size means the driver reads the wrong bytes.
func TestPreemptParamsSize(t *testing.T) {
	var p nvgpu.NVA06C_CTRL_PREEMPT_PARAMS
	// NvBool bWait + NvBool bManualTimeout, padded to the alignment of the
	// NvU32 timeoutUs that follows.
	if got, want := p.SizeBytes(), 8; got != want {
		t.Errorf("NVA06C_CTRL_PREEMPT_PARAMS.SizeBytes() = %d, want %d", got, want)
	}
}

// TestPreemptTimeoutWithinDriverMaximum tests that the timeout nvproxy asks for
// is one the driver will accept, and that it is short enough to be affordable
// within a scheduling period.
func TestPreemptTimeoutWithinDriverMaximum(t *testing.T) {
	if got := preemptChannelGroupTimeout.Microseconds(); got > nvgpu.NVA06C_CTRL_CMD_PREEMPT_MAX_MANUAL_TIMEOUT_US {
		t.Errorf("preemptChannelGroupTimeout = %d us, exceeds the driver maximum of %d us",
			got, nvgpu.NVA06C_CTRL_CMD_PREEMPT_MAX_MANUAL_TIMEOUT_US)
	}
	if preemptChannelGroupTimeout >= computeGatePeriod {
		t.Errorf("preemptChannelGroupTimeout = %v, which is not shorter than the %v period it must fit inside",
			preemptChannelGroupTimeout, computeGatePeriod)
	}
}

// TestPreemptDisabledByDefault tests that preemption is opt-in, so that
// enabling the compute gate does not silently start evicting work.
func TestPreemptDisabledByDefault(t *testing.T) {
	g := newGate(50)
	if g.preempt {
		t.Errorf("preempt = true by default, want false")
	}
	// With preemption off, channel groups are not tracked at all: there is
	// nothing to preempt, so retaining the handles would be pure bookkeeping.
	g.addChannelGroup(&frontendFD{}, nvgpu.Handle{Val: 1}, nvgpu.Handle{Val: 2})
	if got := len(g.tsgs); got != 0 {
		t.Errorf("tracked %d channel groups with preemption disabled, want 0", got)
	}
}

// TestChannelGroupTracking tests that channel groups are remembered while they
// exist and forgotten when they do not.
//
// Forgetting matters for correctness rather than tidiness: the driver reuses
// handles, so a stale record would have nvproxy preempting whatever object
// later inherits the handle.
func TestChannelGroupTracking(t *testing.T) {
	g := &computeGate{}
	g.init(50 /* percent */, false /* scheduled */, true /* preempt */, false /* unschedule */)

	fd1, fd2 := &frontendFD{}, &frontendFD{}
	hClient := nvgpu.Handle{Val: 0xc1}
	hA, hB := nvgpu.Handle{Val: 0xa}, nvgpu.Handle{Val: 0xb}

	g.addChannelGroup(fd1, hClient, hA)
	g.addChannelGroup(fd2, hClient, hB)
	if got, want := len(g.tsgs), 2; got != want {
		t.Fatalf("tracked %d channel groups, want %d", got, want)
	}
	if got := g.tsgs[hA].fd; got != fd1 {
		t.Errorf("channel group %v is reached through the wrong file description", hA)
	}

	// Freeing one leaves the other.
	g.removeChannelGroup(hA)
	if _, ok := g.tsgs[hA]; ok {
		t.Errorf("channel group %v still tracked after being freed", hA)
	}
	if _, ok := g.tsgs[hB]; !ok {
		t.Errorf("channel group %v dropped by an unrelated free", hB)
	}

	// Releasing a file description drops the groups reached through it, since
	// its host FD is about to become invalid.
	g.forget(fd2)
	if got, want := len(g.tsgs), 0; got != want {
		t.Errorf("tracked %d channel groups after releasing their file description, want %d", got, want)
	}
}
