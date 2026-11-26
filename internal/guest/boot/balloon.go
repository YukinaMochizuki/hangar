package boot

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Bounds of the wait for the boot balloon (docs/decisions/guest-agent.md):
// a guest's balloon driver inflated 3 GiB in about 4 s on a testbed.
const (
	// BalloonSlackMiB is how far above its boot memory a guest's MemTotal
	// may be once its balloon is inflated: the kernel's own reservations
	// keep MemTotal below what it is given, so this is room to spare.
	BalloonSlackMiB = 64
	// BalloonPoll is how often MemTotal is read; BalloonTimeout how long
	// the guest waits before it goes on all the same.
	BalloonPoll    = 50 * time.Millisecond
	BalloonTimeout = 15 * time.Second
)

// WaitForBalloon waits until the guest's MemTotal, as meminfo (normally
// /proc/meminfo) says it, is at most bootMiB and BalloonSlackMiB: its
// balloon has taken what the node keeps back of its memory as it boots. It
// reads it every poll, and after timeout says so through logf and returns,
// as does a zero bootMiB at once. It returns how long it waited.
func WaitForBalloon(ctx context.Context, meminfo string, bootMiB uint64, poll, timeout time.Duration, logf func(string, ...any)) time.Duration {
	if bootMiB == 0 {
		return 0
	}
	start := time.Now()
	want := (bootMiB + BalloonSlackMiB) << 10
	var last uint64
	for {
		total, err := memTotalKiB(meminfo)
		if err == nil && total <= want {
			return time.Since(start)
		}
		last = total
		if time.Since(start) >= timeout {
			if err != nil {
				logf("hangar-init: the boot balloon: %v; starting all the same", err)
			} else {
				logf("hangar-init: memory still %d MiB after %v, past the %d MiB this guest boots with; starting all the same", last>>10, timeout, bootMiB)
			}
			return time.Since(start)
		}
		select {
		case <-ctx.Done():
			return time.Since(start)
		case <-time.After(poll):
		}
	}
}

// memTotalKiB is MemTotal in the meminfo file, in KiB.
func memTotalKiB(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "MemTotal:")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: MemTotal %q", path, v)
		}
		return n, nil
	}
	return 0, fmt.Errorf("%s has no MemTotal", path)
}
