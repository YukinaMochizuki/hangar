package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/cli"
	"github.com/YukinaMochizuki/hangar/internal/execws"
)

// execServer plays the apiserver's side of an exec, running scripted
// commands named by their first word: out writes stdout and stderr and
// exits 7; cat echoes stdin to stdout until it closes; wait ends with the
// first signal; size prints the terminal's size at the start and on each
// resize, until a signal; fail closes the exec with FailedPrecondition;
// hang says and reads nothing; deaf reads nothing but pings. A container
// named absent is refused before the upgrade.
type execServer struct {
	mu     sync.Mutex
	start  *computev1alpha1.ContainerExecStart
	user   string
	path   string
	inputs []*computev1alpha1.ContainerExecRequest
}

func (f *execServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/absent:") {
		execws.WriteError(w, r, connect.NewError(connect.CodeNotFound, errors.New(`container "absent" not found`)))
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{execws.Subprotocol}})
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	ctx := r.Context()
	read := func() (*computev1alpha1.ContainerExecRequest, error) {
		_, b, err := c.Read(ctx)
		if err != nil {
			return nil, err
		}
		req := &computev1alpha1.ContainerExecRequest{}
		return req, proto.Unmarshal(b, req)
	}
	write := func(res *computev1alpha1.ContainerExecResponse) {
		b, _ := proto.Marshal(res)
		_ = c.Write(ctx, websocket.MessageBinary, b)
	}
	stdout := func(s string) {
		write(&computev1alpha1.ContainerExecResponse{Message: &computev1alpha1.ContainerExecResponse_Stdout{Stdout: []byte(s)}})
	}
	exit := func(code, sig int32) {
		write(&computev1alpha1.ContainerExecResponse{Message: &computev1alpha1.ContainerExecResponse_Exit{Exit: &computev1alpha1.ContainerExecExit{Code: code, Signal: sig}}})
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
	first, err := read()
	if err != nil {
		return
	}
	f.mu.Lock()
	f.start, f.user, f.path = first.GetStart(), r.Header.Get(auth.DevUserHeader), r.URL.Path
	f.mu.Unlock()
	size := first.GetStart().GetSize()
	switch first.GetStart().GetCommand()[0] {
	case "out":
		stdout("to stdout\n")
		write(&computev1alpha1.ContainerExecResponse{Message: &computev1alpha1.ContainerExecResponse_Stderr{Stderr: []byte("to stderr\n")}})
		exit(7, 0)
		return
	case "fail":
		code, reason := execws.CloseStatus(connect.NewError(connect.CodeFailedPrecondition, errors.New("machine m1's VM is not running")))
		_ = c.Close(code, reason)
		return
	case "size":
		stdout(fmt.Sprintf("%d %d\n", size.GetRows(), size.GetCols()))
	case "hang":
		<-ctx.Done()
		return
	case "deaf":
		// Reads nothing, as a server held up, but pings.
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				go func() { _ = c.Ping(context.WithoutCancel(ctx)) }()
			}
		}
	}
	for {
		req, err := read()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.inputs = append(f.inputs, req)
		f.mu.Unlock()
		switch m := req.GetMessage().(type) {
		case *computev1alpha1.ContainerExecRequest_Stdin:
			stdout(string(m.Stdin))
		case *computev1alpha1.ContainerExecRequest_StdinClose:
			exit(0, 0)
			return
		case *computev1alpha1.ContainerExecRequest_Resize:
			stdout(fmt.Sprintf("%d %d\n", m.Resize.GetRows(), m.Resize.GetCols()))
		case *computev1alpha1.ContainerExecRequest_Signal:
			exit(128+m.Signal, m.Signal)
			return
		}
	}
}

func (f *execServer) got() (*computev1alpha1.ContainerExecStart, []*computev1alpha1.ContainerExecRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.start, slices.Clone(f.inputs)
}

// signals delivers signals to what an exec subscribes, as the process's
// would be.
type signals struct {
	mu   sync.Mutex
	subs map[chan<- os.Signal][]os.Signal
}

func (s *signals) notify(c chan<- os.Signal, sigs ...os.Signal) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs == nil {
		s.subs = map[chan<- os.Signal][]os.Signal{}
	}
	s.subs[c] = sigs
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subs, c)
	}
}

// raise delivers sig once something subscribes to it.
func (s *signals) raise(t *testing.T, sig os.Signal) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		for c, sigs := range s.subs {
			if slices.Contains(sigs, sig) {
				c <- sig
				s.mu.Unlock()
				return
			}
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("nothing took %v", sig)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeTerminal records what is done to it.
type fakeTerminal struct {
	mu         sync.Mutex
	rows, cols uint16
	raw        bool
	raws       int
}

func (f *fakeTerminal) MakeRaw() (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw = true
	f.raws++
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.raw = false
	}, nil
}

func (f *fakeTerminal) Size() (uint16, uint16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, f.cols, nil
}

func (f *fakeTerminal) resize(rows, cols uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.cols = rows, cols
}

func (f *fakeTerminal) state() (raw bool, raws int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.raw, f.raws
}

type execRun struct {
	// ping and pong time the CLI's liveness; zero is its default.
	ping, pong time.Duration
	server     *execServer
	sigs       *signals
	term       *fakeTerminal
	stdin      io.Reader
}

func newExecRun(t *testing.T) *execRun {
	return &execRun{server: &execServer{}, sigs: &signals{}, term: &fakeTerminal{rows: 24, cols: 80}, stdin: strings.NewReader("")}
}

// run runs hangar against the fake server, returning its exit code, stdout
// and stderr.
func (r *execRun) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	srv := httptest.NewServer(r.server)
	t.Cleanup(srv.Close)
	config := filepath.Join(t.TempDir(), "config.yaml")
	env := func(k string) string {
		if k == "HANGAR_CONFIG" {
			return config
		}
		return ""
	}
	opts := cli.Options{
		ExecPing: r.ping, ExecPong: r.pong,
		Signals:  r.sigs.notify,
		Terminal: func(io.Reader) (cli.Terminal, bool) { return r.term, r.term != nil },
	}
	var out, errb bytes.Buffer
	if code := cli.Run(context.Background(), []string{"config", "set-context", "dev", "--endpoint", srv.URL, "--auth", "dev", "--user", "alice@example.com", "--project", "web", "--use"},
		cli.IO{In: strings.NewReader(""), Out: &out, Err: &errb, Env: env}, opts); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	code := cli.Run(context.Background(), args, cli.IO{In: r.stdin, Out: &out, Err: &errb, Env: env}, opts)
	return code, out.String(), errb.String()
}

func TestExecSplitsTheStreamsAndExitsAsTheCommand(t *testing.T) {
	r := newExecRun(t)
	code, out, errOut := r.run(t, "exec", "--env", "A=b=c", "-u", "1000", "-w", "/srv", "ct", "api", "--", "out", "-x")
	if code != 7 || out != "to stdout\n" || errOut != "to stderr\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	start, _ := r.server.got()
	want := &computev1alpha1.ContainerExecStart{
		Command: []string{"out", "-x"}, Env: []*computev1alpha1.EnvVar{{Name: "A", Value: "b=c"}}, User: "1000", WorkingDir: "/srv",
	}
	if !proto.Equal(start, want) {
		t.Errorf("started %v, want %v", start, want)
	}
	r.server.mu.Lock()
	defer r.server.mu.Unlock()
	if r.server.user != "alice@example.com" || r.server.path != "/v1/projects/web/containers/api:exec" {
		t.Errorf("as %q at %q", r.server.user, r.server.path)
	}
}

func TestExecPassesStdinOnWithI(t *testing.T) {
	r := newExecRun(t)
	r.stdin = strings.NewReader("hello\n")
	code, out, _ := r.run(t, "exec", "-i", "ct", "api", "--", "cat")
	if code != 0 || out != "hello\n" {
		t.Errorf("exit %d, stdout %q", code, out)
	}
	if start, _ := r.server.got(); !start.GetStdin() || start.GetTty() {
		t.Errorf("started %v", start)
	}
}

// TestCtrlCGoesToTheCommandWithoutT: SIGINT is passed on, not taken as
// hangar's own, and hangar exits as the signalled command did.
func TestCtrlCGoesToTheCommandWithoutT(t *testing.T) {
	r := newExecRun(t)
	done := make(chan int, 1)
	go func() {
		code, _, _ := r.run(t, "exec", "ct", "api", "--", "wait")
		done <- code
	}()
	r.sigs.raise(t, os.Interrupt)
	select {
	case code := <-done:
		if code != 130 {
			t.Errorf("exit %d, want 130", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the exec did not end")
	}
	_, inputs := r.server.got()
	if len(inputs) != 1 || inputs[0].GetSignal() != int32(syscall.SIGINT) {
		t.Errorf("sent %v", inputs)
	}
}

func TestATerminalIsRawResizedAndRestored(t *testing.T) {
	r := newExecRun(t)
	// A terminal nobody types on.
	stdin, typing := io.Pipe()
	t.Cleanup(func() { _ = typing.Close() })
	r.stdin = stdin
	done := make(chan int, 1)
	var out, errOut string
	go func() {
		var code int
		code, out, errOut = r.run(t, "exec", "-it", "ct", "api", "--", "size")
		done <- code
	}()
	deadline := time.Now().Add(10 * time.Second)
	for raw, _ := r.term.state(); !raw; raw, _ = r.term.state() {
		select {
		case code := <-done:
			t.Fatalf("the exec ended with %d before its terminal was made raw: %s", code, errOut)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the terminal was never made raw")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.term.resize(30, 100)
	r.sigs.raise(t, syscall.SIGWINCH)
	// Ctrl-C ends hangar with -t: the remote process goes with the exec.
	for {
		_, inputs := r.server.got()
		if len(inputs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the resize never came")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.sigs.raise(t, syscall.SIGTERM)
	select {
	case code := <-done:
		if code != cli.ExitExecFailed {
			t.Errorf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the exec did not end")
	}
	if !strings.Contains(out, "24 80\n") || !strings.Contains(out, "30 100\n") {
		t.Errorf("sizes %q", out)
	}
	if raw, raws := r.term.state(); raw || raws != 1 {
		t.Errorf("the terminal is raw %t, made raw %d times", raw, raws)
	}
	if start, _ := r.server.got(); !start.GetTty() || !start.GetStdin() || start.GetSize().GetRows() != 24 {
		t.Errorf("started %v", start)
	}
}

// TestATerminalIsRestoredWhenTheExecFails: however the exec ends, the
// terminal is put back.
func TestATerminalIsRestoredWhenTheExecFails(t *testing.T) {
	r := newExecRun(t)
	code, _, errOut := r.run(t, "exec", "-t", "ct", "api", "--", "fail")
	if code != cli.ExitExecFailed || !strings.Contains(errOut, "not running") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if raw, raws := r.term.state(); raw || raws != 1 {
		t.Errorf("the terminal is raw %t, made raw %d times", raw, raws)
	}
	code, _, _ = r.run(t, "exec", "-t", "ct", "api", "--", "out")
	if raw, _ := r.term.state(); code != 7 || raw {
		t.Errorf("exit %d, the terminal raw %t", code, raw)
	}
}

func TestExecFailures(t *testing.T) {
	r := newExecRun(t)
	for _, tc := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{"exec", "ct", "absent", "--", "out"}, cli.ExitExecFailed, "not found"},
		{[]string{"exec", "ct", "api", "--", "fail"}, cli.ExitExecFailed, "not running"},
		{[]string{"exec", "ct", "api", "out"}, cli.ExitExecFailed, "usage"},
		{[]string{"exec", "ct", "api", "--"}, cli.ExitExecFailed, "usage"},
		{[]string{"exec", "machine", "api", "--", "out"}, cli.ExitExecFailed, "only containers"},
		{[]string{"exec", "--env", "X", "ct", "api", "--", "out"}, cli.ExitExecFailed, "KEY=VALUE"},
	} {
		code, _, errOut := r.run(t, tc.args...)
		if code != tc.code || !strings.Contains(errOut, tc.says) {
			t.Errorf("%v: exit %d, stderr %q", tc.args, code, errOut)
		}
	}
	r.term = nil
	if code, _, errOut := r.run(t, "exec", "-t", "ct", "api", "--", "out"); code != cli.ExitExecFailed || !strings.Contains(errOut, "terminal") {
		t.Errorf("-t without a terminal: exit %d, stderr %q", code, errOut)
	}
}

// TestExecGivesUpOnASilentServer: a server that says nothing, and answers
// no ping, with nothing pending, ends hangar with 125.
func TestExecGivesUpOnASilentServer(t *testing.T) {
	r := newExecRun(t)
	r.ping, r.pong = 100*time.Millisecond, 300*time.Millisecond
	began := time.Now()
	code, _, errOut := r.run(t, "exec", "ct", "api", "--", "hang")
	if code != cli.ExitExecFailed || !strings.Contains(errOut, "stopped answering") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("gave up after %s", took)
	}
}

// TestAServerThatPingsIsThere: a server whose reader is held up still
// pings, so hangar stays; a second Ctrl-C within 2 s then ends it with 125,
// the first having gone to the command.
func TestAServerThatPingsIsThere(t *testing.T) {
	r := newExecRun(t)
	r.ping, r.pong = 100*time.Millisecond, 300*time.Millisecond
	done := make(chan int, 1)
	var errOut string
	go func() {
		var code int
		code, _, errOut = r.run(t, "exec", "ct", "api", "--", "deaf")
		done <- code
	}()
	select {
	case code := <-done:
		t.Fatalf("hangar ended with %d while the server pinged: %s", code, errOut)
	case <-time.After(1500 * time.Millisecond):
	}
	r.sigs.raise(t, os.Interrupt)
	select {
	case code := <-done:
		t.Fatalf("one Ctrl-C ended hangar with %d", code)
	case <-time.After(200 * time.Millisecond):
	}
	r.sigs.raise(t, os.Interrupt)
	select {
	case code := <-done:
		if code != cli.ExitExecFailed || !strings.Contains(errOut, "interrupted twice") {
			t.Errorf("exit %d, stderr %q", code, errOut)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a second Ctrl-C did not end hangar")
	}
}
