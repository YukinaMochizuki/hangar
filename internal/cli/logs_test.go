package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/cli"
)

// fakeLogs serves ContainerLogs with fixed records, recording the
// request and who sent it.
type fakeLogs struct {
	mu   sync.Mutex
	req  *computev1alpha1.ContainerLogsRequest
	user string
	recs []*computev1alpha1.ContainerLogRecord
}

func (f *fakeLogs) ContainerLogs(_ context.Context, req *connect.Request[computev1alpha1.ContainerLogsRequest], stream *connect.ServerStream[computev1alpha1.ContainerLogsResponse]) error {
	f.mu.Lock()
	f.req, f.user = req.Msg, req.Header().Get(auth.DevUserHeader)
	recs := f.recs
	f.mu.Unlock()
	if req.Msg.GetName() == "absent" {
		return connect.NewError(connect.CodeNotFound, nil)
	}
	return stream.Send(&computev1alpha1.ContainerLogsResponse{Records: recs})
}

var logT0 = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func data(boot int64, s computev1alpha1.ContainerLogStream, sec int, text string) *computev1alpha1.ContainerLogRecord {
	return &computev1alpha1.ContainerLogRecord{Boot: boot, Stream: s, Time: timestamppb.New(logT0.Add(time.Duration(sec) * time.Second)),
		Content: &computev1alpha1.ContainerLogRecord_Data{Data: []byte(text)}}
}

func exit(boot int64, sec int, code, signal int32) *computev1alpha1.ContainerLogRecord {
	return &computev1alpha1.ContainerLogRecord{Boot: boot, Stream: computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_EXIT,
		Time:    timestamppb.New(logT0.Add(time.Duration(sec) * time.Second)),
		Content: &computev1alpha1.ContainerLogRecord_Exit{Exit: &computev1alpha1.ContainerExitStatus{Code: code, Signal: signal}}}
}

const (
	stdout = computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_STDOUT
	stderr = computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_STDERR
	initS  = computev1alpha1.ContainerLogStream_CONTAINER_LOG_STREAM_INIT
)

func twoBootLogs() []*computev1alpha1.ContainerLogRecord {
	return []*computev1alpha1.ContainerLogRecord{
		data(1, initS, 0, "hangar-init: starting the container\n"),
		data(1, stdout, 1, "one\ntw"),
		data(1, stdout, 2, "o\n"),
		data(1, stderr, 3, "oops\n"),
		exit(1, 4, 1, 0),
		data(2, stdout, 5, "three\n"),
		exit(2, 6, 137, 9),
	}
}

func runLogs(t *testing.T, f *fakeLogs, args ...string) (int, string, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(computev1alpha1connect.NewContainerLogsServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	config := filepath.Join(t.TempDir(), "config.yaml")
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := cli.Run(context.Background(), args, cli.IO{In: strings.NewReader(""), Out: &out, Err: &errb, Env: func(k string) string {
			if k == "HANGAR_CONFIG" {
				return config
			}
			return ""
		}}, cli.Options{Now: func() time.Time { return logT0.Add(time.Hour) }})
		return code, out.String(), errb.String()
	}
	if code, _, e := run("config", "set-context", "dev", "--endpoint", srv.URL, "--auth", "dev", "--user", "alice@example.com", "--project", "web", "--use"); code != 0 {
		t.Fatal(e)
	}
	return run(args...)
}

// TestLogsSplitsTheStreams: stdout to stdout, stderr to stderr, and
// hangar-init's messages and each exit to stderr as hangar: lines.
func TestLogsSplitsTheStreams(t *testing.T) {
	f := &fakeLogs{recs: twoBootLogs()}
	code, out, errOut := runLogs(t, f, "logs", "ct", "api")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if out != "one\ntwo\nthree\n" {
		t.Errorf("stdout %q", out)
	}
	want := "hangar: starting the container\noops\nhangar: exited with code 1 (boot 1)\nhangar: exited with code 137, killed by signal 9 (boot 2)\n"
	if errOut != want {
		t.Errorf("stderr\n%q\nwant\n%q", errOut, want)
	}
	if f.req.GetProject() != "web" || f.req.GetName() != "api" || f.req.GetFollow() || f.req.TailLines != nil || f.req.Since != nil || f.user != "alice@example.com" {
		t.Errorf("asked %v as %q", f.req, f.user)
	}
}

func TestLogsOptions(t *testing.T) {
	f := &fakeLogs{}
	if code, _, e := runLogs(t, f, "logs", "container", "api", "-f", "--tail", "5", "--since", "10m"); code != 0 {
		t.Fatal(e)
	}
	if !f.req.GetFollow() || f.req.GetTailLines() != 5 || !f.req.GetSince().AsTime().Equal(logT0.Add(50*time.Minute)) {
		t.Errorf("asked %v", f.req)
	}
	if code, _, e := runLogs(t, f, "logs", "ct", "api", "--tail", "-1"); code != 0 || f.req.TailLines != nil {
		t.Errorf("--tail -1: exit %d, %s, asked %v", code, e, f.req)
	}
	if code, _, e := runLogs(t, f, "logs", "ct", "api", "--since", "2026-10-12T09:30:00Z", "--tail", "0"); code != 0 {
		t.Fatal(e)
	}
	if !f.req.GetSince().AsTime().Equal(logT0.Add(30*time.Minute)) || f.req.TailLines == nil || f.req.GetTailLines() != 0 {
		t.Errorf("asked %v", f.req)
	}
	for _, args := range [][]string{
		{"logs", "ct"},
		{"logs", "machine", "x"},
		{"logs", "ct", "api", "--since", "yesterday"},
		{"logs", "ct", "api", "-o", "yaml"},
		{"logs", "ct", "api", "--tail", "-2"},
	} {
		if code, _, _ := runLogs(t, f, args...); code != cli.ExitUsage {
			t.Errorf("hangar %s: exit %d, want usage", strings.Join(args, " "), code)
		}
	}
	if code, _, _ := runLogs(t, f, "logs", "ct", "absent"); code != cli.ExitNotFound {
		t.Errorf("a container not found: exit %d", code)
	}
}

func TestLogsTimestampsStartEachLine(t *testing.T) {
	code, out, _ := runLogs(t, &fakeLogs{recs: twoBootLogs()}, "logs", "ct", "api", "--timestamps")
	if code != 0 {
		t.Fatal(code)
	}
	want := "2026-10-12T09:00:01Z one\n2026-10-12T09:00:01Z tw" + "o\n2026-10-12T09:00:05Z three\n"
	if out != want {
		t.Errorf("stdout\n%q\nwant\n%q", out, want)
	}
}

// TestLogsJSON: one object per record, with its stream and boot; output
// that is not UTF-8 comes in base64.
func TestLogsJSON(t *testing.T) {
	recs := append(twoBootLogs(), data(2, stdout, 7, "\xff\xfe"))
	code, out, _ := runLogs(t, &fakeLogs{recs: recs}, "logs", "ct", "api", "-o", "json")
	if code != 0 {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(recs) {
		t.Fatalf("%d lines for %d records:\n%s", len(lines), len(recs), out)
	}
	var first, exited, binary map[string]any
	for i, p := range map[int]*map[string]any{1: &first, 4: &exited, 7: &binary} {
		if err := json.Unmarshal([]byte(lines[i]), p); err != nil {
			t.Fatal(err)
		}
	}
	if first["stream"] != "stdout" || first["boot"] != 1.0 || first["data"] != "one\ntw" || first["time"] != "2026-10-12T09:00:01Z" {
		t.Errorf("a record of output: %v", first)
	}
	if exited["stream"] != "exit" || exited["exit"].(map[string]any)["code"] != 1.0 {
		t.Errorf("an exit: %v", exited)
	}
	if binary["dataBase64"] != "//4=" || binary["data"] != nil {
		t.Errorf("output that is not UTF-8: %v", binary)
	}
}
