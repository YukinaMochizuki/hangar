package front_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/edge/front"
)

// pipeDoor is a front door on net.Pipe connections, each from client, so
// a synctest bubble can hold it, as it cannot a socket. With TLS it
// serves testdata/door.pem, a self-signed certificate for the door's
// names, valid from 1990 to 2100 as a bubble's clock starts in 2000.
type pipeDoor struct {
	https, http *pipeListener
	log         *syncBuffer
	roots       *x509.CertPool
}

// pipeOptions change what a pipeDoor is started with.
type pipeOptions struct {
	limits front.Limits
	// prepare, if set, is given the front door before it serves.
	prepare func(*front.Front)
	// api, if set, is the apiserver, served on pipes too.
	api       http.Handler
	endpoints http.Handler
	tls       bool
}

// client is the address every connection to a pipeDoor comes from.
var client = net.IPv4(192, 0, 2, 1)

// startPipeDoor runs a pipeDoor without TLS or an apiserver, with limits
// l, until the test ends; prepare, if set, is given the front door before
// it serves.
func startPipeDoor(t *testing.T, l front.Limits, prepare func(*front.Front)) *pipeDoor {
	t.Helper()
	return startPipeDoorWith(t, pipeOptions{limits: l, prepare: prepare})
}

// startPipeDoorWith runs a pipeDoor with o until the test ends.
func startPipeDoorWith(t *testing.T, o pipeOptions) *pipeDoor {
	t.Helper()
	d := &pipeDoor{https: newPipeListener(), http: newPipeListener(), log: &syncBuffer{}}
	dialAPI := func(context.Context) (net.Conn, error) { return nil, errors.New("no apiserver") }
	if o.api != nil {
		apiL := newPipeListener()
		api := &http.Server{Handler: o.api}
		go func() { _ = api.Serve(apiL) }()
		t.Cleanup(func() {
			_ = api.Close()
			apiL.closeClients()
		})
		dialAPI = func(context.Context) (net.Conn, error) { return apiL.dialConn() }
	}
	if o.endpoints == nil {
		o.endpoints = http.NotFoundHandler()
	}
	f, err := front.New(front.Config{
		CPDomain: cpDomain, UserDomain: userDomain,
		DialAPI: dialAPI, Endpoints: o.endpoints, Limits: o.limits,
		Log: slog.New(slog.NewTextHandler(d.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.prepare != nil {
		o.prepare(f)
	}
	tc := &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return nil, errors.New("no certificate")
	}}
	if o.tls {
		pemBytes, err := os.ReadFile("testdata/door.pem")
		if err != nil {
			t.Fatal(err)
		}
		cert, err := tls.X509KeyPair(pemBytes, pemBytes)
		if err != nil {
			t.Fatal(err)
		}
		tc = &tls.Config{Certificates: []tls.Certificate{cert}}
		d.roots = x509.NewCertPool()
		d.roots.AppendCertsFromPEM(pemBytes)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Serve(ctx, front.Listeners{HTTPS: d.https, HTTP: d.http}, tc) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	// Before the door stops, so it waits on none of them.
	t.Cleanup(func() {
		d.https.closeClients()
		d.http.closeClients()
	})
	return d
}

// tlsConfig is a client's for serverName, trusting the door's certificate.
func (d *pipeDoor) tlsConfig(serverName string, protos ...string) *tls.Config {
	return &tls.Config{RootCAs: d.roots, ServerName: serverName, NextProtos: protos}
}

// handshake opens a TLS connection to the door for serverName, offering
// protos.
func (d *pipeDoor) handshake(t *testing.T, serverName string, protos ...string) *tls.Conn {
	t.Helper()
	c := tls.Client(d.https.dial(t), d.tlsConfig(serverName, protos...))
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	return c
}

// newClient is an HTTP client of the door, over HTTP/2 or HTTP/1.1.
func (d *pipeDoor) newClient(t *testing.T, h2 bool) *http.Client {
	tr := &http.Transport{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			if strings.HasSuffix(addr, ":80") {
				return d.http.dialConn()
			}
			return d.https.dialConn()
		},
		TLSClientConfig:   &tls.Config{RootCAs: d.roots},
		ForceAttemptHTTP2: h2,
	}
	if !h2 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// pipeListener accepts the server ends of the net.Pipe connections dial
// makes.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
	port   atomic.Int32

	mu      sync.Mutex
	clients []net.Conn
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 100), Port: 443} }

// dial connects to l from client, until the test ends.
func (l *pipeListener) dial(t *testing.T) net.Conn {
	t.Helper()
	c, err := l.dialConn()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// dialConn is dial for a goroutine other than the test's; closeClients
// closes what it opened.
func (l *pipeListener) dialConn() (net.Conn, error) {
	c, s := net.Pipe()
	l.mu.Lock()
	l.clients = append(l.clients, c)
	l.mu.Unlock()
	select {
	case l.conns <- &fromClient{Conn: s, remote: &net.TCPAddr{IP: client, Port: 40000 + int(l.port.Add(1))}}:
		return c, nil
	case <-l.closed:
		_ = c.Close()
		return nil, errors.New("the front door stopped listening")
	}
}

// closeClients closes the client end of every connection dialed.
func (l *pipeListener) closeClients() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.clients {
		_ = c.Close()
	}
}

// fromClient is a connection's server end, from its remote address.
type fromClient struct {
	net.Conn
	remote net.Addr
}

func (c *fromClient) RemoteAddr() net.Addr { return c.remote }
