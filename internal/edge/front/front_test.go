package front_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/edge/front"
	"github.com/YukinaMochizuki/hangar/internal/edge/front/acmetest"
)

// The test's domains: the control plane's and the endpoints'.
const (
	cpDomain   = "cp.test"
	userDomain = "user.test"
)

var (
	apiHost     = "api." + cpDomain
	consoleHost = "console." + cpDomain
	webHost     = "web-demo." + userDomain
)

// syncBuffer is a log the front door writes while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testCA is an ACME CA for the test's domains, and the DNS server its DNS-01
// challenges are answered in.
type testCA struct {
	dns    *acmetest.Server
	pebble *pebble
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	dns, err := acmetest.Start("127.0.0.1:0", cpDomain, userDomain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dns.Close)
	return &testCA{dns: dns, pebble: startPebble(t, dns.Addr)}
}

// certs keeps the front door's certificates in storage until the test ends.
func (c *testCA) certs(t *testing.T, storage string, log *slog.Logger) *tls.Config {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tc, err := front.Certificates(ctx, front.CertConfig{
		Storage: storage, CA: c.pebble.Directory, CARoots: c.pebble.DirectoryRoots,
		DNS: c.dns, Resolvers: []string{c.dns.Addr},
		Names: []string{apiHost, consoleHost, "*." + userDomain}, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tc
}

// door is a front door the test runs.
type door struct {
	https, http string
	// client speaks HTTP/2 to the front door, client1 HTTP/1.1; both
	// connect to it whatever the URL's host, and trust the test CA.
	client, client1 *http.Client
	roots           *testCA
	log             *syncBuffer
}

// options change what a test door is started with.
type options struct {
	limits     front.Limits
	consoleDir string
	api        http.Handler
	endpoints  http.Handler
	storage    string
	// prepare, if set, is given the front door before it serves.
	prepare func(*front.Front)
	// apiConnContext, if set, is the fake apiserver socket's ConnContext.
	apiConnContext func(context.Context, net.Conn) context.Context
}

func defaultOptions() options {
	l := front.DefaultLimits
	// The tests open many connections from one address.
	l.ConnRate, l.ConnBurst, l.RequestRate, l.RequestBurst = 1000, 1000, 1000, 1000
	return options{limits: l, api: echo("api"), endpoints: echo("endpoints")}
}

// startDoor runs a front door with certificates from c until the test ends.
func startDoor(t *testing.T, c *testCA, o options) *door {
	t.Helper()
	dir := t.TempDir()
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("front door log:\n%s", logs.String())
		}
	})

	// Without an api handler, nothing listens on the socket.
	sock := filepath.Join(dir, "public.sock")
	if o.api != nil {
		apiLn, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		api := &http.Server{Handler: o.api, ConnContext: o.apiConnContext}
		go func() { _ = api.Serve(apiLn) }()
		t.Cleanup(func() { _ = api.Close() })
	}

	f, err := front.New(front.Config{
		CPDomain: cpDomain, UserDomain: userDomain,
		DialAPI:   func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "unix", sock) },
		Endpoints: o.endpoints, ConsoleDir: o.consoleDir, Limits: o.limits, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.prepare != nil {
		o.prepare(f)
	}
	storage := o.storage
	if storage == "" {
		storage = filepath.Join(dir, "certs")
	}
	tc := c.certs(t, storage, log)
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
	go func() { done <- f.Serve(ctx, front.Listeners{HTTPS: https, HTTP: plain}, tc) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})

	d := &door{https: https.Addr().String(), http: plain.Addr().String(), roots: c, log: logs}
	d.client = d.newClient(true)
	d.client1 = d.newClient(false)
	d.waitCertificates(t, apiHost, consoleHost, webHost)
	return d
}

func (d *door) tlsConfig(serverName string) *tls.Config {
	return &tls.Config{RootCAs: d.roots.pebble.Roots, ServerName: serverName}
}

func (d *door) newClient(h2 bool) *http.Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasSuffix(addr, ":80") {
				return (&net.Dialer{}).DialContext(ctx, network, d.http)
			}
			return (&net.Dialer{}).DialContext(ctx, network, d.https)
		},
		TLSClientConfig:   &tls.Config{RootCAs: d.roots.pebble.Roots},
		ForceAttemptHTTP2: h2,
	}
	if !h2 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// handshake opens a TLS connection to the front door for serverName.
func (d *door) handshake(serverName string) (*tls.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", d.https, d.tlsConfig(serverName))
}

// waitCertificates waits until the front door hands out a certificate the
// test CA issued for each name.
func (d *door) waitCertificates(t *testing.T, names ...string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for _, n := range names {
		for {
			c, err := d.handshake(n)
			if err == nil {
				_ = c.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no certificate for %s: %v", n, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// do sends a request and returns its status, body and header.
func do(t *testing.T, c *http.Client, method, url string, body io.Reader, h http.Header) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// echo answers with who it is and what reached it.
func echo(who string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, "too large", http.StatusRequestEntityTooLarge)
				return
			}
		}
		h := r.Header
		_, _ = io.WriteString(w, strings.Join([]string{
			who, r.Method, r.Host, r.URL.RequestURI(),
			"client-ip=" + h.Get("Hangar-Client-Ip"),
			"xff=" + strings.Join(h.Values("X-Forwarded-For"), ","),
			"proto=" + h.Get("X-Forwarded-Proto"),
			"forwarded=" + h.Get("Forwarded"),
			"real-ip=" + h.Get("X-Real-Ip"),
			"body=" + strconv.FormatInt(n, 10),
		}, " "))
	})
}
