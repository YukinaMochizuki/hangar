package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/guest/tailnet"
)

// TestTheDNSWiring: splitting points the container's resolv.conf at the
// split's resolver, which then listens; restoring points it back at the
// boot disk's resolvers, with the same search domains.
func TestTheDNSWiring(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"etc", "run/hangar"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	own := "nameserver 10.0.0.1\nsearch web.hangar.internal\n"
	source := filepath.Join(root, "run/hangar/resolv.conf")
	if err := os.WriteFile(source, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	// A fixed port, found free, so a split again listens where it did.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := l.Addr().String()
	_ = l.Close()
	d := &guestDNS{root: root, listen: listen, nameservers: []string{"10.0.0.1"}, search: []string{"web.hangar.internal"}}
	d.SetSuffix("tail1234.ts.net") // before the split: nothing to tell
	if err := d.Split(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(source); string(b) != "nameserver 127.0.0.11\nsearch web.hangar.internal\n" {
		t.Errorf("split: resolv.conf says %q", b)
	}
	if d.split == nil || len(d.split.Upstreams) != 1 || d.split.Upstreams[0] != "10.0.0.1:53" {
		t.Errorf("split: the resolver forwards to %v, want the boot disk's resolver", d.split)
	}
	if err := d.Restore(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(source); string(b) != own {
		t.Errorf("restored: resolv.conf says %q, want %q", b, own)
	}
	if d.split != nil {
		t.Error("restored: the split's resolver is still there")
	}
	if _, err := net.Dial("tcp", d.listen); err == nil {
		t.Errorf("restored: the split's resolver still listens on %s", d.listen)
	}
	// Joining again listens again, on the same address.
	if err := d.Split(); err != nil || d.split == nil {
		t.Errorf("split again: %v", err)
	}
	t.Cleanup(func() { _ = d.Restore() })
}

// TestTheTailnetStopsWithTheContainer: tailscaled runs while the
// container's process does, and is stopped once it has ended, whether the
// run ends well or not.
func TestTheTailnetStopsWithTheContainer(t *testing.T) {
	for _, fails := range []bool{false, true} {
		var during *tailnet.Tailnet
		exit, err := withTailnet(t.Context(), &guestv1.BootConfig{}, nil, func(string, ...any) {},
			func(_ *guestv1.BootConfig, tn *tailnet.Tailnet) (console.ExitStatus, error) {
				during = tn
				if tn.Stopped() {
					t.Error("the tailnet was stopped before the container ran")
				}
				if fails {
					return console.ExitStatus{Code: exitNotFound}, errors.New("no program")
				}
				return console.ExitStatus{Code: 3}, nil
			})
		if during == nil || !during.Stopped() {
			t.Errorf("the run failing %v: the tailnet was not stopped after it", fails)
		}
		if fails && (err == nil || exit.Code != exitNotFound) || !fails && (err != nil || exit.Code != 3) {
			t.Errorf("the run failing %v: %+v, %v; want its own exit", fails, exit, err)
		}
	}
}

// TestAGuestWithoutATailnetIsLeftAlone: a boot disk naming no tailnet
// starts nothing, changes neither the process nor DNS, and says OFF.
func TestAGuestWithoutATailnetIsLeftAlone(t *testing.T) {
	cfg := &guestv1.BootConfig{
		Process:     &guestv1.Process{Args: []string{"sleep"}, Env: []string{"PATH=/bin"}},
		Nameservers: []string{"10.0.0.1"}, TailscaleDisk: "/dev/vdc",
	}
	got, tn := startTailnet(t.Context(), cfg, nil, func(string, ...any) {})
	if !proto.Equal(got, cfg) {
		t.Errorf("the boot config became %v", got)
	}
	if st := tn.Report(); st.GetState() != guestv1.TailnetState_TAILNET_STATE_OFF || st.GetWantsKey() || tn.Mounted() {
		t.Errorf("%v, mounted %v; want OFF, nothing mounted", st, tn.Mounted())
	}
}
