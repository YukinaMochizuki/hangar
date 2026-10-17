// Package edge is hangar-edge's proxy for public endpoints: the front door
// (internal/edge/front) sends it every request for an endpoint's hostname.
// It routes by Host to the endpoint, makes sure a PRIVATE endpoint's caller
// is signed in there, through a handoff from the console, or holds a Hangar
// token, and may use it, and reaches the endpoint's machine through the node
// agent's edge dial socket (internal/edgedial). It runs unprivileged, and
// reads the API as the Edge principal: public endpoints, role bindings,
// projects and users, and who holds a credential, nothing else. See
// docs/decisions/public-endpoints.md.
package edge

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1/networkv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/edgedial"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// Config configures an Edge.
type Config struct {
	// API reaches the apiserver's edge socket.
	API *client.Client
	// DialNode connects to the node agent's edge dial socket.
	DialNode func(ctx context.Context) (net.Conn, error)
	// Auth reaches the apiserver's EndpointAuthService on the edge socket:
	// who holds a credential, and endpoint sessions for handoff codes.
	Auth networkv1alpha1connect.EndpointAuthServiceClient
	// Domain holds every endpoint's hostname, one label below it.
	Domain string
	// ConsoleURL is the console's origin, such as
	// https://console.example.com, whose endpoint-login hands a browser
	// off to a PRIVATE endpoint.
	ConsoleURL string
	// Recheck is how often the credentials of requests in flight to PRIVATE
	// endpoints are checked again, so a revoked one ends what it let in;
	// zero means DefaultRecheck.
	Recheck time.Duration
	// Now is the clock handoffs are timed by; nil is time.Now.
	Now func() time.Time
	// Retry is how long the edge waits before reading the endpoints again
	// after a failure; zero means a second.
	Retry time.Duration
	// ResponseHeaderTimeout is how long an endpoint has to start its
	// answer; zero is forever.
	ResponseHeaderTimeout time.Duration
	Log                   *slog.Logger
}

// DefaultRecheck is how often, at most, a request in flight is let go on
// with a credential revoked since it started.
const DefaultRecheck = 30 * time.Second

// Edge serves public endpoints. Create it with New, run its view of the API
// with Run, and serve it as an http.Handler.
type Edge struct {
	cfg   Config
	authz *authz.Authorizer
	proxy *httputil.ReverseProxy
	// sealer seals the handoffs in progress into the browsers that start
	// them, under a key made at start and held only in memory.
	sealer *login.Sealer

	mu sync.RWMutex
	// byLabel and byUID are the live endpoints, by hostname label and uid.
	byLabel map[string]endpoint
	byUID   map[string]endpoint
	// conns are the connections open to each endpoint, by uid, idle in the
	// transport's pool or carrying a request: closed when the endpoint goes
	// or anything of it changes (endpoint).
	conns map[string]map[net.Conn]bool
	// active are the requests being served, which end when the endpoint
	// changes, and, for a PRIVATE one, when their caller's credential
	// expires or is revoked, or the caller may no longer use the endpoint.
	active map[*request]bool

	synced     chan struct{}
	syncedOnce sync.Once

	// registering, if set, runs between a request's first authorization
	// and its registration among the active ones: a test widens that
	// window with it.
	registering func()
}

// endpoint is what the edge needs of a PublicEndpoint, and what its
// connections stand on: any of it changing closes them, so none outlives
// the port, target, machine or access it was opened for.
type endpoint struct {
	project, name, uid string
	public             bool
	port               int32
	target, machine    string
}

// New returns an edge for cfg.
func New(cfg Config) *Edge {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Retry == 0 {
		cfg.Retry = time.Second
	}
	if cfg.Recheck == 0 {
		cfg.Recheck = DefaultRecheck
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	sealer, err := login.NewSealer(handoffPurpose)
	if err != nil {
		// Only crypto/rand failing, which it does not.
		panic(err)
	}
	e := &Edge{
		cfg:     cfg,
		sealer:  sealer,
		authz:   authz.NewFromSource(controller.APISource{Client: cfg.API}, nil),
		byLabel: map[string]endpoint{},
		byUID:   map[string]endpoint{},
		conns:   map[string]map[net.Conn]bool{},
		active:  map[*request]bool{},
		synced:  make(chan struct{}),
	}
	e.proxy = &httputil.ReverseProxy{
		Rewrite: e.rewrite,
		Transport: &http.Transport{
			DialContext: e.dialEndpoint, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, DisableCompression: true,
			ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		},
		// Streams and server-sent events reach the client as they come.
		FlushInterval:  -1,
		ErrorHandler:   e.proxyError,
		ModifyResponse: confineEndpointCookies,
	}
	return e
}

// Run follows the endpoints and the authorization data until ctx is done.
func (e *Edge) Run(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- e.authz.Run(ctx) }()
	go e.recheck(ctx)
	go e.recheckCredentials(ctx)
	for ctx.Err() == nil {
		if err := e.follow(ctx); err != nil && ctx.Err() == nil {
			e.cfg.Log.Error("following public endpoints failed; reading them again", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(e.cfg.Retry):
			}
		}
	}
	<-errc
	return nil
}

// Synced is closed once the edge has read the endpoints and the
// authorization data the first time; until then it serves nothing.
func (e *Edge) Synced() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		<-e.synced
		<-e.authz.Synced()
		close(done)
	}()
	return done
}

func (e *Edge) ready() bool {
	select {
	case <-e.synced:
	default:
		return false
	}
	select {
	case <-e.authz.Synced():
		return true
	default:
		return false
	}
}

// follow lists the endpoints and follows their changes until the watch
// ends.
func (e *Edge) follow(ctx context.Context) error {
	src := controller.APISource{Client: e.cfg.API}
	objs, rev, err := src.List(ctx, registry.PublicEndpoint.Kind)
	if err != nil {
		return err
	}
	byUID := map[string]endpoint{}
	for _, o := range objs {
		if ep, live := decode(o); live {
			byUID[ep.uid] = ep
		}
	}
	e.replace(byUID)
	e.syncedOnce.Do(func() { close(e.synced) })
	return src.Watch(ctx, []string{registry.PublicEndpoint.Kind}, rev, func(ev store.Event) error {
		if ev.Type == store.EventProgress {
			return nil
		}
		ep, live := decode(ev.Object)
		if ep.uid == "" {
			return nil // not an endpoint the edge can read
		}
		var closing []net.Conn
		e.mu.Lock()
		old, had := e.byUID[ep.uid]
		if had && (ev.Type == store.EventDeleted || !live || old != ep) {
			closing = e.drop(old)
		}
		if ev.Type != store.EventDeleted && live {
			e.put(ep)
		}
		e.mu.Unlock()
		closeAll(closing)
		return nil
	})
}

// decode reads a stored endpoint, which is live unless it is being deleted
// and serves no more; one that does not read has no uid.
func decode(o store.Object) (ep endpoint, live bool) {
	var pe networkv1alpha1.PublicEndpoint
	if err := proto.Unmarshal(o.Data, &pe); err != nil {
		return endpoint{}, false
	}
	m := pe.GetMetadata()
	spec := pe.GetSpec()
	return endpoint{
		project: m.GetProject(), name: m.GetName(), uid: m.GetUid(),
		public:  spec.GetAccess() == networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC,
		port:    spec.GetPort(),
		target:  prototext(spec.GetTarget()),
		machine: pe.GetStatus().GetMachine(),
	}, m.GetDeleteTime() == nil
}

// prototext is a target as a comparable string.
func prototext(t *networkv1alpha1.PublicEndpointTarget) string {
	switch o := t.GetObject().(type) {
	case *networkv1alpha1.PublicEndpointTarget_Container:
		return "container/" + o.Container
	case *networkv1alpha1.PublicEndpointTarget_VirtualMachine:
		return "virtualmachine/" + o.VirtualMachine
	}
	return ""
}

// replace makes the view exactly endpoints, closing the connections of
// every endpoint gone or changed.
func (e *Edge) replace(endpoints map[string]endpoint) {
	var closing []net.Conn
	e.mu.Lock()
	for uid, old := range e.byUID {
		if ep, ok := endpoints[uid]; !ok || ep != old {
			closing = append(closing, e.drop(old)...)
		}
	}
	for _, ep := range endpoints {
		e.put(ep)
	}
	e.mu.Unlock()
	closeAll(closing)
}

func closeAll(conns []net.Conn) {
	for _, c := range conns {
		_ = c.Close()
	}
}

// put records ep. Callers hold e.mu.
func (e *Edge) put(ep endpoint) {
	e.byUID[ep.uid] = ep
	e.byLabel[label(ep.project, ep.name)] = ep
}

// drop forgets ep, and returns its connections for the caller to close once
// it lets go of e.mu, so nothing ep let through outlives it. Callers hold
// e.mu.
func (e *Edge) drop(ep endpoint) []net.Conn {
	delete(e.byUID, ep.uid)
	if cur, ok := e.byLabel[label(ep.project, ep.name)]; ok && cur.uid == ep.uid {
		delete(e.byLabel, label(ep.project, ep.name))
	}
	var out []net.Conn
	for c := range e.conns[ep.uid] {
		out = append(out, c)
	}
	delete(e.conns, ep.uid)
	for r := range e.active {
		if r.ep.uid == ep.uid {
			r.cancel()
		}
	}
	return out
}

// request is a request being served: what it reached, who asked, and how to
// end it.
type request struct {
	ep endpoint
	// caller is who asked, for a PRIVATE endpoint; nil for a PUBLIC one.
	caller *auth.Principal
	// cred and host are the credential the caller presented and where, to
	// check again while the request lasts.
	cred token.Secret
	host string
	// cancel ends the request, as the edge's own doing (errEnded).
	cancel context.CancelFunc
}

// errEnded is the cause of a request the edge ended itself: its endpoint
// changed or went, or its caller's credential or grant did. Before any of
// the answer is sent, such a request is answered 503 rather than nothing,
// which a client would take for an empty 200.
var errEnded = errors.New("the edge ended the request")

// recheck ends, at each change to the authorization data, the requests to
// PRIVATE endpoints whose callers may no longer use them: an upgraded
// connection or a stream lasts no longer than the grant that let it in.
func (e *Edge) recheck(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.authz.Changes():
		}
		e.mu.RLock()
		for r := range e.active {
			if r.caller != nil && !e.authz.Allowed(*r.caller, authz.PublicEndpointAccess, authz.Target{Project: r.ep.project}) {
				e.cfg.Log.Info("ended a request whose caller may no longer use the endpoint", "endpoint", r.ep.project+"/"+r.ep.name, "principal", r.caller.String())
				r.cancel()
			}
		}
		e.mu.RUnlock()
	}
}

// Hostname is a request's Host, or a TLS server name, as the edge and the
// front door match it: without a port or a trailing dot, in lower case.
func Hostname(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// label is an endpoint's hostname label, as the apiserver makes it.
func label(project, name string) string { return name + "-" + project }

type targetKey struct{}

// ServeHTTP routes a request by its Host to an endpoint.
func (e *Edge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !e.ready() {
		http.Error(w, "the edge is starting; try again shortly", http.StatusServiceUnavailable)
		return
	}
	l, ok := strings.CutSuffix(Hostname(r.Host), "."+e.cfg.Domain)
	if !ok || l == "" || strings.Contains(l, ".") {
		http.Error(w, "no such endpoint", http.StatusNotFound)
		return
	}
	e.mu.RLock()
	ep, ok := e.byLabel[l]
	e.mu.RUnlock()
	if !ok {
		http.Error(w, "no such endpoint", http.StatusNotFound)
		return
	}
	host := Hostname(r.Host)
	// /.hangar/ is the edge's own, on every endpoint: never forwarded.
	if p, ok := reservedPath(r.URL.Path); ok {
		if !ep.public && p == login.EndpointCallbackPath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			e.callback(w, r, host, ep)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ctx, cancelCause := context.WithCancelCause(r.Context())
	defer cancelCause(nil)
	cancel := func() { cancelCause(errEnded) }
	req := &request{ep: ep, cancel: cancel, host: host}
	if !ep.public {
		p, cred, until, ok := e.authenticate(w, r, host, ep)
		if !ok {
			return
		}
		if !e.authz.Allowed(p, authz.PublicEndpointAccess, authz.Target{Project: ep.project}) {
			e.deny(w, r, host, ep, p)
			return
		}
		req.caller, req.cred = &p, cred
		// What the credential let in ends with it.
		stop := time.AfterFunc(time.Until(until), cancel)
		defer stop.Stop()
	}
	if e.registering != nil {
		e.registering()
	}
	// Registered under the lock that changes endpoints: one changed since
	// it was read is not served.
	e.mu.Lock()
	if cur, ok := e.byUID[ep.uid]; !ok || cur != ep {
		e.mu.Unlock()
		http.Error(w, "the endpoint changed; try again", http.StatusServiceUnavailable)
		return
	}
	e.active[req] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.active, req)
		e.mu.Unlock()
	}()
	// Authorized again once registered: a grant taken away between the
	// check above and the registration is in neither, and a recheck after
	// a later change sees the request.
	if req.caller != nil && !e.authz.Allowed(*req.caller, authz.PublicEndpointAccess, authz.Target{Project: ep.project}) {
		e.deny(w, r, host, ep, *req.caller)
		return
	}
	e.proxy.ServeHTTP(w, r.WithContext(context.WithValue(ctx, targetKey{}, ep)))
}

// key is where ep's connections pool in the transport: its uid and a hash
// of all it stands on, so a request after any change never takes a
// connection opened before it.
func (ep endpoint) key() string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%t\x00%d\x00%s\x00%s", ep.project, ep.name, ep.public, ep.port, ep.target, ep.machine)
	return fmt.Sprintf("%s-%016x", ep.uid, h.Sum64())
}

// endpointHost ends the URL host a request to an endpoint goes to, its key
// (endpoint.key) before it: the transport keeps each endpoint's
// connections, as each was opened, apart. It is never resolved;
// dialEndpoint reads the key back from it.
const endpointHost = ".endpoint.invalid"

func (e *Edge) rewrite(pr *httputil.ProxyRequest) {
	ep := pr.In.Context().Value(targetKey{}).(endpoint)
	pr.SetURL(&url.URL{Scheme: "http", Host: ep.key() + endpointHost})
	pr.Out.Host = pr.In.Host
	pr.SetXForwarded()
	// The front door (internal/edge/front) ended the client's TLS.
	pr.Out.Header.Set("X-Forwarded-Proto", "https")
	StripCredentials(pr.Out.Header)
}

// accessCookie is the cookie Cloudflare Access kept a signed-in session in.
const accessCookie = "CF_Authorization"

// accessHeaders carried an Access token or a service token's credentials.
var accessHeaders = []string{"Cf-Access-Jwt-Assertion", "Cf-Access-Token", "Cf-Access-Client-Id", "Cf-Access-Client-Secret"}

// platformCookiePrefix starts the name of every cookie of Hangar's own: the
// endpoint session and its handoffs, and the console's session and sign-in.
// In any case, so no spelling of one reaches an endpoint.
const platformCookiePrefix = "__host-hangar"

// platformHeaderPrefix starts every header of Hangar's own, such as
// Hangar-Client-Ip: no endpoint takes one a client wrote, so none can pass
// for something the platform says.
const platformHeaderPrefix = "Hangar-"

// StripCredentials removes from a request on its way to an endpoint whatever
// signs its caller in anywhere, so no endpoint can replay it, on itself or
// elsewhere: Hangar's cookies and bearer tokens, and what Cloudflare Access
// used. It also drops every Hangar- header a client sent. A cookie or an
// Authorization of the endpoint's own is left as it is.
func StripCredentials(h http.Header) {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), platformHeaderPrefix) {
			delete(h, k)
		}
	}
	for _, k := range accessHeaders {
		h.Del(k)
	}
	if vs := h.Values("Authorization"); len(vs) > 0 {
		var kept []string
		for _, v := range vs {
			if _, ours := hangarBearer(v); !ours {
				kept = append(kept, v)
			}
		}
		h.Del("Authorization")
		for _, v := range kept {
			h.Add("Authorization", v)
		}
	}
	lines := h.Values("Cookie")
	if len(lines) == 0 {
		return
	}
	var kept []string
	for _, line := range lines {
		for part := range strings.SplitSeq(line, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			name = strings.TrimSpace(name)
			if name == "" || strings.EqualFold(name, accessCookie) || strings.HasPrefix(strings.ToLower(name), platformCookiePrefix) {
				continue
			}
			kept = append(kept, strings.TrimSpace(part))
		}
	}
	h.Del("Cookie")
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

// hangarBearer reads an Authorization value: the token, and whether it is a
// Bearer token of Hangar's, by its prefix.
func hangarBearer(v string) (token.Secret, bool) {
	// Split on any whitespace, tabs included, as servers read the header.
	f := strings.Fields(v)
	if len(f) < 2 || !strings.EqualFold(f[0], "Bearer") {
		return "", false
	}
	s := token.Secret(f[1])
	return s, s.Prefix() != ""
}

// confineEndpointCookies keeps an endpoint's cookies to its own host: its
// answer's Set-Cookie of one of Hangar's names goes, so it cannot set or
// clear one, and every other loses its Domain attribute, so the browser keeps
// it host-only rather than sending it to every endpoint under the domain
// (docs/decisions/public-endpoints.md).
func confineEndpointCookies(resp *http.Response) error {
	vs := resp.Header.Values("Set-Cookie")
	if len(vs) == 0 {
		return nil
	}
	resp.Header.Del("Set-Cookie")
	for _, v := range vs {
		name, _, _ := strings.Cut(v, "=")
		name = strings.TrimSpace(name)
		if strings.HasPrefix(strings.ToLower(name), platformCookiePrefix) {
			continue
		}
		resp.Header.Add("Set-Cookie", withoutDomain(v))
	}
	return nil
}

// withoutDomain is the Set-Cookie value v without its Domain attributes, the
// rest kept as written. Attributes split on ";" as browsers split them; a
// cookie value cannot hold one.
func withoutDomain(v string) string {
	parts := strings.Split(v, ";")
	kept := []string{parts[0]}
	for _, p := range parts[1:] {
		attr, _, _ := strings.Cut(p, "=")
		if !strings.EqualFold(strings.TrimSpace(attr), "Domain") {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ";")
}

// dialEndpoint opens a connection to the endpoint addr names, through the
// node, and records it so it is closed if the endpoint goes.
func (e *Edge) dialEndpoint(ctx context.Context, _, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	key, ok := strings.CutSuffix(host, endpointHost)
	if !ok || len(key) < 17 {
		return nil, fmt.Errorf("%s is no endpoint", addr)
	}
	uid := key[:len(key)-17]
	e.mu.RLock()
	ep, ok := e.byUID[uid]
	e.mu.RUnlock()
	if !ok || ep.key() != key {
		return nil, &edgedial.Error{Status: http.StatusNotFound, Message: "the endpoint is gone or changed"}
	}
	c, err := edgedial.Dial(ctx, e.cfg.DialNode, edgedial.Target{Project: ep.project, Name: ep.name, UID: ep.uid})
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur, ok := e.byUID[uid]; !ok || cur != ep {
		_ = c.Close()
		return nil, &edgedial.Error{Status: http.StatusNotFound, Message: "the endpoint changed"}
	}
	tc := &tracked{Conn: c, forget: func(tc net.Conn) {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.conns[uid], tc)
	}}
	if e.conns[uid] == nil {
		e.conns[uid] = map[net.Conn]bool{}
	}
	e.conns[uid][tc] = true
	return tc, nil
}

// tracked is a connection to an endpoint that leaves the edge's records
// when it closes.
type tracked struct {
	net.Conn
	once   sync.Once
	forget func(net.Conn)
}

func (t *tracked) Close() error {
	t.once.Do(func() { t.forget(t) })
	return t.Conn.Close()
}

// CloseWrite passes a half-close on, for an upgraded connection.
func (t *tracked) CloseWrite() error {
	if cw, ok := t.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (e *Edge) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	ep, _ := r.Context().Value(targetKey{}).(endpoint)
	// The edge ended it, closing its connection too: whatever the error, the
	// client is to try again.
	if errors.Is(context.Cause(r.Context()), errEnded) {
		http.Error(w, "the endpoint changed, or the sign-in ended; try again", http.StatusServiceUnavailable)
		return
	}
	status, msg := http.StatusBadGateway, "the endpoint's machine did not answer"
	var de *edgedial.Error
	if errors.As(err, &de) {
		switch de.Status {
		case http.StatusNotFound:
			status, msg = http.StatusNotFound, "no such endpoint"
		case http.StatusConflict:
			status, msg = http.StatusServiceUnavailable, "the endpoint's machine is not running"
		}
	}
	if errors.Is(err, context.Canceled) {
		return // the client went away
	}
	// The front door caps the body (internal/edge/front).
	if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	e.cfg.Log.Info("an endpoint did not answer", "endpoint", ep.project+"/"+ep.name, "error", err)
	http.Error(w, msg, status)
}
