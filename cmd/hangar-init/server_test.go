package main

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/mdlayher/vsock"
	"google.golang.org/protobuf/proto"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
)

// exports records what SetExport was given.
type exports struct {
	guestv1connect.UnimplementedFileShareServerServiceHandler
	mu  sync.Mutex
	got []*guestv1.FileShareExport
}

func (e *exports) SetExport(_ context.Context, req *connect.Request[guestv1.SetExportRequest]) (*connect.Response[guestv1.SetExportResponse], error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, req.Msg.GetExport())
	return connect.NewResponse(&guestv1.SetExportResponse{}), nil
}

// clients of both services over one connection from a peer at cid.
func (l *pipeListener) serverClients(t *testing.T, cid uint32) (guestv1connect.GuestAgentServiceClient, guestv1connect.FileShareServerServiceClient) {
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: protocols, DialContext: func(context.Context, string, string) (net.Conn, error) { return l.dial(cid), nil }}
	t.Cleanup(tr.CloseIdleConnections)
	hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	return guestv1connect.NewGuestAgentServiceClient(hc, "http://guest"), guestv1connect.NewFileShareServerServiceClient(hc, "http://guest")
}

// A FileShare's server answers the host's Ping, Stop and SetExport, and
// nobody else; it runs nothing, so Exec and SetIPv6 are refused.
func TestTheServersAgent(t *testing.T) {
	ln := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	defer func() { _ = ln.Close() }()
	ex := &exports{}
	stopped := make(chan struct{}, 1)
	serveServerAgent(ln, ex, func() { stopped <- struct{}{} }, time.Now(), t.Logf)
	ctx := t.Context()
	want := &guestv1.FileShareExport{Squash: guestv1.FileShareSquash_FILE_SHARE_SQUASH_ROOT, Clients: []*guestv1.FileShareClient{{Address: "10.0.0.2"}}}

	_, guestExports := ln.serverClients(t, 3)
	if _, err := guestExports.SetExport(ctx, connect.NewRequest(&guestv1.SetExportRequest{Export: want})); err == nil {
		t.Error("a peer in the guest set the export")
	}

	agentC, exportsC := ln.serverClients(t, vsock.Host)
	if _, err := agentC.Ping(ctx, connect.NewRequest(&guestv1.PingRequest{})); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if _, err := exportsC.SetExport(ctx, connect.NewRequest(&guestv1.SetExportRequest{Export: want})); err != nil {
		t.Errorf("SetExport: %v", err)
	}
	ex.mu.Lock()
	if len(ex.got) != 1 || !proto.Equal(ex.got[0], want) {
		t.Errorf("the server was given %v, want %v", ex.got, want)
	}
	ex.mu.Unlock()

	stream := agentC.Exec(ctx)
	_ = stream.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Start{Start: &guestv1.ExecStart{Process: &guestv1.Process{Args: []string{"sh"}}}}})
	if _, err := stream.Receive(); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("Exec: %v, want Unimplemented", err)
	}
	_ = stream.CloseResponse()
	if _, err := agentC.SetIPv6(ctx, connect.NewRequest(&guestv1.SetIPv6Request{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("SetIPv6: %v, want Unimplemented", err)
	}

	select {
	case <-stopped:
		t.Fatal("stopped before Stop")
	default:
	}
	if _, err := agentC.Stop(ctx, connect.NewRequest(&guestv1.StopRequest{})); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Error("Stop did not stop the server")
	}
}
