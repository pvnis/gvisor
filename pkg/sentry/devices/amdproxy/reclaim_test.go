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
	"sort"
	"testing"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
)

func TestResourcesTakeOnce(t *testing.T) {
	var r fdResources
	r.init()
	r.addQueue(1)
	r.addQueue(2)
	r.removeQueue(1)
	r.addEvent(7)
	r.addMem(0xa)
	r.addMem(0xb)
	r.mapped(0xa, []uint32{100, 200}, true)
	r.mapped(0xa, []uint32{200}, false)
	r.mapped(0xc, []uint32{100}, true) // another process's buffer: ignored
	r.removeMem(0xb)
	r.setRuntime(true)

	h := r.take()
	if len(h.queues) != 1 || h.queues[0] != 2 {
		t.Errorf("queues = %v, want [2]", h.queues)
	}
	if len(h.events) != 1 || h.events[0] != 7 {
		t.Errorf("events = %v, want [7]", h.events)
	}
	if len(h.mem) != 1 || len(h.mem[0xa]) != 1 || h.mem[0xa][0] != 100 {
		t.Errorf("mem = %v, want buffer 0xa mapped on GPU 100 only", h.mem)
	}
	if _, ok := h.mem[0xc]; ok {
		t.Error("a buffer this description did not allocate would be freed by it")
	}
	if !h.runtime {
		t.Error("runtime not reported as held")
	}
	if again := r.take(); !again.empty() {
		t.Errorf("second take = %+v, want nothing: resources would be given back twice", again)
	}
}

func alive(tgids ...kernel.ThreadID) func(kernel.ThreadID) bool {
	return func(t kernel.ThreadID) bool {
		for _, a := range tgids {
			if a == t {
				return true
			}
		}
		return false
	}
}

func TestVMHandoverWithoutSharing(t *testing.T) {
	ctx := context.Background()
	var vs vmShare
	vs.init(false)
	const gpu = 0x54c
	vs.claim(gpu, 10)

	// The owner is still running: a second process is refused, as before.
	if vs.shareFor(ctx, gpu, 20, alive(10, 20)) {
		t.Fatal("a concurrent process got the address space without sharing")
	}
	if vs.heldByGone(gpu, alive(10, 20)) {
		t.Error("heldByGone while the owner is alive")
	}

	// The owner exits: the next process takes over, and becomes the owner.
	if !vs.heldByGone(gpu, alive(20)) {
		t.Error("heldByGone false after the owner exited")
	}
	if !vs.shareFor(ctx, gpu, 20, alive(20)) {
		t.Fatal("no handover after the owner exited")
	}
	if vs.shareFor(ctx, gpu, 20, alive(20)) {
		t.Error("the new owner's own acquisition was answered locally")
	}
	if vs.shareFor(ctx, gpu, 30, alive(20, 30)) {
		t.Error("a third process got the new owner's address space while it runs")
	}
	if !vs.shareFor(ctx, gpu, 30, alive(30)) {
		t.Error("no second handover")
	}

	// A GPU nobody acquired is forwarded.
	if vs.shareFor(ctx, 0x999, 40, alive()) {
		t.Error("an unacquired GPU was answered locally")
	}
}

func TestVMSharingStillShares(t *testing.T) {
	ctx := context.Background()
	var vs vmShare
	vs.init(true)
	vs.claim(1, 10)
	if !vs.shareFor(ctx, 1, 20, alive(10, 20)) {
		t.Error("sharing on, owner alive: not shared")
	}
}

func TestSignalPageOwnedWithoutSharing(t *testing.T) {
	var es eventShare
	es.init(false)
	if !es.claimPage(10) || !es.claimPage(10) {
		t.Fatal("the first process may not propose its page")
	}
	// The driver keeps one page per KFD process in every mode, so a later
	// process's proposal must be dropped and its waits bounded.
	if es.claimPage(20) {
		t.Error("a second page would be proposed and the event refused with it")
	}
	if !es.mustPoll(20) {
		t.Error("the process without a page would wait forever")
	}
	if es.mustPoll(10) {
		t.Error("the page owner would be polled")
	}
	var got []int
	for tg := range es.denied {
		got = append(got, int(tg))
	}
	sort.Ints(got)
	if len(got) != 1 || got[0] != 20 {
		t.Errorf("denied = %v, want [20]", got)
	}
}
