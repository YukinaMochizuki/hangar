package node

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/tunnel"
)

// serveTunnel keeps the agent's tunnel to the apiserver open, serving
// NodeService on it, until ctx ends.
func (a *Agent) serveTunnel(ctx context.Context) {
	mux := http.NewServeMux()
	mux.Handle(nodev1connect.NewNodeServiceHandler(nodeService{a: a, agent: ctx}, connect.WithReadMaxBytes(1<<20)))
	(&tunnel.Agent{Dial: a.cfg.Tunnel, Handler: mux, Liveness: a.cfg.TunnelLiveness, Log: a.cfg.Log}).Run(ctx)
}

// nodeService is the NodeService the agent serves the apiserver. agent
// ends when the agent stops.
type nodeService struct {
	a     *Agent
	agent context.Context
}

// followEndDelay is how long a follow goes on once its VM is done, for the
// driver's reader to take what the VM wrote last: chvm reads every 250 ms.
const followEndDelay = time.Second

// errVMDone ends following a log whose VM is done.
var errVMDone = errors.New("the machine's VM is done")

func (s nodeService) Logs(ctx context.Context, req *connect.Request[nodev1.LogsRequest], stream *connect.ServerStream[nodev1.LogsResponse]) error {
	if s.a.cfg.Drivers == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this node runs no machines"))
	}
	m := req.Msg
	if m.GetMachineUid() == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("machine_uid is required"))
	}
	opts := driver.LogOptions{Follow: m.GetFollow(), Tail: driver.AllLines}
	if m.TailLines != nil {
		if m.GetTailLines() < 0 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("tail_lines cannot be negative"))
		}
		opts.Tail = m.GetTailLines()
	}
	if m.Since != nil {
		opts.Since = m.GetSince().AsTime()
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if opts.Follow {
		go s.a.endWithVM(ctx, m.GetMachineUid(), cancel)
	}
	err := s.a.cfg.Drivers.Hypervisor.ReadLog(ctx, m.GetMachineUid(), opts, func(recs []driver.LogRecord) error {
		return stream.Send(&nodev1.LogsResponse{Records: logRecords(recs)})
	})
	switch {
	case errors.Is(context.Cause(ctx), errVMDone):
		return nil
	case s.agent.Err() != nil:
		return connect.NewError(connect.CodeUnavailable, errors.New("the node agent is stopping"))
	case ctx.Err() != nil:
		return connect.NewError(connect.CodeCanceled, ctx.Err())
	case err != nil:
		return connect.NewError(connect.CodeUnknown, fmt.Errorf("reading machine %s's log: %w", m.GetMachineUid(), err))
	}
	return nil
}

// endWithVM ends a follow of the machine's log, by cancel, once its VM,
// seen running, has exited for good or is gone, as a stop leaves it: the
// log will have no more. It waits followEndDelay first.
func (a *Agent) endWithVM(ctx context.Context, uid string, cancel context.CancelCauseFunc) {
	poll := a.cfg.LogPoll
	if poll == 0 {
		poll = time.Second
	}
	ticker := a.cfg.Clock.NewTicker(poll)
	defer ticker.Stop()
	seen := false
	for {
		vm, err := a.vmOf(ctx, uid)
		switch {
		case err != nil:
		case vm != nil && vm.State == driver.VMRunning:
			seen = true
		case vm != nil && vm.State == driver.VMExited, vm == nil && seen:
			select {
			case <-ctx.Done():
			case <-a.cfg.Clock.After(followEndDelay):
				cancel(errVMDone)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
		}
	}
}

func logRecords(recs []driver.LogRecord) []*nodev1.LogRecord {
	out := make([]*nodev1.LogRecord, 0, len(recs))
	for _, r := range recs {
		pr := &nodev1.LogRecord{Boot: r.Boot, Time: timestamppb.New(r.Time)}
		switch r.Stream {
		case console.Stdout:
			pr.Stream = nodev1.LogStream_LOG_STREAM_STDOUT
		case console.Stderr:
			pr.Stream = nodev1.LogStream_LOG_STREAM_STDERR
		case console.Init:
			pr.Stream = nodev1.LogStream_LOG_STREAM_INIT
		case console.Exit:
			pr.Stream = nodev1.LogStream_LOG_STREAM_EXIT
			pr.Content = &nodev1.LogRecord_Exit{Exit: &nodev1.ExitStatus{Code: r.Exit.Code, Signal: r.Exit.Signal}}
		default:
			continue
		}
		if r.Stream != console.Exit {
			pr.Content = &nodev1.LogRecord_Data{Data: r.Data}
		}
		out = append(out, pr)
	}
	return out
}
