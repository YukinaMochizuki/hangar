package front_test

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
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

// TestANewcomerGetsPastBusyHolders: an attacker from a few addresses keeps
// every place in the door busy, and takes a place back whenever it may; a
// client from a fresh address still gets in, every time.
func TestANewcomerGetsPastBusyHolders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		l := defaultOptions().limits
		l.Conns, l.ConnsPerClient = 8, 4
		d := startPipeDoorWith(t, pipeOptions{limits: l, api: slowAPI(release), tls: true})
		attackers := []net.IP{net.IPv4(203, 0, 113, 1), net.IPv4(198, 51, 100, 1)}
		// hold opens a connection from ip with a stream in flight on it, or
		// says it was refused.
		hold := func(ip net.IP) bool {
			pc, err := d.https.dialFrom(ip)
			if err != nil {
				t.Fatal(err)
			}
			c := tls.Client(pc, d.tlsConfig(apiHost))
			if c.Handshake() != nil {
				return false
			}
			go func() { _, _ = io.WriteString(c, "GET /stream HTTP/1.1\r\nHost: "+apiHost+"\r\n\r\n") }()
			resp, err := http.ReadResponse(bufio.NewReader(c), nil) //nolint:bodyclose // the stream stays in flight; the connection's close ends it
			return err == nil && resp.StatusCode == http.StatusOK
		}
		for i := range l.Conns {
			if !hold(attackers[i%len(attackers)]) {
				t.Fatalf("the attacker's connection %d refused before the door was full", i)
			}
		}
		for round := range 5 {
			// The attacker takes back what it can before each newcomer.
			for _, ip := range attackers {
				hold(ip)
			}
			ip := net.IPv4(192, 0, 2, byte(10+round))
			c := d.handshakeFrom(t, ip, apiHost)
			go func() { _, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+apiHost+"\r\n\r\n") }()
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatalf("round %d: the newcomer from %s was kept out: %v", round, ip, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("round %d: answered %s", round, resp.Status)
			}
		}
	})
}

// zeroWindow holds c, an HTTP/2 connection to webHost, busy: it opens a
// stream the door cannot answer, giving it no window, and another every
// 20 s, before the stalled-write timeout resets the last; it answers pings,
// and returns when the connection ends or stop closes.
func zeroWindow(c net.Conn, stop <-chan struct{}) {
	fr := http2.NewFramer(c, c)
	writes := make(chan func(), 16)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case w := <-writes:
				w()
			case <-done:
				return
			}
		}
	}()
	stream := uint32(1)
	open := func() {
		var block bytes.Buffer
		enc := hpack.NewEncoder(&block)
		for _, f := range []hpack.HeaderField{
			{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"},
			{Name: ":authority", Value: webHost}, {Name: ":path", Value: "/"},
		} {
			_ = enc.WriteField(f)
		}
		id := stream
		stream += 2
		writes <- func() {
			_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: block.Bytes(), EndStream: true, EndHeaders: true})
		}
	}
	writes <- func() {
		_, _ = io.WriteString(c, http2.ClientPreface)
		_ = fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0})
	}
	open()
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				open()
			case <-done:
				return
			case <-stop:
				_ = c.Close()
				return
			}
		}
	}()
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return
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
		}
	}
}

// TestANewcomerOutlastsARedialingAttacker: an attacker holds every place
// with HTTP/2 connections kept busy by streams given no window, and
// redials a second after any is closed; a client from a fresh network
// that waits two seconds before its first request is still answered,
// every time.
func TestANewcomerOutlastsARedialingAttacker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := defaultOptions().limits
		l.Conns = 4
		d := startPipeDoorWith(t, pipeOptions{limits: l, endpoints: answerEndpoint, tls: true})
		stop := make(chan struct{})
		attackers := make(chan struct{}, l.Conns)
		for range l.Conns {
			go func() {
				defer func() { attackers <- struct{}{} }()
				for {
					pc, err := d.https.dialFrom(net.IPv4(203, 0, 113, 1))
					if err != nil {
						return
					}
					c := tls.Client(pc, d.tlsConfig(webHost, "h2"))
					if c.Handshake() == nil {
						zeroWindow(c, stop)
					}
					_ = c.Close()
					select {
					case <-stop:
						return
					case <-time.After(time.Second):
					}
				}
			}()
		}
		defer func() {
			close(stop)
			for range l.Conns {
				<-attackers
			}
		}()
		time.Sleep(5 * time.Second) //nolint:forbidigo // virtual time: the attacker fills the door
		answered := 0
		for round := range 9 {
			pc, err := d.https.dialFrom(net.IPv4(198, 51, byte(100+round), 7))
			if err != nil {
				t.Fatal(err)
			}
			c := tls.Client(pc, d.tlsConfig(webHost))
			if c.Handshake() == nil {
				time.Sleep(2 * time.Second) //nolint:forbidigo // virtual time: a newcomer slow to ask
				go func() { _, _ = io.WriteString(c, "GET /small HTTP/1.1\r\nHost: "+webHost+"\r\n\r\n") }()
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				if resp, err := http.ReadResponse(bufio.NewReader(c), nil); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						answered++
					}
				}
			}
			_ = c.Close()
			time.Sleep(25 * time.Second) //nolint:forbidigo // virtual time: the attacker churns its streams and takes the place back
		}
		if answered != 9 {
			t.Errorf("%d of 9 newcomers answered; want all", answered)
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
