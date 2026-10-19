package front

import "time"

// SetRefusalSummaryEvery makes f sum up its refused connections every d.
func SetRefusalSummaryEvery(f *Front, d time.Duration) { f.refusalEvery = d }

// OpenConns is how many connections f counts open.
func OpenConns(f *Front) int {
	f.counts.mu.Lock()
	defer f.counts.mu.Unlock()
	return f.counts.conns
}
