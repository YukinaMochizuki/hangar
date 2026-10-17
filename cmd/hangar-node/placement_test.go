package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
)

// TestCheckPlacement: check-placement passes what the node would report as
// given and fails, naming each flag, what it would not; it is what
// deploy/host checks the host env file with.
func TestCheckPlacement(t *testing.T) {
	for _, tt := range []struct {
		args []string
		code int
		says []string
	}{
		{[]string{"--region", "ap-southeast", "--site", "SGP02", "--chassis", "generic-1u-2x-u2",
			"--memory", "DDR4-3200 ECC", "--network", "2 × 10 GbE"}, 0, nil},
		{nil, 0, nil},
		{[]string{"--chassis", "Generic 1U", "--network", strings.Repeat("x", 65)}, 1, []string{"--chassis:", "--network:"}},
		{[]string{"--cpu-generation", "Zen\n3"}, 1, []string{"--cpu-generation:"}},
		{[]string{"--name", "n1"}, 2, nil},
		{[]string{"stray"}, 2, nil},
	} {
		var stderr bytes.Buffer
		if code := checkPlacementMain(tt.args, &stderr); code != tt.code {
			t.Errorf("%q: exit %d, want %d (%s)", tt.args, code, tt.code, stderr.String())
		}
		for _, s := range tt.says {
			if !strings.Contains(stderr.String(), s) {
				t.Errorf("%q: says %q, not %q", tt.args, stderr.String(), s)
			}
		}
		if tt.code == 0 && stderr.Len() > 0 {
			t.Errorf("%q: passed saying %q", tt.args, stderr.String())
		}
	}
}

// TestTheUsageNamesCheckPlacement: deploy/host runs check-placement only on
// a hangar-node whose usage names it.
func TestTheUsageNamesCheckPlacement(t *testing.T) {
	fs := flag.NewFlagSet("hangar-node", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)
	bindPlacement(fs)
	usage(fs)
	if !strings.Contains(out.String(), "hangar-node check-placement") || !strings.Contains(out.String(), "-chassis") {
		t.Errorf("the usage is %q", out.String())
	}
}

// TestThePlacementFlagsFillThePlacement: the agent and check-placement read
// the same flags into the same fields.
func TestThePlacementFlagsFillThePlacement(t *testing.T) {
	fs := flag.NewFlagSet("hangar-node", flag.ContinueOnError)
	p := bindPlacement(fs)
	if err := fs.Parse([]string{"--region=r", "--site=s", "--chassis=c", "--cpu-generation=g", "--storage=d", "--memory=m", "--network=n"}); err != nil {
		t.Fatal(err)
	}
	want := &computev1alpha1.Placement{Region: "r", Site: "s", Chassis: "c", CpuGeneration: "g", Storage: "d", Memory: "m", Network: "n"}
	if !proto.Equal(p, want) {
		t.Errorf("placement %v, want %v", p, want)
	}
}
