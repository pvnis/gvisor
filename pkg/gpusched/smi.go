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
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SMISampler measures per-process GPU use by reading "nvidia-smi pmon".
//
// The sandboxes themselves cannot measure this: the work is submitted by the
// Sentry on the application's behalf, and how long the GPU spends on it is not
// visible from inside. It is visible to the host, where each sandbox appears as
// the process that opened the device, so this runs alongside the scheduler
// rather than inside anything it schedules.
type SMISampler struct {
	mu sync.Mutex
	// utilization is the most recent reading for each process on each GPU.
	utilization Usage
	// updated is when each was last reported, used to forget processes that
	// have stopped using the GPU.
	updated map[Proc]time.Time
	// stale is how long a reading is honoured before the process is treated as
	// no longer using the GPU.
	stale time.Duration
}

// NewSMISampler starts sampling per-process GPU use once a second, and returns
// a Sampler reporting the most recent reading.
//
// nvidia-smi is run once and read continuously rather than invoked per period:
// starting a process for every window would cost more than the scheduling it
// informs.
func NewSMISampler() (*SMISampler, error) {
	// -s u selects utilization; -d 1 samples once a second.
	cmd := exec.Command("nvidia-smi", "pmon", "-s", "u", "-d", "1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("reading from nvidia-smi: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting nvidia-smi: %w", err)
	}
	s := &SMISampler{
		utilization: make(Usage),
		updated:     make(map[Proc]time.Time),
		// Two sampling intervals, so that a single missed line does not make a
		// busy process look idle.
		stale: 3 * time.Second,
	}
	go s.read(out)
	return s, nil
}

// read consumes nvidia-smi's output until it ends.
func (s *SMISampler) read(r interface{ Read([]byte) (int, error) }) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		proc, util, ok := parsePmonLine(sc.Text())
		if !ok {
			continue
		}
		s.mu.Lock()
		s.utilization[proc] = util
		s.updated[proc] = time.Now()
		s.mu.Unlock()
	}
}

// parsePmonLine reads one row of "nvidia-smi pmon -s u" output, which looks
// like:
//
//	# gpu        pid  type    sm   mem   enc   dec   jpg   ofa  command
//	    0     718623     C    99     0     -     -     -     -  runsc-sandbox
//
// A process with no work executing reports "-" rather than a number. A process
// using more than one GPU is reported once per GPU, which is why the first
// column is read rather than skipped: without it the work a process did on one
// device would be charged against its share of another.
func parsePmonLine(line string) (proc Proc, util float64, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return Proc{}, 0, false
	}
	f := strings.Fields(line)
	if len(f) < 4 {
		return Proc{}, 0, false
	}
	index, err := strconv.Atoi(f[0])
	if err != nil {
		return Proc{}, 0, false
	}
	pid, err := strconv.Atoi(f[1])
	if err != nil {
		return Proc{}, 0, false
	}
	proc = Proc{Device: DeviceID(index), PID: pid}
	if f[3] == "-" {
		// Present but idle.
		return proc, 0, true
	}
	sm, err := strconv.ParseFloat(f[3], 64)
	if err != nil {
		return Proc{}, 0, false
	}
	return proc, sm / 100, true
}

// Sample implements Sampler.Sample.
func (s *SMISampler) Sample() Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make(Usage, len(s.utilization))
	for proc, u := range s.utilization {
		if now.Sub(s.updated[proc]) > s.stale {
			// The process has stopped being reported, so it is no longer using
			// the GPU; leaving its last reading in place would hold a share for
			// something that has gone.
			delete(s.utilization, proc)
			delete(s.updated, proc)
			continue
		}
		out[proc] = u
	}
	return out
}
