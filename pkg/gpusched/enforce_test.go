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
	"os"
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

func TestEnforceIdleGetsMinTimeslice(t *testing.T) {
	// Two active, weights 1 and 1: both minTimesliceUs.
	_, st := enforcePlan([]EnforceClient{
		{PID: 10, Weight: 1}, {PID: 20, Weight: 1},
	}, nil)
	// 20 goes idle -> it is NOT detached; it stays attached with the minimum
	// timeslice (the GSP hands its empty slice to 10 on its own). Since 20 was
	// already at minTimesliceUs, nothing changes -> silent.
	cmds, st := enforcePlan([]EnforceClient{
		{PID: 10, Weight: 1}, {PID: 20, Weight: 1, Idle: true},
	}, st)
	if len(cmds) != 0 {
		t.Fatalf("idle with equal weight should be silent, got %+v", cmds)
	}
	// 20 becomes active again with weight 3 vs 10's weight 1: the ratio among
	// the two active sandboxes is 3:1, none detached.
	cmds, _ = enforcePlan([]EnforceClient{
		{PID: 10, Weight: 1}, {PID: 20, Weight: 3},
	}, st)
	want := []enforceCmd{{op: "ts", pid: 20, us: 3 * minTimesliceUs}}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("got %+v, want %+v (20 -> 3x, no detach)", cmds, want)
	}
}

func TestEnforceNeverDetaches(t *testing.T) {
	// Even a long-idle sandbox is never detached -- only re-timesliced.
	cmds, _ := enforcePlan([]EnforceClient{{PID: 10, Weight: 1, Idle: true}}, nil)
	for _, c := range cmds {
		if c.op == "detach" || c.op == "attach" {
			t.Fatalf("plan must not detach/attach, got %+v", cmds)
		}
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

func TestPollActiveParsesDriverLines(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/gpusched"
	// The driver's read format: "pid <p> active <0|1>".
	if err := os.WriteFile(p, []byte("pid 100 active 1\npid 200 active 0\ngarbage\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e := &ProcfsEnforcer{Path: p}
	got, err := e.PollActive()
	if err != nil {
		t.Fatalf("PollActive: %v", err)
	}
	if !got[100] || got[200] {
		t.Fatalf("parsed %v, want {100:true,200:false}", got)
	}
	// Only pids with an "active" line are reported; a pid never mentioned is
	// absent (the caller treats absent as "keep the previous state").
	if _, ok := got[999]; ok {
		t.Fatalf("unexpected pid in %v", got)
	}
}

func osWriteFile(p string, b []byte) error { return os.WriteFile(p, b, 0644) }
func osReadFile(p string) ([]byte, error)  { return os.ReadFile(p) }
