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

package specutils

import (
	"slices"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/gpusched"
	"gvisor.dev/gvisor/runsc/config"
)

// nvidiaDevice returns the device node entry a CRI runtime injects for a GPU.
func nvidiaDevice(minor int64) specs.LinuxDevice {
	return specs.LinuxDevice{
		Path:  "/dev/nvidia" + string(rune('0'+minor)),
		Type:  "c",
		Major: nvgpu.NV_MAJOR_DEVICE_NUMBER,
		Minor: minor,
	}
}

// visibleDevicesSpec returns a spec whose GPUs were allocated through
// NVIDIA_VISIBLE_DEVICES, as a device plugin or Docker's --gpus does.
func visibleDevicesSpec(value string) *specs.Spec {
	return &specs.Spec{
		Process: &specs.Process{Env: []string{"NVIDIA_VISIBLE_DEVICES=" + value}},
	}
}

func TestNvidiaDeviceNames(t *testing.T) {
	conf := &config.Config{NVProxyDocker: true}
	for _, test := range []struct {
		name string
		spec *specs.Spec
		want []string
	}{
		{
			// What HAMi's device plugin allocates.
			name: "one device by uuid",
			spec: visibleDevicesSpec("GPU-0b7a7424-d911-bb25-850c-6feddbc332d1"),
			want: []string{"GPU-0b7a7424-d911-bb25-850c-6feddbc332d1"},
		},
		{
			name: "several devices by uuid",
			spec: visibleDevicesSpec("GPU-aaaa,GPU-bbbb"),
			want: []string{"GPU-aaaa", "GPU-bbbb"},
		},
		{
			// What Docker's --gpus takes.
			name: "devices by index",
			spec: visibleDevicesSpec("0,1"),
			want: []string{"0", "1"},
		},
		{
			name: "whitespace around the names",
			spec: visibleDevicesSpec("0, 1"),
			want: []string{"0", "1"},
		},
		{
			name: "every device",
			spec: visibleDevicesSpec("all"),
			want: []string{gpusched.AllDevices},
		},
		{
			// "none" is driver functionality without a GPU, and "void" is
			// neither; neither names a device to be scheduled on.
			name: "no device",
			spec: visibleDevicesSpec("none"),
			want: nil,
		},
		{
			name: "no gpu at all",
			spec: visibleDevicesSpec("void"),
			want: nil,
		},
		{
			// What a CRI runtime injects, which names nothing in the
			// environment.
			name: "device nodes",
			spec: &specs.Spec{Linux: &specs.Linux{Devices: []specs.LinuxDevice{
				nvidiaDevice(0),
				nvidiaDevice(3),
			}}},
			want: []string{gpusched.MinorName(0), gpusched.MinorName(3)},
		},
		{
			// Device nodes other than a GPU's are not devices to schedule on.
			name: "the control device is not a gpu",
			spec: &specs.Spec{Linux: &specs.Linux{Devices: []specs.LinuxDevice{
				{Path: "/dev/nvidiactl", Major: nvgpu.NV_MAJOR_DEVICE_NUMBER, Minor: 255},
				nvidiaDevice(1),
			}}},
			want: []string{gpusched.MinorName(1)},
		},
		{
			// A node whose minor does not match its name is not one of these.
			name: "a mismatched device node",
			spec: &specs.Spec{Linux: &specs.Linux{Devices: []specs.LinuxDevice{
				{Path: "/dev/nvidia0", Major: nvgpu.NV_MAJOR_DEVICE_NUMBER, Minor: 7},
			}}},
			want: nil,
		},
		{
			// Both sources describe the same allocation, and the scheduler
			// resolves each name to the device it refers to.
			name: "both sources",
			spec: &specs.Spec{
				Process: &specs.Process{Env: []string{"NVIDIA_VISIBLE_DEVICES=GPU-aaaa"}},
				Linux:   &specs.Linux{Devices: []specs.LinuxDevice{nvidiaDevice(2)}},
			},
			want: []string{gpusched.MinorName(2), "GPU-aaaa"},
		},
		{
			name: "nothing requested",
			spec: &specs.Spec{Process: &specs.Process{}},
			want: nil,
		},
		{
			name: "an empty spec",
			spec: &specs.Spec{},
			want: nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := NvidiaDeviceNames(test.spec, conf); !slices.Equal(got, test.want) {
				t.Errorf("NvidiaDeviceNames() = %v, want %v", got, test.want)
			}
		})
	}
}

// TestNvidiaDeviceNamesDeduplicates tests that a GPU named twice is reported
// once, so that it is not treated as two devices competing with each other.
func TestNvidiaDeviceNamesDeduplicates(t *testing.T) {
	spec := visibleDevicesSpec("GPU-aaaa,GPU-aaaa")
	got := NvidiaDeviceNames(spec, &config.Config{NVProxyDocker: true})
	if want := []string{"GPU-aaaa"}; !slices.Equal(got, want) {
		t.Errorf("NvidiaDeviceNames() = %v, want %v", got, want)
	}
}
