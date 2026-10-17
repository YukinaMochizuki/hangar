package front_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/YukinaMochizuki/hangar/internal/edge/front"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// closedSoon says whether the front door closes c within d, having sent
// nothing.
func closedSoon(c net.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	return err != nil && (!errors.As(err, &ne) || !ne.Timeout())
}

// h2Answer sends, on a connection of its own for api., one HTTP/2 GET of
// :authority with extra in its header, and says how it was answered,
// "status N", "reset CODE" or "goaway CODE", with the answer's body.
func h2Answer(t *testing.T, d *door, authority string, extra []hpack.HeaderField) (answer, body string) {
	t.Helper()
	cfg := d.tlsConfig(apiHost)
	cfg.NextProtos = []string{"h2"}
	c, err := tls.Dial("tcp", d.https, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, c)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, f := range append([]hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: authority}, {Name: ":path", Value: "/"},
	}, extra...) {
		if err := enc.WriteField(f); err != nil {
			t.Fatal(err)
		}
	}
	// Frames of 16 KiB at most, the size every peer takes.
	const frame = 16 << 10
	rest := block.Bytes()
	first := rest[:min(len(rest), frame)]
	rest = rest[len(first):]
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: first, EndStream: true, EndHeaders: len(rest) == 0}); err != nil {
		t.Fatal(err)
	}
	for len(rest) > 0 {
		n := min(len(rest), frame)
		if err := fr.WriteContinuation(1, n == len(rest), rest[:n]); err != nil {
			t.Fatal(err)
		}
		rest = rest[n:]
	}
	dec := hpack.NewDecoder(4096, nil)
	var data bytes.Buffer
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("no answer to the stream: %v", err)
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := fr.WriteSettingsAck(); err != nil {
					t.Fatal(err)
				}
			}
		case *http2.HeadersFrame:
			if f.StreamID != 1 {
				continue
			}
			fields, err := dec.DecodeFull(f.HeaderBlockFragment())
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range fields {
				if h.Name == ":status" {
					answer = "status " + h.Value
				}
			}
			if answer == "" {
				t.Fatalf("an answer with no :status: %v", fields)
			}
			if f.StreamEnded() {
				return answer, data.String()
			}
		case *http2.DataFrame:
			if f.StreamID != 1 {
				continue
			}
			data.Write(f.Data())
			if f.StreamEnded() {
				return answer, data.String()
			}
		case *http2.RSTStreamFrame:
			if f.StreamID == 1 {
				return "reset " + f.ErrCode.String(), data.String()
			}
		case *http2.GoAwayFrame:
			return "goaway " + f.ErrCode.String(), data.String()
		}
	}
}

func dialTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestConnectionsPerClient(t *testing.T) {
	o := defaultOptions()
	o.limits.ConnsPerClient = 2
	d := startDoor(t, newCA(t), o)

	// keep opens a connection the front door keeps, once those startDoor
	// and the test closed have left its count.
	keep := func() net.Conn {
		var c net.Conn
		testutil.EventuallyWithin(t, 5*time.Second, "a connection within the limit kept", func() bool {
			c = dialTCP(t, d.https)
			return !closedSoon(c, 200*time.Millisecond)
		})
		return c
	}
	first := keep()
	keep()
	if !closedSoon(dialTCP(t, d.https), 2*time.Second) {
		t.Error("a third connection was kept")
	}
	// :80's count the same.
	if !closedSoon(dialTCP(t, d.http), 2*time.Second) {
		t.Error("a third connection, on :80, was kept")
	}
	_ = first.Close()
	keep()
	if !strings.Contains(d.log.String(), "too many connections from the client") {
		t.Errorf("the refusals are not in the log")
	}
}

// TestRefusalsAreSummedUpInTheLog: the connections refused are summed up
// by a ticker, with no further refusal needed to bring the line out.
func TestRefusalsAreSummedUpInTheLog(t *testing.T) {
	o := defaultOptions()
	o.limits.ConnsPerClient = 1
	o.prepare = func(f *front.Front) { front.SetRefusalSummaryEvery(f, 300*time.Millisecond) }
	d := startDoor(t, newCA(t), o)
	// One connection kept, once startDoor's own have left the count.
	testutil.EventuallyWithin(t, 5*time.Second, "a connection kept", func() bool {
		return !closedSoon(dialTCP(t, d.https), 200*time.Millisecond)
	})
	// Past any summary of startDoor's refusals.
	time.Sleep(time.Second) //nolint:forbidigo // real time
	from := len(d.log.String())
	for range 3 {
		if !closedSoon(dialTCP(t, d.https), 2*time.Second) {
			t.Fatal("a second connection from the client was kept")
		}
	}
	// Nothing more is refused; the summary comes on its own.
	const summary = `msg="refused connections past the limits" by_reason="map[too many connections from the client:`
	testutil.EventuallyWithin(t, 5*time.Second, "a summary of the refusals in the log", func() (bool, string) {
		log := d.log.String()[from:]
		return strings.Contains(log, summary), "\n" + log
	})
}

func TestConnectionRate(t *testing.T) {
	// The certificates are in storage before the door starts, so its
	// first handshakes, one a name, succeed and take 3 of the burst.
	c := newCA(t)
	o := defaultOptions()
	o.storage = t.TempDir()
	startDoor(t, c, o)
	o.limits.ConnRate, o.limits.ConnBurst = 0.1, 6
	d := startDoor(t, c, o)
	var closed int
	for range 5 {
		if closedSoon(dialTCP(t, d.https), 300*time.Millisecond) {
			closed++
		}
	}
	if closed < 2 || closed == 5 {
		t.Errorf("%d of 5 connections closed; want those past the burst of 6 alone", closed)
	}
}

func TestRequestRate(t *testing.T) {
	o := defaultOptions()
	o.limits.RequestRate, o.limits.RequestBurst = 0.25, 3
	d := startDoor(t, newCA(t), o)
	for i := range 3 {
		if code, _, _ := do(t, d.client, http.MethodGet, "https://"+apiHost+"/", nil, nil); code != http.StatusOK {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	code, body, h := do(t, d.client, http.MethodGet, "https://"+webHost+"/", nil, nil)
	if code != http.StatusTooManyRequests || body != "too many requests; try again shortly\n" {
		t.Errorf("past the burst: %d %q", code, body)
	}
	if ra := h.Get("Retry-After"); ra != "4" {
		t.Errorf("Retry-After %q; want 4", ra)
	}
	// :80 counts the same.
	if code, _, _ := do(t, d.client1, http.MethodGet, "http://"+apiHost+"/", nil, nil); code != http.StatusTooManyRequests {
		t.Errorf("past the burst, on :80: %d", code)
	}
}

// TestRequestsInFlight: past its requests in flight a client is refused. Its
// door is its own: at one in flight, a request sent once the last answer
// arrived may be refused too, as the proxy flushes it before it returns.
func TestRequestsInFlight(t *testing.T) {
	release := make(chan struct{})
	o := defaultOptions()
	o.api = fakeAPI(release)
	o.limits.RequestsPerClient = 1
	d := startDoor(t, newCA(t), o)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+apiHost+"/stream", nil)
	resp, err := d.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, d.client, http.MethodGet, "https://"+apiHost+"/", nil, nil); code != http.StatusTooManyRequests {
		t.Errorf("a second request while the stream runs: %d; want 429", code)
	}
	close(release)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	testutil.EventuallyWithin(t, 5*time.Second, "a request answered after the stream ended", func() (bool, string) {
		code, _, _ := do(t, d.client, http.MethodGet, "https://"+apiHost+"/", nil, nil)
		return code == http.StatusOK, fmt.Sprint(code)
	})
}

func TestRequestLimits(t *testing.T) {
	o := defaultOptions()
	o.limits.APIBodyBytes = 1024
	o.limits.EndpointBodyBytes = 2048
	o.limits.MaxHeaderBytes = 4096
	o.limits.HTTP2MaxStreams = 7
	o.limits.ReadHeaderTimeout = 500 * time.Millisecond
	d := startDoor(t, newCA(t), o)

	t.Run("bodies", func(t *testing.T) {
		chunked := func(n int) io.Reader { return io.MultiReader(bytes.NewReader(make([]byte, n))) }
		for _, tt := range []struct {
			name, url string
			body      io.Reader
			code      int
		}{
			{"api, at the limit", "https://" + apiHost + "/", bytes.NewReader(make([]byte, 1024)), 200},
			{"api, said too long", "https://" + apiHost + "/", bytes.NewReader(make([]byte, 1025)), 413},
			{"api, found too long", "https://" + apiHost + "/", chunked(4096), 413},
			{"console's API, said too long", "https://" + consoleHost + "/v1/x", bytes.NewReader(make([]byte, 1025)), 413},
			{"endpoint, within its limit", "https://" + webHost + "/", bytes.NewReader(make([]byte, 2048)), 200},
			{"endpoint, said too long", "https://" + webHost + "/", bytes.NewReader(make([]byte, 2049)), 413},
			{"endpoint, found too long", "https://" + webHost + "/", chunked(4096), 413},
		} {
			for _, c := range []*http.Client{d.client, d.client1} {
				code, body, _ := do(t, c, http.MethodPost, tt.url, tt.body, nil)
				if code != tt.code {
					t.Errorf("%s: %d %q; want %d", tt.name, code, body, tt.code)
				}
				if seeker, ok := tt.body.(io.Seeker); ok {
					_, _ = seeker.Seek(0, io.SeekStart)
				} else {
					break
				}
			}
		}
	})
	t.Run("headers", func(t *testing.T) {
		// net/http reads 4 KiB past MaxHeaderBytes before it refuses.
		if code, _, _ := do(t, d.client1, http.MethodGet, "https://"+apiHost+"/", nil, http.Header{"X-Big": {strings.Repeat("a", 2<<10)}}); code != http.StatusOK {
			t.Errorf("a 2 KiB header: %d; want 200", code)
		}
		if code, _, _ := do(t, d.client1, http.MethodGet, "https://"+apiHost+"/", nil, http.Header{"X-Big": {strings.Repeat("a", 16<<10)}}); code != http.StatusRequestHeaderFieldsTooLarge {
			t.Errorf("a 16 KiB header: %d; want 431", code)
		}
		// Over HTTP/2 Go's client will not send a header list past the
		// limit the server announced, so the streams are written by hand.
		// Many fields past it in all are answered 431; one field longer than
		// the limit by itself cannot be decoded, which ends the connection.
		many := make([]hpack.HeaderField, 32)
		for i := range many {
			many[i] = hpack.HeaderField{Name: fmt.Sprintf("x-big-%d", i), Value: strings.Repeat("a", 512)}
		}
		for _, tt := range []struct {
			name   string
			fields []hpack.HeaderField
			want   string
		}{
			{"a small header", []hpack.HeaderField{{Name: "x-small", Value: "a"}}, "status 200"},
			{"16 KiB in 32 fields", many, "status 431"},
			{"one 16 KiB field", []hpack.HeaderField{{Name: "x-big", Value: strings.Repeat("a", 16<<10)}}, "goaway COMPRESSION_ERROR"},
		} {
			if got, _ := h2Answer(t, d, apiHost, tt.fields); got != tt.want {
				t.Errorf("%s over HTTP/2: %s; want %s", tt.name, got, tt.want)
			}
		}
	})
	t.Run("slow clients", func(t *testing.T) {
		// No handshake at all.
		if !closedSoon(dialTCP(t, d.https), 3*time.Second) {
			t.Error("a connection that never shook hands was kept")
		}
		// A header that never ends.
		c, err := d.handshake(apiHost)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+apiHost+"\r\nX-Slow: "); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		b, err := io.ReadAll(c)
		if took := time.Since(start); took > 3*time.Second {
			t.Errorf("a header that never ends was kept %v: %q, %v", took, b, err)
		}
	})
	t.Run("HTTP/2 settings", func(t *testing.T) {
		cfg := d.tlsConfig(apiHost)
		cfg.NextProtos = []string{"h2"}
		c, err := tls.Dial("tcp", d.https, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if p := c.ConnectionState().NegotiatedProtocol; p != "h2" {
			t.Fatalf("negotiated %q", p)
		}
		if _, err := io.WriteString(c, http2.ClientPreface); err != nil {
			t.Fatal(err)
		}
		fr := http2.NewFramer(c, c)
		if err := fr.WriteSettings(); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		sf, ok := f.(*http2.SettingsFrame)
		if !ok {
			t.Fatalf("first frame %v", f)
		}
		if v, ok := sf.Value(http2.SettingMaxConcurrentStreams); !ok || v != 7 {
			t.Errorf("max concurrent streams %d, %t; want 7", v, ok)
		}
	})
}
