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

package nvproxy

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/nvgpu"
)

// preemptChannelGroup asks the GPU to switch the channel group hObject off the
// engine it is running on, evicting work the sandbox has already submitted.
//
// This control is issued by the Sentry on its own behalf rather than being
// proxied from the sandbox, so unlike the handlers in frontend.go there is no
// task, no guest memory, and nothing to copy back out: the parameters live in
// Sentry memory for the duration of the call. The same shape is already used to
// re-create objects during restore, in save_restore_unsafe.go.
//
// The sandbox cannot prevent this. The control is NON_PRIVILEGED, so the
// sandbox may call it on its own channel groups too, but that is only ever
// self-harm; what matters is that nvproxy holds the host file descriptor and
// the object handles, and a sandbox has no way to revoke either.
//
// What the preempt costs depends on the channel's context-switch preemption
// mode: under WFI the driver waits for the running work to finish on its own,
// while CTA and CILP stop it at a thread block or instruction boundary. See
// ctrlSetCtxswPreemptionMode.
func preemptChannelGroup(fd *frontendFD, hClient, hObject nvgpu.Handle) error {
	ctrlParams := nvgpu.NVA06C_CTRL_PREEMPT_PARAMS{
		// Wait, so that a failure to preempt is observable rather than silent.
		// The driver warns that repeatedly issuing this without waiting "can
		// lead to undefined results".
		BWait:          1,
		BManualTimeout: 1,
		TimeoutUs:      uint32(preemptChannelGroupTimeout.Microseconds()),
	}
	defer runtime.KeepAlive(&ctrlParams) // since we convert to non-pointer-typed P64
	ioctlParams := nvgpu.NVOS54_PARAMETERS{
		HClient:    hClient,
		HObject:    hObject,
		Cmd:        nvgpu.NVA06C_CTRL_CMD_PREEMPT,
		Params:     p64FromPtr(unsafe.Pointer(&ctrlParams)),
		ParamsSize: uint32(ctrlParams.SizeBytes()),
	}
	if _, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(fd.hostFD), frontendIoctlCmd(nvgpu.NV_ESC_RM_CONTROL, nvgpu.SizeofNVOS54Parameters), uintptr(unsafe.Pointer(&ioctlParams))); errno != 0 {
		return errno
	}
	if ioctlParams.Status != nvgpu.NV_OK {
		return fmt.Errorf("NvStatus %d", ioctlParams.Status)
	}
	return nil
}
