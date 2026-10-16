package edge_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1/networkv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/edge"
	"github.com/YukinaMochizuki/hangar/internal/edgedial"
)

const domain = "hangarlab.test"

// consoleURL is the console the edge hands browsers off to.
const consoleURL = "https://console.cp.test"

// world is an apiserver, hangar-edge in front of it, and in place of a node
// a dial server reaching, for an endpoint, the backend serving its port as
// the API has it now. sessions issues credentials into the apiserver's
// store, as signing in would.
type world struct {
	t        *testing.T
	env      *apiservertest.Env
	root     *client.Client
	sessions *session.Manager
	url      string
	edge     *edge.Edge
	// held hears of each request that reaches a backend's "/hold".
	held chan struct{}
}

// backend serves, on port, "/" saying the port and what reached it of the
// credentials a caller may bring, "/read" reading the whole body first,
// "/set-cookie" setting cookies, Hangar's among them, "/hold" answering
// nothing (and saying on held that a request reached it), and "/upgrade"
// switching to an echo of the raw connection. Any other path answers as "/".
func backend(t *testing.T, port int32, held chan<- struct{}) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var hangar []string
		for k := range r.Header {
			if strings.HasPrefix(k, "Hangar-") || strings.HasPrefix(k, "Cf-") {
				hangar = append(hangar, k)
			}
		}
		slices.Sort(hangar)
		_, _ = fmt.Fprintf(w, "port %d path=%q headers=%q authorization=%q cookie=%q", port, r.URL.Path, hangar,
			r.Header.Values("Authorization"), strings.Join(r.Header.Values("Cookie"), " | "))
	})
	mux.HandleFunc("/set-cookie", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "theme=dark")
		w.Header().Add("Set-Cookie", edge.EndpointCookie+"=planted; Secure; Path=/")
		w.Header().Add("Set-Cookie", "__HOST-Hangar_session=planted; Secure; Path=/")
		_, _ = fmt.Fprintf(w, "port %d set cookies", port)
	})
	mux.HandleFunc("/hold", func(_ http.ResponseWriter, r *http.Request) {
		select {
		case held <- struct{}{}:
		default:
		}
		// Answers nothing until the request ends.
		<-r.Context().Done()
	})
	mux.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		_, _ = fmt.Fprintf(w, "port %d read %d", port, n)
	})
	mux.HandleFunc("/upgrade", func(w http.ResponseWriter, _ *http.Request) {
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = brw.Flush()
		_, _ = io.Copy(c, brw)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return newWorldOn(t, apiservertest.Start(t, nil))
}

// newWorldOn is newWorld with the apiserver env.
func newWorldOn(t *testing.T, env *apiservertest.Env) *world {
	t.Helper()
	return newWorldLogging(t, env, slog.New(slog.DiscardHandler))
}

// newWorldLogging is newWorldOn with the edge logging to log.
func newWorldLogging(t *testing.T, env *apiservertest.Env, log *slog.Logger) *world {
	t.Helper()
	return newWorldConfig(t, env, func(c *edge.Config) { c.Log = log })
}

// newWorldConfig is newWorldOn with configure adjusting the edge's config,
// whose Recheck is 100ms unless it says otherwise.
func newWorldConfig(t *testing.T, env *apiservertest.Env, configure func(*edge.Config)) *world {
	t.Helper()
	env.Setup()
	root := env.As(apiservertest.Root)
	held := make(chan struct{}, 1)
	backends := map[int32]string{8080: backend(t, 8080, held), 8081: backend(t, 8081, held)}

	dir, err := os.MkdirTemp("", "edge")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "dial.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	node := &edgedial.Server{UIDs: []uint32{uint32(os.Getuid())}, Log: slog.New(slog.DiscardHandler),
		Open: func(ctx context.Context, tg edgedial.Target) (net.Conn, error) {
			obj, err := root.Get(ctx, registry.PublicEndpoint, client.Key{Project: tg.Project, Name: tg.Name})
			if err != nil {
				return nil, edgedial.Refuse(http.StatusNotFound, "%v", err)
			}
			addr, ok := backends[obj.(*networkv1alpha1.PublicEndpoint).GetSpec().GetPort()]
			if !ok {
				return nil, edgedial.Refuse(http.StatusBadGateway, "nothing serves that port")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- node.Serve(ctx, ln) }()

	cfg := edge.Config{
		API:  env.AsEdge(),
		Auth: networkv1alpha1connect.NewEndpointAuthServiceClient(client.UnixSocket(env.EdgeSocket), client.UnixSocketURL),
		DialNode: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		Domain: domain, ConsoleURL: consoleURL, Recheck: 100 * time.Millisecond,
		Retry: 20 * time.Millisecond, Log: slog.New(slog.DiscardHandler),
	}
	configure(&cfg)
	e := edge.New(cfg)
	ran := make(chan error, 1)
	go func() { ran <- e.Run(ctx) }()
	srv := httptest.NewServer(e)
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-ran
		<-served
	})
	return &world{t: t, env: env, root: root, sessions: session.New(env.Credentials, session.Options{}), url: srv.URL, edge: e, held: held}
}

// consoleSession signs email in on a browser, as the console's sign-in
// does, and returns the session found, its login ending at end, or the
// usual end if end is zero.
func (w *world) consoleSession(email string, end time.Time) (token.Secret, session.Found) {
	w.t.Helper()
	ctx := context.Background()
	b, err := w.sessions.StartBrowser(ctx, auth.Principal{Type: auth.User, ID: email})
	if err != nil {
		w.t.Fatal(err)
	}
	f, err := w.sessions.Check(ctx, b.Secret, session.TypeSession, "")
	if err != nil {
		w.t.Fatal(err)
	}
	if !end.IsZero() {
		f.LoginEnd = end
	}
	return b.Secret, f
}

// endpointSession is a session of email on the endpoint at h, as a handoff
// makes it, ending at end if that is not zero.
func (w *world) endpointSession(email, h string, end time.Time) token.Secret {
	w.t.Helper()
	_, f := w.consoleSession(email, end)
	b, err := w.sessions.StartEndpoint(context.Background(), f, h)
	if err != nil {
		w.t.Fatal(err)
	}
	return b.Secret
}

// cookie is a request's endpoint session cookie.
func cookie(s token.Secret) http.Header {
	return http.Header{"Cookie": {edge.EndpointCookie + "=" + s.Reveal()}}
}

// bearer is a request's bearer token.
func bearer(s token.Secret) http.Header {
	return http.Header{"Authorization": {"Bearer " + s.Reveal()}}
}

// rootCookie is a session of the root on host.
func (w *world) rootCookie() http.Header {
	return cookie(w.endpointSession(apiservertest.Root, host, time.Time{}))
}

// endpoint applies endpoint site of project web on port with access.
func (w *world) endpoint(port int32, access networkv1alpha1.PublicEndpointAccess) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.PublicEndpoint, &networkv1alpha1.PublicEndpoint{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "site"},
		Spec: &networkv1alpha1.PublicEndpointSpec{
			Target: &networkv1alpha1.PublicEndpointTarget{Object: &networkv1alpha1.PublicEndpointTarget_Container{Container: "api"}},
			Port:   port, Access: access,
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

const host = "site-" + apiservertest.Project + "." + domain

// get asks the edge for "/" of host with header set.
func (w *world) get(header http.Header) (int, string) {
	w.t.Helper()
	resp, body := w.do(http.MethodGet, host, "/", header)
	return resp.StatusCode, body
}

// noRedirects is a client that returns redirects rather than following them.
var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// answer is a response the edge gave, its body read and closed.
type answer struct {
	StatusCode int
	Header     http.Header
}

// Cookies are the cookies the answer sets.
func (a *answer) Cookies() []*http.Cookie { return (&http.Response{Header: a.Header}).Cookies() }

// do sends a request to the edge for path of h with header set, following
// no redirect.
func (w *world) do(method, h, path string, header http.Header) (*answer, string) {
	w.t.Helper()
	req, err := http.NewRequestWithContext(w.t.Context(), method, w.url+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Host = h
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := noRedirects.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return &answer{StatusCode: resp.StatusCode, Header: resp.Header}, string(b)
}

func (w *world) eventually(what string, cond func() (bool, string)) {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ok, last := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("never happened: %s; last: %s", what, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAPortChangeReachesTheNewPort: once the edge sees an endpoint's new
// port, no request reaches the old one, however warm its connections were.
func TestAPortChangeReachesTheNewPort(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE)
	root := w.rootCookie()
	w.eventually("the endpoint on 8080", func() (bool, string) {
		_, body := w.get(root)
		return strings.HasPrefix(body, "port 8080 "), body
	})
	// Connections to 8080 are warm in the edge's pool.
	for range 3 {
		w.get(root)
	}
	w.endpoint(8081, networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE)
	w.eventually("the endpoint on 8081", func() (bool, string) {
		_, body := w.get(root)
		return strings.HasPrefix(body, "port 8081 "), body
	})
	for i := range 10 {
		if _, body := w.get(root); !strings.HasPrefix(body, "port 8081 ") {
			t.Fatalf("request %d after the change reached %q", i, body)
		}
	}
}

// TestAHostWithATrailingDotOrAPortReachesItsEndpoint: the edge matches a
// Host as edge.Hostname has it, so the absolute name and an explicit port
// reach the same endpoint as the plain name.
func TestAHostWithATrailingDotOrAPortReachesItsEndpoint(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC)
	get := func(h string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, w.url+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = h
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	w.eventually("the endpoint", func() (bool, string) {
		code, body := get(host)
		return code == http.StatusOK && strings.HasPrefix(body, "port 8080 "), fmt.Sprint(code, body)
	})
	for _, h := range []string{host + ".", host + ":443", strings.ToUpper(host) + ".:443"} {
		if code, body := get(h); code != http.StatusOK || !strings.HasPrefix(body, "port 8080 ") {
			t.Errorf("Host %s: %d %q; want the endpoint", h, code, body)
		}
	}
}

// TestABodyPastItsCapIsTooLarge: a body the front door caps
// (internal/edge/front) and that runs past the cap on its way to the
// endpoint is answered 413, not as an endpoint that did not answer.
func TestABodyPastItsCapIsTooLarge(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC)
	capped := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(rw, r.Body, 100)
		w.edge.ServeHTTP(rw, r)
	}))
	defer capped.Close()
	post := func(n int) (int, string) {
		// No length, so nothing but reading it finds it too long.
		req, err := http.NewRequest(http.MethodPost, capped.URL+"/read", io.MultiReader(strings.NewReader(strings.Repeat("a", n))))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	w.eventually("the endpoint", func() (bool, string) {
		code, body := post(100)
		return code == http.StatusOK && body == "port 8080 read 100", fmt.Sprint(code, body)
	})
	if code, body := post(100 << 10); code != http.StatusRequestEntityTooLarge {
		t.Errorf("past the cap: %d %q; want 413", code, body)
	}
}

// upgrade opens an upgraded connection to host's "/upgrade" with header,
// checked to echo.
func (w *world) upgrade(header http.Header) net.Conn {
	w.t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(w.url, "http://"))
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	req := "GET /upgrade HTTP/1.1\r\nHost: " + host + "\r\nConnection: Upgrade\r\nUpgrade: echo\r\n"
	for k, vs := range header {
		for _, v := range vs {
			req += k + ": " + v + "\r\n"
		}
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		w.t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		w.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		w.t.Fatalf("upgrading: %s", resp.Status)
	}
	if _, err := io.WriteString(c, "ping"); err != nil {
		w.t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
		w.t.Fatalf("echo %q %v", b, err)
	}
	_ = c.SetDeadline(time.Time{})
	return c
}

// closedWithin says whether c is closed, not merely silent, within d.
func closedWithin(c net.Conn, d time.Duration) error {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := c.Read(make([]byte, 1))
	switch {
	case err == nil:
		return errors.New("read data")
	case errors.Is(err, os.ErrDeadlineExceeded):
		return err
	}
	return nil
}

// TestAnUpgradeEndsWithItsGrant: a caller whose grant goes loses what it
// opened, at once; another caller's stays.
func TestAnUpgradeEndsWithItsGrant(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE)
	w.env.AddMember("bob@example.com", "platform.member")
	if _, err := w.root.Apply(context.Background(), registry.RoleBinding,
		binding("bob-owner", "tenant.owner", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: apiservertest.Tenant}})); err != nil {
		t.Fatal(err)
	}
	bob := cookie(w.endpointSession("bob@example.com", host, time.Time{}))
	w.eventually("bob let in", func() (bool, string) {
		status, body := w.get(bob)
		return status == http.StatusOK, body
	})
	bobs := w.upgrade(bob)
	roots := w.upgrade(w.rootCookie())
	if _, err := w.root.Delete(context.Background(), registry.RoleBinding, client.Key{Name: "bob-owner"}); err != nil {
		t.Fatal(err)
	}
	if err := closedWithin(bobs, 5*time.Second); err != nil {
		t.Errorf("bob's upgraded connection outlived his grant: %v", err)
	}
	if err := closedWithin(roots, 200*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("another caller's connection: %v", err)
	}
}

// TestAnUpgradeEndsWithItsSession: what a session let in ends when it
// expires, as it does with the console session it came from; a public
// endpoint's, which no session let in, does not. The edge here checks
// credentials in flight again only hourly, so only the session's own
// expiry can end it in time.
func TestAnUpgradeEndsWithItsSession(t *testing.T) {
	w := newWorldConfig(t, apiservertest.Start(t, nil), func(c *edge.Config) { c.Recheck = time.Hour })
	w.endpoint(8080, networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE)
	root := w.rootCookie()
	w.eventually("the endpoint served", func() (bool, string) {
		status, body := w.get(root)
		return status == http.StatusOK, body
	})
	short := cookie(w.endpointSession(apiservertest.Root, host, time.Now().Add(2*time.Second)))
	c := w.upgrade(short)
	if err := closedWithin(c, 500*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("closed before its token expired: %v", err)
	}
	if err := closedWithin(c, 5*time.Second); err != nil {
		t.Errorf("outlived its token: %v", err)
	}

	// Made public: no token let its upgrades in, so none ends them; one
	// lasts past where the short token's ended.
	w.endpoint(8080, networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC)
	w.eventually("the endpoint public", func() (bool, string) {
		status, body := w.get(nil)
		return status == http.StatusOK, body
	})
	open := w.upgrade(nil)
	if err := closedWithin(open, 3*time.Second); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("a public endpoint's upgrade ended: %v", err)
	}
}

func binding(name, role string, scope *iamv1alpha1.BindingScope) *iamv1alpha1.RoleBinding {
	return &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "bob@example.com"}},
			Role:    role, Scope: scope,
		},
	}
}

// TestAGrantTakenAwayWhileARequestRegistersEndsIt: a grant taken away
// after a request's first authorization and before it is registered among
// the active requests, where no recheck sees it, still refuses it.
func TestAGrantTakenAwayWhileARequestRegistersEndsIt(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PRIVATE)
	w.env.AddMember("bob@example.com", "platform.member")
	if _, err := w.root.Apply(context.Background(), registry.RoleBinding,
		binding("bob-owner", "tenant.owner", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: apiservertest.Tenant}})); err != nil {
		t.Fatal(err)
	}
	bob := cookie(w.endpointSession("bob@example.com", host, time.Time{}))
	w.eventually("bob let in", func() (bool, string) {
		status, body := w.get(bob)
		return status == http.StatusOK, body
	})
	bobP := auth.Principal{Type: auth.User, ID: "bob@example.com"}
	// In the window: the grant goes, the edge's view takes it in, and the
	// recheck that change wakes has had its chance, all before the request
	// is registered.
	edge.SetRegistering(w.edge, func() {
		edge.SetRegistering(w.edge, nil)
		if _, err := w.root.Delete(context.Background(), registry.RoleBinding, client.Key{Name: "bob-owner"}); err != nil {
			t.Error(err)
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for edge.MayUse(w.edge, bobP, apiservertest.Project) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
	})
	if status, body := w.get(bob); status != http.StatusForbidden {
		t.Errorf("a request whose grant went while it registered: %d %s", status, body)
	}
}

// TestARequestTheEdgeEndsIsAnswered: a request the edge ends itself before
// the endpoint answers, as when the endpoint changes under it, is answered
// 503, to try again, never with nothing, which a client takes for an empty
// 200.
func TestARequestTheEdgeEndsIsAnswered(t *testing.T) {
	w := newWorld(t)
	w.endpoint(8080, networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC)
	w.eventually("the endpoint", func() (bool, string) {
		status, body := w.get(nil)
		return status == http.StatusOK, fmt.Sprint(status, body)
	})
	type result struct {
		status int
		body   string
	}
	done := make(chan result, 1)
	go func() {
		resp, body := w.do(http.MethodGet, host, "/hold", nil)
		done <- result{resp.StatusCode, body}
	}()
	// Once the request waits at the endpoint, the endpoint changes.
	select {
	case <-w.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the endpoint")
	}
	w.endpoint(8081, networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC)
	select {
	case r := <-done:
		if r.status != http.StatusServiceUnavailable || !strings.Contains(r.body, "try again") {
			t.Errorf("a request the endpoint's change ended: %d %q; want 503", r.status, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request outlived its endpoint's change")
	}
}
