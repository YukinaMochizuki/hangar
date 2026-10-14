package hostnet

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// packet is what a rule's expressions read of an IPv4 packet.
type packet struct {
	l4           byte
	saddr, daddr string
	sport, dport uint16
}

// accepts runs r's expressions on p as nftables would: true if every match
// holds and the verdict is accept. It knows the expressions rules here use.
func accepts(t *testing.T, r ruleSpec, p packet) bool {
	t.Helper()
	var reg []byte
	for _, e := range r.exprs {
		switch e := e.(type) {
		case *expr.Meta:
			switch e.Key {
			case expr.MetaKeyPROTOCOL:
				reg = etherIPv4
			case expr.MetaKeyL4PROTO:
				reg = []byte{p.l4}
			default:
				t.Fatalf("meta %v", e.Key)
			}
		case *expr.Payload:
			switch {
			case e.Base == expr.PayloadBaseNetworkHeader && e.Offset == 12 && e.Len == 4:
				reg = netip.MustParseAddr(p.saddr).AsSlice()
			case e.Base == expr.PayloadBaseNetworkHeader && e.Offset == 16 && e.Len == 4:
				reg = netip.MustParseAddr(p.daddr).AsSlice()
			case e.Base == expr.PayloadBaseTransportHeader && e.Offset == 0 && e.Len == 2:
				reg = binary.BigEndian.AppendUint16(nil, p.sport)
			case e.Base == expr.PayloadBaseTransportHeader && e.Offset == 2 && e.Len == 2:
				reg = binary.BigEndian.AppendUint16(nil, p.dport)
			default:
				t.Fatalf("payload %+v", e)
			}
		case *expr.Bitwise:
			out := make([]byte, len(reg))
			for i := range reg {
				out[i] = reg[i]&e.Mask[i] ^ e.Xor[i]
			}
			reg = out
		case *expr.Cmp:
			eq := bytes.Equal(reg, e.Data)
			if (e.Op == expr.CmpOpEq) != eq {
				return false
			}
		case *expr.Range:
			in := bytes.Compare(reg, e.FromData) >= 0 && bytes.Compare(reg, e.ToData) <= 0
			if (e.Op == expr.CmpOpEq) != in {
				return false
			}
		case *expr.Verdict:
			return e.Kind == expr.VerdictAccept
		default:
			t.Fatalf("expression %T", e)
		}
	}
	return false
}

// TestThePlatformsSSHPasses: the namespace's TCP from the gateway to a tap's
// port 22 passes, whatever the tap's groups let in, before the tap's own
// chain; nothing else does by it: not UDP to port 22, not the guest's port
// 22 toward the gateway, not TCP from the gateway's port 22, nor from
// another address, nor to another port; nor anything by prerouting or
// forward.
func TestThePlatformsSSHPasses(t *testing.T) {
	gw := netip.MustParseAddr("10.0.0.1")
	locked := tapPolicy{name: "hnt000000000001", policy: secgroup.Policy{EgressAll: true}}
	cs, err := firewallChains(gw, []tapPolicy{locked})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		var ssh []ruleSpec
		for _, r := range c.rules {
			if strings.Contains(r.says, "platform's SSH") {
				ssh = append(ssh, r)
			}
		}
		if c.name != "output" {
			if len(ssh) != 0 {
				t.Errorf("%s lets the platform's SSH through", c.name)
			}
			continue
		}
		if len(ssh) != 1 {
			t.Fatalf("output has %d rules for the platform's SSH", len(ssh))
		}
		r := ssh[0]
		tcp, udp := byte(unix.IPPROTO_TCP), byte(unix.IPPROTO_UDP)
		for _, tc := range []struct {
			what string
			p    packet
			want bool
		}{
			{"TCP from the gateway to the guest's port 22", packet{tcp, "10.0.0.1", "10.0.0.5", 40000, 22}, true},
			{"UDP from the gateway to the guest's port 22", packet{udp, "10.0.0.1", "10.0.0.5", 40000, 22}, false},
			{"TCP from the guest's port 22 to the gateway", packet{tcp, "10.0.0.5", "10.0.0.1", 22, 40000}, false},
			{"TCP from the guest to the gateway's port 22", packet{tcp, "10.0.0.5", "10.0.0.1", 40000, 22}, false},
			{"TCP from the gateway's port 22 to the guest", packet{tcp, "10.0.0.1", "10.0.0.5", 22, 40000}, false},
			{"TCP from another guest to the guest's port 22", packet{tcp, "10.0.0.6", "10.0.0.5", 40000, 22}, false},
			{"TCP from the gateway to the guest's port 2222", packet{tcp, "10.0.0.1", "10.0.0.5", 40000, 2222}, false},
		} {
			if got := accepts(t, r, tc.p); got != tc.want {
				t.Errorf("%s: accepted %t, want %t", tc.what, got, tc.want)
			}
		}
		// Before the tap's own chain, which would drop it.
		at := slices.IndexFunc(c.rules, func(r ruleSpec) bool { return strings.Contains(r.says, "platform's SSH") })
		jump := slices.IndexFunc(c.rules, func(r ruleSpec) bool { return strings.Contains(r.says, locked.name+"'s ingress") })
		if at > jump {
			t.Errorf("the platform's SSH rule is at %d, after %s's ingress at %d", at, locked.name, jump)
		}
	}
}
