package main

import (
	"flag"
	"os"
	"regexp"
	"testing"
)

// TestMaxUnpackWorkReachesTheImageDriver: --max-unpack-work bounds the
// work of unpacking an image in the image driver.
func TestMaxUnpackWorkReachesTheImageDriver(t *testing.T) {
	var hf hostFlags
	fs := flag.NewFlagSet("hangar-node", flag.ContinueOnError)
	hf.bind(fs)
	if err := fs.Parse([]string{"--max-unpack-work=1234567"}); err != nil {
		t.Fatal(err)
	}
	cfg := registryConfig(hf)
	if cfg.Limits.MaxWork != 1234567 {
		t.Errorf("the image driver's MaxWork is %d, --max-unpack-work 1234567", cfg.Limits.MaxWork)
	}
	if cfg.Cache != "/var/lib/hangar/images/cache" {
		t.Errorf("the image cache is %s", cfg.Cache)
	}
}

// TestTheCmdlineIsTheKernels: the guests boot with the command line the
// kernel is built for.
func TestTheCmdlineIsTheKernels(t *testing.T) {
	env, err := os.ReadFile("../../guest/kernel/kernel.env")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^HANGAR_KERNEL_CMDLINE="([^"]*)"$`).FindSubmatch(env)
	if m == nil {
		t.Fatal("no HANGAR_KERNEL_CMDLINE in guest/kernel/kernel.env")
	}
	if string(m[1]) != defaultCmdline {
		t.Errorf("--guest-cmdline defaults to %q, guest/kernel/kernel.env says %q", defaultCmdline, m[1])
	}
}
