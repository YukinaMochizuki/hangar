package hostnet

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// postroutingOf is a table's postrouting chain.
func postroutingOf(t *testing.T, chains []chainSpec) chainSpec {
	t.Helper()
	for _, c := range chains {
		if c.name == "postrouting" {
			return c
		}
	}
	t.Fatal("no postrouting chain")
	return chainSpec{}
}

// snatOf is what a rule SNATs to: the address, and the ports if it names
// any; ok is false for a rule that SNATs nothing.
func snatOf(r ruleSpec) (to netip.Addr, from, through uint16, proto byte, ok bool) {
	regs := map[uint32][]byte{}
	for i, e := range r.exprs {
		switch e := e.(type) {
		case *expr.Cmp:
			if m, isMeta := r.exprs[i-1].(*expr.Meta); isMeta && m.Key == expr.MetaKeyL4PROTO {
				proto = e.Data[0]
			}
		case *expr.Immediate:
			regs[e.Register] = e.Data
		case *expr.NAT:
			if e.Type != expr.NATTypeSourceNAT {
				return
			}
			to, _ = netip.AddrFromSlice(regs[e.RegAddrMin])
			if e.RegProtoMin != 0 {
				from, through = binaryutil.BigEndian.Uint16(regs[e.RegProtoMin]), binaryutil.BigEndian.Uint16(regs[e.RegProtoMax])
			}
			return to, from, through, proto, true
		}
	}
	return
}

// With a shared address the networks' traffic leaves as it, TCP and UDP
// from its upper ports alone, which leaves the lower ones to forwards,
// while the front door stays at the host's own address.
func TestTheNetworksLeaveAsTheSharedAddress(t *testing.T) {
	public, shared := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	chains := rootChains("eth0", public, shared)
	rules := postroutingOf(t, chains).rules
	if len(rules) != 3 {
		t.Fatalf("%d postrouting rules, want 3", len(rules))
	}
	for i, want := range []byte{unix.IPPROTO_TCP, unix.IPPROTO_UDP} {
		to, from, through, proto, ok := snatOf(rules[i])
		if !ok || to != shared || proto != want || from != SharedPortsFrom || through != 65535 {
			t.Errorf("rule %d SNATs protocol %d to %s ports %d-%d, want protocol %d to %s ports %d-65535",
				i, proto, to, from, through, want, shared, SharedPortsFrom)
		}
	}
	// What has no ports, ICMP among it, still leaves as the shared address.
	if to, from, _, proto, ok := snatOf(rules[2]); !ok || to != shared || from != 0 || proto != 0 {
		t.Errorf("the last rule SNATs protocol %d to %s from port %d, want anything to %s", proto, to, from, shared)
	}
	if slices.ContainsFunc(rules, func(r ruleSpec) bool { to, _, _, _, _ := snatOf(r); return to == public }) {
		t.Error("a rule SNATs to the host's own address")
	}
	// The front door is where it was, at the host's own address alone.
	if got, want := comments(t, []chainSpec{inputOf(t, chains)}), comments(t, []chainSpec{inputOf(t, rootChains("eth0", public, netip.Addr{}))}); !slices.Equal(got, want) {
		t.Errorf("input rules with a shared address %q, want those without %q", got, want)
	}
}

// Without a shared address the networks leave as the host's own, as
// before there was one.
func TestWithoutASharedAddressTheNetworksLeaveAsThePublicOne(t *testing.T) {
	public := netip.MustParseAddr("192.0.2.1")
	rules := postroutingOf(t, rootChains("eth0", public, netip.Addr{})).rules
	if len(rules) != 1 {
		t.Fatalf("%d postrouting rules, want 1", len(rules))
	}
	if to, from, _, _, ok := snatOf(rules[0]); !ok || to != public || from != 0 {
		t.Errorf("SNATs to %s from port %d, want %s with any port", to, from, public)
	}
}

func TestTheSharedAddressIsAnotherIPv4(t *testing.T) {
	public := netip.MustParseAddr("192.0.2.1")
	for _, shared := range []netip.Addr{public, netip.MustParseAddr("2001:db8::1")} {
		if _, err := New(Config{Uplink: "eth0", PublicIPv4: public, SharedIPv4: shared}); err == nil {
			t.Errorf("a driver was made with shared address %s", shared)
		}
	}
}
