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

// Package gpumem translates a pod's requests for GPU memory into the
// annotation that makes runsc enforce them.
package gpumem

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/log"
	v1 "k8s.io/api/core/v1"
)

const (
	// MemoryResourceName is the extended resource by which a container
	// requests GPU memory. This is the name used by HAMi
	// (https://project-hami.io), whose scheduler places pods according to it;
	// its values are in mebibytes.
	MemoryResourceName = "nvidia.com/gpumem"

	// LimitAnnotation is the annotation runsc reads to limit the GPU
	// memory a sandbox may allocate, in bytes. The value configured on the
	// runtime is a ceiling that this may lower but not raise; a pod asking for
	// more than the runtime allows will fail to start.
	LimitAnnotation = "dev.gvisor.flag.nvproxy-gpu-memory-limit"

	// bytesPerMiB converts the resource's units to the annotation's.
	bytesPerMiB = 1 << 20
)

// InjectLimit translates a pod's requests for GPU memory into the
// annotation that makes runsc enforce them.
//
// A scheduler that understands GPU memory, such as HAMi's, uses the resource
// to decide which node and GPU a pod is placed on, but placement alone does not
// prevent a container from allocating more than it asked for. Restating the
// request as a runsc annotation has it enforced in the Sentry, where the
// container cannot reach it. Nothing else about the pod is changed, so a
// scheduler that relies on the resource continues to work unmodified.
//
// The limit applies to the sandbox rather than to individual containers, so it
// is the pod's peak demand: containers run together and their requests add up,
// while init containers run before them and only their largest matters.
func InjectLimit(pod *v1.Pod) {
	// An explicit annotation is the pod author's own statement of the limit,
	// which may be deliberately lower than the request. Leave it alone.
	if _, ok := pod.Annotations[LimitAnnotation]; ok {
		return
	}

	var concurrent int64
	for i := range pod.Spec.Containers {
		concurrent += containerMemoryMiB(&pod.Spec.Containers[i])
	}
	peak := concurrent
	for i := range pod.Spec.InitContainers {
		if mib := containerMemoryMiB(&pod.Spec.InitContainers[i]); mib > peak {
			peak = mib
		}
	}
	if peak <= 0 {
		// No container asked for GPU memory, so there is nothing to enforce.
		// Adding a limit here would restrict a pod that never requested one.
		return
	}

	// The resource counts mebibytes, and cannot express a total large enough to
	// overflow this.
	limit := peak * bytesPerMiB
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[LimitAnnotation] = fmt.Sprintf("%d", limit)
	log.Debugf("Injected GPU memory limit of %d bytes from %d MiB of %q requests", limit, peak, MemoryResourceName)
}

// containerMemoryMiB returns the GPU memory a container requests, in
// mebibytes, or 0 if it requests none.
func containerMemoryMiB(c *v1.Container) int64 {
	// A limit is what the container is held to; fall back to the request,
	// which is what an unlimited resource is scheduled by.
	if q, ok := c.Resources.Limits[MemoryResourceName]; ok {
		if v, ok := q.AsInt64(); ok && v > 0 {
			return v
		}
	}
	if q, ok := c.Resources.Requests[MemoryResourceName]; ok {
		if v, ok := q.AsInt64(); ok && v > 0 {
			return v
		}
	}
	return 0
}
