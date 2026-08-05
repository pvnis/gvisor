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
	"sync"
	"sync/atomic"
	"time"
)

// Server divides a GPU between the sandboxes connected to it.
//
// One runs per GPU, outside every sandbox, which is what lets it do what a
// sandbox cannot: see how many others are competing and how much of the GPU
// each is using.
type Server struct {
	period time.Duration

	mu    sync.Mutex
	sched *Scheduler
	conns map[ID]*serverConn

	// prevGrants are the windows that were in effect over the period just
	// ended, which is what the usage reported for it must be judged against.
	// Charging it against the windows being computed for the next period would
	// invent an overrun whenever a sandbox's share shrank.
	prevGrants map[ID]Grant
}

// serverConn is one connected sandbox.
type serverConn struct {
	id   ID
	conn *Conn

	// submissions counts what the sandbox has reported since the last period.
	// It is written by the connection's reader and read by the scheduling tick.
	submissions atomic.Uint64

	// lastAllowance is the window this sandbox was most recently given, used to
	// turn "it submitted something" into an amount of time used. It starts at a
	// whole period so that a sandbox's first report counts as activity rather
	// than as having used nothing.
	lastAllowance time.Duration

	// idleTicks counts consecutive periods in which the sandbox reported
	// nothing. It is what keeps a sandbox from being judged idle by a single
	// period: a sandbox reports once per period on a clock of its own, so
	// whenever the two drift a period will pass in which nothing arrived, and
	// treating that as idleness would make its share oscillate. A sandbox
	// between kernels behaves the same way.
	idleTicks int
}

// idleTicksBeforeYielding is how many consecutive periods a sandbox must report
// nothing in before its share is given to others.
const idleTicksBeforeYielding = 3

// NewServer returns a Server scheduling in periods of the given length.
func NewServer(period time.Duration) *Server {
	if period <= 0 {
		period = DefaultPeriod
	}
	return &Server{
		period: period,
		sched:  New(period),
		conns:  make(map[ID]*serverConn),
	}
}

// Serve accepts sandboxes on l and schedules them until l is closed.
func (s *Server) Serve(l net.Listener) error {
	go s.tickLoop()
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

// handle registers one sandbox and reads its reports until it disconnects.
func (s *Server) handle(nc net.Conn) {
	conn := NewConn(nc)
	defer conn.Close()

	hello, err := conn.RecvHello()
	if err != nil || hello.ID == "" {
		return
	}
	sc := &serverConn{id: ID(hello.ID), conn: conn, lastAllowance: s.period}

	s.mu.Lock()
	// A sandbox reconnecting under an ID already present replaces it, rather
	// than being refused and left unscheduled.
	s.conns[sc.id] = sc
	s.sched.Add(sc.id, hello.Weight, hello.MaxFraction)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		// Only forget this connection if it has not already been replaced.
		if s.conns[sc.id] == sc {
			delete(s.conns, sc.id)
			s.sched.Remove(sc.id)
		}
		s.mu.Unlock()
	}()

	for {
		r, err := conn.RecvReport()
		if err != nil {
			return
		}
		sc.submissions.Add(r.Submissions)
	}
}

// tickLoop schedules one period at a time.
func (s *Server) tickLoop() {
	t := time.NewTicker(s.period)
	defer t.Stop()
	for range t.C {
		s.Tick()
	}
}

// Tick computes and distributes the windows for one period.
//
// It is exported so that the scheduling can be driven directly in tests rather
// than by waiting on a clock.
func (s *Server) Tick() {
	s.mu.Lock()
	// Take what each sandbox did with its last window. A sandbox that
	// submitted nothing is idle, and its share goes to one that will use it.
	for id, sc := range s.conns {
		if sc.submissions.Swap(0) > 0 {
			sc.idleTicks = 0
		} else {
			sc.idleTicks++
		}
		if sc.idleTicks < idleTicksBeforeYielding {
			// It is using the GPU. How much of its window it used cannot be
			// seen from here; measuring that needs the GPU's own accounting.
			s.sched.Observe(id, sc.lastAllowance)
		} else {
			s.sched.Observe(id, 0)
		}
	}
	grants := s.sched.Grants()
	conns := make(map[ID]*serverConn, len(s.conns))
	for id, sc := range s.conns {
		if g, ok := grants[id]; ok {
			sc.lastAllowance = g.Allowance
		}
		conns[id] = sc
	}
	s.sched.Settle(s.prevGrants)
	s.prevGrants = grants
	s.mu.Unlock()

	// Send outside the lock: a sandbox that has stopped reading must not hold
	// up the scheduling of the others.
	for id, sc := range conns {
		g, ok := grants[id]
		if !ok {
			continue
		}
		if err := sc.conn.SendAssignment(AssignmentOf(g)); err != nil {
			sc.conn.Close()
		}
	}
}

// Len returns the number of connected sandboxes.
func (s *Server) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}
