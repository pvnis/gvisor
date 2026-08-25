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

package amdproxy

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/log"
)

// The DBG_TRAP calls this package issues. All of them act on the Sentry's own
// KFD process, from Sentry memory, and none of them are reachable from the
// sandbox: DBG_TRAP is not in kfdFD.Ioctl's dispatch and is not in the seccomp
// allowlist for any descriptor the sandbox controls.

// dbgTrap issues one DBG_TRAP operation. params must point to Sentry memory.
func dbgTrap[Params any](hostFD int32, params *Params) (uintptr, error) {
	n, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(hostFD), uintptr(amdgpu.AMDKFD_IOC_DBG_TRAP), uintptr(unsafe.Pointer(params)))
	if errno != 0 {
		return n, errno
	}
	return n, nil
}

// enableSessionLocked opens a debug session on the Sentry's own KFD process.
//
// The kernel normally requires the caller to be ptrace-attached to the target,
// and refuses with EPERM otherwise -- but it skips that check when the target
// is the caller itself. That exemption is what makes this reachable without a
// second process, and the Sentry is in exactly that position: it is the KFD
// process for the whole sandbox, since KFD keys its kfd_process on the calling
// process's mm and the Sentry is one host process.
//
// Preconditions: ts.mu is held; ts.hostFD >= 0.
func (ts *timeSlicer) enableSessionLocked() error {
	if ts.dbgFD >= 0 {
		return nil
	}
	// The driver wants a pollable descriptor to signal the debugger on. An
	// eventfd satisfies it and nothing here ever reads it: this package drives
	// the session rather than waiting to be told about it.
	dbgFD, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return fmt.Errorf("creating an eventfd for the debug session: %w", err)
	}
	var rinfo amdgpu.KFDRuntimeInfo
	params := amdgpu.KFDIoctlDbgTrapEnableArgs{
		PID: uint32(os.Getpid()),
		Op:  amdgpu.KFD_IOC_DBG_TRAP_ENABLE,
		// Subscribe to the queue-new exception, so that it is raised where
		// this code can consume it. Without the subscription a queue keeps the
		// exception forever and can never be suspended.
		ExceptionMask: amdgpu.KFD_EC_MASK_QUEUE_NEW,
		RInfoPtr:      uint64(uintptr(unsafe.Pointer(&rinfo))),
		RInfoSize:     uint32(unsafe.Sizeof(rinfo)),
		DbgFD:         uint32(dbgFD),
	}
	_, err = dbgTrap(ts.hostFD, &params)
	runtime.KeepAlive(rinfo)
	if err != nil {
		unix.Close(dbgFD)
		// The usual cause is RUNTIME_ENABLE not having been called, which the
		// ROCm runtime does during initialisation. A workload that reached
		// CREATE_QUEUE has normally done it long since.
		return fmt.Errorf("opening a KFD debug session: %w", err)
	}
	ts.dbgFD = dbgFD
	log.Infof("amdproxy: KFD debug session open (runtime_state=%d ttmp_setup=%d); GPU time-slicing active",
		rinfo.RuntimeState, rinfo.TTMPSetup)
	return nil
}

// disableSessionLocked closes the debug session.
//
// Preconditions: ts.mu is held.
func (ts *timeSlicer) disableSessionLocked() {
	if ts.dbgFD < 0 {
		return
	}
	if ts.hostFD >= 0 {
		params := amdgpu.KFDIoctlDbgTrapEnableArgs{
			PID: uint32(os.Getpid()),
			Op:  amdgpu.KFD_IOC_DBG_TRAP_DISABLE,
		}
		if _, err := dbgTrap(ts.hostFD, &params); err != nil {
			log.Warningf("amdproxy: closing the KFD debug session: %v", err)
		}
	}
	unix.Close(ts.dbgFD)
	ts.dbgFD = -1
}

// drainEventsLocked consumes the queue-new exceptions the driver has raised.
//
// KFD flags every queue created while a debug session is open with
// EC_QUEUE_NEW, and a queue still carrying it cannot be suspended: the call
// reports that queue invalid and acts on nothing, without an error. The
// exception has to be consumed by asking for it, repeatedly, until the driver
// answers EAGAIN. Passing the same bit in the suspend call's own
// exception_mask does not clear it, and nothing in the ioctl's documentation
// says any of this.
//
// Preconditions: ts.mu is held; ts.hostFD >= 0.
func (ts *timeSlicer) drainEventsLocked() {
	for i := 0; i < maxDrainedEvents; i++ {
		params := amdgpu.KFDIoctlDbgTrapQueryDebugEventArgs{
			PID:           uint32(os.Getpid()),
			Op:            amdgpu.KFD_IOC_DBG_TRAP_QUERY_DEBUG_EVENT,
			ExceptionMask: amdgpu.KFD_EC_MASK_QUEUE_NEW,
		}
		if _, err := dbgTrap(ts.hostFD, &params); err != nil {
			if err != unix.EAGAIN {
				log.Warningf("amdproxy: draining KFD debug events: %v", err)
			}
			return
		}
	}
	log.Warningf("amdproxy: KFD kept reporting debug events after %d of them; giving up on this round", maxDrainedEvents)
}

// setQueuesLocked suspends or resumes the named queues in a single call.
//
// Preconditions: ts.mu is held; ts.hostFD >= 0; len(ids) > 0.
func (ts *timeSlicer) setQueuesLocked(ids []uint32, resume bool) error {
	// The driver writes per-queue status back into the array, so it must be
	// Sentry memory that outlives the call, and the caller's ids must not be
	// reused afterwards as if they were still plain queue ids.
	defer runtime.KeepAlive(ids)

	var err error
	if resume {
		params := amdgpu.KFDIoctlDbgTrapResumeQueuesArgs{
			PID:           uint32(os.Getpid()),
			Op:            amdgpu.KFD_IOC_DBG_TRAP_RESUME_QUEUES,
			QueueArrayPtr: uint64(uintptr(unsafe.Pointer(&ids[0]))),
			NumQueues:     uint32(len(ids)),
		}
		_, err = dbgTrap(ts.hostFD, &params)
	} else {
		// Clear the queue-new exceptions first; a queue still carrying one is
		// reported invalid and silently left running.
		ts.drainEventsLocked()
		params := amdgpu.KFDIoctlDbgTrapSuspendQueuesArgs{
			PID:           uint32(os.Getpid()),
			Op:            amdgpu.KFD_IOC_DBG_TRAP_SUSPEND_QUEUES,
			ExceptionMask: amdgpu.KFD_EC_MASK_QUEUE_NEW,
			QueueArrayPtr: uint64(uintptr(unsafe.Pointer(&ids[0]))),
			NumQueues:     uint32(len(ids)),
			GracePeriod:   dbgGracePeriod,
		}
		_, err = dbgTrap(ts.hostFD, &params)
	}
	if err != nil {
		return err
	}
	// The call's return value counts the queues it acted on; the ones it could
	// not are marked in place. A queue reported invalid here is one this
	// package believes exists and the driver does not, which is worth saying
	// even though the next period will simply try again.
	for i, id := range ids {
		if id&(amdgpu.KFD_DBG_QUEUE_ERROR_MASK|amdgpu.KFD_DBG_QUEUE_INVALID_MASK) == 0 {
			continue
		}
		reason := "the driver does not have it"
		if id&amdgpu.KFD_DBG_QUEUE_ERROR_MASK != 0 {
			reason = "the hardware reported an error"
		}
		log.Warningf("amdproxy: queue %d of %d not %s: %s", i+1, len(ids), suspendedOrResumed(resume), reason)
	}
	return nil
}

// sampleWavesLocked asks how much wave state the suspension just performed had
// to save, and records whether the sandbox was executing.
//
// This is the activity signal the scheduler is given, and it is the only one on
// this hardware that measures *execution* rather than *submission*.
//
// The obvious alternative -- the queue's read and write pointers -- measures
// submission, and gets the important case backwards. For an AQL queue the
// command processor advances the read pointer when it *consumes* a dispatch
// packet, not while the kernel that packet launched is running, so a sandbox
// executing one long kernel has wptr == rptr and no pointer movement at all.
// Reading pointers would call it idle and hand its share away while it held
// the whole device -- and since CWSR lets this package preempt mid-kernel, the
// throttling would succeed. Measured with a single 10-second dispatch: wave
// state said busy on 222 of 222 samples.
//
// The driver offers nothing better. KFD's per-process sysfs has cu_occupancy,
// which would be ideal, but it is not implemented on RDNA: rocm-smi reports
// "UNKNOWN" on both a Navi 32 and a gfx1103. DRM fdinfo carries full memory
// accounting but no drm-engine-* lines, because KFD queues bypass the DRM
// scheduler entirely. The SMI event stream is exceptional events only.
//
// The reading is one-sided. A queue with no waves resident is certainly not
// executing, so idle is never wrong; busy can be missed, because a stream of
// short kernels sometimes has nothing in flight at the instant of preemption
// (measured: gpuburn read busy on 82% of samples). The scheduler needs three
// consecutive idle periods before it yields a share, which makes a spurious
// yield a 0.18^3 event, and one wrong period costs a window rather than
// anything durable.
//
// Preconditions: ts.mu is held.
func (ts *timeSlicer) sampleWavesLocked() {
	if !ts.sessionOpenLocked() || len(ts.queues) == 0 {
		return
	}
	ts.samples++
	for id, ctlStackSize := range ts.queues {
		// The ioctl is never told how large the buffer is, so the only bound
		// on what it writes is the control stack the queue was created with.
		// Anything smaller than that would be the driver writing past the end
		// of Sentry memory.
		if ctlStackSize == 0 {
			continue
		}
		buf := ts.waveBuf(ctlStackSize)
		params := amdgpu.KFDIoctlGetQueueWaveStateArgs{
			CtlStackAddress: uint64(uintptr(unsafe.Pointer(&buf[0]))),
			QueueID:         id,
		}
		_, err := kfdIoctlOn(ts.hostFD, amdgpu.AMDKFD_IOC_GET_QUEUE_WAVE_STATE, &params)
		runtime.KeepAlive(buf)
		if err != nil {
			// Not worth a warning per period: a queue destroyed between the
			// suspend and this call is an ordinary race.
			log.Debugf("amdproxy: reading wave state of queue %d: %v", id, err)
			continue
		}
		if params.SaveAreaUsedSize > 0 {
			ts.busySamples++
			return
		}
	}
}

// waveBuf returns a scratch buffer of at least n bytes for the control stack
// the driver writes, growing the one it keeps rather than allocating each
// period.
//
// Preconditions: ts.mu is held.
func (ts *timeSlicer) waveBuf(n uint32) []byte {
	if uint32(len(ts.ctlStack)) < n {
		ts.ctlStack = make([]byte, n)
	}
	return ts.ctlStack
}

// kfdIoctlOn issues an ioctl on a host KFD descriptor. params must point to
// Sentry memory.
func kfdIoctlOn[Params any](hostFD int32, cmd amdgpu.KFDIoctl, params *Params) (uintptr, error) {
	n, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(hostFD), uintptr(cmd), uintptr(unsafe.Pointer(params)))
	if errno != 0 {
		return n, errno
	}
	return n, nil
}

func suspendedOrResumed(resume bool) string {
	if resume {
		return "resumed"
	}
	return "suspended"
}
