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

package gpumem

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// gpuContainer returns a container requesting mib mebibytes of GPU memory as a
// limit, or requesting none if mib is 0.
func gpuContainer(mib int64) v1.Container {
	var c v1.Container
	if mib > 0 {
		c.Resources.Limits = v1.ResourceList{
			MemoryResourceName: *resource.NewQuantity(mib, resource.DecimalSI),
		}
	}
	return c
}

func TestInjectGPUMemoryLimit(t *testing.T) {
	const mib = 1 << 20
	for _, test := range []struct {
		name       string
		pod        v1.Pod
		wantLimit  string
		wantAbsent bool
	}{
		{
			name:      "single container",
			pod:       v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{gpuContainer(512)}}},
			wantLimit: "536870912",
		},
		{
			// Containers of a pod run together, so the sandbox must accommodate
			// all of them at once.
			name:      "containers sum",
			pod:       v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{gpuContainer(512), gpuContainer(256)}}},
			wantLimit: "805306368",
		},
		{
			// Init containers run before the others, so they do not add.
			name: "init container does not add",
			pod: v1.Pod{Spec: v1.PodSpec{
				InitContainers: []v1.Container{gpuContainer(128)},
				Containers:     []v1.Container{gpuContainer(512)},
			}},
			wantLimit: "536870912",
		},
		{
			// ...but a larger init container sets the peak on its own.
			name: "large init container sets peak",
			pod: v1.Pod{Spec: v1.PodSpec{
				InitContainers: []v1.Container{gpuContainer(2048)},
				Containers:     []v1.Container{gpuContainer(512)},
			}},
			wantLimit: "2147483648",
		},
		{
			// A pod that asked for no GPU memory must not acquire a limit.
			name:       "no gpu request",
			pod:        v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{gpuContainer(0)}}},
			wantAbsent: true,
		},
		{
			name:       "no containers",
			pod:        v1.Pod{},
			wantAbsent: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := test.pod
			InjectLimit(&pod)
			got, ok := pod.Annotations[LimitAnnotation]
			if test.wantAbsent {
				if ok {
					t.Errorf("annotation = %q, want absent", got)
				}
				return
			}
			if !ok {
				t.Fatalf("annotation absent, want %q", test.wantLimit)
			}
			if got != test.wantLimit {
				t.Errorf("annotation = %q, want %q", got, test.wantLimit)
			}
		})
	}
}

// TestInjectGPUMemoryLimitKeepsExplicit tests that a limit already stated on
// the pod is left alone, since it may deliberately be lower than the request.
func TestInjectGPUMemoryLimitKeepsExplicit(t *testing.T) {
	pod := v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{gpuContainer(4096)}}}
	pod.Annotations = map[string]string{LimitAnnotation: "123"}
	InjectLimit(&pod)
	if got := pod.Annotations[LimitAnnotation]; got != "123" {
		t.Errorf("annotation = %q, want it left at %q", got, "123")
	}
}

// TestInjectGPUMemoryLimitFallsBackToRequests tests that a request is used
// when no limit is stated. Extended resources normally require the two to
// match, but a pod may state only one.
func TestInjectGPUMemoryLimitFallsBackToRequests(t *testing.T) {
	var c v1.Container
	c.Resources.Requests = v1.ResourceList{
		MemoryResourceName: *resource.NewQuantity(256, resource.DecimalSI),
	}
	pod := v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{c}}}
	InjectLimit(&pod)
	if got, want := pod.Annotations[LimitAnnotation], "268435456"; got != want {
		t.Errorf("annotation = %q, want %q", got, want)
	}
}
