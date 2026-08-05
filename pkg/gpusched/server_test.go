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
