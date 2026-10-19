package front

import (
	"container/list"
	"net/netip"
	"time"
)

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
	now := k.now()
	if top, n := k.buckets.top(); n > k.buckets.count(b)+1 {
		heaviest, _ := k.inBucket[top].top()
		return k.clients[heaviest].giveUp(now, k.l.ReadHeaderTimeout)
	}
	if top, n := k.byClient.top(); n > k.byClient.count(c)+1 {
		return k.clients[top].giveUp(now, k.l.ReadHeaderTimeout)
	}
	// Only keep-alives that have served a request: a connection before its
	// first is the newcomer it may be, which a holder's redial must not undo.
	if e := k.idle.Front(); e != nil {
		return e.Value.(*conn)
	}
	return nil
}

// giveUp is the connection cc would lose first: of those older than young,
// unless it has none, its idlest, else its newest, which has had least of
// its time.
func (cc *clientCounts) giveUp(now time.Time, young time.Duration) *conn {
	var idlest, newest *conn
	old := false
	for x := range cc.open {
		if now.Sub(x.opened) >= young {
			old = true
			break
		}
	}
	for x := range cc.open {
		if old && now.Sub(x.opened) < young {
			continue
		}
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

// tally counts connections by key, and finds the key with the most in
// constant time, as counts only ever move by one; of keys tied, the one
// that reached the count first.
type tally[K comparable] struct {
	n map[K]int
	// byN[i] are the keys with i, in the order they reached it; byN[0] is
	// never used, and the last is never empty.
	byN []*level[K]
}

type level[K comparable] struct {
	order *list.List
	at    map[K]*list.Element
}

func newTally[K comparable]() *tally[K] {
	return &tally[K]{n: map[K]int{}, byN: []*level[K]{nil}}
}

func (t *tally[K]) count(k K) int { return t.n[k] }

// add moves k's count by d, one or minus one.
func (t *tally[K]) add(k K, d int) {
	was := t.n[k]
	now := was + d
	if was > 0 {
		l := t.byN[was]
		l.order.Remove(l.at[k])
		delete(l.at, k)
	}
	if now > 0 {
		if len(t.byN) <= now {
			t.byN = append(t.byN, &level[K]{order: list.New(), at: map[K]*list.Element{}})
		}
		l := t.byN[now]
		l.at[k] = l.order.PushBack(k)
		t.n[k] = now
	} else {
		delete(t.n, k)
	}
	for len(t.byN) > 1 && t.byN[len(t.byN)-1].order.Len() == 0 {
		t.byN = t.byN[:len(t.byN)-1]
	}
}

// top is the key with the most, and how many; n is 0 when there is none.
func (t *tally[K]) top() (k K, n int) {
	n = len(t.byN) - 1
	if n == 0 {
		return k, 0
	}
	return t.byN[n].order.Front().Value.(K), n
}

func (t *tally[K]) empty() bool { return len(t.n) == 0 }
