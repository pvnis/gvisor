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
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/log"
)

// Server divides each of a host's GPUs between the sandboxes using it.
//
// It runs outside every sandbox, which is what lets it do what a sandbox
// cannot: see how many others are competing for the same device and how much of
// it each is taking.
//
// One Scheduler is kept per device, so that sandboxes placed on different GPUs
// are not held to shares of each other. A sandbox using several devices is
// registered with each of them, since a device that did not count it would hand
// out more of itself than it has.
type Server struct {
	period time.Duration

	mu sync.Mutex

	// scheds divides one device each, and is grown as devices are used. A
	// device with no sandboxes on it is dropped rather than scheduled empty.
	scheds map[DeviceID]*Scheduler

	conns map[ID]*serverConn

	// prevGrants are the windows that were in effect on each device over the
	// period just ended, which is what the usage reported for it must be judged
	// against. Charging it against the windows being computed for the next
	// period would invent an overrun whenever a sandbox's share shrank.
	prevGrants map[DeviceID]map[ID]Grant

	// pids maps a sandbox to its process on the host, as announced by runsc.
	// It is kept apart from the connections because the announcement and the
	// sandbox's own connection arrive independently, in either order.
	pids map[ID]int

	// devNames maps a sandbox to the GPUs runsc said it was given, in whatever
	// form they were allocated to it. They are resolved against the table each
	// time the registrations are reconciled rather than on arrival, for the
	// same reason as pids: the announcement may arrive after the sandbox has
	// already been registered, and must then move it.
	devNames map[ID][]string

	// table resolves those names onto devices, and is nil on a host whose GPUs
	// could not be enumerated. Every sandbox is then on AnyDevice, which
	// schedules them all together as though they shared one GPU.
	table *DeviceTable

	// sampler measures what each sandbox actually took from each GPU, and is
	// nil if nothing can. Without it a sandbox is credited with the window it
	// was given whenever it submitted anything, which cannot distinguish a
	// sandbox that used its window from one that ran far past it.
	sampler Sampler
}

// SetSampler makes the scheduler judge sandboxes by what they took from the GPU
// rather than by whether they submitted anything.
func (s *Server) SetSampler(sampler Sampler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sampler = sampler
}

// SetDeviceTable tells the scheduler which GPUs the host has, so that sandboxes
// on different ones are scheduled apart.
func (s *Server) SetDeviceTable(table *DeviceTable) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.table = table
}

// serverConn is one connected sandbox.
type serverConn struct {
	id   ID
	conn *Conn

	// weight and maxFraction are what the sandbox asked for, kept so that it
	// can be registered again if the device it is on turns out to be another.
	weight      uint64
	maxFraction float64

	// devices are the devices this sandbox is currently registered with. It
	// starts empty and is reconciled against what runsc announced.
	devices []DeviceID

	// submissions counts what the sandbox has reported since the last period.
	// It is written by the connection's reader and read by the scheduling tick.
	submissions atomic.Uint64

	// lastAllowance is the window this sandbox was most recently given on each
	// device, used to turn "it submitted something" into an amount of time
	// used. A device it has just joined counts as a whole period, so that a
	// sandbox's first report counts as activity rather than as having used
	// nothing.
	lastAllowance map[DeviceID]time.Duration

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
		period:     period,
		scheds:     make(map[DeviceID]*Scheduler),
		conns:      make(map[ID]*serverConn),
		prevGrants: make(map[DeviceID]map[ID]Grant),
		pids:       make(map[ID]int),
		devNames:   make(map[ID][]string),
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
		// runsc reporting where a sandbox lives on the host and which GPUs it
		// was given. It may arrive before or after the sandbox itself connects,
		// so it is recorded separately and read when the windows are computed.
		s.mu.Lock()
		if hello.PID != 0 {
			s.pids[ID(hello.ID)] = hello.PID
		}
		// Devices accumulate rather than replace. A sandbox holds the containers
		// of a pod, which are started one at a time and announced as they are,
		// so each announcement describes a part of what the sandbox holds. They
		// share one gate, so the sandbox competes for all of it.
		for _, dev := range hello.Devices {
			if !slices.Contains(s.devNames[ID(hello.ID)], dev) {
				s.devNames[ID(hello.ID)] = append(s.devNames[ID(hello.ID)], dev)
			}
		}
		s.mu.Unlock()
		return
	}
	sc := &serverConn{
		id:            ID(hello.ID),
		conn:          conn,
		weight:        hello.Weight,
		maxFraction:   hello.MaxFraction,
		lastAllowance: make(map[DeviceID]time.Duration),
		pid:           hello.PID,
	}

	s.mu.Lock()
	// A sandbox reconnecting under an ID already present replaces it, rather
	// than being refused and left unscheduled.
	if old, ok := s.conns[sc.id]; ok {
		s.deregisterLocked(old)
	}
	s.conns[sc.id] = sc
	s.registerLocked(sc, s.devicesForLocked(sc.id))
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		// Only forget this connection if it has not already been replaced.
		if s.conns[sc.id] == sc {
			delete(s.conns, sc.id)
			s.deregisterLocked(sc)
			delete(s.pids, sc.id)
			delete(s.devNames, sc.id)
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

// devicesForLocked returns the devices a sandbox is using, which is AnyDevice
// until runsc has said otherwise.
//
// Preconditions: s.mu must be locked.
func (s *Server) devicesForLocked(id ID) []DeviceID {
	names, ok := s.devNames[id]
	if !ok {
		return []DeviceID{AnyDevice}
	}
	if slices.Contains(names, AllDevices) {
		return s.table.All()
	}
	return s.table.Resolve(names)
}

// registerLocked adds a sandbox to each of the given devices' schedulers.
//
// Preconditions: s.mu must be locked.
func (s *Server) registerLocked(sc *serverConn, devices []DeviceID) {
	sc.devices = devices
	for _, d := range devices {
		sched, ok := s.scheds[d]
		if !ok {
			sched = New(s.period)
			s.scheds[d] = sched
		}
		sched.Add(sc.id, sc.weight, sc.maxFraction)
		if _, ok := sc.lastAllowance[d]; !ok {
			// A device just joined counts as fully used until measured, so that
			// the sandbox is not taken for idle before it has reported once.
			sc.lastAllowance[d] = s.period
		}
	}
	// Which sandboxes ended up on which GPU is the first thing to check when a
	// division looks wrong, and nothing else reports it.
	log.Infof("Sandbox %q is competing for GPU %v", sc.id, devices)
}

// deregisterLocked removes a sandbox from every device it was registered with,
// dropping any device left with nobody on it.
//
// Preconditions: s.mu must be locked.
func (s *Server) deregisterLocked(sc *serverConn) {
	for _, d := range sc.devices {
		sched, ok := s.scheds[d]
		if !ok {
			continue
		}
		sched.Remove(sc.id)
		if sched.Len() == 0 {
			delete(s.scheds, d)
			delete(s.prevGrants, d)
		}
		delete(sc.lastAllowance, d)
	}
	sc.devices = nil
}

// reconcileLocked moves sandboxes onto the devices runsc has since said they
// are using.
//
// A sandbox is registered when it connects, which may be before runsc has
// announced anything about it, so its first periods are spent on AnyDevice.
// Rehoming it here costs it at most one period on the wrong device rather than
// requiring the two announcements to arrive in a particular order.
//
// Preconditions: s.mu must be locked.
func (s *Server) reconcileLocked() {
	for id, sc := range s.conns {
		want := s.devicesForLocked(id)
		if slices.Equal(want, sc.devices) {
			continue
		}
		s.deregisterLocked(sc)
		s.registerLocked(sc, want)
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
	s.reconcileLocked()

	var measured Usage
	if s.sampler != nil {
		measured = s.sampler.Sample()
	}

	// Take what each sandbox did with its last window. A sandbox that submitted
	// nothing is idle, and its share goes to one that will use it. Submissions
	// are counted per sandbox rather than per device, because the sandbox has
	// one gate covering all of them.
	idle := make(map[ID]bool, len(s.conns))
	for id, sc := range s.conns {
		if sc.submissions.Swap(0) > 0 {
			sc.idleTicks = 0
		} else {
			sc.idleTicks++
		}
		idle[id] = sc.idleTicks >= idleTicksBeforeYielding
	}

	grants := make(map[DeviceID]map[ID]Grant, len(s.scheds))
	for d, sched := range s.scheds {
		for id, sc := range s.conns {
			if !slices.Contains(sc.devices, d) {
				continue
			}
			if idle[id] {
				sched.Observe(id, 0)
				continue
			}
			// It is using the GPU. Prefer what the driver says it took from
			// this device, which is the only account that includes work still
			// running after the window closed; fall back to crediting it with
			// the window it was given.
			used := sc.lastAllowance[d]
			pid := sc.pid
			if p, ok := s.pids[id]; ok {
				// What runsc announced is authoritative: a sandbox reporting
				// its own process ID reports the one it has inside its
				// namespace.
				pid = p
			}
			if util, ok := measured.Of(d, pid); ok && pid != 0 {
				used = time.Duration(util * float64(s.period))
				if used <= 0 {
					// It submitted something, so it is not idle even if the GPU
					// was busy with it for too little of the period to register.
					used = time.Nanosecond
				}
			}
			sched.Observe(id, used)
		}
		grants[d] = sched.Grants()
		if len(grants[d]) > 1 {
			// Contention is exactly when a division could go wrong, and it is
			// the one thing nothing else here reports.
			for id, g := range grants[d] {
				weight := uint64(0)
				if sc, ok := s.conns[id]; ok {
					weight = sc.weight
				}
				log.Infof("gpusched: device %v client %q weight=%d idle=%v allowance=%v phase=%v", d, id, weight, idle[id], g.Allowance, g.Phase)
			}
		}
	}

	// Choose the one window each sandbox will be held to, and record it as what
	// its next report is judged against.
	windows := make(map[ID]Grant, len(s.conns))
	conns := make(map[ID]*serverConn, len(s.conns))
	for id, sc := range s.conns {
		for _, d := range sc.devices {
			if g, ok := grants[d][id]; ok {
				sc.lastAllowance[d] = g.Allowance
			}
		}
		if g, ok := narrowest(grants, sc); ok {
			windows[id] = g
		}
		conns[id] = sc
	}

	for d, sched := range s.scheds {
		sched.Settle(s.prevGrants[d])
	}
	s.prevGrants = grants
	s.mu.Unlock()

	// Send outside the lock: a sandbox that has stopped reading must not hold
	// up the scheduling of the others.
	for id, sc := range conns {
		g, ok := windows[id]
		if !ok {
			continue
		}
		if err := sc.conn.SendAssignment(AssignmentOf(g)); err != nil {
			sc.conn.Close()
		}
	}
}

// narrowest returns the smallest window a sandbox was granted across the
// devices it is using.
//
// A sandbox has one gate, which stops it submitting to every GPU it holds at
// once, so it can be given only one window however many devices it is on. The
// smallest is the only safe choice: a larger one would let it exceed its share
// of whichever device granted the smallest. The cost is that a sandbox sharing
// one GPU and holding another to itself is held to the shared device's window
// on both, which leaves the unshared one idle for the remainder of each period.
func narrowest(grants map[DeviceID]map[ID]Grant, sc *serverConn) (Grant, bool) {
	var best Grant
	var found bool
	for _, d := range sc.devices {
		g, ok := grants[d][sc.id]
		if !ok {
			continue
		}
		if !found || g.Allowance < best.Allowance {
			best, found = g, true
		}
	}
	return best, found
}

// Len returns the number of connected sandboxes.
func (s *Server) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// Devices returns the number of GPUs currently being scheduled.
func (s *Server) Devices() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.scheds)
}
