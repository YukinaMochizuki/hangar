// Package edgedial is how hangar-edge reaches a public endpoint's machine:
// it asks the node agent, over a unix socket only hangar-edge may use, for
// a TCP connection to the endpoint's port, naming the endpoint and never an
// address or a port. The node checks the endpoint exists and its machine
// runs there, and dials it from the network's gateway inside its namespace.
// The request is an HTTP/1.1 upgrade; after 101 the connection carries the
// endpoint's bytes both ways. See docs/decisions/public-endpoints.md.
package edgedial

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// Protocol is the Upgrade token of a dial.
const Protocol = "hangar-edge-dial/1"

// UIDHeader carries the uid of the endpoint the edge means, so an endpoint
// made again under the same name is not reached by a request meant for the
// old one.
const UIDHeader = "Hangar-Endpoint-Uid"

// Target names an endpoint.
type Target struct {
	Project, Name, UID string
}

func (t Target) String() string { return t.Project + "/" + t.Name }

// path is where a dial of t is asked for.
func path(t Target) string {
	return "/v1/projects/" + url.PathEscape(t.Project) + "/publicendpoints/" + url.PathEscape(t.Name) + ":dial"
}

// pattern is path as an http.ServeMux pattern: a wildcard cannot share a
// segment with ":dial".
const pattern = "POST /v1/projects/{project}/publicendpoints/{nameDial}"

// Error is a dial the node refused, with the HTTP status it answered.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

// Refuse is the error an Open returns to refuse a dial with status.
func Refuse(status int, format string, args ...any) error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// maxResponse bounds the head and message of a refusal the client reads.
const maxResponse = 16 << 10

// Dial asks the node, over a connection conn opens, for a connection to
// t's port. It returns that connection, or an *Error when the node refused.
func Dial(ctx context.Context, conn func(context.Context) (net.Conn, error), t Target) (net.Conn, error) {
	notUID := func(r rune) bool { return r != '-' && (r < '0' || r > '9') && (r < 'a' || r > 'f') }
	if t.UID == "" || strings.ContainsFunc(t.UID, notUID) {
		return nil, fmt.Errorf("endpoint %s: uid %q is not one", t, t.UID)
	}
	c, err := conn(ctx)
	if err != nil {
		return nil, err
	}
	// The request and its answer end with ctx, the connection after them
	// with its own reads and writes.
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	nc, err := handshake(c, t)
	if !stop() {
		err = errors.Join(err, ctx.Err())
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return nc, nil
}

func handshake(c net.Conn, t Target) (net.Conn, error) {
	req := "POST " + path(t) + " HTTP/1.1\r\nHost: node\r\nConnection: Upgrade\r\nUpgrade: " + Protocol +
		"\r\n" + UIDHeader + ": " + t.UID + "\r\nContent-Length: 0\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		return nil, err
	}
	br := bufio.NewReader(io.LimitReader(c, maxResponse))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, fmt.Errorf("reading the node's answer: %w", err)
	}
	// After a 101 the body is empty, and the connection the endpoint's.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		msg, _ := io.ReadAll(resp.Body)
		return nil, &Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), Protocol) {
		return nil, fmt.Errorf("the node switched to %q, not %s", resp.Header.Get("Upgrade"), Protocol)
	}
	// Whatever the node sent past its answer is the endpoint's already.
	if n := br.Buffered(); n > 0 {
		head, _ := br.Peek(n)
		return &prefixed{Conn: c, head: slices.Clone(head)}, nil
	}
	return c, nil
}

// prefixed is a connection whose first bytes were read already.
type prefixed struct {
	net.Conn
	mu   sync.Mutex
	head []byte
}

func (p *prefixed) Read(b []byte) (int, error) {
	p.mu.Lock()
	if len(p.head) > 0 {
		n := copy(b, p.head)
		p.head = p.head[n:]
		p.mu.Unlock()
		return n, nil
	}
	p.mu.Unlock()
	return p.Conn.Read(b)
}

// CloseWrite ends what is sent, so the endpoint reads the end of its input.
func (p *prefixed) CloseWrite() error { return closeWrite(p.Conn) }

func closeWrite(c net.Conn) error {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Open is what a node does for one dial: the connection to t's port, or an
// error, an *Error to say how to refuse it.
type Open func(ctx context.Context, t Target) (net.Conn, error)

// Server serves dials on a listener, from the uids it accepts alone.
type Server struct {
	// UIDs are the peer uids accepted, hangar-edge's.
	UIDs []uint32
	Open Open
	Log  *slog.Logger
	// Grace is how long, once the edge's side of a dial has ended, the
	// endpoint has to finish what it sends back before both are closed;
	// zero is DefaultGrace.
	Grace time.Duration
}

type peerKey struct{}

// Serve serves ln until ctx is done. Each dial lasts as long as either side
// keeps its connection.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc(pattern, s.dial)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if p, err := auth.PeerOf(c); err == nil {
				return context.WithValue(ctx, peerKey{}, p)
			}
			return ctx
		},
	}
	stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
	defer stop()
	err := srv.Serve(ln)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// dialTimeout bounds the node's dial of a machine that does not answer.
const dialTimeout = 10 * time.Second

func (s *Server) dial(w http.ResponseWriter, r *http.Request) {
	p, ok := r.Context().Value(peerKey{}).(*auth.Peer)
	if !ok || !slices.Contains(s.UIDs, p.UID) {
		http.Error(w, "only hangar-edge dials here", http.StatusForbidden)
		s.Log.Warn("an edge dial refused: not hangar-edge", "peer", p)
		return
	}
	name, ok := strings.CutSuffix(r.PathValue("nameDial"), ":dial")
	if !ok || !strings.EqualFold(r.Header.Get("Upgrade"), Protocol) {
		http.Error(w, "not a dial", http.StatusNotFound)
		return
	}
	t := Target{Project: r.PathValue("project"), Name: name, UID: r.Header.Get(UIDHeader)}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	conn, err := s.Open(ctx, t)
	cancel()
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = &Error{Status: http.StatusBadGateway, Message: err.Error()}
		}
		s.Log.Info("an edge dial refused", "endpoint", t.String(), "uid", t.UID, "status", e.Status, "error", e.Message)
		http.Error(w, e.Message, e.Status)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = conn.Close()
		http.Error(w, "the connection cannot be taken over", http.StatusInternalServerError)
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		_ = conn.Close()
		return
	}
	_, err = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+Protocol+"\r\n\r\n")
	if err != nil {
		_ = c.Close()
		_ = conn.Close()
		return
	}
	var from io.Reader = c
	if n := brw.Reader.Buffered(); n > 0 {
		head, _ := brw.Peek(n)
		from = io.MultiReader(strings.NewReader(string(head)), c)
	}
	go Splice(c, from, conn, cmp.Or(s.Grace, DefaultGrace))
}

// DefaultGrace is how long, once the edge's side of a dial has ended, the
// endpoint has to finish what it sends back (Server.Grace).
const DefaultGrace = 30 * time.Second

// Splice copies a's reads (from from, which reads a) to b and b's to a,
// each side's end of input ending what the other side is sent, and closes
// both once both directions are done, either fails, or grace has passed
// since a's input ended: a b that never closes its side holds nothing past
// it.
func Splice(a net.Conn, from io.Reader, b net.Conn, grace time.Duration) {
	var wg sync.WaitGroup
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	copyHalf := func(dst net.Conn, src io.Reader) bool {
		if _, err := io.Copy(dst, src); err != nil {
			closeBoth()
			return false
		}
		if closeWrite(dst) != nil {
			closeBoth()
			return false
		}
		return true
	}
	var timer *time.Timer
	var mu sync.Mutex
	wg.Add(2)
	go func() {
		defer wg.Done()
		if copyHalf(b, from) {
			mu.Lock()
			timer = time.AfterFunc(grace, closeBoth)
			mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		copyHalf(a, b)
	}()
	wg.Wait()
	mu.Lock()
	if timer != nil {
		timer.Stop()
	}
	mu.Unlock()
	closeBoth()
}
