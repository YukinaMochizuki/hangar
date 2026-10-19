package front

import (
	"bufio"
	"net"
	"net/http"
	"sync"
	"time"
)

// boundWrites holds every response to StalledWriteTimeout a write: a
// client that stops taking it, by an HTTP/2 window it never opens or a
// connection it never reads, is cut. A response with nothing to send has
// no deadline, so Watch, a log followed and an exec's output last as long
// as they are used. A hijacked connection, a WebSocket's, is left to its
// own liveness.
func (l Limits) boundWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &stallWriter{ResponseWriter: w, rc: http.NewResponseController(w), timeout: l.StalledWriteTimeout}
		next.ServeHTTP(sw, r)
		sw.handlerDone()
	})
}

// stallWriter gives each write, flush and informational header a write
// deadline of timeout, and takes it away after. Over HTTP/2 the deadline is
// the stream's alone.
type stallWriter struct {
	http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration

	mu sync.Mutex
	// own is the deadline the handler set itself, which no write's passes.
	own      time.Time
	hijacked bool
}

func (s *stallWriter) Write(p []byte) (int, error) {
	s.arm()
	defer s.disarm()
	return s.ResponseWriter.Write(p)
}

// WriteHeader sends a 1xx at once, so it is held as a write is.
func (s *stallWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		s.arm()
		defer s.disarm()
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *stallWriter) FlushError() error {
	s.arm()
	defer s.disarm()
	return s.rc.Flush()
}

func (s *stallWriter) Flush() { _ = s.FlushError() }

func (s *stallWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := s.rc.Hijack()
	if err == nil {
		s.mu.Lock()
		s.hijacked = true
		s.mu.Unlock()
	}
	return c, brw, err
}

func (s *stallWriter) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.own = t
	return s.rc.SetWriteDeadline(t)
}

func (s *stallWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// deadline is now's write deadline. Callers hold s.mu.
func (s *stallWriter) deadline() time.Time {
	d := time.Now().Add(s.timeout)
	if !s.own.IsZero() && s.own.Before(d) {
		return s.own
	}
	return d
}

func (s *stallWriter) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hijacked {
		_ = s.rc.SetWriteDeadline(s.deadline())
	}
}

func (s *stallWriter) disarm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hijacked {
		_ = s.rc.SetWriteDeadline(s.own)
	}
}

// handlerDone holds what net/http sends of the answer after the handler,
// its buffered end, to one timeout. Over HTTP/1 net/http takes the
// deadline away once the answer is out, before the connection's next
// request.
func (s *stallWriter) handlerDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hijacked {
		_ = s.rc.SetWriteDeadline(s.deadline())
	}
}
