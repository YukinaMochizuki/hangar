package registry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Timeouts bound how long a registry may go without making progress, so a
// stalled or trickling registry fails a pull in seconds, not at the pull's
// own timeout. Zero fields take DefaultTimeouts'.
type Timeouts struct {
	// Dial and TLSHandshake bound connecting; they apply to the default
	// transport only, not to Config.Transport.
	Dial, TLSHandshake time.Duration
	// ResponseHeader bounds the wait for an answer, from sending the
	// request.
	ResponseHeader time.Duration
	// Window and MinBytes bound reading an answer's body: each window it
	// must bring MinBytes, or the read fails.
	Window   time.Duration
	MinBytes int64
}

// DefaultTimeouts suit a registry across the internet: a body may slow to
// 1 KiB/s, for half a minute, before it is given up.
var DefaultTimeouts = Timeouts{
	Dial: 30 * time.Second, TLSHandshake: 10 * time.Second, ResponseHeader: 30 * time.Second,
	Window: 30 * time.Second, MinBytes: 30 << 10,
}

func (t Timeouts) orDefault() Timeouts {
	or := func(v, d time.Duration) time.Duration {
		if v == 0 {
			return d
		}
		return v
	}
	t.Dial = or(t.Dial, DefaultTimeouts.Dial)
	t.TLSHandshake = or(t.TLSHandshake, DefaultTimeouts.TLSHandshake)
	t.ResponseHeader = or(t.ResponseHeader, DefaultTimeouts.ResponseHeader)
	t.Window = or(t.Window, DefaultTimeouts.Window)
	if t.MinBytes == 0 {
		t.MinBytes = DefaultTimeouts.MinBytes
	}
	return t
}

// defaultTransport is http.DefaultTransport, connecting within t. The
// answer and its body are bounded by progress, whatever the transport.
func defaultTransport(t Timeouts) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: t.Dial, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = t.TLSHandshake
	return tr
}

// StallError is a registry, or another server, making no progress within
// its Timeouts. It is a timeout, and not temporary: the pull fails and is
// tried again later as a whole, not request by request.
type StallError struct {
	What string
	// Server says what made no progress; empty is "registry".
	Server string
}

func (e *StallError) Error() string   { return "the " + cmp.Or(e.Server, "registry") + " " + e.What }
func (e *StallError) Timeout() bool   { return true }
func (e *StallError) Temporary() bool { return false }

// progress is a transport that fails a request whose answer, or whose
// body, stalls.
type progress struct {
	base http.RoundTripper
	t    Timeouts
	// server names what it reaches, for StallError.
	server string
}

// Bounded is base, or a transport of the defaults connecting within t if it
// is nil, failing with a StallError a request to server whose answer, or
// whose body, makes no progress within t.
func Bounded(base http.RoundTripper, t Timeouts, server string) http.RoundTripper {
	t = t.orDefault()
	if base == nil {
		base = defaultTransport(t)
	}
	return progress{base, t, server}
}

func (p progress) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	header := time.AfterFunc(p.t.ResponseHeader, func() {
		cancel(&StallError{fmt.Sprintf("sent no answer to %s %s within %v", req.Method, req.URL.Redacted(), p.t.ResponseHeader), p.server})
	})
	resp, err := p.base.RoundTrip(req.WithContext(ctx))
	header.Stop()
	if err != nil {
		cause := context.Cause(ctx)
		cancel(nil)
		if stall := (*StallError)(nil); errors.As(cause, &stall) {
			return nil, stall
		}
		return nil, err
	}
	if s := context.Cause(ctx); s != nil {
		// The answer came as the timer fired: its body is cut off.
		_ = resp.Body.Close()
		cancel(nil)
		return nil, s
	}
	w := &watched{ReadCloser: resp.Body, ctx: ctx, cancel: cancel, t: p.t, what: req.URL.Redacted(), server: p.server}
	w.mu.Lock()
	w.timer = time.AfterFunc(p.t.Window, w.check)
	w.mu.Unlock()
	resp.Body = w
	return resp, nil
}

// watched is a body that must bring MinBytes each Window.
type watched struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	t      Timeouts
	what   string
	server string
	read   atomic.Int64

	mu    sync.Mutex
	ended bool
	timer *time.Timer
}

func (w *watched) check() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return
	}
	if got := w.read.Swap(0); got < w.t.MinBytes {
		w.cancel(&StallError{fmt.Sprintf("sent %d bytes of %s in %v, fewer than %d", got, w.what, w.t.Window, w.t.MinBytes), w.server})
		return
	}
	w.timer.Reset(w.t.Window)
}

func (w *watched) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	w.read.Add(int64(n))
	if err != nil {
		if stall := (*StallError)(nil); errors.As(context.Cause(w.ctx), &stall) {
			return n, stall
		}
		if errors.Is(err, io.EOF) {
			w.end()
		}
	}
	return n, err
}

func (w *watched) Close() error {
	w.end()
	err := w.ReadCloser.Close()
	w.cancel(nil)
	return err
}

func (w *watched) end() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ended = true
	w.timer.Stop()
}
