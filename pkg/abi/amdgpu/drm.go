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

package amdgpu

// DRM device numbers, from include/uapi/drm/drm.h and
// drivers/gpu/drm/drm_drv.c.
const (
	// DRM_MAJOR is the device major number shared by all DRM devices.
	DRM_MAJOR = 226

	// DRM_RENDER_MINOR_BASE is the first minor number assigned to a render
	// node (/dev/dri/renderD*), and DRM_RENDER_MINOR_MAX is the last. Card
	// nodes (/dev/dri/card*) use minors below this range; amdproxy does not
	// implement them, because they carry the modesetting interface that a
	// compute workload has no need for.
	DRM_RENDER_MINOR_BASE = 128
	DRM_RENDER_MINOR_MAX  = 255
)
