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
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DeviceID identifies a GPU on the host.
//
// It is the index nvidia-smi reports, because that is what "nvidia-smi pmon"
// attributes work by and so the only identity that can be joined against a
// measurement.
type DeviceID int

// AnyDevice is the identity of a sandbox whose GPU is not known, and the one
// every sandbox has on a host where the devices cannot be enumerated.
//
// Sandboxes carrying it are scheduled together, which is what a host with one
// GPU wants and the safe reading for a host with several: sandboxes that do not
// contend are held to shares as though they did, which wastes GPU time but
// cannot let one take more than it was given.
const AnyDevice DeviceID = -1

func (d DeviceID) String() string {
	if d == AnyDevice {
		return "any"
	}
	return strconv.Itoa(int(d))
}

// DeviceTable resolves the names a sandbox may be described by onto the GPU
// they refer to.
//
// A sandbox's GPUs reach runsc in whatever form the thing that allocated them
// chose: a device plugin names UUIDs in NVIDIA_VISIBLE_DEVICES, Docker's --gpus
// takes indices, and a CRI runtime injects device nodes identified by minor
// number. None of those is what the driver reports usage by, so they are
// gathered into one table here rather than being resolved at each call site.
type DeviceTable struct {
	// byName maps every name a device is known by to it. Names are matched
	// case-insensitively, since UUIDs and bus IDs are written both ways.
	byName map[string]DeviceID

	// count is how many devices were found, which is what decides whether
	// scheduling them apart is worth anything.
	count int
}

// Devices returns the number of GPUs the table describes.
func (t *DeviceTable) Devices() int {
	if t == nil {
		return 0
	}
	return t.count
}

// Lookup returns the device a name refers to, or AnyDevice if the name is not
// one the table knows.
//
// An unknown name is not an error. A sandbox may name a device that has since
// gone, or one this table failed to see, and scheduling it alongside everything
// else is a worse division of the GPU rather than an unsafe one.
func (t *DeviceTable) Lookup(name string) DeviceID {
	if t == nil {
		return AnyDevice
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if d, ok := t.byName[name]; ok {
		return d
	}
	// A bus ID may be written with either width of domain, so it is matched in
	// the one form the table keeps rather than being stored twice.
	if bus := normalizeBusID(name); bus != "" {
		if d, ok := t.byName[bus]; ok {
			return d
		}
	}
	return AnyDevice
}

// Resolve returns the devices a sandbox described by the given names is using,
// with duplicates removed.
//
// Names that resolve to nothing are dropped rather than becoming AnyDevice, so
// that a sandbox naming one device this table knows and one it does not is
// scheduled on the device it does know. A sandbox whose names all resolve to
// nothing, or which names none, gets AnyDevice.
func (t *DeviceTable) Resolve(names []string) []DeviceID {
	var out []DeviceID
	seen := make(map[DeviceID]struct{}, len(names))
	for _, name := range names {
		d := t.Lookup(name)
		if d == AnyDevice {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	if len(out) == 0 {
		return []DeviceID{AnyDevice}
	}
	return out
}

// All returns every device the table describes, which is what a sandbox
// permitted all of them is using.
func (t *DeviceTable) All() []DeviceID {
	if t == nil || t.count == 0 {
		return []DeviceID{AnyDevice}
	}
	out := make([]DeviceID, 0, t.count)
	for i := 0; i < t.count; i++ {
		out = append(out, DeviceID(i))
	}
	return out
}

// NewDeviceTable enumerates the host's GPUs and the names each answers to.
//
// It returns an error if nvidia-smi cannot be run, which leaves every sandbox
// on AnyDevice; scheduling then behaves as it did before devices were told
// apart.
func NewDeviceTable() (*DeviceTable, error) {
	// The index is what pmon reports; the UUID is what a device plugin names;
	// the bus ID is what ties both to the minor number below.
	out, err := exec.Command("nvidia-smi", "--query-gpu=index,uuid,pci.bus_id", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("listing GPUs with nvidia-smi: %w", err)
	}
	t := parseDeviceQuery(string(out))
	if t.count == 0 {
		return nil, fmt.Errorf("nvidia-smi listed no GPUs")
	}
	t.addMinors(minorsByBusID("/proc/driver/nvidia/gpus"))
	return t, nil
}

// parseDeviceQuery reads the output of the nvidia-smi query above, whose rows
// look like:
//
//	0, GPU-0b7a7424-d911-bb25-850c-6feddbc332d1, 00000000:55:00.0
func parseDeviceQuery(out string) *DeviceTable {
	t := &DeviceTable{byName: make(map[string]DeviceID)}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ",")
		if len(f) < 3 {
			continue
		}
		index, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		d := DeviceID(index)
		// The index alone, as Docker's --gpus and NVIDIA_VISIBLE_DEVICES write
		// it.
		t.add(strconv.Itoa(index), d)
		t.add(strings.TrimSpace(f[1]), d)
		if bus := normalizeBusID(f[2]); bus != "" {
			t.add(bus, d)
		}
		if index+1 > t.count {
			t.count = index + 1
		}
	}
	return t
}

// add records that a device answers to a name, ignoring empty ones.
func (t *DeviceTable) add(name string, d DeviceID) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return
	}
	t.byName[name] = d
}

// addMinors records the device node each GPU is reached through, given a map
// from normalized bus ID to minor number.
//
// The minor is the only name a CRI runtime uses: it injects /dev/nvidia# into
// the container rather than naming the device in the environment. It is not
// reported by nvidia-smi, so it is joined on through the bus ID, and is simply
// absent if the driver's procfs cannot be read.
func (t *DeviceTable) addMinors(minors map[string]int) {
	for bus, minor := range minors {
		d, ok := t.byName[bus]
		if !ok {
			continue
		}
		t.add(minorName(minor), d)
	}
}

// minorName is how a device node is named to the scheduler. It is prefixed
// because a minor number and an index are both small integers meaning
// different things, and are usually but not always equal.
func minorName(minor int) string {
	return fmt.Sprintf("minor:%d", minor)
}

// MinorName returns the name by which a sandbox reached through /dev/nvidia#
// should be reported.
func MinorName(minor int) string { return minorName(minor) }

// minorsByBusID reads the minor number of each GPU from the driver's procfs,
// whose per-device directories are named by bus ID and contain a line
//
//	Device Minor: 	 0
func minorsByBusID(dir string) map[string]int {
	out := make(map[string]int)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		bus := normalizeBusID(e.Name())
		if bus == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "information"))
		if err != nil {
			continue
		}
		if minor, ok := parseDeviceMinor(string(b)); ok {
			out[bus] = minor
		}
	}
	return out
}

// parseDeviceMinor finds the minor number in the driver's per-device
// information file.
func parseDeviceMinor(info string) (int, bool) {
	sc := bufio.NewScanner(strings.NewReader(info))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(key) != "Device Minor" {
			continue
		}
		minor, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, false
		}
		return minor, true
	}
	return 0, false
}

// normalizeBusID puts a PCI bus ID into one form, so that the driver's procfs
// and nvidia-smi can be joined on it.
//
// The two disagree on the width of the domain: procfs names a directory
// 0000:55:00.0 while nvidia-smi reports 00000000:55:00.0. The domain is
// trimmed to four digits, which is what every host has and what procfs uses.
func normalizeBusID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	domain, rest, ok := strings.Cut(s, ":")
	if !ok || rest == "" {
		return ""
	}
	if len(domain) == 0 || len(domain) > 8 {
		// The two forms are four and eight digits wide. Anything else is not a
		// bus ID this understands.
		return ""
	}
	domain = strings.TrimLeft(domain, "0")
	if len(domain) > 4 {
		// A domain too large to write in the narrower form, which is not
		// something these two spellings of the same device differ over.
		return ""
	}
	return strings.Repeat("0", 4-len(domain)) + domain + ":" + rest
}
