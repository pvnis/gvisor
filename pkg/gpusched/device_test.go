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

package gpusched

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// twoGPUs is the output of the nvidia-smi query for a host with two of them.
const twoGPUs = `0, GPU-0b7a7424-d911-bb25-850c-6feddbc332d1, 00000000:55:00.0
1, GPU-1c8b8535-ea22-cc36-961d-7fee0cd443e2, 00000000:af:00.0
`

func TestParseDeviceQuery(t *testing.T) {
	table := parseDeviceQuery(twoGPUs)
	if got, want := table.Devices(), 2; got != want {
		t.Fatalf("Devices() = %d, want %d", got, want)
	}
	for _, test := range []struct {
		name string
		want DeviceID
	}{
		{"0", 0},
		{"1", 1},
		{"GPU-0b7a7424-d911-bb25-850c-6feddbc332d1", 0},
		{"GPU-1c8b8535-ea22-cc36-961d-7fee0cd443e2", 1},
		// A UUID is written both ways, so matching must not depend on case.
		{"gpu-1c8b8535-ea22-cc36-961d-7fee0cd443e2", 1},
		// The bus ID as nvidia-smi reports it, and as the driver's procfs
		// names its directories.
		{"00000000:55:00.0", 0},
		{"0000:af:00.0", 1},
		// Nothing on this host.
		{"GPU-nope", AnyDevice},
		{"7", AnyDevice},
		{"", AnyDevice},
	} {
		if got := table.Lookup(test.name); got != test.want {
			t.Errorf("Lookup(%q) = %v, want %v", test.name, got, test.want)
		}
	}
}

// TestDeviceTableMinors tests that a device node is resolved to the GPU it
// reaches, which is the only name a CRI runtime gives a container.
func TestDeviceTableMinors(t *testing.T) {
	// The driver's procfs, with the minors deliberately not matching the
	// indices: they usually do, and nothing guarantees it.
	dir := t.TempDir()
	for bus, minor := range map[string]string{
		"0000:55:00.0": "1",
		"0000:af:00.0": "0",
	} {
		if err := os.Mkdir(filepath.Join(dir, bus), 0755); err != nil {
			t.Fatalf("creating %q: %v", bus, err)
		}
		info := "Model: \t NVIDIA GeForce RTX 5070\nBus Location: \t " + bus + "\nDevice Minor: \t " + minor + "\n"
		if err := os.WriteFile(filepath.Join(dir, bus, "information"), []byte(info), 0644); err != nil {
			t.Fatalf("writing information for %q: %v", bus, err)
		}
	}

	table := parseDeviceQuery(twoGPUs)
	table.addMinors(minorsByBusID(dir))

	// /dev/nvidia1 is on bus 55, which nvidia-smi calls index 0.
	if got, want := table.Lookup(MinorName(1)), DeviceID(0); got != want {
		t.Errorf("Lookup(%q) = %v, want %v", MinorName(1), got, want)
	}
	if got, want := table.Lookup(MinorName(0)), DeviceID(1); got != want {
		t.Errorf("Lookup(%q) = %v, want %v", MinorName(0), got, want)
	}
	// The index and the minor must not be confused for one another.
	if got, want := table.Lookup("0"), DeviceID(0); got != want {
		t.Errorf("Lookup(%q) = %v, want %v", "0", got, want)
	}
}

// TestDeviceTableWithoutProcfs tests that failing to read the driver's procfs
// costs only the minor numbers, since the other names come from nvidia-smi.
func TestDeviceTableWithoutProcfs(t *testing.T) {
	table := parseDeviceQuery(twoGPUs)
	table.addMinors(minorsByBusID(filepath.Join(t.TempDir(), "absent")))
	if got := table.Lookup(MinorName(0)); got != AnyDevice {
		t.Errorf("Lookup(%q) = %v, want %v", MinorName(0), got, AnyDevice)
	}
	if got, want := table.Lookup("GPU-0b7a7424-d911-bb25-850c-6feddbc332d1"), DeviceID(0); got != want {
		t.Errorf("Lookup(uuid) = %v, want %v", got, want)
	}
}

func TestDeviceTableResolve(t *testing.T) {
	table := parseDeviceQuery(twoGPUs)
	for _, test := range []struct {
		name  string
		names []string
		want  []DeviceID
	}{
		{
			name:  "one device by uuid",
			names: []string{"GPU-1c8b8535-ea22-cc36-961d-7fee0cd443e2"},
			want:  []DeviceID{1},
		},
		{
			name:  "several devices",
			names: []string{"0", "1"},
			want:  []DeviceID{0, 1},
		},
		{
			// The same GPU named twice must not be counted twice, or it would
			// compete with itself.
			name:  "the same device twice",
			names: []string{"0", "GPU-0b7a7424-d911-bb25-850c-6feddbc332d1"},
			want:  []DeviceID{0},
		},
		{
			// A name this host does not know costs the sandbox its place on
			// that device, not its place on the one that did resolve.
			name:  "one known and one unknown",
			names: []string{"GPU-gone", "1"},
			want:  []DeviceID{1},
		},
		{
			name:  "nothing resolves",
			names: []string{"GPU-gone"},
			want:  []DeviceID{AnyDevice},
		},
		{
			name:  "no devices named",
			names: nil,
			want:  []DeviceID{AnyDevice},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := table.Resolve(test.names); !slices.Equal(got, test.want) {
				t.Errorf("Resolve(%v) = %v, want %v", test.names, got, test.want)
			}
		})
	}
}

// TestNilDeviceTable tests that a host whose GPUs could not be enumerated puts
// every sandbox on AnyDevice rather than failing.
func TestNilDeviceTable(t *testing.T) {
	var table *DeviceTable
	if got := table.Lookup("GPU-0b7a7424"); got != AnyDevice {
		t.Errorf("Lookup on a nil table = %v, want %v", got, AnyDevice)
	}
	if got, want := table.Resolve([]string{"0"}), []DeviceID{AnyDevice}; !slices.Equal(got, want) {
		t.Errorf("Resolve on a nil table = %v, want %v", got, want)
	}
	if got, want := table.All(), []DeviceID{AnyDevice}; !slices.Equal(got, want) {
		t.Errorf("All on a nil table = %v, want %v", got, want)
	}
	if got := table.Devices(); got != 0 {
		t.Errorf("Devices on a nil table = %d, want 0", got)
	}
}

func TestDeviceTableAll(t *testing.T) {
	table := parseDeviceQuery(twoGPUs)
	if got, want := table.All(), []DeviceID{0, 1}; !slices.Equal(got, want) {
		t.Errorf("All() = %v, want %v", got, want)
	}
}

func TestNormalizeBusID(t *testing.T) {
	for _, test := range []struct {
		in, want string
	}{
		// The two forms the driver and nvidia-smi use for the same device.
		{"00000000:55:00.0", "0000:55:00.0"},
		{"0000:55:00.0", "0000:55:00.0"},
		{"00000000:AF:00.0", "0000:af:00.0"},
		{" 00000000:55:00.0 ", "0000:55:00.0"},
		// A domain that is genuinely non-zero is kept.
		{"00000001:55:00.0", "0001:55:00.0"},
		// Not bus IDs.
		{"", ""},
		{"0000", ""},
		{"000000000:55:00.0", ""},
	} {
		if got := normalizeBusID(test.in); got != test.want {
			t.Errorf("normalizeBusID(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestParseDeviceMinor(t *testing.T) {
	const info = "Model: \t NVIDIA GeForce RTX 5070\nIRQ: \t 190\nDevice Minor: \t 3\n"
	minor, ok := parseDeviceMinor(info)
	if !ok || minor != 3 {
		t.Errorf("parseDeviceMinor = %d, %v, want 3, true", minor, ok)
	}
	if _, ok := parseDeviceMinor("Model: \t NVIDIA GeForce RTX 5070\n"); ok {
		t.Error("parseDeviceMinor found a minor in a file without one")
	}
}
