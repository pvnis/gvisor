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

// setChannelGroupTimeslice sets how long the GPU's runlist scheduler runs the
// channel group hObject before switching to another, in microseconds. The idea
// was that a sandbox given a longer timeslice than its neighbours would take a
// proportionally greater share of a contended GPU, enforced by the GPU's own
// runlist regardless of how work is submitted -- unlike the compute gate's
// mapping revocation, which misses doorbell-driven workloads.
//
// MEASURED INERT on a Blackwell RTX 5070 (driver 610.43.02, 2026-08-14): the
// control succeeds and the value round-trips (GET_TIMESLICE reads back exactly
// what was set, and it survives GPFIFO_SCHEDULE), but it does NOT weight the
// division. Two saturating cuBLAS sandboxes given a 16:1 timeslice ratio
// (8000 us vs 500 us) split the GPU 218.2 vs 218.2 matmul/s -- exactly 1:1. The
// GSP-managed scheduler on this generation does not apportion engine time by
// per-TSG software timeslice. It may still work on pre-GSP architectures (the
// mechanism is per-TSG in the runlist; cf. Bakita & Anderson ECRTS'25 Fig. 4,
// measured on GA100). Left off by default (setTimesliceUs == 0) and gated only
// by an operator flag, never a container annotation.
//
// Issued on the Sentry's own behalf, same shape and privilege reasoning as
// preemptChannelGroup.
func setChannelGroupTimeslice(fd *frontendFD, hClient, hObject nvgpu.Handle, timesliceUs uint64) error {
	ctrlParams := nvgpu.NVA06C_CTRL_SET_TIMESLICE_PARAMS{
		TimesliceUs: timesliceUs,
	}
	defer runtime.KeepAlive(&ctrlParams) // since we convert to non-pointer-typed P64
	ioctlParams := nvgpu.NVOS54_PARAMETERS{
		HClient:    hClient,
		HObject:    hObject,
		Cmd:        nvgpu.NVA06C_CTRL_CMD_SET_TIMESLICE,
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

// setChannelGroupInterleaveLevel sets the runlist interleave level (LOW/MEDIUM/
// HIGH) of channel group hObject. A HIGH-level TSG is interleaved into the
// runlist between lower-level ones, so it is scheduled more often -- a coarse
// (3-level) priority knob distinct from the per-TSG timeslice.
//
// MEASURED admin-gated on a Blackwell RTX 5070 (driver 610.43.02, 2026-08-14):
// unlike SET_TIMESLICE (non-privileged), this control returns
// NV_ERR_INSUFFICIENT_PERMISSIONS (0x1b) when originated by the Sentry -- the
// same wall as SET_TPC_PARTITION_TABLE. The Sentry deliberately holds no
// CAP_SYS_ADMIN, so it cannot set it, and its efficacy for weighting the
// division is therefore untestable here. Kept off by default; the flag is not
// container-overridable. Issued on the Sentry's own behalf, same shape as
// preemptChannelGroup.
func setChannelGroupInterleaveLevel(fd *frontendFD, hClient, hObject nvgpu.Handle, level uint32) error {
	ctrlParams := nvgpu.NVA06C_CTRL_INTERLEAVE_LEVEL_PARAMS{TsgInterleaveLevel: level}
	defer runtime.KeepAlive(&ctrlParams)
	ioctlParams := nvgpu.NVOS54_PARAMETERS{
		HClient:    hClient,
		HObject:    hObject,
		Cmd:        nvgpu.NVA06C_CTRL_CMD_SET_INTERLEAVE_LEVEL,
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

// scheduleChannelGroup adds the channel group hObject to the GPU's runlist, or
// removes it from the runlist when enable is false.
//
// This is the lever that revoking a mapping is not. Revocation stops the
// sandbox submitting only insofar as submission writes to a command buffer the
// Sentry has unmapped, and a workload replaying a captured CUDA graph does not
// write one: it faults on nothing, so it is neither seen nor stopped. Removing
// the channel group from the runlist is decided entirely by a timer in the
// Sentry and enforced by the GPU's own scheduler, so it holds regardless of
// what the sandbox does or does not touch.
//
// It is issued on the Sentry's own behalf, with the same shape and the same
// reasoning about privilege as preemptChannelGroup above.
func scheduleChannelGroup(fd *frontendFD, hClient, hObject nvgpu.Handle, enable bool) error {
	var ctrlParams nvgpu.NVA06C_CTRL_GPFIFO_SCHEDULE_PARAMS
	if enable {
		ctrlParams.BEnable = 1
	}
	defer runtime.KeepAlive(&ctrlParams) // since we convert to non-pointer-typed P64
	ioctlParams := nvgpu.NVOS54_PARAMETERS{
		HClient:    hClient,
		HObject:    hObject,
		Cmd:        nvgpu.NVA06C_CTRL_CMD_GPFIFO_SCHEDULE,
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
