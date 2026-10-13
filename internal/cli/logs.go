package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

func (a *app) logsCmd() *cobra.Command {
	var follow, timestamps bool
	var tail int64
	var since, output string
	cmd := &cobra.Command{
		Use:   "logs KIND NAME",
		Short: "Print what a container or a virtual machine wrote, across its boots",
		Long: "Prints one stream across every boot the container's node keeps, oldest\n" +
			"first: its stdout to stdout, its stderr to stderr, and hangar-init's\n" +
			"messages and each exit to stderr as hangar: lines. -o json prints one\n" +
			"object per record, with its stream and boot.\n\n" +
			"For a virtual machine, prints its serial console to stdout, and how each\n" +
			"boot ended to stderr, the same way.",
		Args: args(2, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "" && output != "json" {
				return usageErr("unknown output %q; want json", output)
			}
			r, err := findKind(args[0])
			if err != nil {
				return err
			}
			if r.Kind != registry.Container.Kind && r.Kind != registry.VirtualMachine.Kind {
				return usageErr("only containers and virtual machines have logs, not %s", r.Plural)
			}
			req := &computev1alpha1.ContainerLogsRequest{Name: args[1], Follow: follow}
			switch {
			case tail >= 0:
				req.TailLines = &tail
			case tail != -1:
				return usageErr("--tail %d: want a number of lines, or -1 for every line", tail)
			}
			if since != "" {
				t, err := parseSince(since, a.opts.Now())
				if err != nil {
					return err
				}
				req.Since = timestamppb.New(t)
			}
			c, err := a.current()
			if err != nil {
				return err
			}
			if req.Project, err = a.projectFor(r, c); err != nil {
				return err
			}
			creds, err := a.credentials(c)
			if err != nil {
				return err
			}
			hc, base := a.httpClient(c)
			p := &logPrinter{out: cmd.OutOrStdout(), err: cmd.ErrOrStderr(), json: output == "json", timestamps: timestamps}
			if r.Kind == registry.VirtualMachine.Kind {
				logs := computev1alpha1connect.NewVirtualMachineLogsServiceClient(hc, base, connect.WithInterceptors(creds))
				stream, err := logs.VirtualMachineLogs(cmd.Context(), connect.NewRequest(&computev1alpha1.VirtualMachineLogsRequest{
					Project: req.Project, Name: req.Name, Follow: req.Follow, TailLines: req.TailLines, Since: req.Since,
				}))
				if err != nil {
					return err
				}
				defer func() { _ = stream.Close() }()
				for stream.Receive() {
					for _, rec := range stream.Msg().GetRecords() {
						if err := p.printVM(rec); err != nil {
							return err
						}
					}
				}
				return stream.Err()
			}
			logs := computev1alpha1connect.NewContainerLogsServiceClient(hc, base, connect.WithInterceptors(creds))
			stream, err := logs.ContainerLogs(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			defer func() { _ = stream.Close() }()
			for stream.Receive() {
				for _, rec := range stream.Msg().GetRecords() {
					if err := p.print(rec); err != nil {
						return err
					}
				}
			}
			return stream.Err()
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "go on printing what is written, until the container exits for good, the virtual machine powers off, or either is stopped")
	cmd.Flags().Int64Var(&tail, "tail", -1, "print only the last N lines, or every line with -1; each exit counts as one")
	cmd.Flags().StringVar(&since, "since", "", "print only what is newer than a duration ago, such as 10m, or an RFC 3339 time")
	cmd.Flags().BoolVar(&timestamps, "timestamps", false, "start each line with its time")
	cmd.Flags().StringVarP(&output, "output", "o", "", "json, for one object per record")
	return cmd
}

// printVM prints a virtual machine's log record as a container's is
// printed: its serial console as stdout, and the end of each boot as an
// exit, saying how the boot ended.
func (p *logPrinter) printVM(r *computev1alpha1.VirtualMachineLogRecord) error {
	rec := &computev1alpha1.ContainerLogRecord{Boot: r.GetBoot(), Time: r.GetTime()}
	end := r.GetEnd()
	if end == nil {
		rec.Stream = computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_STDOUT
		rec.Content = &computev1alpha1.ContainerLogRecord_Data{Data: r.GetData()}
		return p.print(rec)
	}
	rec.Stream = computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_EXIT
	if p.json {
		return p.printJSON(logJSON{
			Time: rec.GetTime().AsTime().Format(time.RFC3339Nano), Boot: rec.GetBoot(), Stream: streamName(rec.GetStream()),
			Exit: &logJSONExit{Code: end.GetCode(), Stopped: end.GetStopped()},
		})
	}
	var msg string
	switch {
	case end.GetStopped() && end.GetCode() == 0:
		msg = "hangar: stopped; the guest powered off"
	case end.GetStopped():
		msg = "hangar: stopped; the guest did not power off, and was killed"
	case end.GetCode() == 0:
		msg = "hangar: the guest powered off"
	default:
		msg = fmt.Sprintf("hangar: the VM ended without powering off, code %d", end.GetCode())
	}
	if rec.GetBoot() > 0 {
		msg += fmt.Sprintf(" (boot %d)", rec.GetBoot())
	}
	return p.line(rec, []byte(msg+"\n"))
}

func parseSince(s string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			return time.Time{}, usageErr("--since %s is in the future", s)
		}
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Time{}, usageErr("--since %q is neither a duration, such as 10m, nor an RFC 3339 time", s)
}

// logPrinter prints log records as hangar logs does. It keeps, for each
// of stdout and stderr, whether the last line ended, so a timestamp starts
// only a new line.
type logPrinter struct {
	out, err   io.Writer
	json       bool
	timestamps bool
	midLine    map[io.Writer]bool
}

// logJSON is one record as -o json prints it. Data that is not UTF-8 is
// printed in base64 as dataBase64 instead.
type logJSON struct {
	Time       string       `json:"time"`
	Boot       int64        `json:"boot"`
	Stream     string       `json:"stream"`
	Data       *string      `json:"data,omitempty"`
	DataBase64 []byte       `json:"dataBase64,omitempty"`
	Exit       *logJSONExit `json:"exit,omitempty"`
}

type logJSONExit struct {
	Code   int32 `json:"code"`
	Signal int32 `json:"signal"`
	// Stopped is a virtual machine's boot the platform ended.
	Stopped bool `json:"stopped,omitempty"`
}

func streamName(s computev1alpha1.ContainerLogStream) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "CONTAINER_LOG_STREAM_"))
}

func (p *logPrinter) print(rec *computev1alpha1.ContainerLogRecord) error {
	if p.json {
		j := logJSON{Time: rec.GetTime().AsTime().Format(time.RFC3339Nano), Boot: rec.GetBoot(), Stream: streamName(rec.GetStream())}
		if rec.GetStream() == computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_EXIT {
			j.Exit = &logJSONExit{Code: rec.GetExit().GetCode(), Signal: rec.GetExit().GetSignal()}
		} else if data := rec.GetData(); utf8.Valid(data) {
			s := string(data)
			j.Data = &s
		} else {
			j.DataBase64 = data
		}
		return p.printJSON(j)
	}
	switch rec.GetStream() {
	case computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_STDOUT:
		return p.write(p.out, rec, rec.GetData())
	case computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_STDERR:
		return p.write(p.err, rec, rec.GetData())
	case computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_INIT:
		var b []byte
		for line := range strings.Lines(string(rec.GetData())) {
			line = strings.TrimSuffix(line, "\n")
			b = fmt.Appendf(b, "hangar: %s\n", strings.TrimPrefix(line, "hangar-init: "))
		}
		return p.line(rec, b)
	case computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_EXIT:
		st := rec.GetExit()
		msg := fmt.Sprintf("hangar: exited with code %d", st.GetCode())
		if st.GetSignal() != 0 {
			msg += fmt.Sprintf(", killed by signal %d", st.GetSignal())
		}
		if rec.GetBoot() > 0 {
			msg += fmt.Sprintf(" (boot %d)", rec.GetBoot())
		}
		return p.line(rec, []byte(msg+"\n"))
	}
	return nil
}

// printJSON prints one record as -o json does.
func (p *logPrinter) printJSON(j logJSON) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(j); err != nil {
		return err
	}
	_, err := p.out.Write(b.Bytes())
	return err
}

// line writes whole lines of hangar's own to stderr, on a line of their
// own.
func (p *logPrinter) line(rec *computev1alpha1.ContainerLogRecord, b []byte) error {
	if p.midLine[p.err] {
		b = append([]byte{'\n'}, b...)
		p.midLine[p.err] = false
	}
	return p.write(p.err, rec, b)
}

func (p *logPrinter) write(w io.Writer, rec *computev1alpha1.ContainerLogRecord, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if p.midLine == nil {
		p.midLine = map[io.Writer]bool{}
	}
	if p.timestamps {
		stamp := rec.GetTime().AsTime().UTC().Format(time.RFC3339Nano) + " "
		var b []byte
		for line := range strings.Lines(string(data)) {
			if !p.midLine[w] {
				b = append(b, stamp...)
			}
			b = append(b, line...)
			p.midLine[w] = !strings.HasSuffix(line, "\n")
		}
		data = b
	} else {
		p.midLine[w] = data[len(data)-1] != '\n'
	}
	_, err := w.Write(data)
	return err
}
