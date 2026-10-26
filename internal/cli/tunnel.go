package cli

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/coder/websocket"

	"github.com/YukinaMochizuki/hangar/internal/execws"
)

// tunnel relays in and out over the WebSocket at path, a machine
// capability's that carries one connection's bytes (an SSH tunnel, a port
// forward), until either ends: in's end closes the WebSocket, and the
// server's close ends the call.
func (a *app) tunnel(ctx context.Context, sc *sshContext, path, subprotocol string, maxMessage int64, in io.Reader, out io.Writer) error {
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
	conn, resp, err := websocket.Dial(ctx, base+path, &websocket.DialOptions{
		HTTPClient: hc, HTTPHeader: header, Subprotocols: []string{subprotocol},
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
	conn.SetReadLimit(maxMessage)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ws := context.WithoutCancel(ctx)
	stopOnCancel := context.AfterFunc(ctx, func() { _ = conn.CloseNow() })
	defer stopOnCancel()
	go func() {
		if live.Run(ctx, conn) != nil {
			cancel(errors.New("the server stopped answering"))
		}
	}()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				done := live.Pending()
				werr := conn.Write(ws, websocket.MessageBinary, buf[:n])
				done()
				if werr != nil {
					return
				}
			}
			if err != nil {
				// The local side is done with the connection.
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}()
	for {
		typ, b, err := conn.Read(ws)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return execws.ErrorOf(err)
		}
		live.Heard()
		if typ != websocket.MessageBinary {
			continue
		}
		if _, err := out.Write(b); err != nil {
			return err
		}
	}
}
