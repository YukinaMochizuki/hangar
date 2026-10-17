package hostnet

import (
	"fmt"
	"slices"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
)

// What a network may open towards the internet and other networks: new
// connections, IPv4 and IPv6 together, at this rate a second, after a burst
// of this many (docs/decisions/networking.md, "Abuse protection").
const (
	newConnRate  = 100
	newConnBurst = 300
)

// limitChains is a network namespace's inet table: one bucket, both
// families, for the new connections its machines send out of hn-up.
// Established flows and what stays in the network are not counted.
func limitChains() []chainSpec {
	return []chainSpec{{
		name: "forward", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookForward, prio: *nftables.ChainPriorityFilter,
		rules: []ruleSpec{{
			says: fmt.Sprintf("at most %d new connections a second out of %s, after a burst of %d", newConnRate, nsUplink, newConnBurst),
			exprs: slices.Concat(matchIfname(expr.MetaKeyIIFNAME, nsBridge), matchIfname(expr.MetaKeyOIFNAME, nsUplink), matchNew(),
				[]expr.Any{
					&expr.Limit{Type: expr.LimitTypePkts, Rate: newConnRate, Unit: expr.LimitTimeSecond, Burst: newConnBurst, Over: true},
					&expr.Verdict{Kind: expr.VerdictDrop},
				}),
		}},
	}}
}

// matchNew matches a packet starting a connection, as nft's "ct state new"
// does.
func matchNew() []expr.Any { return matchCtState(expr.CtStateBitNEW) }

// matchCtState matches a packet whose conntrack state is bit, as nft's
// "ct state" does.
func matchCtState(bit uint32) []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{
			SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(bit), Xor: make([]byte, 4),
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: make([]byte, 4)},
	}
}
