package serialmux

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestPumpDialsAgainAndEndsGently: the pump hands what comes on Cloud
// Hypervisor's socket to the log; when the connection ends, as at a guest's
// reboot, it dials again and every client is told the screen is new; and
// when it is done, it ends its writing first, reading what Cloud Hypervisor
// still writes, before it closes.
func TestPumpDialsAgainAndEndsGently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	h, log, _ := testHub(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		h.Pump(ctx, func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		})
	}()

	first, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write([]byte("first boot\r\n")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the first boot's output in the log", func() (bool, string) {
		return strings.Contains(log.String(), "first boot"), log.String()
	})
	a := attach(t, h, &nodev1.ConsoleStart{})
	a.state()
	a.output()

	// The guest reboots: Cloud Hypervisor's connection ends.
	_ = first.Close()
	second, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if st := a.state(); st.GetSize().GetColumns() != DefaultColumns || st.GetSize().GetRows() != DefaultRows {
		t.Errorf("after the reconnect the client is told %v", st)
	}
	a.output()
	if _, err := second.Write([]byte("second boot\r\n")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the second boot's output in the log", func() (bool, string) {
		return strings.Contains(log.String(), "second boot"), log.String()
	})

	// Done: the pump's side ends its writing, and still reads.
	cancel()
	if n, err := second.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Cloud Hypervisor's side read %d, %v, not the end of the pump's writing", n, err)
	}
	if _, err := second.Write([]byte("powering off\r\n")); err != nil {
		t.Fatalf("writing after the pump ended its side: %v", err)
	}
	testutil.Eventually(t, "what came after the pump's end in the log", func() (bool, string) {
		return strings.Contains(log.String(), "powering off"), log.String()
	})
	_ = second.Close()
	<-pumped
}
