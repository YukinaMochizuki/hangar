package main

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/mdlayher/vsock"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/buildinfo"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/guest/boot"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/guest/fileshare"
)

// runServer makes the guest the FileShare's server the boot disk names, in
// the initramfs, with no root disk, until the node stops it.
func runServer(cfg *guestv1.BootConfig, logf func(string, ...any), started time.Time) (console.ExitStatus, error) {
	failed := console.ExitStatus{Code: exitBootFailed}
	if err := boot.Network(cfg.GetInterfaces()); err != nil {
		return failed, fmt.Errorf("network: %w", err)
	}
	srv, err := fileshare.Boot(fileshare.Device, logf)
	if err != nil {
		return failed, fmt.Errorf("the share: %w", err)
	}
	if e := cfg.GetFileShareServer().GetExport(); e != nil {
		if err := srv.Export(e); err != nil {
			return failed, fmt.Errorf("the boot disk's export: %w", err)
		}
	}
	// What the clients delete goes back to the host's storage.
	go boot.TrimEvery(cfg.GetTrimInterval().AsDuration(), logf)
	stopped := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(stopped) }) }
	if ln, err := vsock.Listen(agent.Port, nil); err != nil {
		// It serves its boot disk's export all the same.
		logf("hangar-init: guest agent: %v", err)
	} else {
		serveServerAgent(ln, srv, stop, started, logf)
	}
	<-stopped
	logf("hangar-init: stopping the share's server")
	if err := srv.Stop(); err != nil {
		return console.ExitStatus{Code: 1}, err
	}
	return console.ExitStatus{}, nil
}

// serveServerAgent serves a FileShare's server's agent on ln to the host
// alone: Ping, Stop, which calls stop, GrowFilesystems, and SetExport. It
// runs nothing else, so Exec is refused (but in a build with the testbed
// tag, server_testbed.go), as is SetIPv6. It returns at once.
func serveServerAgent(ln net.Listener, exports guestv1connect.FileShareServerServiceHandler, stop func(), started time.Time, logf func(string, ...any)) {
	ln = agent.HostOnly(ln, logf)
	mux := http.NewServeMux()
	mux.Handle(guestv1connect.NewGuestAgentServiceHandler(&agent.Server{
		Reaper: serverReaper(logf), Root: "/",
		Version: buildinfo.Version() + buildMarker, Started: started,
		OnPanic:  func(r any) { logf("hangar-init: guest agent: %v", r) },
		StopMain: func(time.Duration) { stop() },
		Grow:     growFilesystems(logf),
	}, connect.WithReadMaxBytes(agent.MaxMessage)))
	mux.Handle(guestv1connect.NewFileShareServerServiceHandler(exports, connect.WithReadMaxBytes(agent.MaxMessage)))
	serveHTTP(ln, mux)
}
