package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// execNode serves a node's NodeService over HTTP/2 from a fake host.
func execNode(t *testing.T) (*fake.Host, nodev1connect.NodeServiceClient) {
	t.Helper()
	host := fake.New()
	a := &Agent{cfg: Config{Drivers: &Drivers{
		Images: host.Images(), Storage: host.Storage(), Network: host.Network(), Hypervisor: host.Hypervisor(), Guest: host.Guest(),
	}}}
	mux := http.NewServeMux()
	mux.Handle(nodev1connect.NewNodeServiceHandler(nodeService{a: a, agent: t.Context()}))
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return host, nodev1connect.NewNodeServiceClient(srv.Client(), srv.URL)
}

// runVM starts a VM for machine uid on the fake host, running run.
func runVM(t *testing.T, host *fake.Host, uid string, run driver.RunSpec) {
	t.Helper()
	ctx := t.Context()
	if err := host.Storage().EnsureBase(ctx, "sha256:1", 0, func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "image"), nil, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	root, err := host.Storage().EnsureRoot(ctx, uid, "sha256:1", fake.DefaultBaseSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Hypervisor().Start(ctx, driver.VMSpec{MachineUID: uid, VCPUs: 1, MemoryMiB: 64, Root: root, Run: run}); err != nil {
		t.Fatal(err)
	}
}

type execResult struct {
	stdout, stderr string
	exit           *nodev1.ExitStatus
	err            error
}

// nodeExec runs an exec through the node: start, then what send sends,
// closing its side once the exit has come, as the apiserver does.
func nodeExec(t *testing.T, ctx context.Context, c nodev1connect.NodeServiceClient, start *nodev1.ExecStart, send ...*nodev1.ExecRequest) (r execResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second) // a hang fails here, by name
	defer cancel()
	s := c.Exec(ctx)
	defer func() { _ = s.CloseResponse() }()
	for _, req := range append([]*nodev1.ExecRequest{{Message: &nodev1.ExecRequest_Start{Start: start}}}, send...) {
		if err := s.Send(req); err != nil {
			break // Receive says why
		}
	}
	var out, errb strings.Builder
	defer func() { r.stdout, r.stderr = out.String(), errb.String() }()
	for {
		res, err := s.Receive()
		if err != nil {
			r.err = err
			return r
		}
		switch m := res.GetMessage().(type) {
		case *nodev1.ExecResponse_Stdout:
			out.Write(m.Stdout)
		case *nodev1.ExecResponse_Stderr:
			errb.Write(m.Stderr)
		case *nodev1.ExecResponse_Exit:
			r.exit = m.Exit
			_ = s.CloseRequest()
			if _, err := s.Receive(); !errors.Is(err, io.EOF) {
				r.err = err
			}
			return r
		}
	}
}

func stdin(p string) *nodev1.ExecRequest {
	return &nodev1.ExecRequest{Message: &nodev1.ExecRequest_Stdin{Stdin: []byte(p)}}
}

var closeStdin = &nodev1.ExecRequest{Message: &nodev1.ExecRequest_StdinClose{StdinClose: &nodev1.ExecStdinClose{}}}

func signal(sig int32) *nodev1.ExecRequest {
	return &nodev1.ExecRequest{Message: &nodev1.ExecRequest_Signal{Signal: sig}}
}

// TestAnExecInheritsTheContainers: the process gets the container's
// environment, working directory and user, as its VM was started, with
// what the start sets instead.
func TestAnExecInheritsTheContainers(t *testing.T) {
	host, c := execNode(t)
	runVM(t, host, "m1", driver.RunSpec{
		Command: []string{"wait", "main"}, Env: []string{"A=container", "B=container"}, WorkingDir: "/app", User: "app",
	})
	r := nodeExec(t, t.Context(), c, &nodev1.ExecStart{MachineUid: "m1", Args: []string{"show", "A", "B"}})
	if r.err != nil || r.exit.GetCode() != 0 || r.stdout != "container\ncontainer\n/app\napp\n" {
		t.Errorf("inherited: %+v", r)
	}
	r = nodeExec(t, t.Context(), c, &nodev1.ExecStart{
		MachineUid: "m1", Args: []string{"show", "A", "B"}, Env: []string{"B=exec", "C=x=y"}, WorkingDir: "/tmp", User: "root",
	})
	if r.err != nil || r.stdout != "container\nexec\n/tmp\nroot\n" {
		t.Errorf("overridden: %+v", r)
	}
}

func TestAnExecsOutputAndExitComeBack(t *testing.T) {
	host, c := execNode(t)
	runVM(t, host, "m1", driver.RunSpec{Command: []string{"wait", "main"}})
	r := nodeExec(t, t.Context(), c, &nodev1.ExecStart{MachineUid: "m1", Args: []string{"exit", "3", "out", "err"}})
	if r.err != nil || r.stdout != "out" || r.stderr != "err" || r.exit.GetCode() != 3 {
		t.Errorf("%+v", r)
	}
	r = nodeExec(t, t.Context(), c, &nodev1.ExecStart{MachineUid: "m1", Args: []string{"exit", "0", "out", "err"}, Tty: true})
	if r.err != nil || r.stdout != "outerr" || r.stderr != "" {
		t.Errorf("on a terminal: %+v", r)
	}
	r = nodeExec(t, t.Context(), c, &nodev1.ExecStart{MachineUid: "m1", Args: []string{"no-such"}})
	if r.err != nil || r.exit.GetCode() != 127 {
		t.Errorf("not found: %+v", r)
	}
}

func TestStdinOnlyWhenAskedFor(t *testing.T) {
	host, c := execNode(t)
	runVM(t, host, "m1", driver.RunSpec{Command: []string{"wait", "main"}})
	r := nodeExec(t, t.Context(), c, &nodev1.ExecStart{MachineUid: "m1", Args: []string{"cat"}, Stdin: true}, stdin("hello"), closeStdin)
	if r.err != nil || r.stdout != "hello" || r.exit.GetCode() != 0 {
		t.Errorf("with stdin: %+v", r)
	}
	// Without it, the process reads end-of-file at once.
	r = nodeExec(t, t.Context(), c, &nodev1.ExecStart{MachineUid: "m1", Args: []string{"cat"}})
	if r.err != nil || r.stdout != "" || r.exit.GetCode() != 0 {
		t.Errorf("without stdin: %+v", r)
	}
	r = nodeExec(t, t.Context(), c, &nodev1.ExecStart{MachineUid: "m1", Args: []string{"wait", "x"}}, stdin("hello"))
	if connect.CodeOf(r.err) != connect.CodeInvalidArgument {
		t.Errorf("stdin to an exec without it: %+v", r)
	}
	// On a terminal without stdin, nothing is typed, not even ^D: the
	// process runs until it is signalled.
	r = nodeExec(t, t.Context(), c, &nodev1.ExecStart{MachineUid: "m1", Args: []string{"cat"}, Tty: true}, signal(2))
	if r.err != nil || r.exit.GetCode() != 130 || r.exit.GetSignal() != 2 {
		t.Errorf("a terminal without stdin, signalled: %+v", r)
	}
	if n := host.Execs("m1"); n != 0 {
		t.Errorf("%d execs left", n)
	}
}

func TestAResizeReachesTheTerminal(t *testing.T) {
	host, c := execNode(t)
	runVM(t, host, "m1", driver.RunSpec{Command: []string{"wait", "main"}})
	r := nodeExec(t, t.Context(), c,
		&nodev1.ExecStart{MachineUid: "m1", Args: []string{"size"}, Tty: true, Stdin: true, Size: &nodev1.WindowSize{Rows: 24, Cols: 80}},
		&nodev1.ExecRequest{Message: &nodev1.ExecRequest_Resize{Resize: &nodev1.WindowSize{Rows: 50, Cols: 70000}}},
		stdin("\n"))
	if r.err != nil || r.stdout != "24 80\n50 65535\n" {
		t.Errorf("%+v", r)
	}
}

func TestAnExecIsRefused(t *testing.T) {
	host, c := execNode(t)
	runVM(t, host, "m1", driver.RunSpec{Command: []string{"wait", "main"}})
	for _, tc := range []struct {
		what  string
		start *nodev1.ExecStart
		send  []*nodev1.ExecRequest
		code  connect.Code
	}{
		{"no machine", &nodev1.ExecStart{Args: []string{"cat"}}, nil, connect.CodeInvalidArgument},
		{"no program", &nodev1.ExecStart{MachineUid: "m1"}, nil, connect.CodeInvalidArgument},
		{"an env without =", &nodev1.ExecStart{MachineUid: "m1", Args: []string{"cat"}, Env: []string{"A"}}, nil, connect.CodeInvalidArgument},
		{"a machine with no VM", &nodev1.ExecStart{MachineUid: "m2", Args: []string{"cat"}}, nil, connect.CodeFailedPrecondition},
		{"a signal not Linux's", &nodev1.ExecStart{MachineUid: "m1", Args: []string{"wait", "x"}}, []*nodev1.ExecRequest{signal(65)}, connect.CodeInvalidArgument},
		{"a second start", &nodev1.ExecStart{MachineUid: "m1", Args: []string{"wait", "x"}},
			[]*nodev1.ExecRequest{{Message: &nodev1.ExecRequest_Start{Start: &nodev1.ExecStart{}}}}, connect.CodeInvalidArgument},
	} {
		if r := nodeExec(t, t.Context(), c, tc.start, tc.send...); connect.CodeOf(r.err) != tc.code {
			t.Errorf("%s: %+v, want %v", tc.what, r, tc.code)
		}
	}
	if err := host.Exit("m1", 0); err != nil { // restarted: always
		t.Fatal(err)
	}
	host.Silence("m1", true)
	if r := nodeExec(t, t.Context(), c, &nodev1.ExecStart{MachineUid: "m1", Args: []string{"cat"}}); connect.CodeOf(r.err) != connect.CodeUnavailable {
		t.Errorf("a guest that does not answer: %+v", r)
	}
	testutil.Eventually(t, "no exec running", noExecs(host, "m1"))
}

// TestACallerGoingAwayEndsTheProcess: the process runs only as long as the
// call.
func TestACallerGoingAwayEndsTheProcess(t *testing.T) {
	host, c := execNode(t)
	runVM(t, host, "m1", driver.RunSpec{Command: []string{"wait", "main"}})
	ctx, cancel := context.WithCancel(t.Context())
	s := c.Exec(ctx)
	if err := s.Send(&nodev1.ExecRequest{Message: &nodev1.ExecRequest_Start{Start: &nodev1.ExecStart{MachineUid: "m1", Args: []string{"wait", "x"}}}}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the exec running", func() bool { return host.Execs("m1") == 1 })
	cancel()
	_ = s.CloseResponse()
	testutil.Eventually(t, "no exec running", noExecs(host, "m1"))
}

// TestAVMGoingEndsItsExecs: an exec in a guest that restarts fails, as
// Unavailable.
func TestAVMGoingEndsItsExecs(t *testing.T) {
	host, c := execNode(t)
	runVM(t, host, "m1", driver.RunSpec{Command: []string{"wait", "main"}})
	done := make(chan execResult, 1)
	go func() {
		done <- nodeExec(t, t.Context(), c, &nodev1.ExecStart{MachineUid: "m1", Args: []string{"wait", "x"}})
	}()
	testutil.Eventually(t, "the exec running", func() bool { return host.Execs("m1") == 1 })
	if err := host.Exit("m1", 1); err != nil {
		t.Fatal(err)
	}
	if r := <-done; connect.CodeOf(r.err) != connect.CodeUnavailable {
		t.Errorf("an exec whose guest restarted: %+v", r)
	}
}

// TestAnExitCodeOutOfRangeIs255: what a guest says no process can exit
// with is passed on as 255.
func TestAnExitCodeOutOfRangeIs255(t *testing.T) {
	host, c := execNode(t)
	runVM(t, host, "m1", driver.RunSpec{Command: []string{"wait", "main"}})
	for _, code := range []string{"300", "-1", "255"} {
		r := nodeExec(t, t.Context(), c, &nodev1.ExecStart{MachineUid: "m1", Args: []string{"exit", code}})
		if r.err != nil || r.exit.GetCode() != 255 {
			t.Errorf("exit %s: %+v", code, r)
		}
	}
}

// noExecs says whether no exec runs in the guest of machine uid.
func noExecs(host *fake.Host, uid string) func() (bool, string) {
	return func() (bool, string) {
		n := host.Execs(uid)
		return n == 0, fmt.Sprintf("%d execs", n)
	}
}
