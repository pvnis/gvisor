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

// DefaultWholeCardMemory has every container that asks for NVIDIA GPUs state
// its GPU memory in mebibytes, as MemoryResourceName, given that every device
// has cardMiB of memory:
//
//   - A container asking for GPUs but no GPU memory gets whole devices from
//     HAMi; it is made to ask for cardMiB per GPU.
//   - A container asking for MemoryPercentageResourceName gets that share of
//     each device; it is made to ask for the same share of cardMiB instead.
//     HAMi does not accept both resources on one container, so the
//     percentage is removed.
//   - A container asking for all of a device's time, CoresResourceName 100,
//     gets its devices to itself: HAMi places it only on an unused device and
//     nothing else beside it. Whatever memory it asked for, it is made to ask
//     for cardMiB per GPU, which is what it holds.
//
// Neither changes what HAMi gives the container, but without it a
// ResourceQuota on MemoryResourceName charges such a container nothing, and a
// tenant whose memory quota would refuse a slice of a card can take every card
// it has attachments for.
//
// This is only right while every device has cardMiB of memory. On a cluster
// with devices of different sizes there is no one value to state at admission,
// before the pod is placed; cardMiB <= 0 turns this off.
func DefaultWholeCardMemory(pod *v1.Pod, cardMiB int64) {
	if cardMiB <= 0 {
		return
	}
	for i := range pod.Spec.InitContainers {
		stateMemory(&pod.Spec.InitContainers[i], cardMiB)
	}
	for i := range pod.Spec.Containers {
		stateMemory(&pod.Spec.Containers[i], cardMiB)
	}
}

func stateMemory(c *v1.Container, cardMiB int64) {
	if containerRequest(c, CoresResourceName) >= 100 {
		delete(c.Resources.Limits, MemoryPercentageResourceName)
		delete(c.Resources.Requests, MemoryPercentageResourceName)
		setRequest(c, MemoryResourceName, cardMiB)
		log.Debugf("Container %q asks for all of each GPU's time, so holds the GPUs; stated %d MiB of %q per GPU", c.Name, cardMiB, MemoryResourceName)
		return
	}
	if hasResource(c, MemoryResourceName) {
		return
	}
	if hasResource(c, MemoryPercentageResourceName) {
		percent := containerRequest(c, MemoryPercentageResourceName)
		if percent <= 0 || percent > 100 {
			// Not a share HAMi would place; leave it for HAMi to refuse.
			return
		}
		delete(c.Resources.Limits, MemoryPercentageResourceName)
		delete(c.Resources.Requests, MemoryPercentageResourceName)
		mib := cardMiB * percent / 100
		setRequest(c, MemoryResourceName, mib)
		log.Debugf("Container %q asks for %d%% of each GPU's memory; stated %d MiB of %q", c.Name, percent, mib, MemoryResourceName)
		return
	}
	if containerRequest(c, GPUResourceName) > 0 {
		setRequest(c, MemoryResourceName, cardMiB)
		log.Debugf("Container %q asks for whole GPUs; stated %d MiB of %q per GPU", c.Name, cardMiB, MemoryResourceName)
	}
}

// InjectMemoryTotal has every container that asks for GPU memory also ask for
// totalName: its MemoryResourceName times the number of GPUs it asks for.
//
// MemoryResourceName counts memory per GPU, so a ResourceQuota on it charges a
// container on two GPUs for one. totalName is what a quota should bound
// instead. It is a name for the quota alone: nothing advertises it, so the
// scheduler must be told to ignore it (HAMi's scheduler extender lists it as a
// managed resource, ignored by the scheduler), and the kubelet drops extended
// resources its node does not have when it admits a pod. An empty totalName
// turns this off.
func InjectMemoryTotal(pod *v1.Pod, totalName v1.ResourceName) {
	if totalName == "" {
		return
	}
	for i := range pod.Spec.InitContainers {
		injectTotal(&pod.Spec.InitContainers[i], totalName)
	}
	for i := range pod.Spec.Containers {
		injectTotal(&pod.Spec.Containers[i], totalName)
	}
}

func injectTotal(c *v1.Container, totalName v1.ResourceName) {
	mib := containerRequest(c, MemoryResourceName)
	if mib <= 0 {
		return
	}
	// Whatever the container said about the total is replaced: a quota must
	// not take a tenant's word for what it holds.
	setRequest(c, totalName, mib*gpuCount(c))
}

// setRequest has a container ask for value of an extended resource, as both
// limit and request, which an extended resource must have equal.
func setRequest(c *v1.Container, name v1.ResourceName, value int64) {
	q := *resource.NewQuantity(value, resource.DecimalSI)
	if c.Resources.Limits == nil {
		c.Resources.Limits = v1.ResourceList{}
	}
	if c.Resources.Requests == nil {
		c.Resources.Requests = v1.ResourceList{}
	}
	c.Resources.Limits[name] = q
	c.Resources.Requests[name] = q
}

// hasResource reports whether a container names a resource at all, as a limit
// or a request, whatever its value.
func hasResource(c *v1.Container, name v1.ResourceName) bool {
	_, limited := c.Resources.Limits[name]
	_, requested := c.Resources.Requests[name]
	return limited || requested
}
