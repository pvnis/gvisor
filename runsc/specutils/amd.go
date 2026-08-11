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

package specutils

import (
	"regexp"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/config"
)

// KFDDevicePath is the path of the AMD Kernel Fusion Driver's device file,
// which every ROCm application opens.
const KFDDevicePath = "/dev/kfd"

// RenderDeviceRegex matches the path of a DRM render node.
var RenderDeviceRegex = regexp.MustCompile(`^/dev/dri/renderD(\d+)$`)

// AMDProxyEnabled checks both the flag and the OCI spec to determine if
// amdproxy should be enabled.
func AMDProxyEnabled(spec *specs.Spec, conf *config.Config) bool {
	if conf.AMDProxy {
		return true
	}
	return AMDGPUFunctionalityRequested(spec)
}

// AMDGPUFunctionalityRequested returns true if the container should have
// access to AMD GPU functionality.
//
// The AMD GPU device plugin and container runtimes inject the device files
// into the OCI spec the same way they would for runc, so their presence is
// what indicates that a GPU was allocated to this container. /dev/kfd is the
// marker: a render node alone may be present for graphics, which amdproxy
// does not implement, but no ROCm workload runs without KFD.
func AMDGPUFunctionalityRequested(spec *specs.Spec) bool {
	if spec.Linux == nil {
		return false
	}
	for _, dev := range spec.Linux.Devices {
		if dev.Path == KFDDevicePath {
			return true
		}
	}
	return false
}
