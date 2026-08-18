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

package cmd

import (
	"context"
	"net"
	"os"
	"time"

	"github.com/google/subcommands"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/gpusched"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
)

// GPUScheduler implements subcommands.Command for the "gpu-scheduler" command.
type GPUScheduler struct {
	socket         string
	period         time.Duration
	measure        bool
	runlistControl string
}

// Name implements subcommands.Command.Name.
func (*GPUScheduler) Name() string {
	return "gpu-scheduler"
}

// Synopsis implements subcommands.Command.Synopsis.
func (*GPUScheduler) Synopsis() string {
	return "divide a GPU's time between the sandboxes sharing it"
}

// Usage implements subcommands.Command.Usage.
func (*GPUScheduler) Usage() string {
	return `gpu-scheduler [flags] - divide a GPU's time between the sandboxes sharing it.

A sandbox can be stopped from submitting work to a GPU outside a window of time,
but choosing that window is not something a sandbox can do for itself: it cannot
see how many others are competing with it, nor whether they are using the GPU at
all. This command runs outside every sandbox and decides for them.

Sandboxes connect to the socket given by --socket, which they are pointed at
with the runsc flag --nvproxy-gpu-scheduler-socket. Those using the same GPU
divide each period in proportion to their weights, so one running alone receives
all of it, and they are given windows that do not overlap so that they take
turns.

Run one per host. Each of the host's GPUs is divided separately, so sandboxes
placed on different devices do not take time from one another; runsc reports
which GPUs a sandbox was given when it starts it.
`
}

// SetFlags implements subcommands.Command.SetFlags.
func (g *GPUScheduler) SetFlags(f *flag.FlagSet) {
	f.StringVar(&g.socket, "socket", "/run/runsc-gpu-scheduler.sock", "path of the socket sandboxes connect to.")
	f.DurationVar(&g.period, "period", gpusched.DefaultPeriod, "length of the cycle that windows are placed within.")
	f.BoolVar(&g.measure, "measure-usage", true, "judge sandboxes by what they take from the GPU, as reported by nvidia-smi, rather than by whether they submitted anything. Scheduling continues without it if nvidia-smi cannot be run.")
	f.StringVar(&g.runlistControl, "runlist-control", "", "path of the ghost-instrumented driver's runlist control (e.g. /proc/driver/nvidia/gpusched). When set, the scheduler divides each GPU by driving the hardware runlist -- detach/attach and per-TSG timeslice -- which binds doorbell-submission workloads (cuBLAS) the compute gate cannot. Requires the ghost driver; empty leaves enforcement to the gate.")
}

// FetchSpec implements util.SubCommand.FetchSpec.
func (g *GPUScheduler) FetchSpec(conf *config.Config, f *flag.FlagSet) (string, *specs.Spec, error) {
	// This command does not operate on a container, so there is no spec.
	return "", nil, nil
}

// Execute implements subcommands.Command.Execute.
func (g *GPUScheduler) Execute(_ context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if f.NArg() != 0 {
		f.Usage()
		return subcommands.ExitUsageError
	}

	// A socket left behind by a previous run would make listening fail.
	if err := os.Remove(g.socket); err != nil && !os.IsNotExist(err) {
		util.Fatalf("removing the existing socket %q: %v", g.socket, err)
	}
	l, err := net.Listen("unix", g.socket)
	if err != nil {
		util.Fatalf("listening on %q: %v", g.socket, err)
	}
	defer l.Close()
	// Sandboxes run as root, but need not be the same user as this process.
	if err := os.Chmod(g.socket, 0666); err != nil {
		util.Fatalf("setting permissions on %q: %v", g.socket, err)
	}

	server := gpusched.NewServer(g.period)
	// Learn what GPUs the host has, so that sandboxes placed on different ones
	// are not held to shares of each other. Without this every sandbox competes
	// with every other, which is right for a host with one GPU and merely
	// wasteful on a host with several.
	if table, err := gpusched.NewDeviceTable(); err != nil {
		log.Warningf("Scheduling every sandbox together: the host's GPUs could not be enumerated, so sandboxes on different devices cannot be told apart: %v", err)
	} else {
		server.SetDeviceTable(table)
		log.Infof("Scheduling %d GPUs separately", table.Devices())
	}
	if g.measure {
		// A sandbox cannot see how long the GPU spent on what it submitted, so
		// without this a sandbox that runs far past its window is
		// indistinguishable from one that keeps within it.
		sampler, err := gpusched.NewSMISampler()
		if err != nil {
			log.Warningf("Not measuring GPU use, so sandboxes running past their window will not be charged for it: %v", err)
		} else {
			server.SetSampler(sampler)
			log.Infof("Measuring GPU use per sandbox with nvidia-smi")
		}
	}

	if g.runlistControl != "" {
		if _, err := os.Stat(g.runlistControl); err != nil {
			log.Warningf("Runlist control %q not present, dividing the GPU by the compute gate only: %v", g.runlistControl, err)
		} else {
			server.SetEnforcer(&gpusched.ProcfsEnforcer{Path: g.runlistControl})
			log.Infof("Dividing each GPU by its hardware runlist via %q", g.runlistControl)
		}
	}

	log.Infof("GPU scheduler listening on %q with a period of %v", g.socket, g.period)
	if err := server.Serve(l); err != nil {
		util.Fatalf("serving: %v", err)
	}
	return subcommands.ExitSuccess
}
