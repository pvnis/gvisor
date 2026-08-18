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

package gpusched

import (
	"reflect"
	"testing"
)

func TestEnforceProportionalTimeslice(t *testing.T) {
	// Two active sandboxes, weights 3 and 1: the min-weight one anchors at
	// minTimesliceUs and the other scales up by the weight ratio.
	cmds, next := enforcePlan([]EnforceClient{
		{PID: 10, Weight: 3},
		{PID: 20, Weight: 1},
	}, nil)
	want := []enforceCmd{
		{op: "ts", pid: 10, us: 3 * minTimesliceUs},
		{op: "ts", pid: 20, us: minTimesliceUs},
	}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("got %+v, want %+v", cmds, want)
	}
	// A second identical tick is silent: nothing changed.
	cmds2, _ := enforcePlan([]EnforceClient{
		{PID: 10, Weight: 3},
		{PID: 20, Weight: 1},
	}, next)
	if len(cmds2) != 0 {
		t.Fatalf("steady state should be silent, got %+v", cmds2)
	}
}

func TestEnforceIdleDetachedAndReclaimed(t *testing.T) {
	// Start: both active.
	_, st := enforcePlan([]EnforceClient{
		{PID: 10, Weight: 1},
		{PID: 20, Weight: 1},
	}, nil)
	// 20 goes idle -> it is detached, and 10's timeslice does not change (its
	// share grows because the runlist now has only it).
	cmds, st := enforcePlan([]EnforceClient{
		{PID: 10, Weight: 1},
		{PID: 20, Weight: 1, Idle: true},
	}, st)
	want := []enforceCmd{{op: "detach", pid: 20}}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("idle detach: got %+v, want %+v", cmds, want)
	}
	// 20 wakes -> it is re-attached and its timeslice re-set.
	cmds, _ = enforcePlan([]EnforceClient{
		{PID: 10, Weight: 1},
		{PID: 20, Weight: 1},
	}, st)
	want = []enforceCmd{
		{op: "attach", pid: 20},
		{op: "ts", pid: 20, us: minTimesliceUs},
	}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("wake reattach: got %+v, want %+v", cmds, want)
	}
}

func TestEnforceTimesliceCap(t *testing.T) {
	// A weight ratio large enough to exceed maxTimesliceUs is capped.
	cmds, _ := enforcePlan([]EnforceClient{
		{PID: 10, Weight: 1000},
		{PID: 20, Weight: 1},
	}, nil)
	var got uint64
	for _, c := range cmds {
		if c.pid == 10 {
			got = c.us
		}
	}
	if got != maxTimesliceUs {
		t.Fatalf("weight 1000 timeslice = %d, want cap %d", got, maxTimesliceUs)
	}
}

func TestEnforceWeightChangeReissued(t *testing.T) {
	_, st := enforcePlan([]EnforceClient{{PID: 10, Weight: 1}, {PID: 20, Weight: 1}}, nil)
	// 10's weight rises to 4: its timeslice is re-set, 20's is unchanged.
	cmds, _ := enforcePlan([]EnforceClient{{PID: 10, Weight: 4}, {PID: 20, Weight: 1}}, st)
	want := []enforceCmd{{op: "ts", pid: 10, us: 4 * minTimesliceUs}}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("weight change: got %+v, want %+v", cmds, want)
	}
}

// fakeEnforcer records calls, to test runlistEnforcer wiring.
type fakeEnforcer struct{ calls []string }

func (f *fakeEnforcer) SetTimeslice(pid int, us uint64) error {
	f.calls = append(f.calls, "ts")
	return nil
}
func (f *fakeEnforcer) Detach(pid int) error { f.calls = append(f.calls, "detach"); return nil }
func (f *fakeEnforcer) Attach(pid int) error { f.calls = append(f.calls, "attach"); return nil }

func TestRunlistEnforcerAppliesAndRemembers(t *testing.T) {
	f := &fakeEnforcer{}
	r := newRunlistEnforcer(f)
	r.apply([]EnforceClient{{PID: 10, Weight: 3}, {PID: 20, Weight: 1}})
	if len(f.calls) != 2 { // two ts
		t.Fatalf("first apply: %v", f.calls)
	}
	f.calls = nil
	r.apply([]EnforceClient{{PID: 10, Weight: 3}, {PID: 20, Weight: 1}})
	if len(f.calls) != 0 {
		t.Fatalf("steady apply should be silent: %v", f.calls)
	}
}
