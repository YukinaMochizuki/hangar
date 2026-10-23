package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/execws"
)

// ExitExecFailed is what hangar exec exits with when the exec itself
// fails, as docker exec does: any other code is the command's.
const ExitExecFailed = 125

// stdinChunk is the most stdin one message carries.
const stdinChunk = 32 << 10

// How hangar exec tells the server is gone, unless Options say otherwise:
// as the server tells the client (internal/execws).
const (
	defaultExecPing = 30 * time.Second
	defaultExecPong = 30 * time.Second
)

// interruptAgain is how soon a second Ctrl-C, without -t, ends hangar
// instead of going to the command, as in kubectl.
const interruptAgain = 2 * time.Second

var (
	errInterrupted = errors.New("interrupted twice; the command may still run until the exec's end reaches it")
	errServerGone  = errors.New("the server stopped answering")
)

func (a *app) execCmd() *cobra.Command {
	var interactive, tty bool
	var user, workdir string
	var env []string
	cmd := &cobra.Command{
		Use:   "exec [-i] [-t] [--user U] [--env K=V]... [--workdir D] KIND NAME -- COMMAND [ARG...]",
		Short: "Run a command in a running container or virtual machine",
		Long: "Runs a command in a running container, as docker exec does: with the\n" +
			"container's environment, user and working directory unless overridden.\n" +
			"In a virtual machine the platform logs in over SSH, as its image's default\n" +
			"user or --user, which needs compute.machines.ssh; --env and --workdir are\n" +
			"refused there, and hangar ssh is the way to log in from a terminal.\n" +
			"Its stdout goes to stdout and its stderr to stderr; with -t they are one\n" +
			"stream, on a terminal. Without -t, Ctrl-C and other signals are passed\n" +
			"on to the command, and a second Ctrl-C within 2 s ends hangar; with -t,\n" +
			"Ctrl-C is a key the command reads.\n\n" +
			"hangar exits with the command's exit code, 128 plus the signal's number\n" +
			"if a signal ended it. If the exec itself fails, from a usage error to a\n" +
			"server gone, hangar exits with 125, as docker exec does, and says why on\n" +
			"stderr, after \"error:\".",
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 2 || len(args) < 3 {
				return execFailed(usageErr("usage: %s", cmd.UseLine()))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return execFailed(a.runExec(cmd.Context(), args, interactive, tty, user, workdir, env))
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return execFailed(err) })
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "pass stdin on to the command, closing its stdin at end of file")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "run the command on a terminal, this one in raw mode while it runs")
	cmd.Flags().StringVarP(&user, "user", "u", "", "run as this user, such as 1000 or nobody:nogroup, instead of the container's; in a VM, a login name")
	cmd.Flags().StringArrayVarP(&env, "env", "e", nil, "set KEY=VALUE in the command's environment, over the container's")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "", "run in this directory instead of the container's")
	return cmd
}

// execFailed makes err, unless it is the command's own exit, end hangar
// with ExitExecFailed.
func execFailed(err error) error {
	var remote exitStatus
	if err == nil || errors.As(err, &remote) {
		return err
	}
	return &exitError{code: ExitExecFailed, err: err}
}

func (a *app) runExec(ctx context.Context, args []string, interactive, tty bool, user, workdir string, env []string) error {
	r, err := findKind(args[0])
	if err != nil {
		return err
	}
	start := &computev1alpha1.MachineExecStart{Command: args[2:], Stdin: interactive, Tty: tty, User: user, WorkingDir: workdir}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return usageErr("--env %q: want KEY=VALUE", kv)
		}
		start.Env = append(start.Env, &computev1alpha1.EnvVar{Name: k, Value: v})
	}
	var term Terminal
	if tty {
		var ok bool
		if term, ok = a.opts.Terminal(a.io.In); !ok {
			return usageErr("-t needs stdin to be a terminal")
		}
		rows, cols, err := term.Size()
		if err != nil {
			return fmt.Errorf("reading the terminal's size: %w", err)
		}
		start.Size = &computev1alpha1.MachineExecWindowSize{Rows: uint32(rows), Cols: uint32(cols)}
	}
	c, err := a.current()
	if err != nil {
		return err
	}
	project, err := a.projectFor(r, c)
	if err != nil {
		return err
	}
	creds, err := a.credentials(c)
	if err != nil {
		return err
	}
	hc, base := a.execHTTPClient(c)
	return a.exec(ctx, base+execws.Path(project, r.Plural, args[1]), hc, creds, start, term)
}

// execHTTPClient is the client an exec dials with, and the base URL: one
// dialing the socket of a unix:// endpoint, the Options' in tests, else one
// whose connections TCP drops once the server stops answering.
func (a *app) execHTTPClient(c *Context) (*http.Client, string) {
	if path, ok := c.socketPath(); ok {
		return client.UnixSocket(path), client.UnixSocketURL
	}
	if hc, ok := a.opts.HTTPClient.(*http.Client); ok && hc != http.DefaultClient {
		return hc, c.Endpoint
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{KeepAliveConfig: execws.KeepAlive, Control: execws.Control}).DialContext
	return &http.Client{Transport: tr}, c.Endpoint
}

// exec runs start at url. With term, the terminal is raw while it runs and
// put back however it ends.
func (a *app) exec(ctx context.Context, url string, hc *http.Client, creds credentials, start *computev1alpha1.MachineExecStart, term Terminal) error {
	header := http.Header{}
	if err := creds.set(ctx, header.Set); err != nil {
		return err
	}
	every, timeout := a.opts.ExecPing, a.opts.ExecPong
	if every <= 0 {
		every = defaultExecPing
	}
	if timeout <= 0 {
		timeout = defaultExecPong
	}
	live := execws.NewLiveness(every, timeout)
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: hc, HTTPHeader: header, Subprotocols: []string{execws.Subprotocol},
		OnPingReceived: live.OnPing, OnPongReceived: live.OnPong,
	})
	if err != nil {
		if resp == nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusSwitchingProtocols {
			return execws.ReadError(resp)
		}
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(execws.MaxMessage)

	// The connection's reads and writes are not given ctx: coder/websocket
	// closes a connection whose read or write is cancelled. Closing it ends
	// them instead, with why in ctx's cause.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ws := context.WithoutCancel(ctx)
	stopOnCancel := context.AfterFunc(ctx, func() { _ = conn.CloseNow() })
	defer stopOnCancel()
	s := &execClient{conn: conn, ws: ws, live: live}
	if err := s.send(&computev1alpha1.MachineExecRequest{Message: &computev1alpha1.MachineExecRequest_Start{Start: start}}); err != nil {
		return err
	}
	go func() {
		if live.Run(ctx, conn) != nil {
			cancel(errServerGone)
		}
	}()

	if term != nil {
		restore, err := term.MakeRaw()
		if err != nil {
			return fmt.Errorf("putting the terminal in raw mode: %w", err)
		}
		defer restore()
	}
	stopSignals := a.relaySignals(s, term, cancel)
	defer stopSignals()
	if start.GetStdin() {
		in := a.io.In
		// A terminal may read its keys its own way (terminal_windows.go).
		if t, ok := term.(interface{ Input() io.Reader }); ok {
			in = t.Input()
		}
		go s.relayStdin(in)
	}

	for {
		_, b, err := conn.Read(ws)
		if err != nil {
			if ctx.Err() != nil {
				if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
					return cause
				}
				return errors.New("interrupted; the command's exec ends with hangar")
			}
			if err := execws.ErrorOf(err); err != nil {
				return err
			}
			if s.exit == nil {
				return errors.New("the exec ended without saying how its command did")
			}
			return exitStatus(exitCodeOf(s.exit))
		}
		live.Heard()
		res := &computev1alpha1.MachineExecResponse{}
		if err := proto.Unmarshal(b, res); err != nil {
			return fmt.Errorf("a message from the server that is not one: %w", err)
		}
		switch m := res.GetMessage().(type) {
		case *computev1alpha1.MachineExecResponse_Stdout:
			err = s.output(a.io.Out, m.Stdout)
		case *computev1alpha1.MachineExecResponse_Stderr:
			err = s.output(a.io.Err, m.Stderr)
		case *computev1alpha1.MachineExecResponse_Exit:
			s.exit = m.Exit
		}
		if err != nil {
			return err
		}
	}
}

// exitCodeOf is the exit code hangar exits with for the command's exit,
// which only 0 to 255 can be.
func exitCodeOf(e *computev1alpha1.MachineExecExit) int {
	if c := e.GetCode(); c >= 0 && c <= 255 {
		return int(c)
	}
	return 255
}

// relaySignals passes signals on to the exec until stop. Without a
// terminal, SIGINT, SIGTERM, SIGHUP and SIGQUIT go to its process, as
// docker exec passes them on, and a second SIGINT ends the exec by cancel;
// with one, a change of its size (SIGWINCH, or on Windows what watchSize
// sees) resizes the remote terminal.
func (a *app) relaySignals(s *execClient, term Terminal, cancel context.CancelCauseFunc) (stop func()) {
	c := make(chan os.Signal, 4)
	var stopNotify func()
	if term == nil {
		var mu sync.Mutex
		var lastInterrupt time.Time
		take := func(sig os.Signal) bool {
			if sig == os.Interrupt {
				mu.Lock()
				again := !lastInterrupt.IsZero() && time.Since(lastInterrupt) < interruptAgain
				lastInterrupt = time.Now()
				mu.Unlock()
				if again {
					cancel(errInterrupted)
					return true
				}
			}
			select {
			case c <- sig:
			default: // the relay is behind; signals coalesce as Unix's do
			}
			return true
		}
		a.takeInterrupts.Store(&take)
		stopNotify = a.opts.Signals(c, syscall.SIGHUP, syscall.SIGQUIT)
	} else {
		stopResized := a.opts.Signals(c, resized)
		stopWatch := watchSize(term, c)
		stopNotify = func() { stopWatch(); stopResized() }
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			case sig := <-c:
				if term != nil {
					if rows, cols, err := term.Size(); err == nil {
						_ = s.send(&computev1alpha1.MachineExecRequest{Message: &computev1alpha1.MachineExecRequest_Resize{
							Resize: &computev1alpha1.MachineExecWindowSize{Rows: uint32(rows), Cols: uint32(cols)},
						}})
					}
					continue
				}
				if n, ok := sig.(syscall.Signal); ok {
					_ = s.send(&computev1alpha1.MachineExecRequest{Message: &computev1alpha1.MachineExecRequest_Signal{Signal: int32(n)}})
				}
			}
		}
	})
	return func() {
		a.takeInterrupts.Store(nil)
		stopNotify()
		close(done)
		wg.Wait()
	}
}

// execClient is the client's end of an exec. What a slow server or a slow
// local output holds up is pending to live.
type execClient struct {
	conn *websocket.Conn
	ws   context.Context
	live *execws.Liveness
	exit *computev1alpha1.MachineExecExit
}

func (s *execClient) send(req *computev1alpha1.MachineExecRequest) error {
	b, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	done := s.live.Pending()
	defer done()
	return s.conn.Write(s.ws, websocket.MessageBinary, b)
}

func (s *execClient) output(w io.Writer, p []byte) error {
	done := s.live.Pending()
	defer done()
	_, err := w.Write(p)
	return err
}

// relayStdin passes in on to the exec, closing its stdin at end of file. It
// may outlive the exec, blocked reading in, as reading a terminal cannot be
// cut short.
func (s *execClient) relayStdin(in io.Reader) {
	buf := make([]byte, stdinChunk)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if s.send(&computev1alpha1.MachineExecRequest{Message: &computev1alpha1.MachineExecRequest_Stdin{Stdin: buf[:n]}}) != nil {
				return
			}
		}
		if err != nil {
			_ = s.send(&computev1alpha1.MachineExecRequest{Message: &computev1alpha1.MachineExecRequest_StdinClose{StdinClose: &computev1alpha1.MachineExecStdinClose{}}})
			return
		}
	}
}
