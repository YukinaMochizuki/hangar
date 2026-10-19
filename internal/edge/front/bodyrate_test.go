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
)

// TestSlowBodies: through the whole door, a body that stops coming is cut
// within about the body timeout, over HTTP/2 and HTTP/1.1, while a
// stream's response and a WebSocket outlast it. The timeout is real time,
// as the door's sockets and TLS are.
func TestSlowBodies(t *testing.T) {
	release := make(chan struct{})
	o := defaultOptions()
	api := http.NewServeMux()
	api.Handle("/", fakeAPI(release))
	// /early answers before it reads the body, as a refusal does.
	api.HandleFunc("/early", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	})
	o.api = api
	o.limits.BodyReadTimeout = 500 * time.Millisecond
	d := startDoor(t, newCA(t), o)
	// outlast is several timeouts.
	const outlast = 2 * time.Second
	clients := []struct {
		name string
		c    *http.Client
	}{{"HTTP/2", d.client}, {"HTTP/1.1", d.client1}}

	t.Run("a body that stops coming", func(t *testing.T) {
		for _, c := range clients {
			for _, url := range []string{"https://" + apiHost + "/", "https://" + consoleHost + "/hangar.x.v1.S/M"} {
				pr, pw := io.Pipe()
				go func() { _, _ = pw.Write([]byte("a")) }()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
				start := time.Now()
				resp, err := c.c.Do(req)
				took := time.Since(start)
				status := ""
				if err == nil {
					status = resp.Status
					_ = resp.Body.Close()
				}
				_ = pw.Close()
				cancel()
				if took > 3*time.Second {
					t.Errorf("%s %s: a body that stopped was kept %v", c.name, url, took)
				}
				if status != "408 Request Timeout" {
					t.Errorf("%s %s: %q, %v; want 408", c.name, url, status, err)
				}
			}
		}
	})
	// What is left of a body the apiserver did not read is not read slowly
	// for long either.
	t.Run("a body left unread", func(t *testing.T) {
		c, err := d.handshake(apiHost)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if _, err := io.WriteString(c, "POST /early HTTP/1.1\r\nHost: "+apiHost+"\r\nTransfer-Encoding: chunked\r\n\r\n1\r\na\r\n"); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		start := time.Now()
		b, err := io.ReadAll(c)
		if took := time.Since(start); took > 3*time.Second || err != nil {
			t.Errorf("a body left unread kept its connection %v: %v", took, err)
		}
		// The apiserver's answer, or the cut's, whichever comes first.
		if s := string(b); !strings.HasPrefix(s, "HTTP/1.1 401 ") && !strings.HasPrefix(s, "HTTP/1.1 408 ") {
			t.Errorf("answered %q; want 401 or 408", b)
		}
	})
	t.Run("a stream outlasts it", func(t *testing.T) {
		type stream struct {
			name string
			resp *http.Response
			r    *bufio.Reader
		}
		var streams []stream
		for _, c := range clients {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiHost+"/stream", strings.NewReader("watch"))
			resp, err := c.c.Do(req)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			defer func() { _ = resp.Body.Close() }()
			r := bufio.NewReader(resp.Body)
			if line, err := r.ReadString('\n'); err != nil || line != "first\n" {
				t.Fatalf("%s: the stream's first message: %q, %v", c.name, line, err)
			}
			streams = append(streams, stream{c.name, resp, r})
		}
		time.Sleep(outlast) //nolint:forbidigo // the time the body timeout would cut the streams in
		close(release)
		for _, s := range streams {
			if line, err := s.r.ReadString('\n'); err != nil || line != "second\n" {
				t.Errorf("%s: the stream's second message after %v: %q, %v", s.name, outlast, line, err)
			}
		}
	})
	t.Run("a WebSocket outlasts it", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, resp, err := websocket.Dial(ctx, "wss://"+apiHost+"/v1/ws", &websocket.DialOptions{HTTPClient: d.client1})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.CloseNow() }()
		time.Sleep(outlast) //nolint:forbidigo // the time the body timeout would cut the WebSocket in
		if err := c.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
			t.Fatal(err)
		}
		if _, msg, err := c.Read(ctx); err != nil || string(msg) != apiHost+" ping" {
			t.Errorf("after %v idle: %q, %v", outlast, msg, err)
		}
	})
	t.Run("an upgrade with a body", func(t *testing.T) {
		h := http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}}
		if code, _, _ := do(t, d.client1, http.MethodPost, "https://"+apiHost+"/v1/ws", strings.NewReader("body"), h); code != http.StatusBadRequest {
			t.Errorf("%d; want 400", code)
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
