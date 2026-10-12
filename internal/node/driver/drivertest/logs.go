package drivertest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// logReader gathers what a read of a log sends.
type logReader struct {
	mu   sync.Mutex
	recs []driver.LogRecord
}

func (r *logReader) send(recs []driver.LogRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, recs...)
	return nil
}

func (r *logReader) records() []driver.LogRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]driver.LogRecord(nil), r.recs...)
}

func readLog(t *testing.T, hv driver.Hypervisor, machine string, opts driver.LogOptions) []driver.LogRecord {
	t.Helper()
	var r logReader
	must(t, hv.ReadLog(t.Context(), machine, opts, r.send))
	return r.records()
}

// exitIn returns the boot of the first exit with code in recs.
func exitIn(recs []driver.LogRecord, code int32) (int64, bool) {
	for _, r := range recs {
		if r.Stream == console.Exit && r.Exit.Code == code {
			return r.Boot, true
		}
	}
	return 0, false
}

// bootAfter reports whether recs hold a record of a boot after boot.
func bootAfter(recs []driver.LogRecord, boot int64) bool {
	for _, r := range recs {
		if r.Boot > boot {
			return true
		}
	}
	return false
}

// logs: a VM's log is one stream across its boots, numbered, with each
// exit in it; following it sees what comes as it comes, from a log made
// after the follow began, and ends with ctx; the log outlives the VM.
func logs(t *testing.T, h Host) {
	ctx := t.Context()
	all := driver.LogOptions{Tail: driver.AllLines}
	if recs := readLog(t, h.Hypervisor, "m1", all); len(recs) != 0 {
		t.Errorf("a machine never started has %d records", len(recs))
	}
	followCtx, stopFollowing := context.WithCancel(ctx)
	defer stopFollowing()
	var followed logReader
	followDone := make(chan error, 1)
	go func() {
		followDone <- h.Hypervisor.ReadLog(followCtx, "m1", driver.LogOptions{Tail: driver.AllLines, Follow: true}, followed.send)
	}()

	spec := vmSpec(t, h, "m1", driver.RestartAlways)
	must(t, h.Hypervisor.Start(ctx, spec))
	eventually(t, "the guest agent answering", func() bool { return h.Guest.Ready(ctx, "m1") == nil })
	first, _ := vmOf(t, h, "m1")
	h.Exit(t, "m1", 3)
	eventually(t, "the VM started again", func() bool {
		vm, ok := vmOf(t, h, "m1")
		return ok && vm.State == driver.VMRunning && vm.Started.After(first.Started)
	})
	var boot int64
	eventually(t, "the exit and the next boot in the log", func() bool {
		recs := readLog(t, h.Hypervisor, "m1", all)
		b, ok := exitIn(recs, 3)
		boot = b
		return ok && bootAfter(recs, b)
	})
	if boot < 1 {
		t.Errorf("the exit is of boot %d, want one numbered", boot)
	}
	recs := readLog(t, h.Hypervisor, "m1", all)
	for i := 1; i < len(recs); i++ {
		if recs[i].Boot < recs[i-1].Boot {
			t.Fatalf("boot %d after boot %d", recs[i].Boot, recs[i-1].Boot)
		}
	}
	if tail := readLog(t, h.Hypervisor, "m1", driver.LogOptions{Tail: 1}); len(tail) != 1 || tail[0].Boot <= boot {
		t.Errorf("a tail of one line is %+v", tail)
	}
	if none := readLog(t, h.Hypervisor, "m1", driver.LogOptions{Tail: 0}); len(none) != 0 {
		t.Errorf("a tail of no lines is %d records", len(none))
	}

	eventually(t, "the follower seeing the exit and the next boot", func() bool {
		recs := followed.records()
		b, ok := exitIn(recs, 3)
		return ok && b == boot && bootAfter(recs, b)
	})
	stopFollowing()
	select {
	case err := <-followDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("following ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("following did not end with its context")
	}

	must(t, h.Hypervisor.Stop(ctx, "m1"))
	if after := readLog(t, h.Hypervisor, "m1", all); len(after) < len(recs) {
		t.Errorf("%d records after the VM stopped, %d before", len(after), len(recs))
	}
	must(t, h.Hypervisor.DeleteLogs(ctx, "m1"))
	if gone := readLog(t, h.Hypervisor, "m1", all); len(gone) != 0 {
		t.Errorf("%d records after the log was deleted", len(gone))
	}
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
}
