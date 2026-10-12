package keylock_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver/keylock"
)

// TestOneHolderPerKey: a key is held by one at a time, other keys are not
// held up, and no key is kept once nobody holds or waits for it.
func TestOneHolderPerKey(t *testing.T) {
	var m keylock.Map
	var held, most atomic.Int32
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := "a"
			if i%2 == 1 {
				key = string(rune('b' + i%20))
			}
			unlock := m.Lock(key)
			if key == "a" {
				n := held.Add(1)
				for {
					old := most.Load()
					if n <= old || most.CompareAndSwap(old, n) {
						break
					}
				}
				held.Add(-1)
			}
			unlock()
			unlock() // a second unlock changes nothing
		}()
	}
	wg.Wait()
	if most.Load() != 1 {
		t.Errorf("%d held key a at once", most.Load())
	}
	if n := m.Len(); n != 0 {
		t.Errorf("%d keys kept with nobody holding them", n)
	}
	other := m.Lock("x")
	done := make(chan struct{})
	go func() { m.Lock("y")(); close(done) }()
	<-done // not held up by x
	other()
}
