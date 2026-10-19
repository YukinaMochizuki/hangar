package front_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// TestAFullDoorClosesItsIdlestConnection: with the door full, a new
// connection takes the place of the one idle longest, and is refused only
// when each carries a request.
func TestAFullDoorClosesItsIdlestConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		l := defaultOptions().limits
		l.Conns = 2
		d := startPipeDoorWith(t, pipeOptions{limits: l, api: slowAPI(release), tls: true})
		ask := func(c net.Conn, path string) *bufio.Reader {
			t.Helper()
			// What closedSoon left.
			_ = c.SetReadDeadline(time.Time{})
			go func() { _, _ = io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: "+apiHost+"\r\n\r\n") }()
			r := bufio.NewReader(c)
			resp, err := http.ReadResponse(r, nil) //nolint:bodyclose // a stream's is left open, to keep its request in flight; the connection's close ends it
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if path != "/stream" {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			return r
		}

		// Two answered, then idle: older, then newer.
		older := d.handshake(t, apiHost)
		ask(older, "/")
		time.Sleep(time.Second) //nolint:forbidigo // virtual time
		newer := d.handshake(t, apiHost)
		ask(newer, "/")
		third := d.handshake(t, apiHost)
		if !closedSoon(older, time.Second) {
			t.Error("the connection idle longest was kept for a new one")
		}
		if closedSoon(newer, time.Second) {
			t.Error("a connection idle less long was closed")
		}

		// Both open ones carry a stream: a new one is refused.
		ask(newer, "/stream")
		ask(third, "/stream")
		if !closedSoon(d.https.dial(t), time.Second) {
			t.Error("a connection was let in while every one carries a request")
		}
		if closedSoon(newer, time.Second) || closedSoon(third, time.Second) {
			t.Error("a connection carrying a stream was closed")
		}
		if !strings.Contains(d.log.String(), `reason="too many connections"`) {
			t.Error("the refusal is not in the log")
		}
	})
}

// TestAnHTTP2ConnectionWithAStreamIsBusy: an HTTP/2 connection carrying a
// stream is not taken for idle; once the stream ends it is.
func TestAnHTTP2ConnectionWithAStreamIsBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		l := defaultOptions().limits
		l.Conns = 1
		d := startPipeDoorWith(t, pipeOptions{limits: l, api: slowAPI(release), tls: true})
		resp, err := d.newClient(t, true).Get("https://" + apiHost + "/stream")
		if err != nil {
			t.Fatal(err)
		}
		if resp.ProtoMajor != 2 {
			t.Fatalf("over %s", resp.Proto)
		}
		if !closedSoon(d.https.dial(t), time.Second) {
			t.Error("a connection was let in in place of an HTTP/2 one carrying a stream")
		}
		close(release)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		synctest.Wait()
		if closedSoon(d.https.dial(t), time.Second) {
			t.Error("a connection was refused in place of an idle HTTP/2 one")
		}
	})
}
