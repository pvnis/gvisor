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

// Package gpusched divides the time during which sandboxes may submit work to
// a GPU.
//
// A sandbox can be prevented from submitting work outside a window of time; see
// the compute gate in //pkg/sentry/devices/nvproxy. That mechanism can express
// a limit but not a share, because it lives inside one sandbox and a sandbox
// cannot know how many others are competing with it, nor how much of the GPU
// any of them is actually using. This package supplies what it is missing: one
// scheduler runs per GPU, outside every sandbox, and hands each of them a
// window.
//
// The scheduler is deliberately free of I/O so that it can be tested without a
// GPU. Its caller is responsible for learning which sandboxes exist, measuring
// what they used, and delivering the windows it produces.
package gpusched

import (
	"sort"
	"time"
)

const (
	// DefaultPeriod is the length of the cycle that windows are placed within.
	// It matches the period the compute gate applies them over.
	DefaultPeriod = 100 * time.Millisecond

	// MinAllowance is the window kept by a client that is not using the GPU.
	//
	// An idle client cannot be given nothing: it would have no opportunity to
	// resume, and would stay idle because it was idle. The floor is small
	// enough to cost an active neighbour little and large enough to let a
	// waking client submit and be noticed.
	MinAllowance = 5 * time.Millisecond

	// maxDebt bounds how much of an overrun is recovered, so that one very
	// long kernel cannot starve its author for many periods afterwards.
	maxDebt = DefaultPeriod
)

// ID identifies a client of the GPU, and is the container ID in practice.
type ID string

// Grant is the window within each period during which a client may submit work
// to the GPU. A client may submit from Phase until Phase+Allowance, measured
// from the start of each period.
type Grant struct {
	Period    time.Duration
	Allowance time.Duration
	Phase     time.Duration
}

// Fraction returns the share of each period covered by the grant.
func (g Grant) Fraction() float64 {
	if g.Period <= 0 {
		return 0
	}
	return float64(g.Allowance) / float64(g.Period)
}

// client is a sandbox competing for the GPU.
type client struct {
	// weight is this client's share relative to others competing with it. It
	// is meaningful only by comparison: a client of weight 200 receives twice
	// the time of one of weight 100, whichever GPU they are on and however
	// many others are present.
	weight uint64

	// max caps the window regardless of what the client's weight would earn
	// it, and is zero if uncapped. It expresses "never more than this",
	// independently of who else happens to be running.
	max time.Duration

	// used is how much GPU time the client consumed over the last period, as
	// measured by the caller. It is what makes the difference between a client
	// that holds a share and one that uses it.
	used time.Duration

	// debt is time consumed beyond what was granted, recovered from subsequent
	// grants. Work already submitted to a GPU cannot be recalled, so a client
	// that submits a long kernel runs past the end of its window; charging the
	// excess back is what keeps that from being a way to take more than its
	// share.
	debt time.Duration

	// withheld is how much of debt the last call to Grants held back, which
	// Settle deducts from debt. Keeping it separate leaves Grants free of
	// side effects, so that computing the windows twice yields the same answer
	// rather than repaying twice.
	withheld time.Duration
}

// Scheduler divides a GPU's time between the sandboxes using it.
//
// It is not safe for concurrent use; callers that need it must synchronize.
type Scheduler struct {
	period  time.Duration
	clients map[ID]*client
}

// New returns a Scheduler placing windows within periods of the given length.
// A period of zero uses DefaultPeriod.
func New(period time.Duration) *Scheduler {
	if period <= 0 {
		period = DefaultPeriod
	}
	return &Scheduler{
		period:  period,
		clients: make(map[ID]*client),
	}
}

// Add registers a client competing for the GPU with the given weight, and a cap
// on its window expressed as a fraction of each period; maxFraction of zero or
// more than one is uncapped. A weight of zero is treated as one, so that a
// client which omits it still competes.
func (s *Scheduler) Add(id ID, weight uint64, maxFraction float64) {
	if weight == 0 {
		weight = 1
	}
	var max time.Duration
	if maxFraction > 0 && maxFraction < 1 {
		max = time.Duration(float64(s.period) * maxFraction)
	}
	s.clients[id] = &client{weight: weight, max: max}
}

// Remove deregisters a client, which is how its share becomes available to the
// others.
func (s *Scheduler) Remove(id ID) {
	delete(s.clients, id)
}

// Len returns the number of registered clients.
func (s *Scheduler) Len() int {
	return len(s.clients)
}

// Observe records how much GPU time a client used over the last period.
//
// This is what distinguishes holding a share from using one: a client that is
// registered but idle keeps only MinAllowance, and the rest of its share goes
// to clients that are actually running.
func (s *Scheduler) Observe(id ID, used time.Duration) {
	c, ok := s.clients[id]
	if !ok {
		return
	}
	c.used = used
}

// Grants computes each client's window for the next period.
//
// Clients using the GPU divide the period in proportion to their weights, so a
// client running alone receives all of it and no time is reserved for clients
// that would not use it. Windows are placed end to end rather than all starting
// at zero, so that clients take turns instead of contending during the same
// part of every period and leaving the GPU idle for the rest.
func (s *Scheduler) Grants() map[ID]Grant {
	grants := make(map[ID]Grant, len(s.clients))
	if len(s.clients) == 0 {
		return grants
	}

	// Sort for a stable assignment of windows: an arbitrary order would move
	// clients between phases every period for no reason.
	ids := make([]ID, 0, len(s.clients))
	for id := range s.clients {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// Settle what each client owes for overrunning its last window, and
	// determine which are competing for this one.
	var active []ID
	var totalWeight uint64
	for _, id := range ids {
		c := s.clients[id]
		if c.used > 0 {
			active = append(active, id)
			totalWeight += c.weight
		}
	}

	// With nobody using the GPU there is nothing to divide, and holding
	// everyone to a fraction of it would only delay whoever starts first.
	if len(active) == 0 {
		for _, id := range ids {
			grants[id] = Grant{Period: s.period, Allowance: s.cap(s.clients[id], s.period), Phase: 0}
		}
		return grants
	}

	// Divide the period between the clients using it, less what they owe.
	shares := make(map[ID]time.Duration, len(active))
	var assigned time.Duration
	for _, id := range active {
		c := s.clients[id]
		share := time.Duration(uint64(s.period) * c.weight / totalWeight)
		c.withheld = 0
		if c.debt > 0 {
			if repay := min(c.debt, share-MinAllowance); repay > 0 {
				share -= repay
				c.withheld = repay
			}
		}
		share = s.cap(c, share)
		if share < MinAllowance {
			share = MinAllowance
		}
		shares[id] = share
		assigned += share
	}

	// Capping and repayment leave time unclaimed. Offer it to clients that can
	// take it, so that a limit on one does not idle the GPU rather than
	// benefiting another.
	//
	// Clients still owing time are offered it last: the point of charging an
	// overrun back is to give that time to somebody else, and returning it
	// immediately to whoever overran would make the debt meaningless. They
	// remain eligible if nobody else can use it, since holding time away from
	// the only client able to run would waste the GPU rather than share it.
	if slack := s.period - assigned; slack > 0 {
		var eligible, indebted []ID
		for _, id := range active {
			c := s.clients[id]
			if c.max != 0 && shares[id] >= c.max {
				continue
			}
			if c.debt > 0 {
				indebted = append(indebted, id)
			} else {
				eligible = append(eligible, id)
			}
		}
		if len(eligible) == 0 {
			eligible = indebted
		}
		for _, id := range eligible {
			extra := slack / time.Duration(len(eligible))
			shares[id] = s.cap(s.clients[id], shares[id]+extra)
		}
	}

	// Place the windows end to end so that clients take turns.
	var phase time.Duration
	for _, id := range active {
		grants[id] = Grant{Period: s.period, Allowance: shares[id], Phase: phase}
		phase += shares[id]
		if phase >= s.period {
			phase = 0
		}
	}

	// A client that is registered but idle keeps a floor, so that it is able
	// to resume. Its window overlaps an active client's, which costs nothing
	// while it stays idle, and lasts only until its first use is observed.
	for _, id := range ids {
		if _, ok := grants[id]; !ok {
			grants[id] = Grant{Period: s.period, Allowance: s.cap(s.clients[id], MinAllowance), Phase: 0}
		}
	}
	return grants
}

// Settle applies the accounting for the period that has just ended, whose
// windows were the given grants: it credits clients for time withheld from
// them, charges them for time they used beyond the window they actually had,
// and clears the measurements.
//
// The cycle for each period is Observe, then Grants for the period to come,
// then Settle against the windows of the period that ended. Settling against
// the windows just computed would invent an overrun whenever a client's share
// shrank.
func (s *Scheduler) Settle(grants map[ID]Grant) {
	for id, c := range s.clients {
		// Time held back in repayment reduces what is owed.
		c.debt -= c.withheld
		c.withheld = 0
		if c.debt < 0 {
			c.debt = 0
		}
		if over := c.used - grants[id].Allowance; over > 0 {
			c.debt += over
		}
		if c.debt > maxDebt {
			c.debt = maxDebt
		}
		c.used = 0
	}
}

// cap limits d to the client's ceiling.
func (s *Scheduler) cap(c *client, d time.Duration) time.Duration {
	if c.max > 0 && d > c.max {
		return c.max
	}
	if d > s.period {
		return s.period
	}
	return d
}
