package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
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
	project, err := s.get(ctx, registry.Project, resource.Key{Name: key.Project})
	if err != nil {
		return apiError(err)
	}
	if project == nil {
		return failedPrecondition("project %q is gone", key.Project)
	}
	call, err := s.callNode(p, project.(*tenancyv1alpha1.Project).GetSpec().GetTenant(), node, fmt.Sprintf("container %q", key.Name))
	if err != nil {
		return err
	}
	defer call.release()
	logs, err := call.nodes.Logs(ctx, connect.NewRequest(&nodev1.LogsRequest{
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
	return call.failed(err)
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
