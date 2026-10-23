package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/mdlayher/vsock"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/buildinfo"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/guest/converter"
)

// runConverter makes the guest a converter VM, in the initramfs, with no
// root disk and no network: it serves the host qemu-img on the file it
// has as a disk until the node stops it (docs/decisions/custom-images.md).
// Only the converter initramfs carries qemu-img; the platform's has none,
// and its guest fails here.
func runConverter(logf func(string, ...any), started time.Time) (console.ExitStatus, error) {
	failed := console.ExitStatus{Code: exitBootFailed}
	if _, err := os.Stat(converter.QemuImg); err != nil {
		return failed, fmt.Errorf("a converter VM needs the converter initramfs, with qemu-img: %w", err)
	}
	ln, err := vsock.Listen(agent.Port, nil)
	if err != nil {
		return failed, fmt.Errorf("guest agent: %w", err)
	}
	stopped := make(chan struct{})
	var once sync.Once
	serveConverterAgent(ln, func() { once.Do(func() { close(stopped) }) }, started, logf)
	<-stopped
	logf("hangar-init: stopping the converter")
	return console.ExitStatus{}, nil
}

// serveConverterAgent serves a converter VM's agent on ln to the host
// alone: Ping, Stop, which calls stop, and ImageConverterService. It runs
// nothing else, so Exec is refused, as is SetIPv6. It returns at once.
func serveConverterAgent(ln net.Listener, stop func(), started time.Time, logf func(string, ...any)) {
	ln = agent.HostOnly(ln, logf)
	mux := http.NewServeMux()
	mux.Handle(guestv1connect.NewGuestAgentServiceHandler(&agent.Server{
		Root: "/", Version: buildinfo.Version() + buildMarker, Started: started,
		OnPanic:  func(r any) { logf("hangar-init: guest agent: %v", r) },
		StopMain: func(time.Duration) { stop() },
	}, connect.WithReadMaxBytes(agent.MaxMessage)))
	mux.Handle(guestv1connect.NewImageConverterServiceHandler(&converter.Server{}, connect.WithReadMaxBytes(agent.MaxMessage)))
	serveHTTP(ln, mux)
}
