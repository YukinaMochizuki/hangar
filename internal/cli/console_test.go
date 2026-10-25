package cli_test

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/consolews"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// consoleServer plays the apiserver's side of a serial console: it tells
// the client its state, at 80x24, holding the input unless read-only, draws
// "the screen", echoes what is typed, tells it the new size on a resize, and
// closes cleanly once "bye" is typed, or at once if the VM is named gone.
type consoleServer struct {
	mu     sync.Mutex
	path   string
	start  *computev1alpha1.MachineConsoleStart
	inputs []*computev1alpha1.MachineConsoleRequest
	// end, closed, has the guest end the console, as typing bye does.
	end     chan struct{}
	endOnce sync.Once
}

func (f *consoleServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/absent:") {
		execws.WriteError(w, r, connect.NewError(connect.CodeNotFound, errors.New(`virtual machine "absent" not found`)))
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{consolews.Subprotocol}})
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	ctx := r.Context()
	read := func() (*computev1alpha1.MachineConsoleRequest, error) {
		_, b, err := c.Read(ctx)
		if err != nil {
			return nil, err
		}
		req := &computev1alpha1.MachineConsoleRequest{}
		return req, proto.Unmarshal(b, req)
	}
	write := func(res *computev1alpha1.MachineConsoleResponse) {
		b, _ := proto.Marshal(res)
		_ = c.Write(ctx, websocket.MessageBinary, b)
	}
	state := func(input bool, cols, rows uint32) {
		write(&computev1alpha1.MachineConsoleResponse{Message: &computev1alpha1.MachineConsoleResponse_State{State: &computev1alpha1.MachineConsoleState{
			Input: input, Size: &computev1alpha1.MachineConsoleSize{Columns: cols, Rows: rows},
		}}})
	}
	output := func(s string) {
		write(&computev1alpha1.MachineConsoleResponse{Message: &computev1alpha1.MachineConsoleResponse_Output{Output: []byte(s)}})
	}
	first, err := read()
	if err != nil {
		return
	}
	f.mu.Lock()
	f.start, f.path = first.GetStart(), r.URL.Path
	f.mu.Unlock()
	if strings.Contains(r.URL.Path, "/gone:") {
		_ = c.Close(websocket.StatusNormalClosure, "")
		return
	}
	input := !first.GetStart().GetReadOnly()
	state(input, 80, 24)
	output("the screen")
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			req, err := read()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.inputs = append(f.inputs, req)
			f.mu.Unlock()
			switch m := req.GetMessage().(type) {
			case *computev1alpha1.MachineConsoleRequest_Input:
				output(string(m.Input))
				if strings.Contains(string(m.Input), "bye") {
					f.bye()
				}
			case *computev1alpha1.MachineConsoleRequest_Resize:
				state(input, m.Resize.GetColumns(), m.Resize.GetRows())
			}
		}
	}()
	select {
	case <-f.ended():
		_ = c.Close(websocket.StatusNormalClosure, "")
	case <-gone:
	}
}

func (f *consoleServer) ended() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.end == nil {
		f.end = make(chan struct{})
	}
	return f.end
}

// bye has the console's guest end it, cleanly.
func (f *consoleServer) bye() {
	end := f.ended()
	f.endOnce.Do(func() { close(end) })
}

func (f *consoleServer) got() (*computev1alpha1.MachineConsoleStart, string, []*computev1alpha1.MachineConsoleRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.start, f.path, slices.Clone(f.inputs)
}

// consoleRun is an execRun whose server is a console's.
func consoleRun(t *testing.T, stdin string) (*execRun, *consoleServer) {
	r := newExecRun(t)
	srv := &consoleServer{}
	r.handler = srv
	r.stdin = strings.NewReader(stdin)
	return r, srv
}

// typed is what the client typed at the console, in order.
func typed(inputs []*computev1alpha1.MachineConsoleRequest) string {
	var b strings.Builder
	for _, in := range inputs {
		b.Write(in.GetInput())
	}
	return b.String()
}

// TestConsoleTypesUntilCtrlBracket: hangar console attaches to the VM's
// console, says how on stderr, draws the screen on stdout, and passes
// what is typed on, Ctrl-C too, on a raw terminal it puts back; Ctrl-]
// leaves, and nothing after it is sent.
func TestConsoleTypesUntilCtrlBracket(t *testing.T) {
	r, srv := consoleRun(t, "")
	keys, typing := io.Pipe()
	r.stdin = keys
	done := make(chan struct{})
	var code int
	var out, errOut string
	go func() {
		defer close(done)
		code, out, errOut = r.run(t, "console", "vm", "web")
	}()
	// Typed once the screen is drawn, as a person would.
	testutil.Eventually(t, "the screen drawn", func() (bool, string) {
		return strings.Contains(r.out.String(), "the screen"), r.out.String()
	})
	go func() { _, _ = typing.Write([]byte("ls\r\x03\x1dnever sent")) }()
	<-done
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	start, path, inputs := srv.got()
	if path != "/v1/projects/web/virtualmachines/web:console" || start.GetReadOnly() || start.GetForce() {
		t.Errorf("attached at %s with %v", path, start)
	}
	testutil.Eventually(t, "the typing at the server", func() (bool, string) {
		_, _, inputs = srv.got()
		return typed(inputs) == "ls\r\x03", typed(inputs)
	})
	if !strings.Contains(out, "the screen") {
		t.Errorf("stdout %q lacks the screen", out)
	}
	for _, want := range []string{"attached to vm/web's serial console, 80x24, holding the input; Ctrl-] leaves", "stty rows 24 cols 80", "left the console"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q lacks %q", errOut, want)
		}
	}
	if raw, raws := r.term.state(); raw || raws != 1 {
		t.Errorf("the terminal: raw %t after, made raw %d times", raw, raws)
	}
}

// TestConsoleSetsTheSizeItIsGiven: with --size, once told it holds the
// input, hangar console resizes the screen, and says how to tell the guest.
func TestConsoleSetsTheSizeItIsGiven(t *testing.T) {
	r, srv := consoleRun(t, "")
	r.stdin = blockingReader{}
	done := make(chan struct{})
	var code int
	var errOut string
	go func() {
		defer close(done)
		code, _, errOut = r.run(t, "console", "--size", "160x48", "vm", "web")
	}()
	testutil.Eventually(t, "the resize at the server", func() (bool, string) {
		_, _, inputs := srv.got()
		return len(inputs) == 1 && inputs[0].GetResize().GetColumns() == 160 && inputs[0].GetResize().GetRows() == 48, ""
	})
	srv.bye()
	<-done
	if code != 0 || !strings.Contains(errOut, "the screen is 160x48: tell the guest with stty rows 48 cols 160") || !strings.Contains(errOut, "the console ended") {
		t.Errorf("exited %d, stderr %q", code, errOut)
	}
}

// TestConsoleWatchesReadOnly: --read-only needs no terminal, asks for no
// input, sends nothing typed, and says it is watching, and that an
// interrupt leaves, Ctrl-] being no key without one.
func TestConsoleWatchesReadOnly(t *testing.T) {
	r, srv := consoleRun(t, "typed\r")
	r.term = nil
	done := make(chan struct{})
	var code int
	var out, errOut string
	go func() {
		defer close(done)
		code, out, errOut = r.run(t, "console", "--read-only", "vm", "web")
	}()
	testutil.Eventually(t, "the start at the server", func() (bool, string) {
		start, _, _ := srv.got()
		return start != nil, ""
	})
	srv.bye()
	<-done
	start, _, inputs := srv.got()
	if !start.GetReadOnly() || typed(inputs) != "" {
		t.Errorf("a read-only console started with %v and typed %q", start, typed(inputs))
	}
	if code != 0 || !strings.Contains(out, "the screen") || !strings.Contains(errOut, "80x24, watching; an interrupt leaves") {
		t.Errorf("exited %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// TestConsoleWatchesOnATerminalUntilCtrlBracket: --read-only on a terminal
// makes it raw too, sends nothing typed, and leaves on Ctrl-].
func TestConsoleWatchesOnATerminalUntilCtrlBracket(t *testing.T) {
	r, srv := consoleRun(t, "")
	keys, typing := io.Pipe()
	r.stdin = keys
	done := make(chan struct{})
	var code int
	var errOut string
	go func() {
		defer close(done)
		code, _, errOut = r.run(t, "console", "--read-only", "vm", "web")
	}()
	testutil.Eventually(t, "the screen drawn", func() (bool, string) {
		return strings.Contains(r.out.String(), "the screen"), r.out.String()
	})
	go func() { _, _ = typing.Write([]byte("typed\r\x1d")) }()
	<-done
	_, _, inputs := srv.got()
	if code != 0 || typed(inputs) != "" || !strings.Contains(errOut, "watching; Ctrl-] leaves") || !strings.Contains(errOut, "left the console") {
		t.Errorf("exited %d, typed %q, stderr %q", code, typed(inputs), errOut)
	}
	if raw, raws := r.term.state(); raw || raws != 1 {
		t.Errorf("the terminal: raw %t after, made raw %d times", raw, raws)
	}
}

// TestConsoleRefusals: only a VM has a console; --read-only takes neither
// --force nor --size; without a terminal it only watches; a size must be in
// bounds; what the server refuses, or a console that ends, is said.
func TestConsoleRefusals(t *testing.T) {
	for _, tt := range []struct {
		args   []string
		noTerm bool
		code   int
		want   string
	}{
		{[]string{"console", "container", "api"}, false, 2, "only virtual machines"},
		{[]string{"console", "--read-only", "--force", "vm", "web"}, false, 2, "--force takes the input"},
		{[]string{"console", "--read-only", "--size", "80x24", "vm", "web"}, false, 2, "--size takes the input"},
		{[]string{"console", "--size", "501x24", "vm", "web"}, false, 2, "within 500x200"},
		{[]string{"console", "--size", "80", "vm", "web"}, false, 2, "want COLSxROWS"},
		{[]string{"console", "vm", "web"}, true, 2, "needs stdin to be a terminal"},
		{[]string{"console", "vm", "absent"}, false, 3, `virtual machine "absent" not found`},
		{[]string{"console", "vm", "gone"}, false, 0, "the console ended, with its VM"},
	} {
		r, _ := consoleRun(t, "")
		if tt.noTerm {
			r.term = nil
		}
		code, _, errOut := r.run(t, tt.args...)
		if code != tt.code || !strings.Contains(errOut, tt.want) {
			t.Errorf("hangar %s: exited %d, stderr %q; want %d and %q", strings.Join(tt.args, " "), code, errOut, tt.code, tt.want)
		}
	}
}

// blockingReader is a terminal no one types at.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }
