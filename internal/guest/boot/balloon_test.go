package boot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func writeMeminfo(t *testing.T, path string, totalMiB uint64) {
	t.Helper()
	content := fmt.Sprintf("MemTotal:       %d kB\nMemFree:          123456 kB\nMemAvailable:     234567 kB\n", totalMiB<<10)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTheGuestWaitsForItsBootBalloon: hangar-init goes on once MemTotal is
// down to the memory its guest boots with and the slack, read every poll;
// after the timeout it goes on all the same, saying so; with no boot
// memory it never waits.
func TestTheGuestWaitsForItsBootBalloon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		meminfo := filepath.Join(t.TempDir(), "meminfo")
		writeMeminfo(t, meminfo, 3900)
		var said []string
		logf := func(format string, args ...any) { said = append(said, fmt.Sprintf(format, args...)) }
		// Inflated to 1024 + 40 after 2 s.
		go func() {
			time.Sleep(2 * time.Second) //nolint:forbidigo // virtual time
			writeMeminfo(t, meminfo, 1064)
		}()
		waited := WaitForBalloon(t.Context(), meminfo, 1024, BalloonPoll, BalloonTimeout, logf)
		if waited < 2*time.Second || waited > 2*time.Second+BalloonPoll {
			t.Errorf("waited %v, want 2 s and at most a poll", waited)
		}
		if len(said) > 0 {
			t.Errorf("said %q", said)
		}

		// Never down to it: the timeout, and a word.
		writeMeminfo(t, meminfo, 1200)
		if waited := WaitForBalloon(t.Context(), meminfo, 1024, BalloonPoll, BalloonTimeout, logf); waited < BalloonTimeout || waited > BalloonTimeout+BalloonPoll {
			t.Errorf("waited %v, want the timeout", waited)
		}
		if len(said) != 1 || !strings.Contains(said[0], "1200 MiB") {
			t.Errorf("said %q, want the memory it still had", said)
		}

		// No boot memory: no wait, and no meminfo read.
		if waited := WaitForBalloon(t.Context(), filepath.Join(t.TempDir(), "none"), 0, BalloonPoll, BalloonTimeout, logf); waited != 0 {
			t.Errorf("waited %v with no boot memory", waited)
		}
	})
}

func TestMemTotalIsReadAsTheKernelWritesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	writeMeminfo(t, path, 2048)
	if n, err := memTotalKiB(path); err != nil || n != 2048<<10 {
		t.Errorf("MemTotal %d KiB, %v; want %d", n, err, 2048<<10)
	}
	for _, bad := range []string{"MemFree: 1 kB\n", "MemTotal: lots kB\n"} {
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if n, err := memTotalKiB(path); err == nil {
			t.Errorf("%q: MemTotal %d", bad, n)
		}
	}
}
