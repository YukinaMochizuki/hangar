package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/consolews"
	"github.com/YukinaMochizuki/hangar/internal/execws"
)

// detachKey ends hangar console: Ctrl-], as telnet's and virsh console's.
// Ctrl-C and every other key go to the guest.
const detachKey = 0x1d

func (a *app) consoleCmd() *cobra.Command {
	var readOnly, force bool
	var size string
	cmd := &cobra.Command{
		Use:   "console [--read-only] [--force] [--size COLSxROWS] KIND NAME",
		Short: "Attach this terminal to a virtual machine's serial console",
		Long: "Attaches to the virtual machine's serial console, ttyS0, as a terminal on\n" +
			"its serial port would: it is drawn the screen as it is, then what the guest\n" +
			"writes. It needs compute.machines.exec. Ctrl-] leaves; every other key,\n" +
			"Ctrl-C included, goes to the guest.\n\n" +
			"Many may watch at once, one at a time types: the first to attach holds\n" +
			"the input, and one attaching while it is held watches, unless --force\n" +
			"takes it. The serial console bypasses the VM's security groups and\n" +
			"reaches its boot loader; it is slow, a few KB/s, a way to debug a VM, not\n" +
			"to work in one, which hangar ssh is.\n\n" +
			"The screen is 80x24 until whoever holds the input sets another size with\n" +
			"--size; a serial line carries no window size, so the guest learns it only\n" +
			"when its user runs stty rows ROWS cols COLS in it. A reboot brings 80x24\n" +
			"back.",
		Example: "  hangar console vm web\n  hangar console --read-only vm web\n  hangar console --force --size 160x48 vm web",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := findKind(args[0])
			if err != nil {
				return err
			}
			if r.Kind != registry.VirtualMachine.Kind {
				return usageErr("only virtual machines have an interactive serial console, not %s", r.Plural)
			}
			var want *computev1alpha1.MachineConsoleSize
			if size != "" {
				if want, err = parseConsoleSize(size); err != nil {
					return err
				}
				if readOnly {
					return usageErr("--size takes the input, which --read-only never holds")
				}
			}
			if readOnly && force {
				return usageErr("--force takes the input, which --read-only never holds")
			}
			return a.runConsole(cmd.Context(), args[1], &computev1alpha1.MachineConsoleStart{ReadOnly: readOnly, Force: force}, want)
		},
	}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "only watch: never hold the input")
	cmd.Flags().BoolVar(&force, "force", false, "take the input from whoever holds it, who goes on watching")
	cmd.Flags().StringVar(&size, "size", "", "set the screen's size, such as 160x48, once holding the input")
	return cmd
}

// parseConsoleSize reads COLSxROWS.
func parseConsoleSize(s string) (*computev1alpha1.MachineConsoleSize, error) {
	c, r, ok := strings.Cut(s, "x")
	cols, err1 := strconv.ParseUint(c, 10, 16)
	rows, err2 := strconv.ParseUint(r, 10, 16)
	if !ok || err1 != nil || err2 != nil || cols < 1 || cols > 500 || rows < 1 || rows > 200 {
		return nil, usageErr("--size %q: want COLSxROWS, such as 160x48, within 500x200", s)
	}
	return &computev1alpha1.MachineConsoleSize{Columns: uint32(cols), Rows: uint32(rows)}, nil
}

func (a *app) runConsole(ctx context.Context, name string, start *computev1alpha1.MachineConsoleStart, size *computev1alpha1.MachineConsoleSize) error {
	term, ok := a.opts.Terminal(a.io.In)
	if !ok {
		if !start.GetReadOnly() {
			return usageErr("hangar console needs stdin to be a terminal, unless --read-only")
		}
		term = nil
	}
	sc, err := a.sshContext()
	if err != nil {
		return err
	}
	header := http.Header{}
	if err := sc.creds.set(ctx, header.Set); err != nil {
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
	hc, base := a.execHTTPClient(sc.c)
	conn, resp, err := websocket.Dial(ctx, base+consolews.Path(sc.project, registry.VirtualMachine.Plural, name), &websocket.DialOptions{
		HTTPClient: hc, HTTPHeader: header, Subprotocols: []string{consolews.Subprotocol},
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
	conn.SetReadLimit(consolews.MaxMessage)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ws := context.WithoutCancel(ctx)
	stopOnCancel := context.AfterFunc(ctx, func() { _ = conn.CloseNow() })
	defer stopOnCancel()
	s := &consoleClient{conn: conn, ws: ws, live: live}
	if err := s.send(&computev1alpha1.MachineConsoleRequest{Message: &computev1alpha1.MachineConsoleRequest_Start{Start: start}}); err != nil {
		return err
	}
	go func() {
		if live.Run(ctx, conn) != nil {
			cancel(errServerGone)
		}
	}()
	// Keys are read once the console says how this client is attached: on
	// a terminal, raw, read-only too, which only watches for Ctrl-].
	keys := func() {}
	if term != nil {
		restore, err := term.MakeRaw()
		if err != nil {
			return fmt.Errorf("putting the terminal in raw mode: %w", err)
		}
		defer restore()
		in := a.io.In
		if t, ok := term.(interface{ Input() io.Reader }); ok {
			in = t.Input()
		}
		keys = func() { go s.relayKeys(in, !start.GetReadOnly(), cancel) }
	}

	var was *computev1alpha1.MachineConsoleState
	for {
		_, b, err := conn.Read(ws)
		if err != nil {
			if ctx.Err() != nil {
				cause := context.Cause(ctx)
				switch {
				case errors.Is(cause, errDetached), errors.Is(cause, context.Canceled) && term == nil:
					// Ctrl-], or, watching with no terminal, an interrupt.
					a.consoleNote("left the console")
					return nil
				case !errors.Is(cause, context.Canceled):
					return cause
				}
				return errors.New("interrupted")
			}
			if err := execws.ErrorOf(err); err != nil {
				return err
			}
			a.consoleNote("the console ended, with its VM")
			return nil
		}
		live.Heard()
		res := &computev1alpha1.MachineConsoleResponse{}
		if err := proto.Unmarshal(b, res); err != nil {
			return fmt.Errorf("a message from the server that is not one: %w", err)
		}
		switch m := res.GetMessage().(type) {
		case *computev1alpha1.MachineConsoleResponse_Output:
			done := live.Pending()
			_, err = a.io.Out.Write(m.Output)
			done()
		case *computev1alpha1.MachineConsoleResponse_State:
			a.consoleState(name, was, m.State, start.GetReadOnly(), term != nil)
			if was == nil {
				keys()
				if size != nil && m.State.GetInput() && !proto.Equal(size, m.State.GetSize()) {
					err = s.send(&computev1alpha1.MachineConsoleRequest{Message: &computev1alpha1.MachineConsoleRequest_Resize{Resize: size}})
				}
			}
			was = m.State
		}
		if err != nil {
			return err
		}
	}
}

var errDetached = errors.New("detached")

// consoleState tells the user, on stderr, how the terminal is attached
// when that changes: first, when the input is taken or held again, and
// when the screen's size changes, with the stty that tells the guest.
func (a *app) consoleState(name string, was, now *computev1alpha1.MachineConsoleState, readOnly, onTerminal bool) {
	cols, rows := now.GetSize().GetColumns(), now.GetSize().GetRows()
	stty := fmt.Sprintf("stty rows %d cols %d", rows, cols)
	switch {
	case was == nil:
		how := "holding the input"
		if !now.GetInput() {
			how = "watching: another client holds the input; --force takes it"
		}
		if readOnly {
			how = "watching"
		}
		leave := "Ctrl-] leaves"
		if !onTerminal {
			leave = "an interrupt leaves"
		}
		a.consoleNote(fmt.Sprintf("attached to vm/%s's serial console, %dx%d, %s; %s. The guest knows its size only from %s", name, cols, rows, how, leave, stty))
	case was.GetInput() && !now.GetInput():
		a.consoleNote("another client took the input; watching")
	case !was.GetInput() && now.GetInput():
		a.consoleNote("holding the input")
	case !proto.Equal(was.GetSize(), now.GetSize()):
		a.consoleNote(fmt.Sprintf("the screen is %dx%d: tell the guest with %s", cols, rows, stty))
	}
}

// consoleNote says what hangar console says, on its own line of stderr, as
// a raw terminal needs it.
func (a *app) consoleNote(msg string) {
	_, _ = fmt.Fprintf(a.io.Err, "\r\n[hangar: %s]\r\n", msg)
}

// consoleClient is the client's end of a console.
type consoleClient struct {
	conn *websocket.Conn
	ws   context.Context
	live *execws.Liveness
}

func (s *consoleClient) send(req *computev1alpha1.MachineConsoleRequest) error {
	b, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	done := s.live.Pending()
	defer done()
	return s.conn.Write(s.ws, websocket.MessageBinary, b)
}

// relayKeys passes what is typed on to the guest, if send, until the
// detach key, which ends the console by cancel. It may outlive the
// console, blocked reading in, as reading a terminal cannot be cut short.
func (s *consoleClient) relayKeys(in io.Reader, send bool, cancel context.CancelCauseFunc) {
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		keys := buf[:n]
		detach := false
		if i := bytes.IndexByte(keys, detachKey); i >= 0 {
			keys, detach = keys[:i], true
		}
		if len(keys) > 0 && send {
			if s.send(&computev1alpha1.MachineConsoleRequest{Message: &computev1alpha1.MachineConsoleRequest_Input{Input: bytes.Clone(keys)}}) != nil {
				return
			}
		}
		if detach {
			cancel(errDetached)
			return
		}
		if err != nil {
			return
		}
	}
}
