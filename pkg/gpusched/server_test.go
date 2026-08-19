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
	"net"
	"path/filepath"
	"testing"
	"time"
)

// testServer returns a Server listening on a socket in a temporary directory,
// along with a function to connect a sandbox to it.
func testServer(t *testing.T) (*Server, func(t *testing.T, h Hello) *Conn) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "sched.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	// A period long enough that the internal ticker does not fire during a
	// test; scheduling is driven by calling Tick directly.
	s := NewServer(time.Hour)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()

	connect := func(t *testing.T, h Hello) *Conn {
		t.Helper()
		nc, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatalf("dialing: %v", err)
		}
		t.Cleanup(func() { nc.Close() })
		conn := NewConn(nc)
		if err := conn.SendHello(h); err != nil {
			t.Fatalf("sending hello: %v", err)
		}
		return conn
	}
	return s, connect
}

// waitForClients blocks until the server has registered n sandboxes, since
// registration completes on the server's own goroutine.
func waitForClients(t *testing.T, s *Server, n int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if s.Len() == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server registered %d sandboxes, want %d", s.Len(), n)
}

// TestServerAssignsWindow tests the exchange end to end: a sandbox announces
// itself and is given a window.
func TestServerAssignsWindow(t *testing.T) {
	s, connect := testServer(t)
	c := connect(t, Hello{ID: "a", Weight: 100})
	waitForClients(t, s, 1)

	s.Tick()
	a, err := c.RecvAssignment()
	if err != nil {
		t.Fatalf("receiving assignment: %v", err)
	}
	if g := a.Grant(); g.Period <= 0 || g.Allowance <= 0 {
		t.Errorf("received %+v, want a usable window", g)
	}
}

// TestServerSharesBetweenSandboxes tests that two sandboxes reporting activity
// are given halves of the period that do not overlap.
func TestServerSharesBetweenSandboxes(t *testing.T) {
	s, connect := testServer(t)
	a := connect(t, Hello{ID: "a", Weight: 100})
	b := connect(t, Hello{ID: "b", Weight: 100})
	waitForClients(t, s, 2)

	// Both report every period, as a running sandbox does. The first tick
	// observes them competing; the second divides the period between them.
	for i := 0; i < 2; i++ {
		if err := a.SendReport(Report{Submissions: 1}); err != nil {
			t.Fatalf("reporting: %v", err)
		}
		if err := b.SendReport(Report{Submissions: 1}); err != nil {
			t.Fatalf("reporting: %v", err)
		}
		waitForReports(t, s, 2)
		s.Tick()
		if i == 0 {
			drain(t, a)
			drain(t, b)
		}
	}

	ga, gb := recvGrant(t, a), recvGrant(t, b)
	if ga.Fraction() > 0.6 || gb.Fraction() > 0.6 {
		t.Errorf("windows were %v and %v, want about half each", ga, gb)
	}
	// The windows must not overlap, or the sandboxes would contend.
	if ga.Phase < gb.Phase+gb.Allowance && gb.Phase < ga.Phase+ga.Allowance {
		t.Errorf("windows %+v and %+v overlap", ga, gb)
	}
}

// TestServerReclaimsFromDeparted tests that a sandbox's share returns to the
// others when it disconnects, which is what a fixed percentage cannot do.
func TestServerReclaimsFromDeparted(t *testing.T) {
	s, connect := testServer(t)
	a := connect(t, Hello{ID: "a", Weight: 100})
	b := connect(t, Hello{ID: "b", Weight: 100})
	waitForClients(t, s, 2)

	for i := 0; i < 2; i++ {
		a.SendReport(Report{Submissions: 1})
		b.SendReport(Report{Submissions: 1})
		waitForReports(t, s, 2)
		s.Tick()
		if i == 0 {
			drain(t, a)
			drain(t, b)
		}
	}
	if got := recvGrant(t, a).Fraction(); got > 0.6 {
		t.Fatalf("with two sandboxes a received %.0f%%, want about half", got*100)
	}
	drain(t, b)

	// b goes away.
	b.Close()
	waitForClients(t, s, 1)
	a.SendReport(Report{Submissions: 1})
	waitForReports(t, s, 1)
	s.Tick()
	if got := recvGrant(t, a).Fraction(); got < 0.9 {
		t.Errorf("after its neighbour left a received %.0f%%, want nearly all", got*100)
	}
}

// TestServerIdleSandboxYieldsShare tests that a connected sandbox reporting no
// activity does not hold a share away from one that is running.
func TestServerIdleSandboxYieldsShare(t *testing.T) {
	s, connect := testServer(t)
	busy := connect(t, Hello{ID: "busy", Weight: 100})
	connect(t, Hello{ID: "idle", Weight: 100})
	waitForClients(t, s, 2)

	// The idle sandbox is not judged so by a single period; tick until the
	// hysteresis expires.
	for i := 0; i < idleTicksBeforeYielding+2; i++ {
		busy.SendReport(Report{Submissions: 1})
		waitForReports(t, s, 1)
		s.Tick()
		drain(t, busy)
	}
	busy.SendReport(Report{Submissions: 1})
	waitForReports(t, s, 1)
	s.Tick()

	if got := recvGrant(t, busy).Fraction(); got < 0.9 {
		t.Errorf("active sandbox received %.0f%% beside an idle one, want nearly all", got*100)
	}
}

// waitForReports blocks until the server has taken in at least n reports.
func waitForReports(t *testing.T, s *Server, n int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		var seen int
		s.mu.Lock()
		for _, sc := range s.conns {
			if sc.submissions.Load() > 0 {
				seen++
			}
		}
		s.mu.Unlock()
		if seen >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server did not receive %d reports", n)
}

func recvGrant(t *testing.T, c *Conn) Grant {
	t.Helper()
	a, err := c.RecvAssignment()
	if err != nil {
		t.Fatalf("receiving assignment: %v", err)
	}
	return a.Grant()
}

func drain(t *testing.T, c *Conn) {
	t.Helper()
	if _, err := c.RecvAssignment(); err != nil {
		t.Fatalf("draining assignment: %v", err)
	}
}

// announce reports a sandbox's devices as runsc does, and waits for the server
// to have taken them in.
func announce(t *testing.T, s *Server, connect func(*testing.T, Hello) *Conn, id string, devices ...string) {
	t.Helper()
	c := connect(t, Hello{ID: id, Devices: devices, AnnounceOnly: true})
	defer c.Close()
	for i := 0; i < 200; i++ {
		s.mu.Lock()
		_, ok := s.devNames[ID(id)]
		s.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server did not record the devices announced for %q", id)
}

// runPeriods has both sandboxes report and the server schedule, returning the
// windows from the last period. Several are needed because the first tick only
// observes that they are competing.
func runPeriods(t *testing.T, s *Server, n int, a, b *Conn) (Grant, Grant) {
	t.Helper()
	var ga, gb Grant
	for i := 0; i < n; i++ {
		if err := a.SendReport(Report{Submissions: 1}); err != nil {
			t.Fatalf("reporting: %v", err)
		}
		if err := b.SendReport(Report{Submissions: 1}); err != nil {
			t.Fatalf("reporting: %v", err)
		}
		waitForReports(t, s, 2)
		s.Tick()
		ga, gb = recvGrant(t, a), recvGrant(t, b)
	}
	return ga, gb
}

// TestServerSchedulesDevicesApart tests the point of knowing which GPU a
// sandbox is on: two sandboxes on different devices never contend, so holding
// either to a share of the other would idle both GPUs for half of every period.
func TestServerSchedulesDevicesApart(t *testing.T) {
	s, connect := testServer(t)
	s.SetDeviceTable(parseDeviceQuery(twoGPUs))
	announce(t, s, connect, "a", "0")
	announce(t, s, connect, "b", "GPU-1c8b8535-ea22-cc36-961d-7fee0cd443e2")
	a := connect(t, Hello{ID: "a", Weight: 100})
	b := connect(t, Hello{ID: "b", Weight: 100})
	waitForClients(t, s, 2)

	ga, gb := runPeriods(t, s, 3, a, b)
	if ga.Fraction() < 0.9 || gb.Fraction() < 0.9 {
		t.Errorf("sandboxes on separate GPUs received %.0f%% and %.0f%%, want all of their own devices",
			ga.Fraction()*100, gb.Fraction()*100)
	}
	if got, want := s.Devices(), 2; got != want {
		t.Errorf("scheduling %d devices, want %d", got, want)
	}
}

// TestServerSharesOneDevice tests that naming the same GPU still divides it,
// which is what telling devices apart must not break.
func TestServerSharesOneDevice(t *testing.T) {
	s, connect := testServer(t)
	s.SetDeviceTable(parseDeviceQuery(twoGPUs))
	// The same device, named the two ways it is allocated.
	announce(t, s, connect, "a", "0")
	announce(t, s, connect, "b", "GPU-0b7a7424-d911-bb25-850c-6feddbc332d1")
	a := connect(t, Hello{ID: "a", Weight: 100})
	b := connect(t, Hello{ID: "b", Weight: 100})
	waitForClients(t, s, 2)

	ga, gb := runPeriods(t, s, 3, a, b)
	if ga.Fraction() > 0.6 || gb.Fraction() > 0.6 {
		t.Errorf("sandboxes on one GPU received %.0f%% and %.0f%%, want about half each",
			ga.Fraction()*100, gb.Fraction()*100)
	}
	if got, want := s.Devices(), 1; got != want {
		t.Errorf("scheduling %d devices, want %d", got, want)
	}
}

// TestServerHoldsMultiDeviceSandboxToItsNarrowestWindow tests that a sandbox
// holding two GPUs is counted by both and held to the smaller share.
//
// It has one gate covering every device it holds, so it can be given only one
// window. Anything wider than the smallest would let it exceed its share of
// whichever device granted that one.
func TestServerHoldsMultiDeviceSandboxToItsNarrowestWindow(t *testing.T) {
	s, connect := testServer(t)
	s.SetDeviceTable(parseDeviceQuery(twoGPUs))
	// "both" has a GPU to itself and shares another with "rival".
	announce(t, s, connect, "both", "0", "1")
	announce(t, s, connect, "rival", "1")
	both := connect(t, Hello{ID: "both", Weight: 100})
	rival := connect(t, Hello{ID: "rival", Weight: 100})
	waitForClients(t, s, 2)

	gBoth, gRival := runPeriods(t, s, 3, both, rival)
	if gBoth.Fraction() > 0.6 {
		t.Errorf("a sandbox sharing one of its two GPUs received %.0f%%, want the shared device's half",
			gBoth.Fraction()*100)
	}
	// The sandbox it shares with must not have been squeezed by the device it
	// does not hold.
	if gRival.Fraction() < 0.4 {
		t.Errorf("the sandbox sharing one GPU received %.0f%%, want about half", gRival.Fraction()*100)
	}
}

// TestServerRehomesOnAnnouncement tests that a sandbox connecting before runsc
// has said which GPU it is on is moved when the announcement arrives.
//
// The two arrive independently and in either order, so a sandbox's first
// periods may be spent scheduled against every other sandbox on the host.
func TestServerRehomesOnAnnouncement(t *testing.T) {
	s, connect := testServer(t)
	s.SetDeviceTable(parseDeviceQuery(twoGPUs))

	// Both connect before anything is known about them, so both land together.
	a := connect(t, Hello{ID: "a", Weight: 100})
	b := connect(t, Hello{ID: "b", Weight: 100})
	waitForClients(t, s, 2)
	if ga, gb := runPeriods(t, s, 3, a, b); ga.Fraction() > 0.6 || gb.Fraction() > 0.6 {
		t.Fatalf("before their devices were known the sandboxes received %.0f%% and %.0f%%, want a share each",
			ga.Fraction()*100, gb.Fraction()*100)
	}

	// runsc catches up, and they turn out to be on different GPUs.
	announce(t, s, connect, "a", "0")
	announce(t, s, connect, "b", "1")
	ga, gb := runPeriods(t, s, 3, a, b)
	if ga.Fraction() < 0.9 || gb.Fraction() < 0.9 {
		t.Errorf("after their devices were announced the sandboxes received %.0f%% and %.0f%%, want all of their own",
			ga.Fraction()*100, gb.Fraction()*100)
	}
}

// TestServerUnknownDeviceStillScheduled tests that a sandbox naming a GPU this
// host does not have is scheduled rather than dropped. It joins the sandboxes
// whose devices are unknown, which costs it a share it may not have needed but
// cannot let it take one it was not given.
func TestServerUnknownDeviceStillScheduled(t *testing.T) {
	s, connect := testServer(t)
	s.SetDeviceTable(parseDeviceQuery(twoGPUs))
	announce(t, s, connect, "a", "GPU-departed")
	a := connect(t, Hello{ID: "a", Weight: 100})
	waitForClients(t, s, 1)

	a.SendReport(Report{Submissions: 1})
	waitForReports(t, s, 1)
	s.Tick()
	if got := recvGrant(t, a).Fraction(); got <= 0 {
		t.Errorf("a sandbox naming an unknown GPU received no window at all")
	}
}

// TestServerAllDevices tests that a container permitted every GPU is counted by
// every GPU, as one given "all" by Docker is.
func TestServerAllDevices(t *testing.T) {
	s, connect := testServer(t)
	s.SetDeviceTable(parseDeviceQuery(twoGPUs))
	announce(t, s, connect, "a", AllDevices)
	announce(t, s, connect, "b", "1")
	a := connect(t, Hello{ID: "a", Weight: 100})
	b := connect(t, Hello{ID: "b", Weight: 100})
	waitForClients(t, s, 2)

	ga, gb := runPeriods(t, s, 3, a, b)
	if got, want := s.Devices(), 2; got != want {
		t.Errorf("scheduling %d devices, want %d", got, want)
	}
	// It holds both GPUs, and shares the second, so it is held to a share of
	// that one on both.
	if ga.Fraction() > 0.6 {
		t.Errorf("a sandbox holding every GPU received %.0f%%, want the shared device's half", ga.Fraction()*100)
	}
	if gb.Fraction() < 0.4 {
		t.Errorf("the sandbox sharing with it received %.0f%%, want about half", gb.Fraction()*100)
	}
}

// TestServerMeasuresPerDevice tests that what a sandbox took from one GPU is
// not charged against its share of another.
//
// A sandbox saturating a device it has to itself is behaving perfectly well;
// counting that against a device it shares would charge it an overrun it never
// committed and hand its share to its neighbour.
func TestServerMeasuresPerDevice(t *testing.T) {
	s, connect := testServer(t)
	s.SetDeviceTable(parseDeviceQuery(twoGPUs))
	announce(t, s, connect, "wide", "0", "1")
	announce(t, s, connect, "narrow", "1")
	wide := connect(t, Hello{ID: "wide", Weight: 100, PID: 111})
	narrow := connect(t, Hello{ID: "narrow", Weight: 100, PID: 222})
	waitForClients(t, s, 2)

	// "wide" saturates the GPU it holds alone and keeps within its half of the
	// one it shares. "narrow" does the same with its half.
	s.SetSampler(&FakeSampler{Utilization: Usage{
		{Device: 0, PID: 111}: 1.0,
		{Device: 1, PID: 111}: 0.5,
		{Device: 1, PID: 222}: 0.5,
	}})

	gWide, gNarrow := runPeriods(t, s, 6, wide, narrow)
	if gWide.Allowance < gNarrow.Allowance {
		t.Errorf("the sandbox saturating a GPU of its own was cut to %v against its neighbour's %v; want its own device not charged against the shared one",
			gWide.Allowance, gNarrow.Allowance)
	}
}

// TestServerToleratesAMissedReport tests that a sandbox is not judged idle by a
// single period without a report.
//
// A sandbox reports on a clock of its own, so whenever it drifts against the
// scheduler's a period will pass in which nothing arrived; a sandbox between
// kernels looks the same. Reacting to one such period would make its share
// oscillate.
func TestServerToleratesAMissedReport(t *testing.T) {
	s, connect := testServer(t)
	a := connect(t, Hello{ID: "a", Weight: 100})
	b := connect(t, Hello{ID: "b", Weight: 100})
	waitForClients(t, s, 2)

	for i := 0; i < 2; i++ {
		a.SendReport(Report{Submissions: 1})
		b.SendReport(Report{Submissions: 1})
		waitForReports(t, s, 2)
		s.Tick()
		drain(t, a)
		drain(t, b)
	}

	// A period passes in which b reports nothing.
	a.SendReport(Report{Submissions: 1})
	waitForReports(t, s, 1)
	s.Tick()
	drain(t, a)
	if got := recvGrant(t, b).Fraction(); got < 0.4 {
		t.Errorf("after one missed report b was cut to %.0f%%, want about half", got*100)
	}
}

// TestServerRetainsPIDAcrossReconnect verifies that a sandbox's host pid,
// announced once out of band by runsc, is not forgotten when the sandbox's
// persistent connection drops and reconnects. The pid is what the runlist
// enforcer keys on, and the announcement is never re-sent, so losing it on a
// transient drop would leave the sandbox scheduled by weight but never enforced
// -- the exact gap that held an automatic two-tenant division to ~1.5:1
// instead of the mechanism's proven ~3:1.
func TestServerRetainsPIDAcrossReconnect(t *testing.T) {
	s, connect := testServer(t)

	// runsc announces the sandbox's host pid out of band.
	connect(t, Hello{ID: "sb", PID: 4242, AnnounceOnly: true})
	for i := 0; i < 200; i++ {
		if s.pidFor("sb") == 4242 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.pidFor("sb"); got != 4242 {
		t.Fatalf("after announcement pid = %d, want 4242", got)
	}

	// The Sentry's persistent connection (no pid; it cannot see its host pid)
	// registers, then drops.
	c := connect(t, Hello{ID: "sb", Weight: 100})
	waitForClients(t, s, 1)
	c.Close()
	for i := 0; i < 200; i++ {
		if s.Len() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The announced pid must survive the disconnect, so a reconnecting sandbox
	// is still enforceable.
	if got := s.pidFor("sb"); got != 4242 {
		t.Fatalf("pid lost on disconnect: got %d, want 4242", got)
	}

	// It reconnects and is enforceable again with the retained pid.
	connect(t, Hello{ID: "sb", Weight: 100})
	waitForClients(t, s, 1)
	if got := s.pidFor("sb"); got != 4242 {
		t.Fatalf("after reconnect pid = %d, want 4242", got)
	}
}
