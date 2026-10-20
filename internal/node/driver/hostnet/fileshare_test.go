package hostnet

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/google/nftables/userdata"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// frame is what the firewall reads of a frame on the bridge: the links it
// comes in by and goes out of (empty for the namespace's own stack), its
// ether type, and of an IPv4 packet its addresses, protocol and ports, and
// what conntrack makes of it.
type frame struct {
	iif, oif     string
	arp          bool
	l4           byte
	saddr, daddr string
	sport, dport uint16
	// ct is new, established, related or invalid, untracked if empty for
	// ARP; reply says the packet goes the connection's reply way.
	ct    string
	reply bool
}

var ctBits = map[string]uint32{
	"new": expr.CtStateBitNEW, "established": expr.CtStateBitESTABLISHED,
	"related": expr.CtStateBitRELATED, "invalid": expr.CtStateBitINVALID,
}

// passes runs f through the firewall as the bridge does: what a tap sends
// through prerouting, then through forward when it goes to a tap; what the
// namespace sends through output.
func passes(t *testing.T, chains []chainSpec, f frame) bool {
	t.Helper()
	byName := map[string]chainSpec{}
	for _, c := range chains {
		byName[c.name] = c
	}
	if f.iif != "" && !run(t, byName, "prerouting", f) {
		return false
	}
	switch {
	case f.oif == "":
		return true // the namespace's, which no chain here checks
	case f.iif == "":
		return run(t, byName, "output", f)
	}
	return run(t, byName, "forward", f)
}

// run runs a base chain on f, following jumps, as nftables would: true if
// it is accepted. It knows the expressions the firewall's rules use, and
// fails the test on any other.
func run(t *testing.T, chains map[string]chainSpec, name string, f frame) bool {
	t.Helper()
	v, ok := runChain(t, chains, name, f)
	if !ok {
		return true // a base chain's policy: accept
	}
	return v
}

func runChain(t *testing.T, chains map[string]chainSpec, name string, f frame) (accepted, decided bool) {
	t.Helper()
	c, ok := chains[name]
	if !ok {
		t.Fatalf("no chain %s", name)
	}
	for _, r := range c.rules {
		kind, jump, matched := evalRule(t, r, f)
		if !matched {
			continue
		}
		switch kind {
		case expr.VerdictAccept:
			return true, true
		case expr.VerdictDrop:
			return false, true
		case expr.VerdictJump:
			if v, ok := runChain(t, chains, jump, f); ok {
				return v, true
			}
		}
	}
	return false, false
}

// evalRule runs r's expressions on f: whether every match holds, and then
// its verdict.
func evalRule(t *testing.T, r ruleSpec, f frame) (expr.VerdictKind, string, bool) {
	t.Helper()
	ifname := func(n string) []byte {
		b := make([]byte, unix.IFNAMSIZ)
		copy(b, n)
		return b
	}
	addr := func(s string) []byte {
		if s == "" {
			return make([]byte, 4)
		}
		return netip.MustParseAddr(s).AsSlice()
	}
	var reg []byte
	for _, e := range r.exprs {
		switch e := e.(type) {
		case *expr.Meta:
			switch e.Key {
			case expr.MetaKeyIIFNAME:
				reg = ifname(f.iif)
			case expr.MetaKeyOIFNAME:
				reg = ifname(f.oif)
			case expr.MetaKeyPROTOCOL:
				reg = etherIPv4
				if f.arp {
					reg = etherARP
				}
			case expr.MetaKeyL4PROTO:
				reg = []byte{f.l4}
			default:
				t.Fatalf("%q: meta %v", r.says, e.Key)
			}
		case *expr.Payload:
			switch {
			case e.Base == expr.PayloadBaseNetworkHeader && e.Offset == 12 && e.Len == 4:
				reg = addr(f.saddr)
			case e.Base == expr.PayloadBaseNetworkHeader && e.Offset == 16 && e.Len == 4:
				reg = addr(f.daddr)
			case e.Base == expr.PayloadBaseTransportHeader && e.Offset == 0 && e.Len == 2:
				reg = binary.BigEndian.AppendUint16(nil, f.sport)
			case e.Base == expr.PayloadBaseTransportHeader && e.Offset == 2 && e.Len == 2:
				reg = binary.BigEndian.AppendUint16(nil, f.dport)
			default:
				t.Fatalf("%q: payload %+v", r.says, e)
			}
		case *expr.Ct:
			switch e.Key {
			case expr.CtKeySTATE:
				bits, ok := ctBits[f.ct]
				if f.arp && f.ct == "" {
					bits, ok = expr.CtStateBitUNTRACKED, true
				}
				if !ok {
					t.Fatalf("frame %+v has no conntrack state", f)
				}
				reg = binary.NativeEndian.AppendUint32(nil, bits)
			case expr.CtKeyDIRECTION:
				reg = []byte{0}
				if f.reply {
					reg = []byte{1}
				}
			default:
				t.Fatalf("%q: ct %v", r.says, e.Key)
			}
		case *expr.Bitwise:
			out := make([]byte, len(reg))
			for i := range reg {
				out[i] = reg[i]&e.Mask[i] ^ e.Xor[i]
			}
			reg = out
		case *expr.Cmp:
			// A shorter operand compares a prefix, as an interface name's.
			eq := bytes.Equal(reg[:min(len(reg), len(e.Data))], e.Data)
			if (e.Op == expr.CmpOpEq) != eq {
				return 0, "", false
			}
		case *expr.Range:
			in := bytes.Compare(reg, e.FromData) >= 0 && bytes.Compare(reg, e.ToData) <= 0
			if (e.Op == expr.CmpOpEq) != in {
				return 0, "", false
			}
		case *expr.Verdict:
			return e.Kind, e.Chain, true
		default:
			t.Fatalf("%q: expression %T", r.says, e)
		}
	}
	return expr.VerdictContinue, "", true
}

// TestAFileShareServerSendsOnlyReplies: a file share's server's tap takes
// only new TCP connections to 2049 from its share's clients, and what
// follows on them, and sends only replies on them, whatever the network's
// services, the platform's ways in, its groups or anyone's give; a client
// reaches the share's 2049 whatever its own groups let out, egress-locked
// or not, and nothing else of the server. A server's tap no rule names,
// as one made since the firewall was written, takes and sends nothing but
// ARP.
func TestAFileShareServerSendsOnlyReplies(t *testing.T) {
	const (
		gw, server, locked, open, other = "10.0.0.1", "10.0.0.9", "10.0.0.5", "10.0.0.6", "10.0.0.7"
		srvTap, newTap                  = "hnts00000000009", "hnts00000000010"
		lockedTap, openTap, otherTap    = "hnt000000000005", "hnt000000000006", "hnt000000000007"
	)
	share := []netip.Addr{netip.MustParseAddr(server)}
	anything := secgroup.Rule{Protocol: secgroup.All, Peers: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}
	cs, err := firewallChains(netip.MustParseAddr(gw), []tapPolicy{
		// Groups given to a server's tap are no part of its rules.
		{name: srvTap, policy: secgroup.Policy{Ingress: []secgroup.Rule{anything}, EgressAll: true}, edgePorts: []uint16{80},
			clients: []netip.Addr{netip.MustParseAddr(locked), netip.MustParseAddr(open)}},
		{name: lockedTap, shares: share},
		{name: openTap, policy: secgroup.Policy{EgressAll: true}, shares: share},
		{name: otherTap, policy: secgroup.Policy{Ingress: []secgroup.Rule{anything}, EgressAll: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.name == "in_"+srvTap || c.name == "out_"+srvTap {
			t.Errorf("the server's tap has a chain of its groups', %s", c.name)
		}
	}
	tcp, udp := byte(unix.IPPROTO_TCP), byte(unix.IPPROTO_UDP)
	syn := func(iif, oif, from, to string, dport uint16) frame {
		return frame{iif: iif, oif: oif, l4: tcp, saddr: from, daddr: to, sport: 40000, dport: dport, ct: "new"}
	}
	for _, c := range []struct {
		what string
		f    frame
		want bool
	}{
		{"a locked client connects to 2049", syn(lockedTap, srvTap, locked, server, 2049), true},
		{"an open client connects to 2049", syn(openTap, srvTap, open, server, 2049), true},
		{"a client's connection goes on", frame{iif: lockedTap, oif: srvTap, l4: tcp, saddr: locked, daddr: server, sport: 40000, dport: 2049, ct: "established"}, true},
		{"the server replies", frame{iif: srvTap, oif: lockedTap, l4: tcp, saddr: server, daddr: locked, sport: 2049, dport: 40000, ct: "established", reply: true}, true},
		{"ARP from the server", frame{iif: srvTap, oif: lockedTap, arp: true}, true},
		{"ARP to the server", frame{iif: lockedTap, oif: srvTap, arp: true}, true},

		{"a client to another port of the server", syn(openTap, srvTap, open, server, 22), false},
		{"a client's UDP to 2049", frame{iif: openTap, oif: srvTap, l4: udp, saddr: open, daddr: server, sport: 40000, dport: 2049, ct: "new"}, false},
		{"a locked client's 2049 elsewhere", syn(lockedTap, otherTap, locked, other, 2049), false},
		{"a machine not declaring the share connects", syn(otherTap, srvTap, other, server, 2049), false},
		{"a machine not declaring the share, on a connection", frame{iif: otherTap, oif: srvTap, l4: tcp, saddr: other, daddr: server, sport: 40000, dport: 2049, ct: "established"}, false},
		{"a client to a server's tap no rule names", syn(lockedTap, newTap, locked, server, 2049), false},
		{"related to a client's connection", frame{iif: openTap, oif: srvTap, l4: tcp, saddr: open, daddr: server, sport: 40000, dport: 2049, ct: "related"}, false},

		{"the server connects to a client", syn(srvTap, lockedTap, server, locked, 22), false},
		{"the server connects from 2049", frame{iif: srvTap, oif: lockedTap, l4: tcp, saddr: server, daddr: locked, sport: 2049, dport: 2049, ct: "new"}, false},
		{"the server on a connection it made", frame{iif: srvTap, oif: lockedTap, l4: tcp, saddr: server, daddr: locked, sport: 2049, dport: 40000, ct: "established"}, false},
		{"the server replies from another port", frame{iif: srvTap, oif: otherTap, l4: tcp, saddr: server, daddr: other, sport: 22, dport: 40000, ct: "established", reply: true}, false},
		{"the server asks the resolver", frame{iif: srvTap, l4: udp, saddr: server, daddr: gw, sport: 40000, dport: 53, ct: "new"}, false},
		{"the server asks DHCP", frame{iif: srvTap, l4: udp, saddr: "0.0.0.0", daddr: "255.255.255.255", sport: 68, dport: 67, ct: "new"}, false},
		{"the server asks the metadata service", frame{iif: srvTap, l4: tcp, saddr: server, daddr: "169.254.169.254", sport: 40000, dport: 80, ct: "new"}, false},
		{"the server goes out", frame{iif: srvTap, l4: tcp, saddr: server, daddr: "1.1.1.1", sport: 40000, dport: 443, ct: "new"}, false},
		{"the server a tap's new UDP", frame{iif: srvTap, oif: otherTap, l4: udp, saddr: server, daddr: other, sport: 2049, dport: 2049, ct: "new"}, false},
		{"a tap no rule names sends", frame{iif: newTap, oif: lockedTap, l4: tcp, saddr: server, daddr: locked, sport: 2049, dport: 40000, ct: "established", reply: true}, true},
		{"a tap no rule names asks DHCP", frame{iif: newTap, l4: udp, saddr: "0.0.0.0", daddr: "255.255.255.255", sport: 68, dport: 67, ct: "new"}, false},

		{"the platform's SSH to the server", syn("", srvTap, gw, server, 22), false},
		{"an edge port of the server", syn("", srvTap, gw, server, 80), false},
		{"DHCP replies to the server", frame{oif: srvTap, l4: udp, saddr: gw, daddr: server, sport: 67, dport: 68, ct: "new"}, false},
		{"the namespace's replies to the server", frame{oif: srvTap, l4: udp, saddr: gw, daddr: server, sport: 53, dport: 40000, ct: "established", reply: true}, false},
		{"the gateway to 2049", syn("", srvTap, gw, server, 2049), false},
	} {
		if got := passes(t, cs, c.f); got != c.want {
			t.Errorf("%s: passed %t, want %t", c.what, got, c.want)
		}
	}
	// The control: the same frames to and from a tap of no server, open
	// to all, pass, so what stops them above is the server's chain.
	for _, f := range []frame{
		syn("", otherTap, gw, other, 22),
		{iif: otherTap, l4: udp, saddr: other, daddr: gw, sport: 40000, dport: 53, ct: "new"},
		{iif: otherTap, l4: udp, saddr: "0.0.0.0", daddr: "255.255.255.255", sport: 68, dport: 67, ct: "new"},
		syn(openTap, otherTap, open, other, 22),
	} {
		if !passes(t, cs, f) {
			t.Errorf("control %s: dropped", fmt.Sprint(f))
		}
	}
}

// asWritten is the firewall's chains as the kernel would give their rules
// back: each with its expressions and its comment.
func asWritten(t *testing.T, cs []chainSpec) map[string][]*nftables.Rule {
	t.Helper()
	out := map[string][]*nftables.Rule{}
	for _, c := range cs {
		for _, r := range c.rules {
			comment, err := r.comment()
			if err != nil {
				t.Fatal(err)
			}
			out[c.name] = append(out[c.name], &nftables.Rule{Exprs: r.exprs, UserData: userdata.AppendString(nil, userdata.TypeComment, comment)})
		}
	}
	return out
}

// TestShareRulesAreReadBack: the file share rules in force are read back
// from a firewall's rules: a server's tap's clients, a client's tap's
// servers, by machine and interface; nothing of a tap the host has not, or
// of a rule not as the driver writes it, its expressions or its comment
// changed.
func TestShareRulesAreReadBack(t *testing.T) {
	const srvTap, cliTap, otherTap = "hnts00000000009", "hnt000000000005", "hnt000000000007"
	srv, cli := netip.MustParseAddr("10.0.0.9"), netip.MustParseAddr("10.0.0.5")
	cs, err := firewallChains(netip.MustParseAddr("10.0.0.1"), []tapPolicy{
		{name: srvTap, clients: []netip.Addr{cli, netip.MustParseAddr("10.0.0.6")}},
		{name: cliTap, policy: secgroup.Policy{EgressAll: true}, shares: []netip.Addr{srv}},
		{name: otherTap, policy: secgroup.Policy{EgressAll: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ifaces := map[string]string{srvTap: "srv/eth0", cliTap: "cli/eth0", otherTap: "other/eth0"}
	got := shareRulesOf(asWritten(t, cs), ifaces)
	want := map[string]driver.ShareRules{
		"srv/eth0": {Clients: []netip.Addr{cli, netip.MustParseAddr("10.0.0.6")}},
		"cli/eth0": {Servers: []netip.Addr{srv}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	// A tap the host has not.
	delete(ifaces, cliTap)
	if got := shareRulesOf(asWritten(t, cs), ifaces); len(got["cli/eth0"].Servers) != 0 {
		t.Errorf("read back a tap the host has not: %+v", got)
	}
	ifaces[cliTap] = "cli/eth0"
	// Rules changed: an expression, a comment.
	rules := asWritten(t, cs)
	for _, r := range rules["fileshare_in"] {
		if _, saddr, _ := ruleFields(r.Exprs); saddr == cli {
			r.Exprs = slices.DeleteFunc(slices.Clone(r.Exprs), func(e expr.Any) bool { _, ct := e.(*expr.Ct); return ct })
		}
	}
	for _, r := range rules["out_"+cliTap] {
		r.UserData = userdata.AppendString(nil, userdata.TypeComment, "hangar: another rule [0000000000000000]")
	}
	got = shareRulesOf(rules, ifaces)
	if c := got["srv/eth0"].Clients; !slices.Equal(c, []netip.Addr{netip.MustParseAddr("10.0.0.6")}) {
		t.Errorf("a client rule not as written read back: %v", c)
	}
	if s := got["cli/eth0"].Servers; len(s) != 0 {
		t.Errorf("a server rule not as written read back: %v", s)
	}
}
