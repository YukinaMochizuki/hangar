package cli

import (
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestCredentialsLockExcludes: a second lock on the credentials waits for
// the first to be let go, on every platform's lock.
func TestCredentialsLockExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hangar", "credentials.yaml")
	unlock, err := lockCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	var taken atomic.Bool
	got := make(chan func())
	go func() {
		second, err := lockCredentials(path)
		if err != nil {
			t.Error(err)
			second = func() {}
		}
		taken.Store(true)
		got <- second
	}()
	// The second lock blocks in the kernel, which says nothing while it
	// waits; a window long enough for it to have been taken stands in.
	testutil.Unchanged(t, "the second lock, while the first is held", 200*time.Millisecond, func() (string, error) {
		return strconv.FormatBool(taken.Load()), nil
	})
	unlock()
	select {
	case second := <-got:
		second()
	case <-time.After(testutil.Wait):
		t.Fatal("the second lock not taken once the first was let go")
	}
}
