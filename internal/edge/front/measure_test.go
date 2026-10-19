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
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/edge/front"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// measureConns is how many connections TestMemoryPerConnection opens of
// each kind.
const measureConns = 2000

// TestMemoryPerConnection measures what the edge holds a connection, its
// clients in a process of their own; HANGAR_MEASURE_CONNS=1 runs it, the
// numbers are in docs/decisions/front-door.md.
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
	l.Conns, l.ConnsPerClient, l.ConnsPer24 = 1<<20, 1<<20, 1<<20
	l.ConnRate, l.ConnBurst, l.RequestRate, l.RequestBurst = 1e6, 1<<20, 1e6, 1<<20
	l.ReadHeaderTimeout, l.IdleTimeout, l.HTTP2SendPingTimeout = time.Hour, time.Hour, time.Hour
	f, err := front.New(front.Config{
		CPDomain: cpDomain, UserDomain: userDomain,
		DialAPI: func(context.Context) (net.Conn, error) { return nil, io.EOF },
		Endpoints: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		}),
		Limits: l, Log: slog.New(slog.DiscardHandler),
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

	for _, kind := range []string{"tls", "http1", "http2"} {
		before := heldMemory()
		cmd := exec.Command(os.Args[0], "-test.run=^TestMeasureClient$")
		cmd.Env = append(os.Environ(), "HANGAR_MEASURE_CLIENT="+https.Addr().String(), "HANGAR_MEASURE_KIND="+kind,
			"HANGAR_MEASURE_N="+strconv.Itoa(measureConns))
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
			return n == measureConns, strconv.Itoa(n)
		})
		after := heldMemory()
		t.Logf("%s: heap and stacks %.1f KiB a connection", kind, float64(after-before)/measureConns/1024)
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

// TestMeasureClient is TestMemoryPerConnection's clients: run by it, it
// opens HANGAR_MEASURE_N connections of HANGAR_MEASURE_KIND, says ready,
// and holds them until its stdin closes.
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
	tc := &tls.Config{RootCAs: roots, ServerName: webHost}
	protocols := &http.Protocols{}
	protocols.SetHTTP1(kind == "http1")
	protocols.SetHTTP2(kind == "http2")
	tr := &http.Transport{
		TLSClientConfig: tc, Protocols: protocols,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	var keep []io.Closer
	for i := range n {
		if kind == "tls" {
			c, err := tls.Dial("tcp", addr, tc)
			if err != nil {
				t.Fatalf("connection %d: %v", i, err)
			}
			keep = append(keep, c)
			continue
		}
		cc, err := tr.NewClientConn(context.Background(), "https", webHost+":443")
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		keep = append(keep, cc)
		req, _ := http.NewRequest(http.MethodGet, "https://"+webHost+"/", nil)
		resp, err := cc.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.ProtoMajor != map[string]int{"http1": 1, "http2": 2}[kind] {
			t.Fatalf("%s answered over %s", kind, resp.Proto)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	for _, c := range keep {
		_ = c.Close()
	}
}
