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
			InjectMemoryLimit(&pod)
			got, ok := pod.Annotations[MemoryLimitAnnotation]
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
	pod.Annotations = map[string]string{MemoryLimitAnnotation: "123"}
	InjectMemoryLimit(&pod)
	if got := pod.Annotations[MemoryLimitAnnotation]; got != "123" {
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
	InjectMemoryLimit(&pod)
	if got, want := pod.Annotations[MemoryLimitAnnotation], "268435456"; got != want {
		t.Errorf("annotation = %q, want %q", got, want)
	}
}

// coresContainer returns a container requesting pct percent of a GPU's compute
// as a limit, or requesting none if pct is 0.
func coresContainer(pct int64) v1.Container {
	var c v1.Container
	if pct > 0 {
		c.Resources.Limits = v1.ResourceList{
			CoresResourceName: *resource.NewQuantity(pct, resource.DecimalSI),
		}
	}
	return c
}

func TestInjectWeight(t *testing.T) {
	for _, test := range []struct {
		name       string
		pod        v1.Pod
		wantWeight string
		wantAbsent bool
	}{
		{
			name:       "single container",
			pod:        v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{coresContainer(30)}}},
			wantWeight: "30",
		},
		{
			// The containers share one sandbox, and so one window on the GPU.
			name:       "containers sum",
			pod:        v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{coresContainer(30), coresContainer(20)}}},
			wantWeight: "50",
		},
		{
			name: "init container does not add",
			pod: v1.Pod{Spec: v1.PodSpec{
				InitContainers: []v1.Container{coresContainer(10)},
				Containers:     []v1.Container{coresContainer(30)},
			}},
			wantWeight: "30",
		},
		{
			// A weight above a whole device means nothing, and asking for one
			// must not silently become a share larger than everyone else's.
			name:       "clamped to a whole device",
			pod:        v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{coresContainer(80), coresContainer(80)}}},
			wantWeight: "100",
		},
		{
			// HAMi reads 0 as "no particular share". Such a pod must keep the
			// runtime's weight rather than be given one of its own, so that it
			// competes evenly with the other pods that asked for nothing.
			name:       "no request",
			pod:        v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{coresContainer(0)}}},
			wantAbsent: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := test.pod
			InjectWeight(&pod)
			got, ok := pod.Annotations[WeightAnnotation]
			if test.wantAbsent {
				if ok {
					t.Errorf("annotation = %q, want absent", got)
				}
				return
			}
			if !ok {
				t.Fatalf("annotation absent, want %q", test.wantWeight)
			}
			if got != test.wantWeight {
				t.Errorf("annotation = %q, want %q", got, test.wantWeight)
			}
		})
	}
}

// TestInjectWeightKeepsExplicit tests that a weight the pod states itself is
// left alone, since it may deliberately be lower than the request.
func TestInjectWeightKeepsExplicit(t *testing.T) {
	pod := v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{coresContainer(80)}}}
	pod.Annotations = map[string]string{WeightAnnotation: "5"}
	InjectWeight(&pod)
	if got := pod.Annotations[WeightAnnotation]; got != "5" {
		t.Errorf("annotation = %q, want it left at %q", got, "5")
	}
}

// TestMemoryAndWeightAreIndependent tests that a pod asking for one dimension
// does not acquire a limit on the other.
func TestMemoryAndWeightAreIndependent(t *testing.T) {
	pod := v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{gpuContainer(512)}}}
	InjectMemoryLimit(&pod)
	InjectWeight(&pod)
	if got, ok := pod.Annotations[WeightAnnotation]; ok {
		t.Errorf("weight annotation = %q, want absent for a pod asking only for memory", got)
	}
	if _, ok := pod.Annotations[MemoryLimitAnnotation]; !ok {
		t.Error("memory annotation absent")
	}
}

func TestStandDownHAMi(t *testing.T) {
	pod := v1.Pod{Spec: v1.PodSpec{
		InitContainers: []v1.Container{{Name: "init"}},
		Containers:     []v1.Container{{Name: "a"}, {Name: "b"}},
	}}
	StandDownHAMi(&pod)
	for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		var got string
		for _, e := range c.Env {
			if e.Name == disableControlEnv {
				got = e.Value
			}
		}
		if got != "true" {
			t.Errorf("container %q has %s=%q, want %q", c.Name, disableControlEnv, got, "true")
		}
	}
}

// TestStandDownHAMiKeepsExplicit tests that a container asking for HAMi's
// enforcement keeps it. That only holds it to more than the Sentry does, which
// it is free to choose.
func TestStandDownHAMiKeepsExplicit(t *testing.T) {
	pod := v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{{
		Env: []v1.EnvVar{{Name: disableControlEnv, Value: "false"}},
	}}}}
	StandDownHAMi(&pod)
	env := pod.Spec.Containers[0].Env
	if len(env) != 1 || env[0].Value != "false" {
		t.Errorf("env = %+v, want the container's own value left alone", env)
	}
}
