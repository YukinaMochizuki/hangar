package front_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// TestAHijackedConnectionHasNoWriteDeadline: a connection a handler took
// over, and still uses after it returns, is given no write deadline.
func TestAHijackedConnectionHasNoWriteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		done := make(chan struct{})
		endpoints := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			c, brw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			go func() {
				defer close(done)
				defer func() { _ = c.Close() }()
				_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
				_ = brw.Flush()
				time.Sleep(time.Hour) //nolint:forbidigo // virtual time
				_, _ = c.Write([]byte("late"))
			}()
		})
		d, _ := tlsDoor(t, nil, endpoints)
		c := d.handshake(t, webHost)
		go func() {
			_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+webHost+"\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		}()
		r := bufio.NewReader(c)
		resp, err := http.ReadResponse(r, nil)
		if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("the upgrade: %v, %v", resp, err)
		}
		_ = resp.Body.Close()
		late := make([]byte, 4)
		if _, err := io.ReadFull(r, late); err != nil || string(late) != "late" {
			t.Errorf("what the handler's goroutine wrote an hour on: %q, %v", late, err)
		}
		<-done
	})
}

// TestAKeepAliveConnectionAnswersAfterIdling: the write deadline an answer
// was left with is gone by the connection's next request, so what net/http
// writes itself, a 100 Continue, goes out however long the connection
// idled first.
func TestAKeepAliveConnectionAnswersAfterIdling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		endpoints := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_, _ = io.WriteString(w, "read "+string(b))
		})
		d, _ := tlsDoor(t, nil, endpoints)
		c := d.handshake(t, webHost)
		r := bufio.NewReader(c)
		answer := func() string {
			t.Helper()
			resp, err := http.ReadResponse(r, nil)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp.Status + " " + string(b)
		}
		go func() { _, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+webHost+"\r\n\r\n") }()
		if got := answer(); got != "200 OK read " {
			t.Fatalf("the first request: %q", got)
		}
		time.Sleep(time.Minute) //nolint:forbidigo // virtual time
		go func() {
			_, _ = io.WriteString(c, "POST / HTTP/1.1\r\nHost: "+webHost+"\r\nExpect: 100-continue\r\nContent-Length: 4\r\n\r\n")
		}()
		if got := answer(); got != "100 Continue " {
			t.Fatalf("after a minute idle, the 100 Continue: %q", got)
		}
		go func() { _, _ = io.WriteString(c, "body") }()
		if got := answer(); got != "200 OK read body" {
			t.Errorf("after a minute idle: %q", got)
		}
	})
}

// answerEndpoint answers / with 1 MiB, which it is still writing when it
// is held, and /small with 100 bytes, which net/http sends after it. The
// probes of a client that takes no answer use it rather than the API's
// proxy, whose writer waits on a mutex while a write is held, which a
// synctest bubble cannot wait past; the write deadlines are the door's,
// the same for every site.
var answerEndpoint = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	n := 1 << 20
	if r.URL.Path == "/small" {
		n = 100
	}
	_, _ = w.Write(make([]byte, n))
})

// TestAnHTTP2ClientThatOpensNoWindowIsCut: a client that answers pings but
// gives the door no window to send in has its stream reset once a write
// has waited the stalled-write timeout.
func TestAnHTTP2ClientThatOpensNoWindowIsCut(t *testing.T) {
	for _, path := range []string{"/", "/small"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { testNoWindow(t, path) })
		})
	}
}

func testNoWindow(t *testing.T, path string) {
	d, l := tlsDoor(t, nil, answerEndpoint)
	c := d.handshake(t, webHost, "h2")
	fr := http2.NewFramer(c, c)
	start := time.Now()
	// One goroutine writes, as a Framer takes one writer at a time.
	writes := make(chan func(), 16)
	defer close(writes)
	go func() {
		for w := range writes {
			w()
		}
	}()
	writes <- func() {
		_, _ = io.WriteString(c, http2.ClientPreface)
		_ = fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0})
		var block bytes.Buffer
		enc := hpack.NewEncoder(&block)
		for _, f := range []hpack.HeaderField{
			{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"},
			{Name: ":authority", Value: webHost}, {Name: ":path", Value: path},
		} {
			_ = enc.WriteField(f)
		}
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndStream: true, EndHeaders: true})
	}
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("the connection ended before the stream was reset: %v", err)
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				writes <- func() { _ = fr.WriteSettingsAck() }
			}
		case *http2.PingFrame:
			if !f.IsAck() {
				data := f.Data
				writes <- func() { _ = fr.WritePing(true, data) }
			}
		case *http2.DataFrame:
			t.Fatalf("data sent with no window: %v", f)
		case *http2.RSTStreamFrame:
			if took := time.Since(start); took > l.StalledWriteTimeout+time.Second {
				t.Errorf("the stream was reset after %v; want within %v", took, l.StalledWriteTimeout)
			}
			return
		case *http2.GoAwayFrame:
			t.Fatalf("goaway %v before the stream was reset", f.ErrCode)
		}
		if time.Since(start) > 10*time.Minute {
			t.Fatal("the stream was never reset")
		}
	}
}

// TestAnHTTP1ClientThatReadsNothingIsCut: a client that never reads its
// answer has its connection closed once a write has waited the
// stalled-write timeout.
func TestAnHTTP1ClientThatReadsNothingIsCut(t *testing.T) {
	for _, path := range []string{"/", "/small"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { testReadsNothing(t, path) })
		})
	}
}

func testReadsNothing(t *testing.T, path string) {
	d, l := tlsDoor(t, nil, answerEndpoint)
	c := d.handshake(t, webHost)
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: "+webHost+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(l.StalledWriteTimeout + time.Second) //nolint:forbidigo // virtual time
	// A pipe holds nothing: what the door did not write before it gave
	// up is gone, and a connection still open would answer now.
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := c.Read(make([]byte, 1)); err == nil {
		t.Errorf("read %d bytes of an answer after %v; want the connection closed", n, l.StalledWriteTimeout+time.Second)
	} else if ne := (net.Error)(nil); errors.As(err, &ne) && ne.Timeout() {
		t.Errorf("the connection is still open: %v", err)
	}
}
