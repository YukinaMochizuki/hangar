package front_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	"github.com/YukinaMochizuki/hangar/internal/edge/front"
)

// slowAPI is fakeAPI, with /early, which answers before it reads the body
// as a refusal does.
func slowAPI(release <-chan struct{}) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", fakeAPI(release))
	mux.HandleFunc("/early", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	})
	return mux
}

// tlsDoor is a pipeDoor with TLS, slowAPI as its apiserver, and endpoints
// if set, with the default limits.
func tlsDoor(t *testing.T, release <-chan struct{}, endpoints http.Handler) (*pipeDoor, front.Limits) {
	t.Helper()
	l := defaultOptions().limits
	return startPipeDoorWith(t, pipeOptions{limits: l, api: slowAPI(release), endpoints: endpoints, tls: true}), l
}

// TestASlowBodyIsCut: a body that stops coming is answered 408 within the
// body timeout, over HTTP/2 and HTTP/1.1, to the API and to the console's
// API paths; over HTTP/2 its connection is kept.
func TestASlowBodyIsCut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, l := tlsDoor(t, nil, nil)
		for _, h2 := range []bool{true, false} {
			c := d.newClient(t, h2)
			dials := d.https.port.Load()
			for _, url := range []string{"https://" + apiHost + "/", "https://" + consoleHost + "/hangar.x.v1.S/M"} {
				pr, pw := io.Pipe()
				go func() { _, _ = pw.Write([]byte("a")) }()
				req, _ := http.NewRequest(http.MethodPost, url, pr)
				start := time.Now()
				resp, err := c.Do(req)
				took := time.Since(start)
				_ = pw.Close()
				if err != nil {
					t.Fatalf("h2 %t, %s: %v", h2, url, err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusRequestTimeout || took > l.BodyReadTimeout+time.Millisecond {
					t.Errorf("h2 %t, %s: %s after %v; want 408 within %v", h2, url, resp.Status, took, l.BodyReadTimeout)
				}
			}
			if !h2 {
				continue
			}
			// One connection a site; a request after the cut takes no other.
			resp, err := c.Get("https://" + apiHost + "/")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if n := d.https.port.Load() - dials; n != 2 {
				t.Errorf("over HTTP/2 the requests to two sites took %d connections; want two", n)
			}
		}
	})
}

// TestABodyLeftUnreadIsBounded: what net/http reads of a body the
// apiserver left unread is held to the body timeout too.
func TestABodyLeftUnreadIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, l := tlsDoor(t, nil, nil)
		c := d.handshake(t, apiHost)
		go func() {
			_, _ = io.WriteString(c, "POST /early HTTP/1.1\r\nHost: "+apiHost+"\r\nTransfer-Encoding: chunked\r\n\r\n1\r\na\r\n")
		}()
		start := time.Now()
		b, _ := io.ReadAll(c)
		// net/http waits 500 ms between closing its side for writing and
		// closing the connection, so the client reads all of the answer.
		if took := time.Since(start); took > l.BodyReadTimeout+time.Second {
			t.Errorf("a body left unread kept its connection %v", took)
		}
		// The apiserver's answer, or the cut's, whichever comes first.
		if s := string(b); !strings.HasPrefix(s, "HTTP/1.1 401 ") && !strings.HasPrefix(s, "HTTP/1.1 408 ") {
			t.Errorf("answered %q; want 401 or 408", b)
		}
	})
}

// TestAnAnswerGoesOutBeforeTheBody: over HTTP/1 a handler may answer while
// the body is still coming, and the answer is not held until net/http has
// read the body.
func TestAnAnswerGoesOutBeforeTheBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		endpoints := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			w.(http.Flusher).Flush()
			<-release
		})
		d, _ := tlsDoor(t, nil, endpoints)
		c := d.handshake(t, webHost)
		go func() {
			_, _ = io.WriteString(c, "POST / HTTP/1.1\r\nHost: "+webHost+"\r\nTransfer-Encoding: chunked\r\n\r\n1\r\na\r\n")
		}()
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("no answer while the body is still coming: %v", err)
		}
		// Before the handler is released: its read deadline ends the drain.
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("answered %s", resp.Status)
		}
	})
}

// TestAHandlerSlowToReadCostsTheClientNothing: a body that arrives in time
// is not cut while its handler, or the backend behind it, takes a while
// between reads.
func TestAHandlerSlowToReadCostsTheClientNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		endpoints := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			first := make([]byte, 1)
			if _, err := io.ReadFull(r.Body, first); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			time.Sleep(time.Minute) //nolint:forbidigo // virtual time: a handler busy elsewhere
			rest, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, string(first)+string(rest))
		})
		d, _ := tlsDoor(t, nil, endpoints)
		for _, h2 := range []bool{true, false} {
			// The body stays open past the timeout, all of it sent while the
			// handler is away.
			pr, pw := io.Pipe()
			go func() {
				_, _ = pw.Write([]byte("a"))
				time.Sleep(50 * time.Second) //nolint:forbidigo // virtual time
				_, _ = pw.Write([]byte("b"))
				time.Sleep(5 * time.Second) //nolint:forbidigo // virtual time
				_ = pw.Close()
			}()
			req, _ := http.NewRequest(http.MethodPost, "https://"+webHost+"/", pr)
			resp, err := d.newClient(t, h2).Do(req)
			if err != nil {
				t.Fatalf("h2 %t: %v", h2, err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || string(b) != "ab" {
				t.Errorf("h2 %t: %s %q; want the whole body read", h2, resp.Status, b)
			}
		}
	})
}

// TestAStreamOutlastsTheTimeouts: a server stream asked with a body, and
// silent for minutes, is cut by no timeout: its body ended, and it has
// nothing to write.
func TestAStreamOutlastsTheTimeouts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		d, _ := tlsDoor(t, release, nil)
		type stream struct {
			h2 bool
			r  *bufio.Reader
		}
		var streams []stream
		for _, h2 := range []bool{true, false} {
			req, _ := http.NewRequest(http.MethodPost, "https://"+apiHost+"/stream", strings.NewReader("watch"))
			resp, err := d.newClient(t, h2).Do(req)
			if err != nil {
				t.Fatalf("h2 %t: %v", h2, err)
			}
			defer func() { _ = resp.Body.Close() }()
			r := bufio.NewReader(resp.Body)
			if line, err := r.ReadString('\n'); err != nil || line != "first\n" {
				t.Fatalf("h2 %t: the stream's first message: %q, %v", h2, line, err)
			}
			streams = append(streams, stream{h2, r})
		}
		time.Sleep(10 * time.Minute) //nolint:forbidigo // virtual time
		close(release)
		for _, s := range streams {
			if line, err := s.r.ReadString('\n'); err != nil || line != "second\n" {
				t.Errorf("h2 %t: the stream's second message after 10 min: %q, %v", s.h2, line, err)
			}
		}
	})
}

// TestAWebSocketOutlastsTheTimeouts: an idle WebSocket, as an exec waiting
// on its user is, is cut by no timeout.
func TestAWebSocketOutlastsTheTimeouts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, _ := tlsDoor(t, nil, nil)
		ctx := context.Background()
		c, resp, err := websocket.Dial(ctx, "wss://"+apiHost+"/v1/ws", &websocket.DialOptions{HTTPClient: d.newClient(t, false)})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.CloseNow() }()
		time.Sleep(10 * time.Minute) //nolint:forbidigo // virtual time
		if err := c.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
			t.Fatal(err)
		}
		if _, msg, err := c.Read(ctx); err != nil || string(msg) != apiHost+" ping" {
			t.Errorf("after 10 min idle: %q, %v", msg, err)
		}
	})
}

func TestAnUpgradeWithABodyIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, _ := tlsDoor(t, nil, nil)
		req, _ := http.NewRequest(http.MethodPost, "https://"+apiHost+"/v1/ws", strings.NewReader("body"))
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		resp, err := d.newClient(t, false).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s; want 400", resp.Status)
		}
	})
}

// TestSlowClientsOnPlainHTTP: on :80, whose handler reads no body, a body
// net/http reads after it is bounded, and a client that sends no request,
// or no more, is closed.
func TestSlowClientsOnPlainHTTP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := defaultOptions().limits
		d := startPipeDoor(t, l, nil)
		answered := func(c net.Conn) {
			t.Helper()
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusPermanentRedirect {
				t.Fatalf("answered %s", resp.Status)
			}
		}

		// A body that stops: the redirect is sent, and the connection closed
		// after the body timeout.
		c := d.http.dial(t)
		go func() {
			_, _ = io.WriteString(c, "POST / HTTP/1.1\r\nHost: "+apiHost+"\r\nTransfer-Encoding: chunked\r\n\r\n1\r\na\r\n")
		}()
		answered(c)
		start := time.Now()
		// net/http waits 500 ms between closing its side for writing and
		// closing the connection, so the client reads all of the answer.
		if !closedSoon(c, time.Hour) || time.Since(start) > l.BodyReadTimeout+500*time.Millisecond {
			t.Errorf("a body that stopped kept its connection %v; want at most %v", time.Since(start), l.BodyReadTimeout)
		}

		// No request at all.
		start = time.Now()
		if !closedSoon(d.http.dial(t), time.Hour) || time.Since(start) > l.ReadHeaderTimeout {
			t.Errorf("a connection with no request was kept %v; want at most %v", time.Since(start), l.ReadHeaderTimeout)
		}

		// A request, then nothing: closed after the idle timeout, not before.
		c = d.http.dial(t)
		go func() { _, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+apiHost+"\r\n\r\n") }()
		answered(c)
		start = time.Now()
		if !closedSoon(c, time.Hour) {
			t.Fatal("an idle connection was kept")
		}
		if took := time.Since(start); took != l.IdleTimeout {
			t.Errorf("an idle connection was closed after %v; want %v", took, l.IdleTimeout)
		}
	})
}
