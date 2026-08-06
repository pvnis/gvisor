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
	v1 "k8s.io/api/core/v1"
)

// disableControlEnv is the variable HAMi's libvgpu.so reads to decide whether
// to intercept the CUDA API at all. Setting it leaves the library loaded but
// inert, so every call reaches the driver unchanged.
const disableControlEnv = "CUDA_DISABLE_CONTROL"

// StandDownHAMi tells HAMi's preloaded library not to enforce anything,
// leaving the sandbox as the only thing limiting the container's use of the
// GPU.
//
// HAMi's device plugin preloads libvgpu.so into every GPU container, which
// gives a workload a limiter it can inspect, subvert, or simply switch off with
// this same variable -- that being the point: a control the workload can reach
// is not a control. Setting it from admission removes the redundant copy rather
// than the enforcement. A container that unsets it again only re-enables a
// second limiter on top of the Sentry's, which can restrict it further but
// never grant it more, so this is safe to lose in either direction.
//
// This assumes the runtime is configured to enforce what the pod is scheduled
// against: --nvproxy-gpu-memory-limit for memory, and
// --nvproxy-gpu-scheduler-socket for compute. Without the latter, nothing
// divides GPU time and the nvidia.com/gpucores requests are honoured at
// placement only.
func StandDownHAMi(pod *v1.Pod) {
	for i := range pod.Spec.Containers {
		setEnv(&pod.Spec.Containers[i], disableControlEnv, "true")
	}
	for i := range pod.Spec.InitContainers {
		setEnv(&pod.Spec.InitContainers[i], disableControlEnv, "true")
	}
}

// setEnv gives a container an environment variable, unless it states one of
// that name itself. A container asking for HAMi's enforcement is asking to be
// held to more than the sandbox holds it to, which it is free to do.
func setEnv(c *v1.Container, name, value string) {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return
		}
	}
	c.Env = append(c.Env, v1.EnvVar{Name: name, Value: value})
}
