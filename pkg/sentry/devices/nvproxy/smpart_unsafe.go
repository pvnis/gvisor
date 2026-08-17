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

// smPartitionTPCs, when non-empty, is the set of global TPC indices every
// sandbox would be confined to: nvproxy tries to impose it on each context by
// originating the TPC-partition RM controls, so that a container is held to a
// subset of the GPU's SMs whether or not it asks to be (unlike a green context,
// which a cooperative container creates for itself).
//
// It is nil (off), because imposing it does NOT work from userspace, for a
// reason established by measurement on a Blackwell RTX 5070 (driver 610.43.02),
// down to the driver source and binary:
//
//   - imposeTPCPartitionMode (SET_TPC_PARTITION_MODE, static) SUCCEEDS -- it is
//     non-privileged, and nvproxy can originate it on the container's own client.
//   - imposeTPCPartitionTable (SET_TPC_PARTITION_TABLE), which actually assigns
//     the TPCs, is refused NV_ERR_INSUFFICIENT_PERMISSIONS (0x1b) in every
//     configuration a userspace caller can construct. RM's privilege model is
//     caller-based (pCallContext->secInfo.privLevel) over an ordered ladder;
//     os_is_administrator() == capable(CAP_SYS_ADMIN) maps a root process to
//     RS_PRIV_LEVEL_USER_ROOT, one rung below RS_PRIV_LEVEL_KERNEL (reachable
//     only by the driver/GSP itself). Measured:
//       * The Sentry (uid 0 but *deliberately without* CAP_SYS_ADMIN; CapEff has
//         no bit 21) originating the control -> 0x1b.
//       * A root+CAP_SYS_ADMIN host helper (pidfd_getfd of the Sentry's nvidiactl
//         fd) issuing the control on the container's own client -> 0x1b. A
//         USER_ROOT *caller* therefore does not satisfy the gate, so the control
//         needs either RS_PRIV_LEVEL_KERNEL or an admin-allocated *owning client*
//         -- neither reachable here.
//   - Acquiring the context share into a separately-allocated admin client fails
//     at NV_ESC_RM_DUP_OBJECT: RM gates cross-client access by RS_ACCESS share
//     policy (cliresCtrlCmdClientSetInheritedSharePolicy), and the container's
//     context is not shared with an outside client -> 0x1b. (RM performs the
//     equivalent dup of hKernelGraphicsContext across clients only from its own
//     KERNEL-priv internal channel-group-duplication path.)
//   - Client privilege is fixed at allocation from the allocating caller and has
//     no downgrade control, so "make the container's client admin, then drop"
//     is impossible; and making it admin at all would hand a hostile container a
//     fully privileged RM client (every admin-gated control), a worse posture
//     than the feature is worth.
//
// So a *hard, imposed* SM/TPC partition needs an RS_PRIV_LEVEL_KERNEL actor (a
// kernel-mode RM component / GSP), not achievable by the Sentry or a userspace
// host helper. This parallels the temporal-side ceiling (doorbell submission is
// uninterceptable): both are driver/hardware limits, not gVisor's. The
// achievable spatial mechanism is the *cooperative green context* (measured to
// partition and isolate). See SECURITY-FINDINGS.md for the full trace and the
// two remaining (large, unproven) levers. The code below is the record of what
// does and does not work.
var smPartitionTPCs []uint16 = nil

// imposeTPCPartitionMode puts a channel group into static TPC-partition mode.
// Non-privileged; succeeds on the container's own client. On its own it imposes
// nothing without a partition table (which is privileged; see above).
func imposeTPCPartitionMode(fd *frontendFD, hClient, hDevice, hChannelGroup nvgpu.Handle) error {
	params := nvgpu.NV0080_CTRL_GR_TPC_PARTITION_MODE_PARAMS{
		HChannelGroup: hChannelGroup,
		Mode:          nvgpu.NV0080_CTRL_GR_TPC_PARTITION_MODE_STATIC,
	}
	defer runtime.KeepAlive(&params)
	return rmOriginateControl(fd, hClient, hDevice, nvgpu.NV0080_CTRL_CMD_GR_SET_TPC_PARTITION_MODE, unsafe.Pointer(&params), uint32(unsafe.Sizeof(params)))
}

// imposeTPCPartitionTable confines a context share to the given global TPC
// indices. Privileged: fails NV_ERR_INSUFFICIENT_PERMISSIONS from the Sentry,
// which lacks CAP_SYS_ADMIN. See smPartitionTPCs.
func imposeTPCPartitionTable(fd *frontendFD, hClient, hContextShare nvgpu.Handle, tpcs []uint16) error {
	var params nvgpu.NV9067_CTRL_TPC_PARTITION_TABLE_PARAMS
	params.NumUsedTpc = uint16(len(tpcs))
	for i, tpc := range tpcs {
		params.TpcList[i].GlobalTpcIndex = tpc
		params.TpcList[i].LmemBlockIndex = uint16(i)
	}
	defer runtime.KeepAlive(&params)
	return rmOriginateControl(fd, hClient, hContextShare, nvgpu.NV9067_CTRL_CMD_SET_TPC_PARTITION_TABLE, unsafe.Pointer(&params), uint32(unsafe.Sizeof(params)))
}

// rmOriginateControl issues an RM control on the host on the Sentry's own
// behalf, with parameters living in Sentry memory for the duration of the call
// -- the same mechanism preemptChannelGroup and the restore path use.
func rmOriginateControl(fd *frontendFD, hClient, hObject nvgpu.Handle, cmd uint32, params unsafe.Pointer, paramsSize uint32) error {
	ioctlParams := nvgpu.NVOS54_PARAMETERS{
		HClient:    hClient,
		HObject:    hObject,
		Cmd:        cmd,
		Params:     p64FromPtr(params),
		ParamsSize: paramsSize,
	}
	if _, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(fd.hostFD), frontendIoctlCmd(nvgpu.NV_ESC_RM_CONTROL, nvgpu.SizeofNVOS54Parameters), uintptr(unsafe.Pointer(&ioctlParams))); errno != 0 {
		return errno
	}
	if ioctlParams.Status != nvgpu.NV_OK {
		return fmt.Errorf("NvStatus %#x", ioctlParams.Status)
	}
	return nil
}
