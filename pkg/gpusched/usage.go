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

// Sampler reports how much of a GPU each process is using.
//
// Without one, the scheduler can only tell that a sandbox submitted something,
// not how much of the GPU that turned into. The difference matters because a
// GPU cannot be made to abandon work already submitted to it: a sandbox that
// submits one very long kernel keeps the GPU past the end of its window, and
// looks from the inside exactly like one that submitted a short kernel.
// Measuring what a sandbox actually consumed is what allows the excess to be
// charged back.
type Sampler interface {
	// Sample returns, for each process using the GPU, the fraction of recent
	// time during which it had work executing, between 0 and 1. Processes not
	// using the GPU may be absent rather than reported as zero.
	Sample() map[int]float64
}

// FakeSampler is a Sampler returning values set by a test.
type FakeSampler struct {
	// Utilization is the fraction of the GPU each process is using.
	Utilization map[int]float64
}

// Sample implements Sampler.Sample.
func (f *FakeSampler) Sample() map[int]float64 {
	return f.Utilization
}
