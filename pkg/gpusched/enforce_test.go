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
	"fmt"
	"os"
	"reflect"
	"strings"
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

func TestEnforceTimesliceIndependentOfIdle(t *testing.T) {
	// Weights 3 and 1: the larger gets 3x, the smaller the floor. This is the
	// steady division that must be held.
	cmds, st := enforcePlan([]EnforceClient{
		{PID: 10, Weight: 3}, {PID: 20, Weight: 1},
	}, nil)
	want := []enforceCmd{
		{op: "ts", pid: 10, us: 3 * minTimesliceUs},
		{op: "ts", pid: 20, us: minTimesliceUs},
	}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("initial plan got %+v, want %+v", cmds, want)
	}
	// 20 reads idle for a sample. Because the timeslice is a function of the
	// configured weights and not of the idle signal, NOTHING changes -- the
	// larger tenant keeps its 3x slice rather than collapsing to the floor,
	// which is the whole point: a busy tenant's neighbour blipping idle must
	// not rescale it. The GSP hands 20's empty slice to 10 on its own.
	cmds, st = enforcePlan([]EnforceClient{
		{PID: 10, Weight: 3}, {PID: 20, Weight: 1, Idle: true},
	}, st)
	if len(cmds) != 0 {
		t.Fatalf("a neighbour reading idle must not change any timeslice, got %+v", cmds)
	}
	// And 20 coming back active is likewise silent: its slice never moved.
	cmds, _ = enforcePlan([]EnforceClient{
		{PID: 10, Weight: 3}, {PID: 20, Weight: 1},
	}, st)
	if len(cmds) != 0 {
		t.Fatalf("idle->active with unchanged weights must be silent, got %+v", cmds)
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

func (f *fakeEnforcer) SetTimeslice(pid int, gpu string, us uint64) error {
	f.calls = append(f.calls, "ts")
	return nil
}
func (f *fakeEnforcer) Detach(pid int, gpu string) error {
	f.calls = append(f.calls, "detach")
	return nil
}
func (f *fakeEnforcer) Attach(pid int, gpu string) error {
	f.calls = append(f.calls, "attach")
	return nil
}

func TestRunlistEnforcerAppliesAndRemembers(t *testing.T) {
	f := &fakeEnforcer{}
	r := newRunlistEnforcer(f)
	r.apply([]EnforceClient{{PID: 10, Weight: 1, TSGs: 1}, {PID: 20, Weight: 1, TSGs: 1}})
	if len(f.calls) != 2 { // two ts
		t.Fatalf("first apply: %v", f.calls)
	}
	f.calls = nil
	r.apply([]EnforceClient{{PID: 10, Weight: 1, TSGs: 1}, {PID: 20, Weight: 1, TSGs: 1}})
	if len(f.calls) != 0 {
		t.Fatalf("steady apply should be silent: %v", f.calls)
	}
}

func TestRunlistEnforcerReassertsPeriodically(t *testing.T) {
	f := &fakeEnforcer{}
	r := newRunlistEnforcer(f)
	// First apply issues the two timeslices.
	r.apply([]EnforceClient{{PID: 10, Weight: 1, TSGs: 1}, {PID: 20, Weight: 1, TSGs: 1}})
	// Steady ticks until just before the re-assert boundary are silent.
	for i := 0; i < reassertEveryTicks-2; i++ {
		f.calls = nil
		r.apply([]EnforceClient{{PID: 10, Weight: 1, TSGs: 1}, {PID: 20, Weight: 1, TSGs: 1}})
		if len(f.calls) != 0 {
			t.Fatalf("tick %d before re-assert should be silent: %v", i, f.calls)
		}
	}
	// The re-assert tick re-issues every timeslice even though nothing changed,
	// so a TSG the workload created since the last write is re-bound.
	f.calls = nil
	r.apply([]EnforceClient{{PID: 10, Weight: 1, TSGs: 1}, {PID: 20, Weight: 1, TSGs: 1}})
	if len(f.calls) != 2 {
		t.Fatalf("re-assert tick should re-issue both timeslices, got %v", f.calls)
	}
}

func TestPollActiveParsesDriverLines(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/gpusched"
	// The driver's read format: "pid <p> active <0|1> [tsgs <n>]". The older
	// form without the count must still parse.
	if err := os.WriteFile(p, []byte("pid 100 active 1 tsgs 6\npid 200 active 0\ngarbage\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e := &ProcfsEnforcer{Path: p}
	got, err := e.PollActive()
	if err != nil {
		t.Fatalf("PollActive: %v", err)
	}
	if !got[100].Active || got[200].Active {
		t.Fatalf("parsed %v, want {100:active,200:idle}", got)
	}
	if got[100].TSGs != 6 {
		t.Fatalf("pid 100 TSGs = %d, want 6", got[100].TSGs)
	}
	if got[200].TSGs != 0 {
		t.Fatalf("pid 200 (no tsgs field) TSGs = %d, want 0", got[200].TSGs)
	}
	// Only pids with an "active" line are reported; a pid never mentioned is
	// absent (the caller treats absent as "keep the previous state").
	if _, ok := got[999]; ok {
		t.Fatalf("unexpected pid in %v", got)
	}
}

func osWriteFile(p string, b []byte) error { return os.WriteFile(p, b, 0644) }
func osReadFile(p string) ([]byte, error)  { return os.ReadFile(p) }

// gpuFakeEnforcer records each command with the GPU it was addressed to.
type gpuFakeEnforcer struct{ calls []string }

func (f *gpuFakeEnforcer) record(op string, pid int, gpu string) error {
	f.calls = append(f.calls, fmt.Sprintf("%s %d %s", op, pid, gpu))
	return nil
}
func (f *gpuFakeEnforcer) SetTimeslice(pid int, gpu string, us uint64) error {
	return f.record("ts", pid, gpu)
}
func (f *gpuFakeEnforcer) Detach(pid int, gpu string) error { return f.record("detach", pid, gpu) }
func (f *gpuFakeEnforcer) Attach(pid int, gpu string) error { return f.record("attach", pid, gpu) }

// detachesOf returns the detach commands f recorded for pid, as "gpu" strings.
func (f *gpuFakeEnforcer) detachesOf(pid int) []string {
	var out []string
	prefix := fmt.Sprintf("detach %d ", pid)
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, strings.TrimPrefix(c, prefix))
		}
	}
	return out
}

func TestPollActiveByGPUParsesDeviceLines(t *testing.T) {
	p := t.TempDir() + "/gpusched"
	report := "pid 100 active 1 tsgs 9\n" +
		"pid 200 active 0 tsgs 2\n" +
		"dev 0000:05:00.0 pid 100 active 1 tsgs 7\n" +
		"dev 0000:06:00.0 pid 100 active 0 tsgs 2\n" +
		"dev 0000:06:00.0 pid 200 active 0 tsgs 2\n" +
		"dev notabus pid 300 active 1 tsgs 1\n"
	if err := os.WriteFile(p, []byte(report), 0644); err != nil {
		t.Fatal(err)
	}
	byPID, byGPU, err := (&ProcfsEnforcer{Path: p}).PollActiveByGPU()
	if err != nil {
		t.Fatalf("PollActiveByGPU: %v", err)
	}
	if got := byPID[100]; !got.Active || got.TSGs != 9 {
		t.Errorf("pid 100 = %+v, want active with 9 TSGs", got)
	}
	if got := byGPU["0000:05:00.0"][100]; !got.Active || got.TSGs != 7 {
		t.Errorf("pid 100 on 05:00.0 = %+v, want active with 7 TSGs", got)
	}
	if got := byGPU["0000:06:00.0"][100]; got.Active || got.TSGs != 2 {
		t.Errorf("pid 100 on 06:00.0 = %+v, want idle with 2 TSGs", got)
	}
	if len(byGPU) != 2 {
		t.Errorf("GPUs = %v, want the two valid bus IDs only", byGPU)
	}
	// A per-GPU line must not be mistaken for a per-pid one.
	if _, ok := byPID[300]; ok {
		t.Errorf("per-GPU line leaked into per-pid report: %v", byPID)
	}
}

func TestProcfsEnforcerAddressesGPU(t *testing.T) {
	p := t.TempDir() + "/gpusched"
	e := &ProcfsEnforcer{Path: p}
	for _, test := range []struct {
		do   func() error
		want string
	}{
		{func() error { return e.Detach(42, "0000:05:00.0") }, "detach 42 0000:05:00.0"},
		{func() error { return e.Attach(42, AllGPUs) }, "attach 42"},
		{func() error { return e.SetTimeslice(42, "0000:06:00.0", 4000) }, "ts 42 4000 0000:06:00.0"},
	} {
		if err := os.WriteFile(p, nil, 0644); err != nil {
			t.Fatal(err)
		}
		if err := test.do(); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); string(got) != test.want {
			t.Errorf("wrote %q, want %q", got, test.want)
		}
	}
}

// crossGPUClients is a weight-25 tenant alone on GPU A beside a 75/25 pair on
// GPU B, every tenant busy with one channel group.
func crossGPUClients() (loneA []EnforceClient, pairB []EnforceClient) {
	return []EnforceClient{{PID: 10, Weight: 25, TSGs: 1}},
		[]EnforceClient{{PID: 20, Weight: 75, TSGs: 1}, {PID: 30, Weight: 25, TSGs: 1}}
}

// TestRunlistEnforcerDividesGPUsSeparately tests that a tenant alone on its GPU
// is never taken off the runlist because of tenants on another GPU, and that
// the division on the contended GPU is addressed to that GPU.
func TestRunlistEnforcerDividesGPUsSeparately(t *testing.T) {
	const gpuA, gpuB = "0000:05:00.0", "0000:06:00.0"
	f := &gpuFakeEnforcer{}
	r := newRunlistEnforcer(f)
	loneA, pairB := crossGPUClients()
	for i := 0; i < 200; i++ {
		r.applyByGPU(map[string][]EnforceClient{gpuA: loneA, gpuB: pairB})
	}
	if d := f.detachesOf(10); len(d) != 0 {
		t.Errorf("lone tenant on %s detached %d times (on %v), want never", gpuA, len(d), d)
	}
	d := f.detachesOf(30)
	if len(d) == 0 {
		t.Fatalf("weight-25 tenant sharing %s never detached; the division is not being enforced", gpuB)
	}
	for _, gpu := range d {
		if gpu != gpuB {
			t.Errorf("tenant on %s detached on %q", gpuB, gpu)
		}
	}

	// The same tenants in one undivided planner -- what the scheduler did
	// before GPUs were enforced separately -- detach the lone tenant. This is
	// the defect the per-GPU planners fix.
	f = &gpuFakeEnforcer{}
	r = newRunlistEnforcer(f)
	for i := 0; i < 200; i++ {
		r.apply(append(append([]EnforceClient(nil), loneA...), pairB...))
	}
	if len(f.detachesOf(10)) == 0 {
		t.Errorf("undivided planner no longer detaches the lone tenant; this test no longer shows the defect")
	}
}

// TestRunlistEnforcerMultiGPUSandbox tests that a sandbox holding two GPUs,
// one of them shared, is detached only on the shared one.
func TestRunlistEnforcerMultiGPUSandbox(t *testing.T) {
	const gpuA, gpuB = "0000:05:00.0", "0000:06:00.0"
	f := &gpuFakeEnforcer{}
	r := newRunlistEnforcer(f)
	for i := 0; i < 200; i++ {
		r.applyByGPU(map[string][]EnforceClient{
			gpuA: {{PID: 10, Weight: 25, TSGs: 1}},
			gpuB: {{PID: 10, Weight: 25, TSGs: 1}, {PID: 20, Weight: 75, TSGs: 1}},
		})
	}
	d := f.detachesOf(10)
	if len(d) == 0 {
		t.Fatalf("sandbox never detached on shared %s; the division is not being enforced", gpuB)
	}
	for _, gpu := range d {
		if gpu != gpuB {
			t.Errorf("sandbox detached on %q, want only on shared %s", gpu, gpuB)
		}
	}
}

// TestEnforceClientsPerGPU tests that a sandbox on two GPUs becomes a client
// of each, idle on the one it has stopped using, and that without per-GPU
// reports it is enforced across all of them as before.
func TestEnforceClientsPerGPU(t *testing.T) {
	s := &Server{table: parseDeviceQuery("0, GPU-a, 00000000:05:00.0\n1, GPU-b, 00000000:06:00.0\n")}
	sc := &serverConn{weight: 50, devices: []DeviceID{0, 1}}
	byGPU := map[string]map[int]TenantState{
		"0000:05:00.0": {7: {Active: true, TSGs: 3}},
		"0000:06:00.0": {7: {Active: false, TSGs: 1}},
	}
	var enforce map[string][]EnforceClient
	for i := 0; i < idleTicksBeforeYielding; i++ {
		enforce = make(map[string][]EnforceClient)
		s.enforceClientsLocked(enforce, sc, 7, false, 4, byGPU)
	}
	want := map[string][]EnforceClient{
		"0000:05:00.0": {{PID: 7, Weight: 50, Idle: false, TSGs: 3}},
		"0000:06:00.0": {{PID: 7, Weight: 50, Idle: true, TSGs: 1}},
	}
	if !reflect.DeepEqual(enforce, want) {
		t.Errorf("per-GPU clients = %+v, want %+v", enforce, want)
	}

	enforce = make(map[string][]EnforceClient)
	s.enforceClientsLocked(enforce, sc, 7, false, 4, nil)
	if want := map[string][]EnforceClient{AllGPUs: {{PID: 7, Weight: 50, TSGs: 4}}}; !reflect.DeepEqual(enforce, want) {
		t.Errorf("without per-GPU reports = %+v, want %+v", enforce, want)
	}
}

// tsFakeEnforcer records each command with its GPU and, for timeslices, value.
type tsFakeEnforcer struct{ calls []string }

func (f *tsFakeEnforcer) SetTimeslice(pid int, gpu string, us uint64) error {
	f.calls = append(f.calls, fmt.Sprintf("ts %d %s %d", pid, gpu, us))
	return nil
}
func (f *tsFakeEnforcer) Detach(pid int, gpu string) error {
	f.calls = append(f.calls, fmt.Sprintf("detach %d %s", pid, gpu))
	return nil
}
func (f *tsFakeEnforcer) Attach(pid int, gpu string) error {
	f.calls = append(f.calls, fmt.Sprintf("attach %d %s", pid, gpu))
	return nil
}

// lastTimeslice returns the last timeslice written to pid on gpu, or 0.
func (f *tsFakeEnforcer) lastTimeslice(pid int, gpu string) uint64 {
	var us uint64
	prefix := fmt.Sprintf("ts %d %s ", pid, gpu)
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			fmt.Sscanf(strings.TrimPrefix(c, prefix), "%d", &us)
		}
	}
	return us
}

func (f *tsFakeEnforcer) has(cmd string) bool {
	for _, c := range f.calls {
		if c == cmd {
			return true
		}
	}
	return false
}

// TestRealtimeShortensQuantumOnItsGPU tests the lab real-time class: on the
// GPU a real-time process is on, every sandbox runs at the short quantum and
// the real-time process at its own timeslice, never detached; another GPU
// keeps the default quantum; and when the real-time process leaves, its GPU
// returns to the default.
func TestRealtimeShortensQuantumOnItsGPU(t *testing.T) {
	const gpuA, gpuB = "0000:05:00.0", "0000:06:00.0"
	const rtPID = 99
	f := &tsFakeEnforcer{}
	r := newRunlistEnforcer(f)
	r.rtOthersUs, r.rtOwnUs = 500, 4000
	byGPU := map[string][]EnforceClient{
		gpuA: {{PID: 10, Weight: 100, TSGs: 1}, {PID: 20, Weight: 100, TSGs: 2}},
		gpuB: {{PID: 30, Weight: 100, TSGs: 1}},
	}
	r.rt = map[string][]int{gpuA: {rtPID}}
	for i := 0; i < 50; i++ {
		r.applyByGPU(byGPU)
	}
	for _, pid := range []int{10, 20} {
		if got := f.lastTimeslice(pid, gpuA); got != 500 {
			t.Errorf("sandbox %d beside the real-time process: timeslice %d, want 500", pid, got)
		}
	}
	if got := f.lastTimeslice(rtPID, gpuA); got != 4000 {
		t.Errorf("real-time process timeslice %d, want 4000", got)
	}
	if got := f.lastTimeslice(30, gpuB); got != defaultCreditParams().quantumUs {
		t.Errorf("sandbox on the other GPU: timeslice %d, want the default %d", got, defaultCreditParams().quantumUs)
	}
	if f.has(fmt.Sprintf("detach %d %s", rtPID, gpuA)) {
		t.Errorf("real-time process was detached")
	}
	if f.lastTimeslice(rtPID, gpuB) != 0 {
		t.Errorf("real-time process written on a GPU it is not on")
	}

	r.rt = nil
	r.applyByGPU(byGPU)
	for _, pid := range []int{10, 20} {
		if got := f.lastTimeslice(pid, gpuA); got != defaultCreditParams().quantumUs {
			t.Errorf("after the real-time process left, sandbox %d timeslice %d, want %d", pid, got, defaultCreditParams().quantumUs)
		}
	}
}

// TestRealtimeOffByDefault tests that without the real-time settings nothing
// changes: the default quantum everywhere, and no write to a real-time pid.
func TestRealtimeOffByDefault(t *testing.T) {
	const gpuA = "0000:05:00.0"
	f := &tsFakeEnforcer{}
	r := newRunlistEnforcer(f)
	r.rt = map[string][]int{gpuA: {99}}
	r.applyByGPU(map[string][]EnforceClient{gpuA: {{PID: 10, Weight: 100, TSGs: 1}}})
	if got := f.lastTimeslice(10, gpuA); got != defaultCreditParams().quantumUs {
		t.Errorf("timeslice %d with the real-time class unset, want %d", got, defaultCreditParams().quantumUs)
	}
	if f.lastTimeslice(99, gpuA) != 0 {
		t.Errorf("real-time pid written with the real-time class unset")
	}
}

// TestRealtimeShortensUnmanagedProcesses tests that a process the scheduler
// does not manage (a sandbox started before it restarted) is shortened beside
// a real-time process and given the default quantum back when it leaves.
func TestRealtimeShortensUnmanagedProcesses(t *testing.T) {
	const gpuA = "0000:05:00.0"
	f := &tsFakeEnforcer{}
	r := newRunlistEnforcer(f)
	r.rtOthersUs, r.rtOwnUs = 500, 4000
	r.rt = map[string][]int{gpuA: {99}}
	r.rtUnmanaged = map[string][]int{gpuA: {77}}
	r.applyByGPU(map[string][]EnforceClient{})
	if got := f.lastTimeslice(77, gpuA); got != 500 {
		t.Errorf("unmanaged process beside the real-time one: %d, want 500", got)
	}
	if got := f.lastTimeslice(99, gpuA); got != 4000 {
		t.Errorf("real-time process on a GPU with no sandboxes: %d, want 4000", got)
	}
	r.rt, r.rtUnmanaged = nil, nil
	r.applyByGPU(map[string][]EnforceClient{})
	if got := f.lastTimeslice(77, gpuA); got != defaultCreditParams().quantumUs {
		t.Errorf("after the real-time process left: %d, want %d", got, defaultCreditParams().quantumUs)
	}
}
