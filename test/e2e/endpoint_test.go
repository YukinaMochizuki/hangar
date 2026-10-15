package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare/cftest"
	"github.com/YukinaMochizuki/hangar/internal/edge"
	"github.com/YukinaMochizuki/hangar/internal/edge/accesstest"
)

const endpointDomain = "hangarlab.test"

// edgeWorld is a world with Cloudflare's fake behind the apiserver, a node
// agent taking dials, and hangar-edge in front, trusting issuer's tokens.
type edgeWorld struct {
	*world
	issuer *accesstest.Issuer
	// url is the edge's listener, which a request reaches with any Host.
	url string
}

func newEdgeWorld(t *testing.T) *edgeWorld {
	t.Helper()
	const token, account = "cf-token", "0123456789abcdef0123456789abcdef"
	f := cftest.New(token, account, endpointDomain)
	t.Cleanup(f.Close)
	cf, err := cloudflare.New(token, cloudflare.WithBaseURL(f.URL()), cloudflare.WithRetry(1, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	bypass, err := cf.CreateAccessPolicy(t.Context(), account, cloudflare.AccessPolicy{
		Name: "bypass", Decision: "bypass", Include: []json.RawMessage{cloudflare.EveryoneRule()},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := newIdleWorldWith(t, func(c *apiserver.Config) {
		c.PublicEndpoints = apiserver.PublicEndpointsConfig{Domain: endpointDomain, Cloudflare: cf, AccountID: account, BypassPolicyID: bypass.ID}
	})
	dir, err := os.MkdirTemp("", "edge")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "dial.sock")
	if w.edgeDial, err = net.Listen("unix", sock); err != nil {
		t.Fatal(err)
	}
	w.startAgent()

	iss := accesstest.NewIssuer(t)
	access, err := edge.NewAccessVerifier(t.Context(), iss.Config())
	if err != nil {
		t.Fatal(err)
	}
	e := edge.New(edge.Config{
		API: w.env.AsEdge(),
		DialNode: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		Domain: endpointDomain, Access: access, Retry: 20 * time.Millisecond,
		Log: slog.New(slog.DiscardHandler),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	srv := httptest.NewServer(e)
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-done
	})
	return &edgeWorld{world: w, issuer: iss, url: srv.URL}
}

// guestHTTP serves HTTP on the container's eth0 at port, as a guest's web
// server would: "/" says who answered and what reached it, "/stream" sends
// a line, waits for next, and sends another, and "/upgrade" switches to an
// echo of the raw connection.
func (w *edgeWorld) guestHTTP(container string, port uint16, who string, next <-chan struct{}) {
	w.t.Helper()
	m := w.machine(container)
	addr := netip.MustParsePrefix(m.GetSpec().GetInterfaces()[0].GetIpv4()).Addr()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		cookies := []string{}
		for _, c := range r.Cookies() {
			cookies = append(cookies, c.Name)
		}
		_, _ = fmt.Fprintf(rw, "%s host=%s jwt=%q cookies=%s proto=%s", who, r.Host, r.Header.Get(edge.AccessJWTHeader),
			strings.Join(cookies, ","), r.Header.Get("X-Forwarded-Proto"))
	})
	mux.HandleFunc("/stream", func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(rw, "first\n")
		rw.(http.Flusher).Flush()
		<-next
		_, _ = io.WriteString(rw, "second\n")
	})
	mux.HandleFunc("/upgrade", func(rw http.ResponseWriter, _ *http.Request) {
		c, brw, err := http.NewResponseController(rw).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = brw.Flush()
		_, _ = io.Copy(c, brw)
	})
	w.host.ServeGuest(w.networkUID("default"), netip.AddrPortFrom(addr, port), func(c net.Conn) {
		_ = http.Serve(&oneConn{c: c}, mux)
	})
}

// oneConn is a listener of one connection.
type oneConn struct {
	c    net.Conn
	once sync.Once
	done chan struct{}
	mu   sync.Mutex
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.done == nil {
		l.done = make(chan struct{})
	}
	l.mu.Unlock()
	var c net.Conn
	l.once.Do(func() { c = l.c })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done == nil {
		l.done = make(chan struct{})
	}
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *oneConn) Addr() net.Addr { return l.c.LocalAddr() }

// get asks the edge for path on host, with the token if set, and returns
// the status and body.
func (w *edgeWorld) get(host, path, token string, cookies ...*http.Cookie) (int, string) {
	w.t.Helper()
	req, err := http.NewRequestWithContext(w.t.Context(), http.MethodGet, w.url+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Host = host
	if token != "" {
		req.Header.Set(edge.AccessJWTHeader, token)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// waitServing waits until the edge answers host's path with status.
func (w *edgeWorld) waitServing(host, token string, status int) string {
	w.t.Helper()
	var body string
	w.eventually(fmt.Sprintf("%s answering %d", host, status), func() (bool, string) {
		var got int
		got, body = w.get(host, "/", token)
		return got == status, fmt.Sprintf("%d %s", got, body)
	})
	return body
}

func (w *edgeWorld) endpointReady(name string) *networkv1alpha1.PublicEndpoint {
	w.t.Helper()
	var ep *networkv1alpha1.PublicEndpoint
	w.eventually("endpoint "+name+" ready", func() (bool, string) {
		var ok bool
		ep, ok = get[*networkv1alpha1.PublicEndpoint](w.world, registry.PublicEndpoint, name)
		return ok && conditions.IsTrue(ep.GetStatus().GetConditions(), apiserver.PublicEndpointReady), fmt.Sprintf("%v", ep.GetStatus())
	})
	return ep
}

// TestPublicEndpointsThroughTheEdge: a container's public port and an
// endpoint made by hand are reached through the edge by Host, private ones
// only by whom their tenant allows, with Access's proof of sign-in left
// behind, streams and upgrades going through, and nothing once the
// endpoint is gone.
func TestPublicEndpointsThroughTheEdge(t *testing.T) {
	w := newEdgeWorld(t)
	c := container("api", "nginx")
	c.Spec.Ports = []*computev1alpha1.ContainerPort{{Name: "http", Port: 8080, Public: true}}
	w.apply(c)
	w.waitReason("api", "Running")
	next := make(chan struct{})
	w.guestHTTP("api", 8080, "api:8080", next)
	w.guestHTTP("api", 8081, "api:8081", next)
	ep := w.endpointReady("api-http")
	privateHost := "api-http-web." + endpointDomain
	if ep.GetStatus().GetUrl() != "https://"+privateHost || ep.GetSpec().GetAccess() != networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE {
		t.Fatalf("the container's endpoint: %v", ep)
	}

	// Only its port is open to the edge, on that interface alone.
	m := w.machine("api")
	ports := w.host.EdgePorts(w.networkUID("default"))
	if got := ports[m.GetMetadata().GetUid()+"/eth0"]; !slices.Equal(got, []uint16{8080}) {
		t.Errorf("the edge's ports: %v", ports)
	}

	// Private: nobody without a sign-in, nobody outside the tenant.
	rootToken := w.issuer.UserToken(t, apiservertest.Root)
	w.waitServing(privateHost, rootToken, http.StatusOK)
	if status, _ := w.get(privateHost, "/", ""); status != http.StatusUnauthorized {
		t.Errorf("a private endpoint without a token: %d", status)
	}
	if status, _ := w.get(privateHost, "/", w.issuer.UserToken(t, "mallory@example.com")); status != http.StatusForbidden {
		t.Errorf("a private endpoint by a stranger: %d", status)
	}
	// A member of the platform with no grant in the tenant is refused, and
	// one given the tenant is let in.
	w.env.AddMember("bob@example.com", "platform.member")
	bob := w.issuer.UserToken(t, "bob@example.com")
	w.waitServing(privateHost, bob, http.StatusForbidden)
	if _, err := w.root.Apply(t.Context(), registry.RoleBinding, roleBinding("bob-owner", "bob@example.com", "tenant.owner",
		&iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: apiservertest.Tenant}})); err != nil {
		t.Fatal(err)
	}
	w.waitServing(privateHost, bob, http.StatusOK)
	// What proves the sign-in never reaches the guest; other cookies do.
	_, body := w.get(privateHost, "/", rootToken, &http.Cookie{Name: "CF_Authorization", Value: rootToken}, &http.Cookie{Name: "theme", Value: "dark"})
	if want := "api:8080 host=" + privateHost + ` jwt="" cookies=theme proto=https`; body != want {
		t.Errorf("the guest saw %q, want %q", body, want)
	}

	// A public endpoint by hand: anyone, no sign-in.
	publicEP := &networkv1alpha1.PublicEndpoint{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "site"},
		Spec: &networkv1alpha1.PublicEndpointSpec{
			Target: &networkv1alpha1.PublicEndpointTarget{Object: &networkv1alpha1.PublicEndpointTarget_Container{Container: "api"}},
			Port:   8081, Access: networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC,
		},
	}
	if _, err := w.root.Apply(t.Context(), registry.PublicEndpoint, publicEP); err != nil {
		t.Fatal(err)
	}
	w.endpointReady("site")
	publicHost := "site-web." + endpointDomain
	if body := w.waitServing(publicHost, "", http.StatusOK); !strings.HasPrefix(body, "api:8081 ") {
		t.Errorf("the public endpoint reached %q", body)
	}

	// A stream reaches the client as it is written.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, w.url+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = publicHost
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("the stream's first line: %q, %v", line, err)
	}
	close(next)
	if line, err := br.ReadString('\n'); err != nil || line != "second\n" {
		t.Errorf("the stream's second line: %q, %v", line, err)
	}
	_ = resp.Body.Close()

	// An upgrade becomes the guest's raw connection.
	up := w.upgrade(publicHost)
	if _, err := up.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, up, 4); got != "ping" {
		t.Errorf("the upgraded connection echoed %q", got)
	}

	// Deleted, the endpoint closes what it let through, answers nothing
	// more, and its port is closed to the edge.
	if _, err := w.root.Delete(t.Context(), registry.PublicEndpoint, client.Key{Project: project, Name: "site"}); err != nil {
		t.Fatal(err)
	}
	_ = up.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := up.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("an upgraded connection outlived its endpoint: %v", err)
	}
	w.waitServing(publicHost, "", http.StatusNotFound)
	w.eventually("the deleted endpoint's port closed", func() (bool, string) {
		got := w.host.EdgePorts(w.networkUID("default"))[m.GetMetadata().GetUid()+"/eth0"]
		return slices.Equal(got, []uint16{8080}), fmt.Sprintf("%v", got)
	})

	// Neither a host of no endpoint nor one outside the domain is served.
	for _, host := range []string{"nothing-web." + endpointDomain, "api-http-web.example.com", "a.api-http-web." + endpointDomain} {
		if status, _ := w.get(host, "/", rootToken); status != http.StatusNotFound {
			t.Errorf("%s: %d", host, status)
		}
	}

	// The container's port no longer public: its endpoint goes, and so does
	// the opening.
	c.Spec.Ports[0].Public = false
	w.apply(c)
	w.waitServing(privateHost, rootToken, http.StatusNotFound)
	w.eventually("every port closed", func() (bool, string) {
		got := w.host.EdgePorts(w.networkUID("default"))
		return len(got) == 0, fmt.Sprintf("%v", got)
	})
}

// upgrade opens an upgraded connection to host's /upgrade through the edge.
func (w *edgeWorld) upgrade(host string) net.Conn {
	w.t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(w.url, "http://"))
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(c, "GET /upgrade HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n", host); err != nil {
		w.t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		w.t.Fatalf("upgrading: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		w.t.Fatalf("upgrading: %v", resp.Status)
	}
	_ = c.SetDeadline(time.Time{})
	return c
}

func readN(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func roleBinding(name, user, role string, scope *iamv1alpha1.BindingScope) *iamv1alpha1.RoleBinding {
	return &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: user}},
			Role:    role, Scope: scope,
		},
	}
}

// TestAPortChangeThroughTheEdge: a private endpoint moved from 8080 to
// 8081 is reached on 8081 by the next request once the node has opened the
// new port, however warm the edge's connections to 8080 were.
func TestAPortChangeThroughTheEdge(t *testing.T) {
	w := newEdgeWorld(t)
	c := container("api", "nginx")
	c.Spec.Ports = []*computev1alpha1.ContainerPort{{Name: "http", Port: 8080, Public: true}}
	w.apply(c)
	w.waitReason("api", "Running")
	w.guestHTTP("api", 8080, "api:8080", nil)
	w.guestHTTP("api", 8081, "api:8081", nil)
	w.endpointReady("api-http")
	privateHost := "api-http-web." + endpointDomain
	token := w.issuer.UserToken(t, apiservertest.Root)
	if body := w.waitServing(privateHost, token, http.StatusOK); !strings.HasPrefix(body, "api:8080 ") {
		t.Fatalf("before the change: %q", body)
	}
	for range 3 {
		w.get(privateHost, "/", token)
	}

	c.Spec.Ports[0].Port = 8081
	w.apply(c)
	uid := w.machine("api").GetMetadata().GetUid()
	w.eventually("the node to open 8081 alone", func() (bool, string) {
		got := w.host.EdgePorts(w.networkUID("default"))[uid+"/eth0"]
		return slices.Equal(got, []uint16{8081}), fmt.Sprintf("%v", got)
	})
	w.eventually("the edge to see the change", func() (bool, string) {
		ep, _ := get[*networkv1alpha1.PublicEndpoint](w.world, registry.PublicEndpoint, "api-http")
		return ep.GetSpec().GetPort() == 8081, ""
	})
	for i := range 5 {
		_, body := w.waitServingAny(privateHost, token)
		if !strings.HasPrefix(body, "api:8081 ") {
			t.Fatalf("request %d after the change reached %q", i, body)
		}
	}
}

// waitServingAny asks host until the edge answers it with anything but a
// 503 for an endpoint it is still taking in, and returns that answer.
func (w *edgeWorld) waitServingAny(host, token string) (int, string) {
	w.t.Helper()
	var status int
	var body string
	w.eventually(host+" answering", func() (bool, string) {
		status, body = w.get(host, "/", token)
		return status != http.StatusServiceUnavailable, fmt.Sprintf("%d %s", status, body)
	})
	return status, body
}
