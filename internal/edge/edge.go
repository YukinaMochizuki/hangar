// Package edge is hangar-edge: the HTTP proxy cloudflared sends every
// request for a public endpoint's hostname to. It routes by Host to the
// endpoint, checks that a PRIVATE endpoint's caller signed in through
// Access and may use it, and reaches the endpoint's machine through the
// node agent's edge dial socket (internal/edgedial). It runs unprivileged,
// and reads the API as the Edge principal: public endpoints, role bindings
// and projects, nothing else. See docs/decisions/public-endpoints.md.
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
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/auth"
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
	// Domain holds every endpoint's hostname, one label below it.
	Domain string
	// Access verifies the Access token of the endpoint domain's wildcard
	// application, which every PRIVATE endpoint is behind.
	Access Verifier
	// Retry is how long the edge waits before reading the endpoints again
	// after a failure; zero means a second.
	Retry time.Duration
	Log   *slog.Logger
}

// Verifier verifies an Access token and says until when it holds;
// *auth.Cloudflare is one.
type Verifier interface {
	AuthenticateUntil(ctx context.Context, header http.Header) (auth.Principal, time.Time, bool, error)
}

// Edge serves public endpoints. Create it with New, run its view of the API
// with Run, and serve it as an http.Handler.
type Edge struct {
	cfg   Config
	authz *authz.Authorizer
	proxy *httputil.ReverseProxy

	mu sync.RWMutex
	// byLabel and byUID are the live endpoints, by hostname label and uid.
	byLabel map[string]endpoint
	byUID   map[string]endpoint
	// conns are the connections open to each endpoint, by uid, idle in the
	// transport's pool or carrying a request: closed when the endpoint goes
	// or anything of it changes (endpoint).
	conns map[string]map[net.Conn]bool
	// active are the requests being served, which end when the endpoint
	// changes, and, for a PRIVATE one, when their caller's token expires or
	// the caller may no longer use the endpoint.
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
	e := &Edge{
		cfg:     cfg,
		authz:   authz.NewFromSource(controller.APISource{Client: cfg.API}, nil),
		byLabel: map[string]endpoint{},
		byUID:   map[string]endpoint{},
		conns:   map[string]map[net.Conn]bool{},
		active:  map[*request]bool{},
		synced:  make(chan struct{}),
	}
	e.proxy = &httputil.ReverseProxy{
		Rewrite:   e.rewrite,
		Transport: &http.Transport{DialContext: e.dialEndpoint, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, DisableCompression: true},
		// Streams and server-sent events reach the client as they come.
		FlushInterval: -1,
		ErrorHandler:  e.proxyError,
	}
	return e
}

// Run follows the endpoints and the authorization data until ctx is done.
func (e *Edge) Run(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- e.authz.Run(ctx) }()
	go e.recheck(ctx)
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
	cancel context.CancelFunc
}

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

// label is an endpoint's hostname label, as the apiserver makes it.
func label(project, name string) string { return name + "-" + project }

type targetKey struct{}

// ServeHTTP routes a request by its Host to an endpoint.
func (e *Edge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !e.ready() {
		http.Error(w, "the edge is starting; try again shortly", http.StatusServiceUnavailable)
		return
	}
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	l, ok := strings.CutSuffix(host, "."+e.cfg.Domain)
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
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req := &request{ep: ep, cancel: cancel}
	if !ep.public {
		p, until, ok, err := e.cfg.Access.AuthenticateUntil(r.Context(), r.Header)
		if err != nil || !ok {
			e.cfg.Log.Info("a private endpoint refused: no valid Access token", "endpoint", ep.project+"/"+ep.name, "error", err)
			http.Error(w, "sign in through Cloudflare Access to use this endpoint", http.StatusUnauthorized)
			return
		}
		if !e.authz.Allowed(p, authz.PublicEndpointAccess, authz.Target{Project: ep.project}) {
			e.cfg.Log.Info("a private endpoint refused", "endpoint", ep.project+"/"+ep.name, "principal", p.String())
			http.Error(w, fmt.Sprintf("%s may not use this endpoint", p.ID), http.StatusForbidden)
			return
		}
		req.caller = &p
		// What the token let in ends with it. Access's tokens all expire;
		// one that says nothing lets nothing outlast this request's start.
		if until.IsZero() {
			until = time.Now()
		}
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
		e.cfg.Log.Info("a private endpoint refused", "endpoint", ep.project+"/"+ep.name, "principal", req.caller.String())
		http.Error(w, fmt.Sprintf("%s may not use this endpoint", req.caller.ID), http.StatusForbidden)
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
	// Cloudflare ended the client's TLS.
	pr.Out.Header.Set("X-Forwarded-Proto", "https")
	StripAccess(pr.Out.Header)
}

// accessCookie is the cookie Access keeps a signed-in session in.
const accessCookie = "CF_Authorization"

// accessHeaders carry an Access token or a service token's credentials.
var accessHeaders = []string{auth.CloudflareJWTHeader, "Cf-Access-Token", "Cf-Access-Client-Id", "Cf-Access-Client-Secret"}

// StripAccess removes what proves a caller signed in through Access, or
// lets it: every endpoint shares one Access application, so an endpoint
// given a visitor's token, or a service token's secret, could use it on any
// other endpoint that visitor may use.
func StripAccess(h http.Header) {
	for _, k := range accessHeaders {
		h.Del(k)
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
			if name == "" || strings.EqualFold(name, accessCookie) {
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
	e.cfg.Log.Info("an endpoint did not answer", "endpoint", ep.project+"/"+ep.name, "error", err)
	http.Error(w, msg, status)
}
