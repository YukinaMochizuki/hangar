package front_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/edge/front"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// measureConns is how many connections TestMemoryPerConnection opens of
// each kind; measureStreams how many proxied streams each HTTP/2 one of
// "h2-proxied" carries.
const (
	measureConns   = 2000
	measureStreams = 20
)

// measureKinds are what TestMemoryPerConnection measures: idle, then busy.
var measureKinds = []string{"tls", "http1", "http2", "h1-header", "h1-unread", "h2-unread", "h1-proxied", "h2-proxied"}

// TestMemoryPerConnection measures what the edge holds a connection, its
// clients and the apiserver in a process of their own; HANGAR_MEASURE_CONNS=1
// runs it, the numbers are in docs/decisions/front-door.md.
func TestMemoryPerConnection(t *testing.T) {
	if os.Getenv("HANGAR_MEASURE_CONNS") == "" {
		t.Skip("set HANGAR_MEASURE_CONNS=1 to measure")
	}
	pemBytes, err := os.ReadFile("testdata/door.pem")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(pemBytes, pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	l := front.DefaultLimits
	l.Conns, l.ConnsPerClient, l.ConnsPer24, l.RequestsPerClient = 1<<20, 1<<20, 1<<20, 1<<20
	l.ConnRate, l.ConnBurst, l.RequestRate, l.RequestBurst = 1e6, 1<<20, 1e6, 1<<20
	l.ReadHeaderTimeout, l.IdleTimeout, l.HTTP2SendPingTimeout = time.Hour, time.Hour, time.Hour
	l.BodyReadTimeout = time.Hour
	if w := os.Getenv("HANGAR_MEASURE_WINDOWS"); w != "" {
		// conn,stream,frame, to measure other settings than the defaults.
		f := strings.Split(w, ",")
		l.HTTP2ConnWindow, _ = strconv.Atoi(f[0])
		l.HTTP2StreamWindow, _ = strconv.Atoi(f[1])
		l.HTTP2MaxFrame, _ = strconv.Atoi(f[2])
	}
	apiSock := filepath.Join(t.TempDir(), "api.sock")
	endpoints := http.NewServeMux()
	endpoints.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	// /hold neither reads the body nor answers, until its kind is measured:
	// with a body unread, net/http would not see the client go.
	var release atomic.Pointer[chan struct{}]
	endpoints.HandleFunc("/hold", func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-*release.Load():
		}
	})
	f, err := front.New(front.Config{
		CPDomain: cpDomain, UserDomain: userDomain,
		DialAPI: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", apiSock)
		},
		Endpoints: endpoints, Limits: l, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	https, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- f.Serve(ctx, front.Listeners{HTTPS: https, HTTP: plain}, &tls.Config{Certificates: []tls.Certificate{cert}})
	}()
	defer func() {
		cancel()
		<-done
	}()

	for _, kind := range measureKinds {
		conns := measureConns
		if kind == "h2-proxied" {
			conns = measureConns / measureStreams
		}
		released := make(chan struct{})
		release.Store(&released)
		before := heldMemory()
		cmd := exec.Command(os.Args[0], "-test.run=^TestMeasureClient$")
		cmd.Env = append(os.Environ(), "HANGAR_MEASURE_CLIENT="+https.Addr().String(), "HANGAR_MEASURE_KIND="+kind,
			"HANGAR_MEASURE_N="+strconv.Itoa(conns), "HANGAR_MEASURE_API="+apiSock)
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || !strings.HasPrefix(line, "ready") {
			t.Fatalf("%s: the clients said %q, %v", kind, line, err)
		}
		testutil.EventuallyWithin(t, time.Minute, "every connection counted", func() (bool, string) {
			n := front.OpenConns(f)
			return n == conns, strconv.Itoa(n)
		})
		held := heldMemory() - before
		if kind == "h2-proxied" {
			t.Logf("%s: heap and stacks %.1f KiB a connection, %.1f KiB a proxied stream", kind,
				float64(held)/float64(conns)/1024, float64(held)/float64(conns*measureStreams)/1024)
		} else {
			t.Logf("%s: heap and stacks %.1f KiB a connection", kind, float64(held)/float64(conns)/1024)
		}
		close(released)
		_ = stdin.Close()
		_ = cmd.Wait()
		testutil.EventuallyWithin(t, time.Minute, "every connection closed", func() (bool, string) {
			n := front.OpenConns(f)
			return n == 0, strconv.Itoa(n)
		})
	}
}

// heldMemory is the heap and the stacks the process holds once its garbage
// is collected. Not its resident set, which keeps pages a first run freed.
func heldMemory() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapInuse + m.StackInuse)
}

// TestMeasureClient is TestMemoryPerConnection's clients, and its
// apiserver: run by it, it opens HANGAR_MEASURE_N connections of
// HANGAR_MEASURE_KIND, says ready once they hold what they are to, and
// keeps them until its stdin closes.
func TestMeasureClient(t *testing.T) {
	addr := os.Getenv("HANGAR_MEASURE_CLIENT")
	if addr == "" {
		t.Skip("run by TestMemoryPerConnection")
	}
	kind := os.Getenv("HANGAR_MEASURE_KIND")
	n, _ := strconv.Atoi(os.Getenv("HANGAR_MEASURE_N"))
	pemBytes, err := os.ReadFile("testdata/door.pem")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pemBytes)
	quit := make(chan struct{})

	// The apiserver: it holds every request until the end, counting them.
	var arrived atomic.Int64
	if strings.HasSuffix(kind, "-proxied") {
		ln, err := net.Listen("unix", os.Getenv("HANGAR_MEASURE_API"))
		if err != nil {
			t.Fatal(err)
		}
		api := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			arrived.Add(1)
			<-quit
		})}
		go func() { _ = api.Serve(ln) }()
		defer func() { _ = api.Close() }()
	}

	host := webHost
	if strings.HasSuffix(kind, "-proxied") {
		host = apiHost
	}
	tc := &tls.Config{RootCAs: roots, ServerName: host}
	protocols := &http.Protocols{}
	protocols.SetHTTP1(!strings.HasPrefix(kind, "h2") && kind != "http2")
	protocols.SetHTTP2(strings.HasPrefix(kind, "h2") || kind == "http2")
	tr := &http.Transport{
		TLSClientConfig: tc, Protocols: protocols,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	var sent atomic.Int64
	var keep []io.Closer
	var mu sync.Mutex
	held := func(c io.Closer) {
		mu.Lock()
		defer mu.Unlock()
		keep = append(keep, c)
	}
	for i := range n {
		switch kind {
		case "tls", "h1-header", "h1-unread":
			c, err := tls.Dial("tcp", addr, tc)
			if err != nil {
				t.Fatalf("connection %d: %v", i, err)
			}
			held(c)
			switch kind {
			case "h1-header":
				// A header of 60 KiB, never ended.
				if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+host+"\r\nX-Big: "+strings.Repeat("a", 60<<10)); err != nil {
					t.Fatal(err)
				}
			case "h1-unread":
				go func() {
					_, _ = io.WriteString(c, "POST /hold HTTP/1.1\r\nHost: "+host+"\r\nContent-Length: 4194304\r\n\r\n")
					chunk := make([]byte, 16<<10)
					for range 256 {
						if _, err := c.Write(chunk); err != nil {
							return
						}
						sent.Add(int64(len(chunk)))
					}
				}()
			}
		default:
			cc, err := tr.NewClientConn(context.Background(), "https", host+":443")
			if err != nil {
				t.Fatalf("connection %d: %v", i, err)
			}
			held(cc)
			switch kind {
			case "http1", "http2":
				req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
				resp, err := cc.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			case "h2-unread":
				go func() {
					req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/hold", &counting{n: 4 << 20, sent: &sent})
					if resp, err := cc.RoundTrip(req); err == nil {
						_ = resp.Body.Close()
					}
				}()
			case "h1-proxied", "h2-proxied":
				streams := 1
				if kind == "h2-proxied" {
					streams = measureStreams
				}
				for range streams {
					go func() {
						req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/hold", nil)
						if resp, err := cc.RoundTrip(req); err == nil {
							_ = resp.Body.Close()
						}
					}()
				}
			}
		}
	}
	switch kind {
	case "h1-unread", "h2-unread":
		// Ready once what the clients send has stopped for a while: the
		// door holds all it takes ahead of its handlers.
		for last := int64(-1); ; {
			time.Sleep(2 * time.Second) //nolint:forbidigo // real clients writing to a real door
			if now := sent.Load(); now == last {
				break
			} else {
				last = now
			}
		}
	case "h1-proxied", "h2-proxied":
		want := int64(n)
		if kind == "h2-proxied" {
			want *= measureStreams
		}
		for arrived.Load() < want {
			time.Sleep(100 * time.Millisecond) //nolint:forbidigo // real requests reaching a real apiserver
		}
	case "h1-header":
		time.Sleep(2 * time.Second) //nolint:forbidigo // the door reading what real clients sent
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	close(quit)
	mu.Lock()
	defer mu.Unlock()
	for _, c := range keep {
		_ = c.Close()
	}
}

// counting is a body of n zero bytes that adds what is read of it to sent.
type counting struct {
	n    int
	sent *atomic.Int64
}

func (c *counting) Read(p []byte) (int, error) {
	if c.n == 0 {
		return 0, io.EOF
	}
	k := min(len(p), c.n)
	clear(p[:k])
	c.n -= k
	c.sent.Add(int64(k))
	return k, nil
}
