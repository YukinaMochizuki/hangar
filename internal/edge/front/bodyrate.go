package front

import (
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/YukinaMochizuki/hangar/internal/edge"
)

// limitBodyRate holds every request's body, and nothing else, to
// MinBodyRate and BodyReadTimeout (docs/decisions/front-door.md).
func (l Limits) limitBodyRate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		// A WebSocket opens with a GET without a body (RFC 6455). One with
		// a body could be hijacked, and its connection then left with the
		// deadline that bounds what net/http discards of the body.
		if isUpgrade(r) {
			page(w, http.StatusBadRequest, pageUpgradeBody)
			return
		}
		rc := http.NewResponseController(w)
		http1 := r.ProtoMajor == 1
		if http1 {
			// Otherwise net/http reads up to 256 KiB of a body not yet read
			// before it writes the answer, with no deadline; full duplex
			// leaves that to Close and to after the handler, both bounded.
			_ = rc.EnableFullDuplex()
		}
		b := &rateBody{
			body:     r.Body,
			deadline: rc.SetReadDeadline,
			rate:     float64(l.MinBodyRate),
			timeout:  l.BodyReadTimeout,
			http1:    http1,
		}
		r = r.WithContext(edge.WithBodyCut(r.Context(), b.cut))
		r.Body = b
		next.ServeHTTP(w, r)
		b.handlerDone()
	})
}

// isUpgrade says whether r asks to switch protocols.
func isUpgrade(r *http.Request) bool {
	return r.Header.Get("Upgrade") != "" && httpguts.HeaderValuesContainsToken(r.Header["Connection"], "Upgrade")
}

// rateBody is a request's body that fails with edge.ErrBodyTooSlow once
// its client falls behind; how a wait's deadline is set is in
// docs/decisions/front-door.md.
type rateBody struct {
	body     io.ReadCloser
	deadline func(time.Time) error
	rate     float64
	timeout  time.Duration
	http1    bool

	mu     sync.Mutex
	read   int64
	waited time.Duration
	// until is the deadline of the read in progress, zero between reads.
	until time.Time
	// closing is set while Close discards what is left of the body.
	closing bool
	// eof is set once the body has ended, done once the handler returned:
	// after either the deadline is net/http's again.
	eof, done, slow bool
}

func (b *rateBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.slow {
		b.mu.Unlock()
		return 0, edge.ErrBodyTooSlow
	}
	if b.eof || b.done {
		b.mu.Unlock()
		return b.body.Read(p)
	}
	earned := time.Duration(float64(b.read) / b.rate * float64(time.Second))
	wait := min(b.timeout, b.timeout+earned-b.waited)
	if wait <= 0 {
		b.slow = true
		b.mu.Unlock()
		return 0, edge.ErrBodyTooSlow
	}
	start := time.Now()
	b.until = start.Add(wait)
	_ = b.deadline(b.until)
	b.mu.Unlock()

	n, err := b.body.Read(p)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.waited += time.Since(start)
	b.read += int64(n)
	b.until = time.Time{}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		// The deadline stays, past: over HTTP/1 the connection is done
		// with, and nothing more of it is read.
		b.slow = true
		return n, edge.ErrBodyTooSlow
	}
	if !b.done && !b.closing {
		// Left set, the deadline would run while nothing reads: over HTTP/2
		// it would end the stream's body of a client not behind at all.
		_ = b.deadline(time.Time{})
	}
	if err == io.EOF {
		b.eof = true
	}
	return n, err
}

// Close closes the body. Over HTTP/1 net/http then reads up to 256 KiB of
// what is left, to keep the connection, with no deadline of its own; one
// timeout bounds that.
func (b *rateBody) Close() error {
	b.mu.Lock()
	guard := b.http1 && !b.eof && !b.slow && !b.done && !b.closing
	if guard {
		b.closing = true
		_ = b.deadline(time.Now().Add(b.timeout))
	}
	b.mu.Unlock()

	err := b.body.Close()

	if guard {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.closing = false
		if errors.Is(err, os.ErrDeadlineExceeded) {
			b.slow = true
		} else if !b.done && b.until.IsZero() {
			_ = b.deadline(time.Time{})
		}
	}
	return err
}

// cut says whether the body was cut for coming too slowly, a read past its
// deadline included: net/http may end the request's context before that
// read returns.
func (b *rateBody) cut() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.slow || (!b.until.IsZero() && !time.Now().Before(b.until))
}

// handlerDone hands the body back to net/http as the handler returns. Over
// HTTP/1 it reads what is left as Close does, after the handler; one
// timeout bounds that, and none is left to a client already too slow.
func (b *rateBody) handlerDone() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.done = true
	if !b.http1 || b.eof {
		return
	}
	left := b.timeout
	if b.slow {
		left = 0
	}
	_ = b.deadline(time.Now().Add(left))
}
