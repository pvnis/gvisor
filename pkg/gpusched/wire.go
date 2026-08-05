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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// The protocol between a sandbox and the scheduler is a line of JSON in each
// direction. It is deliberately unremarkable: the messages are small, one is
// exchanged per period, and being able to read a capture of them is worth more
// than the bytes a compact encoding would save.

// Hello is the first message a sandbox sends, describing what it is asking for.
type Hello struct {
	// ID identifies the sandbox, and is its container ID.
	ID string `json:"id"`

	// Weight is its share relative to other sandboxes competing with it.
	Weight uint64 `json:"weight"`

	// MaxFraction caps its window as a fraction of each period, and is zero if
	// it is uncapped.
	MaxFraction float64 `json:"max_fraction,omitempty"`
}

// Report is what a sandbox sends each period, describing what it did with the
// window it was given.
type Report struct {
	// Submissions is how many times the sandbox was held before submitting
	// work since its last report. Zero means it did not use the GPU, and its
	// share can go to a sandbox that will.
	Submissions uint64 `json:"submissions"`
}

// Assignment is what the scheduler sends a sandbox in return: the window during
// which it may submit work.
type Assignment struct {
	PeriodNS    int64 `json:"period_ns"`
	AllowanceNS int64 `json:"allowance_ns"`
	PhaseNS     int64 `json:"phase_ns"`
}

// AssignmentOf returns the wire form of a grant.
func AssignmentOf(g Grant) Assignment {
	return Assignment{
		PeriodNS:    int64(g.Period),
		AllowanceNS: int64(g.Allowance),
		PhaseNS:     int64(g.Phase),
	}
}

// Grant returns the grant an assignment describes.
func (a Assignment) Grant() Grant {
	return Grant{
		Period:    time.Duration(a.PeriodNS),
		Allowance: time.Duration(a.AllowanceNS),
		Phase:     time.Duration(a.PhaseNS),
	}
}

// Conn is one end of the protocol.
type Conn struct {
	r *bufio.Reader
	w io.Writer
	c io.Closer
}

// NewConn returns a Conn speaking the protocol over rwc.
func NewConn(rwc io.ReadWriteCloser) *Conn {
	return &Conn{r: bufio.NewReader(rwc), w: rwc, c: rwc}
}

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.c.Close() }

// send writes one message.
func (c *Conn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := c.w.Write(b); err != nil {
		return fmt.Errorf("writing to the GPU scheduler: %w", err)
	}
	return nil
}

// recv reads one message into v.
func (c *Conn) recv(v any) error {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// SendHello announces a sandbox to the scheduler.
func (c *Conn) SendHello(h Hello) error { return c.send(h) }

// RecvHello reads a sandbox's announcement.
func (c *Conn) RecvHello() (Hello, error) {
	var h Hello
	err := c.recv(&h)
	return h, err
}

// SendReport reports what a sandbox did with its window.
func (c *Conn) SendReport(r Report) error { return c.send(r) }

// RecvReport reads such a report.
func (c *Conn) RecvReport() (Report, error) {
	var r Report
	err := c.recv(&r)
	return r, err
}

// SendAssignment gives a sandbox its window.
func (c *Conn) SendAssignment(a Assignment) error { return c.send(a) }

// RecvAssignment reads the window the scheduler has given.
func (c *Conn) RecvAssignment() (Assignment, error) {
	var a Assignment
	err := c.recv(&a)
	return a, err
}
