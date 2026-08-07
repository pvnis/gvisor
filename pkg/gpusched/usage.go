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

// Proc is one process's use of one GPU.
//
// Usage is reported per device rather than per process because a process may
// be using several: charging what it took from one GPU against its share of
// another would make a sandbox that saturates a device it has to itself look
// like one exceeding its share of a device it is sharing.
type Proc struct {
	Device DeviceID
	PID    int
}

// Usage is how much of each GPU each process is using, between 0 and 1.
type Usage map[Proc]float64

// Of returns how much of a device a process is using.
//
// AnyDevice asks what the process is using across every GPU, which is what a
// host whose devices could not be told apart can answer. The total is capped at
// one whole device, since it is being compared against a single period.
func (u Usage) Of(d DeviceID, pid int) (float64, bool) {
	if d != AnyDevice {
		v, ok := u[Proc{Device: d, PID: pid}]
		return v, ok
	}
	var total float64
	var found bool
	for p, v := range u {
		if p.PID != pid {
			continue
		}
		found = true
		total += v
	}
	if total > 1 {
		total = 1
	}
	return total, found
}

// Sampler reports how much of each GPU each process is using.
//
// Without one, the scheduler can only tell that a sandbox submitted something,
// not how much of the GPU that turned into. The difference matters because a
// GPU cannot be made to abandon work already submitted to it: a sandbox that
// submits one very long kernel keeps the GPU past the end of its window, and
// looks from the inside exactly like one that submitted a short kernel.
// Measuring what a sandbox actually consumed is what allows the excess to be
// charged back.
type Sampler interface {
	// Sample returns, for each process using a GPU, the fraction of recent time
	// during which it had work executing on it, between 0 and 1. Processes not
	// using a GPU may be absent rather than reported as zero.
	Sample() Usage
}

// FakeSampler is a Sampler returning values set by a test.
type FakeSampler struct {
	// Utilization is the fraction of each GPU each process is using.
	Utilization Usage
}

// Sample implements Sampler.Sample.
func (f *FakeSampler) Sample() Usage {
	return f.Utilization
}
