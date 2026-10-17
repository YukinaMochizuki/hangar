package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

const placementBound = "at most 64 printable characters, or it is reported empty"

// bindPlacement defines the placement flags on fs, which fill the returned
// Placement as fs parses them.
func bindPlacement(fs *flag.FlagSet) *computev1alpha1.Placement {
	p := &computev1alpha1.Placement{}
	fs.StringVar(&p.Region, "region", "", "the region the host is in, such as ap-southeast, which the machines on it report; "+placementBound)
	fs.StringVar(&p.Site, "site", "", "the site within the region the host is in, such as SGP02, which the machines on it report; "+placementBound)
	fs.StringVar(&p.Chassis, "chassis", "", "names the host's chassis, a DNS-1123 label (at most 63 lowercase letters, digits and '-', starting and ending with a letter or digit) such as generic-1u-2x-u2, which a client draws the machines on it with; empty, or one that is not a label, is reported empty and drawn as a generic server")
	fs.StringVar(&p.CpuGeneration, "cpu-generation", "", "the CPU's microarchitecture and code name, such as \"Zen 3 (Milan)\"; empty for what the agent knows of the CPU; "+placementBound)
	fs.StringVar(&p.Storage, "storage", "", "the host's disks as the machines on it report them, such as \"2 × NVMe SSD\"; empty for the physical disks the agent finds; "+placementBound)
	fs.StringVar(&p.Memory, "memory", "", "the kind of the host's memory, never its size, such as \"DDR4-3200 ECC\"; empty for what the agent reads of the SMBIOS table as it starts; "+placementBound)
	fs.StringVar(&p.Network, "network", "", "the host's Ethernet ports by the most speed each supports, such as \"2 × 10 GbE\"; empty for what their drivers say as the agent starts; "+placementBound)
	return p
}

// placementProblems says, by flag, what of p the node would not report as
// given; nothing when it would report all of it.
func placementProblems(p *computev1alpha1.Placement) []string {
	var out []string
	for _, f := range computeapi.PlacementFields(p) {
		if err := computeapi.CheckPlacementValue(f.Name, *f.Value); err != nil {
			out = append(out, fmt.Sprintf("--%s: %v", strings.ReplaceAll(f.Name, "_", "-"), err))
		}
	}
	return out
}

// usage writes hangar-node's usage, the agent's flags in fs among it. It
// names check-placement, which deploy/host looks for before it runs it: a
// hangar-node without it would start an agent instead.
func usage(fs *flag.FlagSet) {
	_, _ = fmt.Fprintln(fs.Output(), "usage: hangar-node [flags]\n       hangar-node check-placement [placement flags]")
	fs.PrintDefaults()
}

// checkPlacementMain is hangar-node check-placement: it parses the
// placement flags in args alone and exits 1, saying why on stderr, unless
// the node would report every one as given; 2 for flags it cannot parse.
func checkPlacementMain(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("check-placement", flag.ContinueOnError)
	fs.SetOutput(stderr)
	p := bindPlacement(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "check-placement takes flags alone, not %q\n", fs.Args())
		return 2
	}
	problems := placementProblems(p)
	for _, pr := range problems {
		_, _ = fmt.Fprintln(stderr, pr)
	}
	if len(problems) > 0 {
		return 1
	}
	return 0
}
