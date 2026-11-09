package authz

import (
	"math"
	"slices"
)

// Now, as a revision, asks about the view as it stands.
const Now int64 = math.MaxInt64

// version is one value a key of the view held, over the revisions
// [since, until); until is 0 while it still holds.
type version[V any] struct {
	value V
	since int64
	until int64
}

func (v version[V]) holdsAt(rev int64) bool {
	return v.since <= rev && (v.until == 0 || rev < v.until)
}

// history maps each key to the values it held, oldest first. Only the last
// may still hold. Values that no longer hold are tombstones, kept until
// prune drops them.
type history[K comparable, V any] map[K][]version[V]

// at returns the value k held at rev; at Now, the value it holds.
func (h history[K, V]) at(k K, rev int64) (V, bool) {
	vs := h[k]
	for i := len(vs) - 1; i >= 0; i-- {
		if vs[i].holdsAt(rev) {
			return vs[i].value, true
		}
	}
	var zero V
	return zero, false
}

// put makes v the value k holds from rev on. Whatever k held must have
// been closed first.
func (h history[K, V]) put(k K, v V, rev int64) {
	h[k] = append(h[k], version[V]{value: v, since: rev})
}

// close ends the value k holds at rev, and reports whether that left a
// tombstone: a value that held from an earlier revision. One put at rev
// itself held for no revision at all and is dropped.
func (h history[K, V]) close(k K, rev int64) bool {
	vs := h[k]
	if len(vs) == 0 || vs[len(vs)-1].until != 0 {
		return false
	}
	last := &vs[len(vs)-1]
	if last.since < rev {
		last.until = rev
		return true
	}
	if len(vs) == 1 {
		delete(h, k)
	} else {
		h[k] = vs[:len(vs)-1]
	}
	return false
}

// prune drops k's tombstones that stopped holding at or before before.
func (h history[K, V]) prune(k K, before int64) {
	vs := slices.DeleteFunc(h[k], func(v version[V]) bool { return v.until != 0 && v.until <= before })
	if len(vs) == 0 {
		delete(h, k)
	} else {
		h[k] = vs
	}
}

// tombstone is a value of the view that stopped holding at until, and what
// drops it once it is older than the history the view keeps.
type tombstone struct {
	until int64
	prune func(before int64)
}

// RetainHistory makes the view keep what it held over its last `revisions`
// revisions, the window a watch can resume from in the store
// (apiserver.Config.EventRetention). It keeps none until this is called.
func (a *Authorizer) RetainHistory(revisions int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.retention = revisions
}

// prune drops the tombstones older than the history the view keeps.
// Callers hold a.mu.
func (a *Authorizer) prune() {
	a.pruneBefore(a.rev - a.retention)
}

// pruneBefore drops the tombstones that stopped holding at or before
// before. Callers hold a.mu.
func (a *Authorizer) pruneBefore(before int64) {
	n := 0
	for n < len(a.tombstones) && a.tombstones[n].until <= before {
		a.tombstones[n].prune(before)
		// What is left reaches back only to here.
		a.historyFrom = max(a.historyFrom, a.tombstones[n].until)
		n++
	}
	clear(a.tombstones[:n])
	a.tombstones = a.tombstones[n:]
}

// closed records a tombstone left at until. Tombstones are recorded in the
// order of their revisions, which pruneBefore relies on. Callers hold a.mu.
func (a *Authorizer) closed(until int64, prune func(before int64)) {
	a.tombstones = append(a.tombstones, tombstone{until: until, prune: prune})
}
