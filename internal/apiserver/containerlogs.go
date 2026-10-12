package apiserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/tunnel"
)

// openTunnel takes a node agent's tunnel (internal/tunnel). The tunnel is
// the node the listener's chain authenticates the request as, which on the
// node socket is the name PeerCred is configured with; nothing the request
// says names the node.
func (s *Server) openTunnel(chain auth.Chain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, err := chain.Authenticate(r.Context(), r.Header)
		if err != nil {
			http.Error(w, "unauthenticated: "+err.Error(), http.StatusUnauthorized)
			return
		}
		p, _ := auth.FromContext(ctx)
		if p.Type != auth.Node || !slices.Contains(s.cfg.Nodes, p.ID) {
			http.Error(w, p.String()+" is not a node agent this apiserver accepts", http.StatusForbidden)
			return
		}
		s.tunnels.Accept(w, r, p.ID)
	}
}

// nodeClient is node's NodeService, reached over the tunnel the node keeps
// open, and a channel closed once that tunnel drops. Reaching nodes some
// other way changes only this.
func (s *Server) nodeClient(node string) (nodev1connect.NodeServiceClient, <-chan struct{}, error) {
	t, err := s.tunnels.Node(node)
	if err != nil {
		return nil, nil, err
	}
	return nodev1connect.NewNodeServiceClient(t.HTTPClient(), "http://"+node, connect.WithReadMaxBytes(maxNodeMessage)), t.Done(), nil
}

// maxNodeMessage bounds a message from a node: a batch of log records is
// about driver.MaxLogBatch, and one record at most 64 KiB more.
const maxNodeMessage = 1 << 20

// containerLogs serves ContainerLogsService.
type containerLogs struct{ s *Server }

func (l containerLogs) ContainerLogs(ctx context.Context, req *connect.Request[computev1alpha1.ContainerLogsRequest], stream *connect.ServerStream[computev1alpha1.ContainerLogsResponse]) error {
	s := l.s
	b := s.backend(registry.Container)
	p, err := b.begin(ctx)
	if err != nil {
		return err
	}
	m := req.Msg
	key := resource.Key{Project: m.GetProject(), Name: m.GetName()}
	if err := b.checkKey(key); err != nil {
		return err
	}
	if m.TailLines != nil && m.GetTailLines() < 0 {
		return invalid("tail_lines cannot be negative")
	}
	if m.Since != nil {
		if err := m.GetSince().CheckValid(); err != nil {
			return invalid("since: %v", err)
		}
	}
	_, obj, err := b.read(ctx, p, authz.Logs, key)
	if err != nil {
		return err
	}
	c := obj.(*computev1alpha1.Container)
	mobj, err := s.get(ctx, registry.Machine, resource.Key{Project: key.Project, Name: machineName(key.Name)})
	if err != nil {
		return apiError(err)
	}
	machine, _ := mobj.(*computev1alpha1.Machine)
	if machine == nil || !ownedBy(machine.GetMetadata(), c.GetMetadata().GetUid()) {
		return failedPrecondition("container %q has no machine yet", key.Name)
	}
	node := machine.GetSpec().GetNode()
	if node == "" {
		return failedPrecondition("container %q is not placed on a node yet", key.Name)
	}
	nodes, dropped, err := s.nodeClient(node)
	if errors.Is(err, tunnel.ErrNoTunnel) {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("container %q's node %s is not connected", key.Name, node))
	}
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	logs, err := nodes.Logs(ctx, connect.NewRequest(&nodev1.LogsRequest{
		MachineUid: machine.GetMetadata().GetUid(),
		Follow:     m.GetFollow(),
		TailLines:  m.TailLines,
		Since:      m.GetSince(),
	}))
	if err == nil {
		defer func() { _ = logs.Close() }()
		for logs.Receive() {
			if err := stream.Send(&computev1alpha1.ContainerLogsResponse{Records: containerLogRecords(logs.Msg().GetRecords())}); err != nil {
				return err
			}
		}
		err = logs.Err()
	}
	if err == nil || ctx.Err() != nil {
		return ctx.Err()
	}
	return nodeError(err, dropped, fmt.Sprintf("container %q's node %s", key.Name, node))
}

// tunnelGrace is how long a failed call waits to see whether it failed
// because its tunnel dropped: the call can learn of it first.
const tunnelGrace = 200 * time.Millisecond

// nodeError is what a call to a node that failed with err fails with: a
// tunnel that dropped under it makes it Unavailable.
func nodeError(err error, dropped <-chan struct{}, what string) error {
	code, msg := connect.CodeUnknown, err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) {
		code, msg = ce.Code(), ce.Message()
	}
	t := time.NewTimer(tunnelGrace)
	defer t.Stop()
	select {
	case <-dropped:
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s dropped its connection: %s", what, msg))
	case <-t.C:
	}
	return connect.NewError(code, fmt.Errorf("%s: %s", what, msg))
}

func containerLogRecords(recs []*nodev1.LogRecord) []*computev1alpha1.ContainerLogRecord {
	out := make([]*computev1alpha1.ContainerLogRecord, 0, len(recs))
	for _, r := range recs {
		cr := &computev1alpha1.ContainerLogRecord{Boot: r.GetBoot(), Time: r.GetTime()}
		switch r.GetStream() {
		case nodev1.LogStream_LOG_STREAM_STDOUT:
			cr.Stream = computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_STDOUT
		case nodev1.LogStream_LOG_STREAM_STDERR:
			cr.Stream = computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_STDERR
		case nodev1.LogStream_LOG_STREAM_INIT:
			cr.Stream = computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_INIT
		case nodev1.LogStream_LOG_STREAM_EXIT:
			cr.Stream = computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_EXIT
		default:
			continue
		}
		switch c := r.GetContent().(type) {
		case *nodev1.LogRecord_Data:
			cr.Content = &computev1alpha1.ContainerLogRecord_Data{Data: c.Data}
		case *nodev1.LogRecord_Exit:
			cr.Content = &computev1alpha1.ContainerLogRecord_Exit{Exit: &computev1alpha1.ContainerExitStatus{Code: c.Exit.GetCode(), Signal: c.Exit.GetSignal()}}
		}
		out = append(out, cr)
	}
	return out
}
