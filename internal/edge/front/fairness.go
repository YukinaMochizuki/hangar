package front

import "net/netip"

// bucket is the network fair eviction weighs before its clients: the IPv4
// /24 or the IPv6 /48; a host's own source is a bucket by itself, as it is
// a client (docs/decisions/front-door.md).
func (l Limits) bucket(addr netip.Addr) netip.Prefix {
	addr = addr.Unmap()
	switch {
	case addr.Is4() && !hostTransit.Contains(addr):
		p, _ := addr.Prefix(24)
		return p
	case addr.Is6() && !l.own(addr):
		p, _ := addr.Prefix(48)
		return p
	}
	return l.client(addr)
}

// victim is the connection to close for a new one from client c of bucket
// b with the door full, or nil to refuse it; the order it looks in is in
// docs/decisions/front-door.md. Callers hold k.mu.
func (k *counts) victim(b, c netip.Prefix) *conn {
	if top, n := k.buckets.top(); n > k.buckets.count(b)+1 {
		heaviest, _ := k.inBucket[top].top()
		return k.clients[heaviest].giveUp()
	}
	if top, n := k.byClient.top(); n > k.byClient.count(c)+1 {
		return k.clients[top].giveUp()
	}
	if e := k.idle.Front(); e != nil {
		return e.Value.(*conn)
	}
	return nil
}

// giveUp is the connection cc would lose first: its idlest, else its
// newest, which has had least of its time.
func (cc *clientCounts) giveUp() *conn {
	var idlest, newest *conn
	for x := range cc.open {
		if !x.busy && (idlest == nil || x.idleSince.Before(idlest.idleSince)) {
			idlest = x
		}
		if newest == nil || x.opened.After(newest.opened) {
			newest = x
		}
	}
	if idlest != nil {
		return idlest
	}
	return newest
}

// tally counts connections by key, and finds a key with the most in
// constant time, as counts only ever move by one.
type tally[K comparable] struct {
	n map[K]int
	// byN[i] are the keys with i; byN[0] is never used, and the last is
	// never empty.
	byN []map[K]struct{}
}

func newTally[K comparable]() *tally[K] {
	return &tally[K]{n: map[K]int{}, byN: []map[K]struct{}{nil}}
}

func (t *tally[K]) count(k K) int { return t.n[k] }

// add moves k's count by d, one or minus one.
func (t *tally[K]) add(k K, d int) {
	was := t.n[k]
	now := was + d
	if was > 0 {
		delete(t.byN[was], k)
	}
	if now > 0 {
		if len(t.byN) <= now {
			t.byN = append(t.byN, map[K]struct{}{})
		}
		t.byN[now][k] = struct{}{}
		t.n[k] = now
	} else {
		delete(t.n, k)
	}
	for len(t.byN) > 1 && len(t.byN[len(t.byN)-1]) == 0 {
		t.byN = t.byN[:len(t.byN)-1]
	}
}

// top is a key with the most, and how many; n is 0 when there is none.
func (t *tally[K]) top() (k K, n int) {
	n = len(t.byN) - 1
	for k := range t.byN[n] {
		return k, n
	}
	return k, 0
}

func (t *tally[K]) empty() bool { return len(t.n) == 0 }
