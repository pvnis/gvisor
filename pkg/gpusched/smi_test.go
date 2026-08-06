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
	"testing"
	"time"
)

// TestParsePmonLine tests reading the rows of "nvidia-smi pmon -s u", including
// the forms that are not measurements.
func TestParsePmonLine(t *testing.T) {
	for _, test := range []struct {
		name     string
		line     string
		wantPID  int
		wantUtil float64
		wantOK   bool
	}{
		{
			name:     "a sandbox saturating the GPU",
			line:     "    0     718623     C     99      0      -      -      -      -    runsc-sandbox",
			wantPID:  718623,
			wantUtil: 0.99,
			wantOK:   true,
		},
		{
			name:     "a partly busy process",
			line:     "    0     718623     C     25      0      -      -      -      -    runsc-sandbox",
			wantPID:  718623,
			wantUtil: 0.25,
			wantOK:   true,
		},
		{
			name:    "present but with nothing executing",
			line:    "    0     718623     C      -      -      -      -      -      -    runsc-sandbox",
			wantPID: 718623,
			wantOK:  true,
		},
		{name: "the header", line: "# gpu         pid   type     sm    mem    enc    dec"},
		{name: "the units row", line: "# Idx           #    C/G      %      %      %      %"},
		{name: "no process on the GPU", line: "    0          -     -      -      -      -      -"},
		{name: "blank", line: "   "},
	} {
		t.Run(test.name, func(t *testing.T) {
			pid, util, ok := parsePmonLine(test.line)
			if ok != test.wantOK {
				t.Fatalf("parsePmonLine(%q) ok = %v, want %v", test.line, ok, test.wantOK)
			}
			if !ok {
				return
			}
			if pid != test.wantPID || util != test.wantUtil {
				t.Errorf("parsePmonLine(%q) = pid %d, util %v; want pid %d, util %v",
					test.line, pid, util, test.wantPID, test.wantUtil)
			}
		})
	}
}

// TestSMISamplerForgetsDepartedProcesses tests that a process which stops being
// reported stops being counted, rather than holding a share of the GPU on the
// strength of its last reading.
func TestSMISamplerForgetsDepartedProcesses(t *testing.T) {
	s := &SMISampler{
		utilization: map[int]float64{100: 0.9, 200: 0.5},
		updated:     map[int]time.Time{100: time.Now(), 200: time.Now().Add(-time.Minute)},
		stale:       3 * time.Second,
	}
	got := s.Sample()
	if _, ok := got[100]; !ok {
		t.Errorf("a process reported just now was dropped")
	}
	if _, ok := got[200]; ok {
		t.Errorf("a process last reported a minute ago is still counted")
	}
}

// TestServerChargesMeasuredOverrun tests the reason for measuring at all.
//
// A GPU cannot be made to abandon work already submitted to it, so a sandbox
// that submits one very long kernel keeps the GPU past the end of its window.
// From inside it looks exactly like one that submitted a short kernel, so only
// what the driver reports can tell them apart, and charging the excess back is
// what stops it being a way to take more than a share.
func TestServerChargesMeasuredOverrun(t *testing.T) {
	s, connect := testServer(t)
	hog := connect(t, Hello{ID: "hog", Weight: 100, PID: 111})
	victim := connect(t, Hello{ID: "victim", Weight: 100, PID: 222})
	waitForClients(t, s, 2)

	// The hog takes the whole GPU despite being granted half of it; the victim
	// takes only what it was given.
	s.SetSampler(&FakeSampler{Utilization: map[int]float64{111: 1.0, 222: 0.5}})

	var hogGrant, victimGrant Grant
	for i := 0; i < 6; i++ {
		hog.SendReport(Report{Submissions: 1})
		victim.SendReport(Report{Submissions: 1})
		waitForReports(t, s, 2)
		s.Tick()
		hogGrant, victimGrant = recvGrant(t, hog), recvGrant(t, victim)
	}

	if hogGrant.Allowance >= victimGrant.Allowance {
		t.Errorf("the sandbox running past its window was granted %v and its neighbour %v; want the overrun charged back",
			hogGrant.Allowance, victimGrant.Allowance)
	}
}

// TestServerUsesMeasurementOverSubmission tests that a sandbox which submits
// constantly but barely occupies the GPU is not credited with its whole window.
// Counting submissions cannot tell the two apart.
func TestServerUsesMeasurementOverSubmission(t *testing.T) {
	s, connect := testServer(t)
	light := connect(t, Hello{ID: "light", Weight: 100, PID: 111})
	heavy := connect(t, Hello{ID: "heavy", Weight: 100, PID: 222})
	waitForClients(t, s, 2)

	// Both submit every period, but "light" hardly uses the GPU.
	s.SetSampler(&FakeSampler{Utilization: map[int]float64{111: 0.02, 222: 0.9}})

	var lightGrant Grant
	for i := 0; i < 4; i++ {
		light.SendReport(Report{Submissions: 1})
		heavy.SendReport(Report{Submissions: 1})
		waitForReports(t, s, 2)
		s.Tick()
		lightGrant = recvGrant(t, light)
		recvGrant(t, heavy)
	}
	// It is still using the GPU, so it must keep a window.
	if lightGrant.Allowance <= 0 {
		t.Errorf("a sandbox using little of the GPU was given no window at all")
	}
}

// TestServerWithoutSamplerStillSchedules tests that measurement is optional: a
// host where nvidia-smi cannot be run must still divide the GPU.
func TestServerWithoutSamplerStillSchedules(t *testing.T) {
	s, connect := testServer(t)
	a := connect(t, Hello{ID: "a", Weight: 100, PID: 111})
	b := connect(t, Hello{ID: "b", Weight: 100, PID: 222})
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
	if got := recvGrant(t, a).Fraction(); got > 0.6 || got < 0.4 {
		t.Errorf("without a sampler a received %.0f%%, want about half", got*100)
	}
}

// TestServerTakesPIDFromAnnouncement tests that the process ID runsc reports is
// what the measurements are attributed to.
//
// A sandbox cannot report this itself: it runs in a process namespace where it
// is process 1, while the GPU driver reports the number it has on the host.
func TestServerTakesPIDFromAnnouncement(t *testing.T) {
	s, connect := testServer(t)

	// runsc reports where the sandbox lives, on a connection of its own.
	announce := connect(t, Hello{ID: "hog", PID: 4242, AnnounceOnly: true})
	announce.Close()

	// The sandbox connects without a process ID, as it must.
	hog := connect(t, Hello{ID: "hog", Weight: 100})
	victim := connect(t, Hello{ID: "victim", Weight: 100, PID: 555})
	waitForClients(t, s, 2)

	// The announced process is taking the whole GPU despite its window.
	s.SetSampler(&FakeSampler{Utilization: map[int]float64{4242: 1.0, 555: 0.5}})

	var hogGrant, victimGrant Grant
	for i := 0; i < 6; i++ {
		hog.SendReport(Report{Submissions: 1})
		victim.SendReport(Report{Submissions: 1})
		waitForReports(t, s, 2)
		s.Tick()
		hogGrant, victimGrant = recvGrant(t, hog), recvGrant(t, victim)
	}
	if hogGrant.Allowance >= victimGrant.Allowance {
		t.Errorf("the announced process overran and was granted %v against its neighbour's %v; want the overrun charged back",
			hogGrant.Allowance, victimGrant.Allowance)
	}
}

// TestServerAnnouncementIsNotAClient tests that the connection runsc uses to
// report a process ID does not itself receive a share of the GPU.
func TestServerAnnouncementIsNotAClient(t *testing.T) {
	s, connect := testServer(t)
	a := connect(t, Hello{ID: "a", PID: 111, AnnounceOnly: true})
	a.Close()
	// Give the server a moment to process it.
	for i := 0; i < 100 && s.Len() != 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Len() != 0 {
		t.Errorf("an announcement registered %d clients, want none", s.Len())
	}
}
