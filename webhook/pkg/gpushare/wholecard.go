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

package gpushare

import (
	"gvisor.dev/gvisor/pkg/log"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// DefaultWholeCardMemory has a container that asks for GPUs but no GPU memory
// ask for a whole card's memory, cardMiB, per GPU.
//
// HAMi gives such a container whole devices, but nothing about the pod says
// so: a ResourceQuota on MemoryResourceName charges it nothing, and a tenant
// whose memory quota would refuse a slice of a card can take every card it has
// attachments for. Stating the memory makes the quota count what the
// container holds.
//
// This is only right while every device has cardMiB of memory. On a cluster
// with devices of different sizes there is no one value to state at admission,
// before the pod is placed; cardMiB <= 0 turns the default off.
//
// Containers that state their memory either way, in mebibytes or as a
// percentage, are left as they are.
func DefaultWholeCardMemory(pod *v1.Pod, cardMiB int64) {
	if cardMiB <= 0 {
		return
	}
	for i := range pod.Spec.InitContainers {
		defaultWholeCard(&pod.Spec.InitContainers[i], cardMiB)
	}
	for i := range pod.Spec.Containers {
		defaultWholeCard(&pod.Spec.Containers[i], cardMiB)
	}
}

func defaultWholeCard(c *v1.Container, cardMiB int64) {
	if containerRequest(c, GPUResourceName) <= 0 ||
		hasResource(c, MemoryResourceName) || hasResource(c, MemoryPercentageResourceName) {
		return
	}
	// An extended resource must be requested as much as it is limited, so set
	// both.
	q := *resource.NewQuantity(cardMiB, resource.DecimalSI)
	if c.Resources.Limits == nil {
		c.Resources.Limits = v1.ResourceList{}
	}
	if c.Resources.Requests == nil {
		c.Resources.Requests = v1.ResourceList{}
	}
	c.Resources.Limits[MemoryResourceName] = q
	c.Resources.Requests[MemoryResourceName] = q
	log.Debugf("Container %q asks for whole GPUs; stated %d MiB of %q per GPU", c.Name, cardMiB, MemoryResourceName)
}

// hasResource reports whether a container names a resource at all, as a limit
// or a request, whatever its value.
func hasResource(c *v1.Container, name v1.ResourceName) bool {
	_, limited := c.Resources.Limits[name]
	_, requested := c.Resources.Requests[name]
	return limited || requested
}
