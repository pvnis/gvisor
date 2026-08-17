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
		wantProc Proc
		wantUtil float64
		wantOK   bool
	}{
		{
			name:     "a sandbox saturating the GPU",
			line:     "    0     718623     C     99      0      -      -      -      -    runsc-sandbox",
			wantProc: Proc{Device: 0, PID: 718623},
			wantUtil: 0.99,
			wantOK:   true,
		},
		{
			name:     "a partly busy process",
			line:     "    0     718623     C     25      0      -      -      -      -    runsc-sandbox",
			wantProc: Proc{Device: 0, PID: 718623},
			wantUtil: 0.25,
			wantOK:   true,
		},
		{
			// The same process on the host's second GPU, which is a different
			// thing entirely: what it took from one device says nothing about
			// its share of another.
			name:     "a process on another GPU",
			line:     "    1     718623     C     40      0      -      -      -      -    runsc-sandbox",
			wantProc: Proc{Device: 1, PID: 718623},
			wantUtil: 0.40,
			wantOK:   true,
		},
		{
			name:     "present but with nothing executing",
			line:     "    0     718623     C      -      -      -      -      -      -    runsc-sandbox",
			wantProc: Proc{Device: 0, PID: 718623},
			wantOK:   true,
		},
		{name: "the header", line: "# gpu         pid   type     sm    mem    enc    dec"},
		{name: "the units row", line: "# Idx           #    C/G      %      %      %      %"},
		{name: "no process on the GPU", line: "    0          -     -      -      -      -      -"},
		{name: "blank", line: "   "},
	} {
		t.Run(test.name, func(t *testing.T) {
			proc, util, ok := parsePmonLine(test.line)
			if ok != test.wantOK {
				t.Fatalf("parsePmonLine(%q) ok = %v, want %v", test.line, ok, test.wantOK)
			}
			if !ok {
				return
			}
			if proc != test.wantProc || util != test.wantUtil {
				t.Errorf("parsePmonLine(%q) = %+v, util %v; want %+v, util %v",
					test.line, proc, util, test.wantProc, test.wantUtil)
			}
		})
	}
}

// TestSMISamplerForgetsDepartedProcesses tests that a process which stops being
// reported stops being counted, rather than holding a share of the GPU on the
// strength of its last reading.
func TestSMISamplerForgetsDepartedProcesses(t *testing.T) {
	fresh := Proc{Device: 0, PID: 100}
	departed := Proc{Device: 0, PID: 200}
	s := &SMISampler{
		utilization: Usage{fresh: 0.9, departed: 0.5},
		updated:     map[Proc]time.Time{fresh: time.Now(), departed: time.Now().Add(-time.Minute)},
		stale:       3 * time.Second,
	}
	got := s.Sample()
	if _, ok := got[fresh]; !ok {
		t.Errorf("a process reported just now was dropped")
	}
	if _, ok := got[departed]; ok {
		t.Errorf("a process last reported a minute ago is still counted")
	}
}

// TestServerDoesNotChargeMeasuredOverrun tests that driver-measured utilization
// is not turned into debt.
//
// A sandbox that submits one very long kernel keeps the GPU past its window, and
// charging that excess back sounds like the way to stop it taking more than its
// share. It was tried -- it is what --measure-usage did -- and it diverges:
// nvidia-smi pmon samples once a second and blames most of a contended device on
// whichever sandbox happens to be running when it samples, so that sandbox is
// charged a huge overrun and throttled into running even less (two equal pods
// split 31/618 instead of 324/324). So measurement no longer feeds debt;
// overruns are bounded by preempting the channel groups at the window's close.
// Two equally weighted sandboxes that both submit therefore get equal windows
// however their measured use differs.
func TestServerDoesNotChargeMeasuredOverrun(t *testing.T) {
	s, connect := testServer(t)
	hog := connect(t, Hello{ID: "hog", Weight: 100, PID: 111})
	victim := connect(t, Hello{ID: "victim", Weight: 100, PID: 222})
	waitForClients(t, s, 2)

	// The hog occupies the whole GPU despite being granted half of it; the victim
	// takes only what it was given. The scheduler must not turn that into debt.
	s.SetSampler(&FakeSampler{Utilization: Usage{{PID: 111}: 1.0, {PID: 222}: 0.5}})

	var hogGrant, victimGrant Grant
	for i := 0; i < 6; i++ {
		hog.SendReport(Report{Submissions: 1})
		victim.SendReport(Report{Submissions: 1})
		waitForReports(t, s, 2)
		s.Tick()
		hogGrant, victimGrant = recvGrant(t, hog), recvGrant(t, victim)
	}

	if hogGrant.Allowance != victimGrant.Allowance {
		t.Errorf("equally weighted sandboxes were granted %v and %v; measured use must not be charged back as debt",
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
	s.SetSampler(&FakeSampler{Utilization: Usage{{PID: 111}: 0.02, {PID: 222}: 0.9}})

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

// TestServerTakesPIDFromAnnouncement tests that the host process ID runsc
// reports is what a sandbox's GPU use is attributed to.
//
// A sandbox cannot report this itself: it runs in a process namespace where it
// is process 1, while the driver reports the number it has on the host. A
// sandbox that submits by ringing a doorbell faults almost never, so it reports
// no submissions and would be taken for idle; only the driver's utilization,
// keyed by the announced host PID, keeps it scheduled. Were the announced PID
// ignored, its own PID would not match the sample and it would be starved.
func TestServerTakesPIDFromAnnouncement(t *testing.T) {
	s, connect := testServer(t)

	// runsc reports where the sandbox lives, on a connection of its own.
	announce := connect(t, Hello{ID: "hog", PID: 4242, AnnounceOnly: true})
	announce.Close()

	// The sandbox connects without a process ID, as it must, and submits nothing
	// the gate can see -- it rings a doorbell.
	hog := connect(t, Hello{ID: "hog", Weight: 100})
	victim := connect(t, Hello{ID: "victim", Weight: 100, PID: 555})
	waitForClients(t, s, 2)

	// Only the sample, keyed by the announced PID, shows the hog active.
	s.SetSampler(&FakeSampler{Utilization: Usage{{PID: 4242}: 1.0}})

	var hogGrant Grant
	for i := 0; i < 6; i++ {
		victim.SendReport(Report{Submissions: 1})
		waitForReports(t, s, 1)
		s.Tick()
		hogGrant = recvGrant(t, hog)
		recvGrant(t, victim)
	}

	// Seen as active only through its announced PID, it keeps a real share; had
	// the announcement been ignored it would have been idled to the floor.
	if hogGrant.Fraction() < 0.3 {
		t.Errorf("a sandbox active only by its announced PID got %.0f%% of the GPU; want about half", hogGrant.Fraction()*100)
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
