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

	// pids maps a sandbox to its process on the host, as announced by runsc.
	// It is kept apart from the connections because the announcement and the
	// sandbox's own connection arrive independently, in either order.
	pids map[ID]int

	// sampler measures what each sandbox actually took from the GPU, and is nil
	// if nothing can. Without it a sandbox is credited with the window it was
	// given whenever it submitted anything, which cannot distinguish a sandbox
	// that used its window from one that ran far past it.
	sampler Sampler
}

// SetSampler makes the scheduler judge sandboxes by what they took from the GPU
// rather than by whether they submitted anything.
func (s *Server) SetSampler(sampler Sampler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sampler = sampler
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

	// pid is the sandbox's process on the host, by which the driver reports
	// what it used.
	pid int

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
		pids:   make(map[ID]int),
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
	if hello.AnnounceOnly {
		// runsc reporting where a sandbox lives on the host. It may arrive
		// before or after the sandbox itself connects, so it is recorded
		// separately and read when the windows are computed.
		s.mu.Lock()
		s.pids[ID(hello.ID)] = hello.PID
		s.mu.Unlock()
		return
	}
	sc := &serverConn{id: ID(hello.ID), conn: conn, lastAllowance: s.period, pid: hello.PID}

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
			delete(s.pids, sc.id)
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
	var measured map[int]float64
	if s.sampler != nil {
		measured = s.sampler.Sample()
	}
	// Take what each sandbox did with its last window. A sandbox that
	// submitted nothing is idle, and its share goes to one that will use it.
	for id, sc := range s.conns {
		if sc.submissions.Swap(0) > 0 {
			sc.idleTicks = 0
		} else {
			sc.idleTicks++
		}
		if sc.idleTicks >= idleTicksBeforeYielding {
			s.sched.Observe(id, 0)
			continue
		}
		// It is using the GPU. Prefer what the driver says it took, which is
		// the only account that includes work still running after the window
		// closed; fall back to crediting it with the window it was given.
		used := sc.lastAllowance
		pid := sc.pid
		if p, ok := s.pids[id]; ok {
			// What runsc announced is authoritative: a sandbox reporting its
			// own process ID reports the one it has inside its namespace.
			pid = p
		}
		if util, ok := measured[pid]; ok && pid != 0 {
			used = time.Duration(util * float64(s.period))
			if used <= 0 {
				// It submitted something, so it is not idle even if the GPU was
				// busy with it for too little of the period to register.
				used = time.Nanosecond
			}
		}
		s.sched.Observe(id, used)
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
