package authz

// Tombstones returns how many values that no longer hold the view keeps,
// and how many it has queued to drop.
func (a *Authorizer) Tombstones() (kept, queued int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, g := range a.grants {
		kept += closedIn(g)
	}
	kept += closedIn(a.tenantOf) + closedIn(a.usernames) + closedIn(a.emails)
	return kept, len(a.tombstones)
}

func closedIn[K comparable, V any](h history[K, V]) int {
	n := 0
	for _, vs := range h {
		for _, v := range vs {
			if v.until != 0 {
				n++
			}
		}
	}
	return n
}
