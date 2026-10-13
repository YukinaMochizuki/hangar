package execws

import (
	"testing"
	"time"
)

// TestSilenceCountsOnlyWithNothingPending: the other end is gone once it
// has said nothing, and nothing has been pending, for a whole window.
func TestSilenceCountsOnlyWithNothingPending(t *testing.T) {
	l := NewLiveness(100*time.Millisecond, 300*time.Millisecond)
	t0 := time.Now()
	if l.gone(t0.Add(399 * time.Millisecond)) {
		t.Error("gone before a window of silence")
	}
	if !l.gone(t0.Add(401 * time.Millisecond)) {
		t.Error("not gone after a window of silence")
	}
	done := l.Pending()
	if l.gone(t0.Add(time.Hour)) {
		t.Error("gone with something pending")
	}
	done()
	if l.gone(time.Now().Add(300 * time.Millisecond)) {
		t.Error("gone less than a window after the pending ended")
	}
	l.Heard()
	l.idleSince.Store(0)
	if l.gone(time.Now().Add(300 * time.Millisecond)) {
		t.Error("gone less than a window after the other end was heard")
	}
}
