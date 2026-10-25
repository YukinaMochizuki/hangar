package serialmux

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

// redial is how often a connection to Cloud Hypervisor's serial socket is
// tried while there is none: it listens from its start, and again after the
// guest reboots, when it makes the socket afresh.
const redial = 200 * time.Millisecond

// halfCloser is a connection whose writing can end on its own.
type halfCloser interface {
	CloseWrite() error
}

// Pump is the hub's connection to Cloud Hypervisor's serial socket, dialled
// by dial, until ctx ends, and again whenever it ends. It is never closed
// with output unread, which ends Cloud Hypervisor's serial thread for good
// (docs/decisions/serial-console.md).
func (h *Hub) Pump(ctx context.Context, dial func(context.Context) (net.Conn, error)) {
	first := true
	buf := make([]byte, 32<<10)
	for ctx.Err() == nil {
		c, err := dial(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(redial):
			}
			continue
		}
		h.Connected(c, !first)
		first = false
		stop := context.AfterFunc(ctx, func() { endGently(c) })
		for {
			n, err := c.Read(buf)
			if n > 0 {
				h.Output(buf[:n])
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					h.l.Info("serial console: the connection ended", "error", err)
				}
				break
			}
		}
		stop()
		h.Disconnected()
		_ = c.Close()
	}
}

// endGently ends c's writing; its reader goes on until Cloud Hypervisor's
// end closes it.
func endGently(c net.Conn) {
	if hc, ok := c.(halfCloser); ok {
		_ = hc.CloseWrite()
		return
	}
	_ = c.Close()
}
