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

// Package gpushare translates a pod's requests for a share of a GPU into the
// annotations that make runsc enforce them.
//
// A GPU-aware scheduler such as HAMi's (https://project-hami.io) decides which
// node and which device a pod runs on, and refuses to place more work on a GPU
// than it can hold. That is placement, and it is worth keeping: it runs on the
// control plane, well away from the workload, and nothing about it needs to be
// trusted by the sandbox.
//
// What placement does not do is stop a container from taking more than it was
// placed for. HAMi's answer is to preload libvgpu.so into the container and
// have it intercept the CUDA API, which puts the enforcement inside the address
// space of the process it is meant to constrain; a container can switch it off
// with a documented environment variable. Restating the same requests as runsc
// annotations has them enforced in the Sentry instead, where the container
// cannot reach them.
//
// Nothing here changes what a pod asks for, so a scheduler reading these
// resources continues to work unmodified.
package gpushare

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/log"
	v1 "k8s.io/api/core/v1"
)

const (
	// MemoryResourceName is the extended resource by which a container
	// requests GPU memory, in mebibytes. This is the name HAMi's scheduler
	// places pods by.
	MemoryResourceName = "nvidia.com/gpumem"

	// CoresResourceName is the extended resource by which a container requests
	// GPU compute. HAMi states it as a percentage of a device, and will not
	// place more than 100 on one GPU; 0 means the container asks for no
	// particular share.
	CoresResourceName = "nvidia.com/gpucores"

	// MemoryLimitAnnotation is the annotation runsc reads to limit the GPU
	// memory a sandbox may allocate, in bytes. The value configured on the
	// runtime is a ceiling that this may lower but not raise; a pod asking for
	// more than the runtime allows will fail to start.
	MemoryLimitAnnotation = "dev.gvisor.flag.nvproxy-gpu-memory-limit"

	// WeightAnnotation is the annotation runsc reads to decide a sandbox's
	// share of a GPU relative to the others scheduled alongside it. It is
	// subject to the same ceiling as MemoryLimitAnnotation.
	WeightAnnotation = "dev.gvisor.flag.nvproxy-gpu-weight"

	// bytesPerMiB converts the memory resource's units to the annotation's.
	bytesPerMiB = 1 << 20
)

// InjectMemoryLimit has runsc enforce the GPU memory a pod was scheduled
// against.
//
// The limit applies to the sandbox rather than to individual containers, so it
// is the pod's peak demand: containers run together and their requests add up,
// while init containers run before them and only their largest matters.
func InjectMemoryLimit(pod *v1.Pod) {
	// An explicit annotation is the pod author's own statement of the limit,
	// which may be deliberately lower than the request. Leave it alone.
	if _, ok := pod.Annotations[MemoryLimitAnnotation]; ok {
		return
	}

	peak := peakRequest(pod, MemoryResourceName)
	if peak <= 0 {
		// No container asked for GPU memory, so there is nothing to enforce.
		// Adding a limit here would restrict a pod that never requested one.
		return
	}

	// The resource counts mebibytes, and cannot express a total large enough to
	// overflow this.
	setAnnotation(pod, MemoryLimitAnnotation, fmt.Sprintf("%d", peak*bytesPerMiB))
	log.Debugf("Injected GPU memory limit of %d MiB from %q requests", peak, MemoryResourceName)
}

// InjectWeight has runsc give the pod the share of GPU compute it was
// scheduled against.
//
// The requested percentage becomes a weight rather than a cap, because a cap is
// the wrong tool for the stated goal. Percentages of a GPU are only meaningful
// against a particular device, they leave the GPU idle whenever the holder has
// nothing to submit, and they say nothing about what should happen when the sum
// of them is below 100. A weight is relative and needs no reference to the
// hardware: a sandbox gets its weight's share of whatever the contending
// sandboxes are actually asking for, and that share grows when its neighbours
// fall idle. Where the requests do add up to 100 -- which is what HAMi's
// scheduler enforces at placement time -- the two readings agree, and a pod
// asking for 30 gets 30% of a contended GPU.
//
// A pod that asks for nothing in particular is left without an annotation and
// takes the weight configured on the runtime, so it competes on equal terms
// with the other unannotated pods rather than being starved by them.
func InjectWeight(pod *v1.Pod) {
	if _, ok := pod.Annotations[WeightAnnotation]; ok {
		return
	}

	// Containers of a pod share one sandbox, and so one window on the GPU, so
	// what they ask for adds up in the same way as their memory.
	peak := peakRequest(pod, CoresResourceName)
	if peak <= 0 {
		return
	}
	// HAMi will not place more than a device's worth on one GPU, so a larger
	// total than this cannot have been scheduled by it; clamp rather than
	// reject, so that a pod admitted by some other scheduler still starts.
	if peak > 100 {
		peak = 100
	}

	setAnnotation(pod, WeightAnnotation, fmt.Sprintf("%d", peak))
	log.Debugf("Injected GPU weight of %d from %q requests", peak, CoresResourceName)
}

// peakRequest returns the most of a resource a pod needs at one time, in the
// resource's own units, or 0 if it requests none.
func peakRequest(pod *v1.Pod, name v1.ResourceName) int64 {
	var concurrent int64
	for i := range pod.Spec.Containers {
		concurrent += containerRequest(&pod.Spec.Containers[i], name)
	}
	peak := concurrent
	for i := range pod.Spec.InitContainers {
		if v := containerRequest(&pod.Spec.InitContainers[i], name); v > peak {
			peak = v
		}
	}
	return peak
}

// containerRequest returns how much of a resource a container asks for, or 0
// if it asks for none.
func containerRequest(c *v1.Container, name v1.ResourceName) int64 {
	// A limit is what the container is held to; fall back to the request,
	// which is what an unlimited resource is scheduled by.
	if q, ok := c.Resources.Limits[name]; ok {
		if v, ok := q.AsInt64(); ok && v > 0 {
			return v
		}
	}
	if q, ok := c.Resources.Requests[name]; ok {
		if v, ok := q.AsInt64(); ok && v > 0 {
			return v
		}
	}
	return 0
}

// setAnnotation records a value on the pod, creating the annotation map if the
// pod has none.
func setAnnotation(pod *v1.Pod, key, value string) {
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[key] = value
}
