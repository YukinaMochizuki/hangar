// Package serialmux is a firmware VM's interactive serial console: the one
// client of Cloud Hypervisor's serial socket, it hands what the guest
// writes on to the console log, keeps the screen it draws, and lets clients
// attach, each first drawn the screen whole, then sent what comes; many
// watch, one at a time types. It runs in the VM's console helper, as the
// VM's user (docs/decisions/serial-console.md). It parses what the guest
// writes, so it holds nothing the guest could not reach otherwise.
package serialmux

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
)

// MaxMessage is the largest message either end of an attached client's
// connection sends or reads: output is sent in pieces of at most maxOutput,
// though a drawing of the screen, whose cells the guest sets, can be
// megabytes.
const MaxMessage = 64 << 10

const maxOutput = 32 << 10

// queued is how many messages an attached client may fall behind by
// before it is let go: the guest's output is never held up for one.
const queued = 1024

// writeTimeout bounds a write to an attached client, whose reading has
// stopped past it.
const writeTimeout = 10 * time.Second

// Hub multiplexes the serial console. Its zero value is not usable; use
// New.
type Hub struct {
	log io.Writer
	l   *slog.Logger

	mu     sync.Mutex
	screen *screen
	// serial is the console's connection to Cloud Hypervisor, nil while it
	// is being dialled again.
	serial io.Writer
	subs   map[*client]bool
	// input is the client whose typing reaches the guest, if any.
	input *client
	// logFailing says the log's last write failed, logged once.
	logFailing bool
}

// New is a hub handing what the guest writes on to log.
func New(log io.Writer, l *slog.Logger) *Hub {
	return &Hub{log: log, l: l, screen: newScreen(), subs: map[*client]bool{}}
}

// client is an attached connection.
type client struct {
	conn net.Conn
	out  chan *nodev1.ConsoleResponse
	// gone is closed once the client is let go.
	gone chan struct{}
}

// Connected sets the connection to Cloud Hypervisor typing goes to: a new
// one, after the guest rebooted, starts a new screen at the default size,
// as the guest's tty forgot its size, and every client is drawn it.
func (h *Hub) Connected(w io.Writer, fresh bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.serial = w
	if !fresh {
		return
	}
	h.screen.columns, h.screen.rows = DefaultColumns, DefaultRows
	h.screen.reset()
	h.broadcastLocked(true)
}

// Disconnected says the connection to Cloud Hypervisor is gone.
func (h *Hub) Disconnected() {
	h.mu.Lock()
	h.serial = nil
	h.mu.Unlock()
}

// Close lets every client go, as the VM's end does, and the screen: the
// hub takes nothing after.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		h.dropLocked(c)
	}
	h.screen.close()
}

// Output takes what the guest wrote: the log, the screen and every client
// get it in one critical section, so an attach's drawing and what it is
// sent after neither miss nor repeat a byte.
func (h *Hub) Output(p []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.log.Write(p); err != nil {
		if !h.logFailing {
			h.l.Error("serial console: the log fails", "error", err)
		}
		h.logFailing = true
	} else {
		h.logFailing = false
	}
	before := h.screen.resets
	h.screen.write(p)
	if h.screen.resets != before {
		h.l.Warn("serial console: the screen was started afresh", "resets", h.screen.resets)
	}
	for _, msg := range outputs(p) {
		for c := range h.subs {
			h.sendLocked(c, msg)
		}
	}
}

// outputs is p as output messages of at most maxOutput bytes.
func outputs(p []byte) []*nodev1.ConsoleResponse {
	var out []*nodev1.ConsoleResponse
	for len(p) > 0 {
		n := min(len(p), maxOutput)
		out = append(out, &nodev1.ConsoleResponse{Message: &nodev1.ConsoleResponse_Output{Output: bytes.Clone(p[:n])}})
		p = p[n:]
	}
	return out
}

// sendLocked queues msg to c, letting c go if it has fallen behind.
func (h *Hub) sendLocked(c *client, msg *nodev1.ConsoleResponse) {
	select {
	case c.out <- msg:
	default:
		h.l.Info("serial console: letting go of a client that does not keep up")
		h.dropLocked(c)
	}
}

func (h *Hub) dropLocked(c *client) {
	if !h.subs[c] {
		return
	}
	delete(h.subs, c)
	close(c.gone)
	// Ends a write it is stuck in, too.
	_ = c.conn.Close()
	if h.input == c {
		h.input = nil
	}
}

// stateLocked is what c is told of how it is attached.
func (h *Hub) stateLocked(c *client) *nodev1.ConsoleResponse {
	cols, rows := h.screen.size()
	return &nodev1.ConsoleResponse{Message: &nodev1.ConsoleResponse_State{State: &nodev1.ConsoleState{
		Input: h.input == c,
		Size:  &nodev1.ConsoleSize{Columns: uint32(cols), Rows: uint32(rows)},
	}}}
}

// broadcastLocked tells every client its state, and, with redraw, draws
// each the screen whole.
func (h *Hub) broadcastLocked(redraw bool) {
	var drawn []*nodev1.ConsoleResponse
	if redraw {
		drawn = outputs(h.screen.redraw())
	}
	for c := range h.subs {
		h.sendLocked(c, h.stateLocked(c))
		for _, msg := range drawn {
			if h.subs[c] {
				h.sendLocked(c, msg)
			}
		}
	}
}

// Serve attaches conn, a client, until it goes or is let go: its first
// message says how (nodev1.ConsoleStart), what it types reaches the guest
// while it holds the input, and so does a resize. It closes conn.
func (h *Hub) Serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	first := &nodev1.ConsoleRequest{}
	if err := readMessage(r, first); err != nil || first.GetStart() == nil {
		h.l.Info("serial console: a client did not start", "error", err)
		return
	}
	start := first.GetStart()
	c := &client{conn: conn, out: make(chan *nodev1.ConsoleResponse, queued), gone: make(chan struct{})}

	h.mu.Lock()
	h.subs[c] = true
	if !start.GetReadOnly() && (h.input == nil || start.GetForce()) {
		was := h.input
		h.input = c
		if was != nil {
			h.sendLocked(was, h.stateLocked(was))
		}
	}
	h.sendLocked(c, h.stateLocked(c))
	for _, msg := range outputs(h.screen.redraw()) {
		h.sendLocked(c, msg)
	}
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.read(c, r)
	}()
	h.write(c)
	h.mu.Lock()
	h.dropLocked(c)
	h.mu.Unlock()
	_ = conn.Close()
	<-done
}

// write sends c what is queued for it until it is let go.
func (h *Hub) write(c *client) {
	for {
		select {
		case msg := <-c.out:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := protodelim.MarshalTo(c.conn, msg); err != nil {
				return
			}
		case <-c.gone:
			return
		}
	}
}

// read takes c's typing and resizes until it ends.
func (h *Hub) read(c *client, r *bufio.Reader) {
	defer func() {
		h.mu.Lock()
		h.dropLocked(c)
		h.mu.Unlock()
	}()
	for {
		req := &nodev1.ConsoleRequest{}
		if err := readMessage(r, req); err != nil {
			return
		}
		switch m := req.GetMessage().(type) {
		case *nodev1.ConsoleRequest_Input:
			h.mu.Lock()
			w := h.serial
			if h.input != c {
				w = nil
			}
			h.mu.Unlock()
			if w != nil {
				// Cloud Hypervisor takes a lone newline for a carriage
				// return; nothing else is changed.
				_, _ = w.Write(m.Input)
			}
		case *nodev1.ConsoleRequest_Resize:
			h.mu.Lock()
			if h.input == c {
				h.screen.resize(int(m.Resize.GetColumns()), int(m.Resize.GetRows()))
				h.broadcastLocked(true)
			}
			h.mu.Unlock()
		default:
			return
		}
	}
}

func readMessage(r *bufio.Reader, m proto.Message) error {
	err := protodelim.UnmarshalOptions{MaxSize: MaxMessage}.UnmarshalFrom(r, m)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return io.EOF
	}
	return err
}
