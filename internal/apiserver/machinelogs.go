package apiserver

import (
	"context"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// Logs streams the console log of the machine the named object runs as,
// from the machine's node.
func (h machineCapabilities) Logs(ctx context.Context, req *connect.Request[computev1alpha1.LogsRequest], stream *connect.ServerStream[computev1alpha1.LogsResponse]) error {
	s := h.s
	m := req.Msg
	kind, err := kindNamed(m.GetKind())
	if err != nil {
		return err
	}
	r, err := s.reach(ctx, kind, resource.Key{Project: m.GetProject(), Name: m.GetName()}, authz.Logs, func() error {
		if m.TailLines != nil && *m.TailLines < 0 {
			return invalid("tail_lines cannot be negative")
		}
		if m.Since != nil {
			if err := m.GetSince().CheckValid(); err != nil {
				return invalid("since: %v", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	machine, err := s.machineOf(ctx, r)
	if err != nil {
		return err
	}
	call, err := s.callMachine(ctx, r, machine)
	if err != nil {
		return err
	}
	defer call.release()
	logs, err := call.nodes.Logs(ctx, connect.NewRequest(&nodev1.LogsRequest{
		MachineUid: machine.GetMetadata().GetUid(), Follow: m.GetFollow(), TailLines: m.TailLines, Since: m.GetSince(),
	}))
	if err == nil {
		defer func() { _ = logs.Close() }()
		console := machine.GetSpec().GetFirmware() != nil
		for logs.Receive() {
			if err := stream.Send(&computev1alpha1.LogsResponse{Records: machineLogRecords(console, logs.Msg().GetRecords())}); err != nil {
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

// machineLogRecords are a machine's log records as its node frames them. A
// machine that boots firmware, a virtual machine's, has a serial console
// rather than a process's streams; only a boot the platform ended has a
// signal in its exit (driver.StoppedSignal).
func machineLogRecords(console bool, recs []*nodev1.LogRecord) []*computev1alpha1.MachineLogRecord {
	out := make([]*computev1alpha1.MachineLogRecord, 0, len(recs))
	for _, r := range recs {
		mr := &computev1alpha1.MachineLogRecord{Boot: r.GetBoot(), Time: r.GetTime()}
		switch c := r.GetContent().(type) {
		case *nodev1.LogRecord_Data:
			mr.Content = &computev1alpha1.MachineLogRecord_Data{Data: c.Data}
			if console {
				mr.Stream = computev1alpha1.MachineLogStream_MACHINE_LOG_STREAM_CONSOLE
				break
			}
			switch r.GetStream() {
			case nodev1.LogStream_LOG_STREAM_STDOUT:
				mr.Stream = computev1alpha1.MachineLogStream_MACHINE_LOG_STREAM_STDOUT
			case nodev1.LogStream_LOG_STREAM_STDERR:
				mr.Stream = computev1alpha1.MachineLogStream_MACHINE_LOG_STREAM_STDERR
			case nodev1.LogStream_LOG_STREAM_INIT:
				mr.Stream = computev1alpha1.MachineLogStream_MACHINE_LOG_STREAM_INIT
			default:
				continue
			}
		case *nodev1.LogRecord_Exit:
			mr.Stream = computev1alpha1.MachineLogStream_MACHINE_LOG_STREAM_EXIT
			if console {
				mr.Content = &computev1alpha1.MachineLogRecord_Exit{Exit: &computev1alpha1.MachineExitStatus{
					Code: c.Exit.GetCode(), Stopped: c.Exit.GetSignal() != 0,
				}}
			} else {
				mr.Content = &computev1alpha1.MachineLogRecord_Exit{Exit: &computev1alpha1.MachineExitStatus{
					Code: c.Exit.GetCode(), Signal: c.Exit.GetSignal(),
				}}
			}
		default:
			continue
		}
		out = append(out, mr)
	}
	return out
}
