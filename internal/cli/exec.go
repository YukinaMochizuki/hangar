package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/execws"
)

// stdinChunk is the most stdin one message carries.
const stdinChunk = 32 << 10

func (a *app) execCmd() *cobra.Command {
	var interactive, tty bool
	var user, workdir string
	var env []string
	cmd := &cobra.Command{
		Use:   "exec [-i] [-t] [--user U] [--env K=V]... [--workdir D] KIND NAME -- COMMAND [ARG...]",
		Short: "Run a command in a running container",
		Long: "Runs a command in a running container, as docker exec does: with the\n" +
			"container's environment, user and working directory unless overridden.\n" +
			"Its stdout goes to stdout and its stderr to stderr; with -t they are one\n" +
			"stream, on a terminal. Without -t, Ctrl-C and other signals are passed\n" +
			"on to the command; with -t, Ctrl-C is a key the command reads.\n\n" +
			"hangar exits with the command's exit code, 128 plus the signal's number\n" +
			"if a signal ended it. If the exec itself fails, hangar exits with its\n" +
			"own codes: 1 for an error, 2 for usage, 3 for a container not found, 5\n" +
			"for a timeout, which a command can exit with as well; the error is then\n" +
			"on stderr, after \"error:\".",
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 2 || len(args) < 3 {
				return usageErr("usage: %s", cmd.UseLine())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := findKind(args[0])
			if err != nil {
				return err
			}
			if r.Kind != registry.Container.Kind {
				return usageErr("only containers can exec, not %s", r.Plural)
			}
			start := &computev1alpha1.ContainerExecStart{Command: args[2:], Stdin: interactive, Tty: tty, User: user, WorkingDir: workdir}
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
				start.Size = &computev1alpha1.ContainerExecWindowSize{Rows: uint32(rows), Cols: uint32(cols)}
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
			return a.exec(cmd.Context(), c.Endpoint+execws.Path(project, args[1]), creds, start, term)
		},
	}
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "pass stdin on to the command, closing its stdin at end of file")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "run the command on a terminal, this one in raw mode while it runs")
	cmd.Flags().StringVarP(&user, "user", "u", "", "run as this user, such as 1000 or nobody:nogroup, instead of the container's")
	cmd.Flags().StringArrayVarP(&env, "env", "e", nil, "set KEY=VALUE in the command's environment, over the container's")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "", "run in this directory instead of the container's")
	return cmd
}

// exec runs start at url. With term, the terminal is raw while it runs and
// put back however it ends.
func (a *app) exec(ctx context.Context, url string, creds credentials, start *computev1alpha1.ContainerExecStart, term Terminal) error {
	header := http.Header{}
	if err := creds.set(ctx, header.Set); err != nil {
		return err
	}
	hc, _ := a.opts.HTTPClient.(*http.Client)
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: hc, HTTPHeader: header, Subprotocols: []string{execws.Subprotocol}})
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
	// them instead.
	ws := context.WithoutCancel(ctx)
	stopOnCancel := context.AfterFunc(ctx, func() { _ = conn.CloseNow() })
	defer stopOnCancel()
	s := &execClient{conn: conn, ws: ws}
	if err := s.send(&computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Start{Start: start}}); err != nil {
		return err
	}

	if term != nil {
		restore, err := term.MakeRaw()
		if err != nil {
			return fmt.Errorf("putting the terminal in raw mode: %w", err)
		}
		defer restore()
	}
	stopSignals := a.relaySignals(s, term)
	defer stopSignals()
	if start.GetStdin() {
		go s.relayStdin(a.io.In)
	}

	out, errOut := a.io.Out, a.io.Err
	for {
		_, b, err := conn.Read(ws)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := execws.ErrorOf(err); err != nil {
				return err
			}
			if s.exit == nil {
				return errors.New("the exec ended without saying how its command did")
			}
			return exitStatus(s.exit.GetCode())
		}
		res := &computev1alpha1.ContainerExecResponse{}
		if err := proto.Unmarshal(b, res); err != nil {
			return fmt.Errorf("a message from the server that is not one: %w", err)
		}
		switch m := res.GetMessage().(type) {
		case *computev1alpha1.ContainerExecResponse_Stdout:
			if _, err := out.Write(m.Stdout); err != nil {
				return err
			}
		case *computev1alpha1.ContainerExecResponse_Stderr:
			if _, err := errOut.Write(m.Stderr); err != nil {
				return err
			}
		case *computev1alpha1.ContainerExecResponse_Exit:
			s.exit = m.Exit
		}
	}
}

// relaySignals passes signals on to the exec until stop. Without a
// terminal, SIGINT, SIGTERM, SIGHUP and SIGQUIT go to its process, as
// docker exec passes them on. With one, Ctrl-C is a key like any other,
// SIGWINCH resizes the remote terminal, and SIGINT and SIGTERM end hangar
// as they end any command.
func (a *app) relaySignals(s *execClient, term Terminal) (stop func()) {
	c := make(chan os.Signal, 4)
	var stopNotify func()
	if term == nil {
		take := func(sig os.Signal) bool {
			select {
			case c <- sig:
			default: // the relay is behind; signals coalesce as Unix's do
			}
			return true
		}
		a.takeInterrupts.Store(&take)
		stopNotify = a.opts.Signals(c, syscall.SIGHUP, syscall.SIGQUIT)
	} else {
		stopNotify = a.opts.Signals(c, syscall.SIGWINCH)
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
						_ = s.send(&computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Resize{
							Resize: &computev1alpha1.ContainerExecWindowSize{Rows: uint32(rows), Cols: uint32(cols)},
						}})
					}
					continue
				}
				if n, ok := sig.(syscall.Signal); ok {
					_ = s.send(&computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Signal{Signal: int32(n)}})
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

// execClient is the client's end of an exec.
type execClient struct {
	conn *websocket.Conn
	ws   context.Context
	exit *computev1alpha1.ContainerExecExit
}

func (s *execClient) send(req *computev1alpha1.ContainerExecRequest) error {
	b, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	return s.conn.Write(s.ws, websocket.MessageBinary, b)
}

// relayStdin passes in on to the exec, closing its stdin at end of file. It
// may outlive the exec, blocked reading in, as reading a terminal cannot be
// cut short.
func (s *execClient) relayStdin(in io.Reader) {
	buf := make([]byte, stdinChunk)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if s.send(&computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Stdin{Stdin: buf[:n]}}) != nil {
				return
			}
		}
		if err != nil {
			_ = s.send(&computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_StdinClose{StdinClose: &computev1alpha1.ContainerExecStdinClose{}}})
			return
		}
	}
}
