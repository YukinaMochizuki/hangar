package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
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
	_, obj, err := checkLogs(ctx, b, p, key, m.TailLines, m.GetSince())
	if err != nil {
		return err
	}
	return s.machineLogs(ctx, p, "container", obj.(*computev1alpha1.Container).GetMetadata(), machineName(key.Name),
		&nodev1.LogsRequest{Follow: m.GetFollow(), TailLines: m.TailLines, Since: m.GetSince()},
		func(recs []*nodev1.LogRecord) error {
			return stream.Send(&computev1alpha1.ContainerLogsResponse{Records: containerLogRecords(recs)})
		})
}

// checkLogs checks a request for an object's logs and reads the object, if
// the caller may read its logs.
func checkLogs(ctx context.Context, b *backend, p auth.Principal, key resource.Key, tail *int64, since *timestamppb.Timestamp) (store.Object, proto.Message, error) {
	if err := b.checkKey(key); err != nil {
		return store.Object{}, nil, err
	}
	if tail != nil && *tail < 0 {
		return store.Object{}, nil, invalid("tail_lines cannot be negative")
	}
	if since != nil {
		if err := since.CheckValid(); err != nil {
			return store.Object{}, nil, invalid("since: %v", err)
		}
	}
	return b.read(ctx, p, authz.Logs, key)
}

// machineLogs streams the console log of the machine name, which the object
// owner, what it is in words, runs as, from the machine's node, through
// send.
func (s *Server) machineLogs(ctx context.Context, p auth.Principal, what string, owner *metav1.ObjectMeta, name string,
	req *nodev1.LogsRequest, send func([]*nodev1.LogRecord) error) error {
	key := resource.Key{Project: owner.GetProject(), Name: owner.GetName()}
	mobj, err := s.get(ctx, registry.Machine, resource.Key{Project: key.Project, Name: name})
	if err != nil {
		return apiError(err)
	}
	machine, _ := mobj.(*computev1alpha1.Machine)
	if machine == nil || !ownedBy(machine.GetMetadata(), owner.GetUid()) {
		return failedPrecondition("%s %q has no machine yet", what, key.Name)
	}
	node := machine.GetSpec().GetNode()
	if node == "" {
		return failedPrecondition("%s %q is not placed on a node yet", what, key.Name)
	}
	project, err := s.get(ctx, registry.Project, resource.Key{Name: key.Project})
	if err != nil {
		return apiError(err)
	}
	if project == nil {
		return failedPrecondition("project %q is gone", key.Project)
	}
	call, err := s.callNode(p, project.(*tenancyv1alpha1.Project).GetSpec().GetTenant(), node, fmt.Sprintf("%s %q", what, key.Name))
	if err != nil {
		return err
	}
	defer call.release()
	req.MachineUid = machine.GetMetadata().GetUid()
	logs, err := call.nodes.Logs(ctx, connect.NewRequest(req))
	if err == nil {
		defer func() { _ = logs.Close() }()
		for logs.Receive() {
			if err := send(logs.Msg().GetRecords()); err != nil {
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

// virtualMachineLogs serves VirtualMachineLogsService.
type virtualMachineLogs struct{ s *Server }

func (l virtualMachineLogs) VirtualMachineLogs(ctx context.Context, req *connect.Request[computev1alpha1.VirtualMachineLogsRequest], stream *connect.ServerStream[computev1alpha1.VirtualMachineLogsResponse]) error {
	s := l.s
	b := s.backend(registry.VirtualMachine)
	p, err := b.begin(ctx)
	if err != nil {
		return err
	}
	m := req.Msg
	key := resource.Key{Project: m.GetProject(), Name: m.GetName()}
	_, obj, err := checkLogs(ctx, b, p, key, m.TailLines, m.GetSince())
	if err != nil {
		return err
	}
	return s.machineLogs(ctx, p, "virtual machine", obj.(*computev1alpha1.VirtualMachine).GetMetadata(), vmMachineName(key.Name),
		&nodev1.LogsRequest{Follow: m.GetFollow(), TailLines: m.TailLines, Since: m.GetSince()},
		func(recs []*nodev1.LogRecord) error {
			return stream.Send(&computev1alpha1.VirtualMachineLogsResponse{Records: vmLogRecords(recs)})
		})
}

// vmLogRecords are a VM's serial console records, as its launcher frames
// them: output, and how each boot ended.
func vmLogRecords(recs []*nodev1.LogRecord) []*computev1alpha1.VirtualMachineLogRecord {
	out := make([]*computev1alpha1.VirtualMachineLogRecord, 0, len(recs))
	for _, r := range recs {
		vr := &computev1alpha1.VirtualMachineLogRecord{Boot: r.GetBoot(), Time: r.GetTime()}
		switch c := r.GetContent().(type) {
		case *nodev1.LogRecord_Data:
			vr.Content = &computev1alpha1.VirtualMachineLogRecord_Data{Data: c.Data}
		case *nodev1.LogRecord_Exit:
			// In a VM's console log, only a boot the platform ended has a
			// signal (driver.StoppedSignal).
			vr.Content = &computev1alpha1.VirtualMachineLogRecord_End{End: &computev1alpha1.VirtualMachineBootEnd{
				Code: c.Exit.GetCode(), Stopped: c.Exit.GetSignal() != 0,
			}}
		default:
			continue
		}
		out = append(out, vr)
	}
	return out
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
