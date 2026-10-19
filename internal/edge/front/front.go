// Package front is hangar-edge's front door: the only listener on the
// host's :443 and :80, with nothing in front of it. It ends TLS with
// certificates it gets itself over ACME DNS-01 (certs.go), keeps every
// client within its limits (limits.go), holds slow request bodies
// (bodyrate.go) and responses a client stops taking (writes.go), and
// routes each request by Host:
// api.<cp-domain> to the apiserver, console.<cp-domain> to the console's
// files and its API paths to the apiserver, and <label>.<user-domain> to
// the public endpoints (internal/edge). :80 only redirects to https. See
// docs/decisions/front-door.md.
package front

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/edge"
)

// Config configures a Front.
type Config struct {
	// CPDomain is the control plane's domain: the API is api.<CPDomain>,
	// the console console.<CPDomain>.
	CPDomain string
	// UserDomain holds the public endpoints, one label below it.
	UserDomain string
	// DialAPI connects to the apiserver's public socket, the one it takes
	// from hangar-edge alone.
	DialAPI func(ctx context.Context) (net.Conn, error)
	// Endpoints serves the public endpoints.
	Endpoints http.Handler
	// ConsoleDir holds the console's files, index.html at its top; empty,
	// or without index.html, the console is not installed.
	ConsoleDir string
	Limits     Limits
	Log        *slog.Logger
}

// Front routes the requests the front door takes. Create it with New and
// serve it with Serve.
type Front struct {
	cfg    Config
	counts *counts
	api    *httputil.ReverseProxy
	// refusalEvery is how often the refused connections are summed up in
	// the log.
	refusalEvery time.Duration
}

// New returns a front door for cfg.
func New(cfg Config) (*Front, error) {
	if cfg.CPDomain == "" || cfg.UserDomain == "" {
		return nil, errors.New("the front door needs the control plane's domain and the endpoints' domain")
	}
	if err := cfg.Limits.Validate(); err != nil {
		return nil, err
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cfg.CPDomain, cfg.UserDomain = strings.ToLower(cfg.CPDomain), strings.ToLower(cfg.UserDomain)
	f := &Front{cfg: cfg, counts: newCounts(cfg.Limits), refusalEvery: refusalSummaryEvery}
	f.api = &httputil.ReverseProxy{
		Rewrite: f.rewriteAPI,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return cfg.DialAPI(ctx)
			},
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
		// Connect's server streams and Watch reach the client as they
		// come.
		FlushInterval: -1,
		ErrorHandler:  f.apiError,
	}
	return f, nil
}

// Names are the hostnames the front door needs certificates for.
func (f *Front) Names() []string {
	return []string{"api." + f.cfg.CPDomain, "console." + f.cfg.CPDomain, "*." + f.cfg.UserDomain}
}

// site is which of the front door's sites a hostname is.
type site int

const (
	noSite site = iota
	apiSite
	consoleSite
	endpointSite
)

// siteOf says which site host, a request's Host or a TLS server name, is.
func (f *Front) siteOf(host string) site {
	host = edge.Hostname(host)
	switch host {
	case "api." + f.cfg.CPDomain:
		return apiSite
	case "console." + f.cfg.CPDomain:
		return consoleSite
	}
	if l, ok := strings.CutSuffix(host, "."+f.cfg.UserDomain); ok && l != "" && !strings.Contains(l, ".") {
		return endpointSite
	}
	return noSite
}

// spoofable are the headers that say who the client is or how it came,
// which only the front door may set: whatever the client sent of them is
// dropped. X-Forwarded-* are dropped and set again by the proxies.
var spoofable = []string{auth.ClientIPHeader, "Forwarded", "X-Real-Ip", "Cf-Connecting-Ip", "True-Client-Ip"}

// ServeHTTP routes a request that came in over TLS.
func (f *Front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s := f.siteOf(r.Host)
	if s == noSite {
		page(w, http.StatusNotFound, pageNoSuchHost)
		return
	}
	// A connection opened for one site's certificate carries only that
	// site's requests: browsers reuse a connection for any name its
	// certificate covers, and nothing else.
	if r.TLS != nil && f.siteOf(r.TLS.ServerName) != s {
		page(w, http.StatusMisdirectedRequest, pageMisdirected)
		return
	}
	for _, h := range spoofable {
		r.Header.Del(h)
	}
	// The control plane's sites are HTTPS alone. Not with
	// includeSubDomains, nor on the endpoints: each answers for its own.
	if s == apiSite || s == consoleSite {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	switch s {
	case apiSite:
		f.serveAPI(w, r)
	case consoleSite:
		if isAPIPath(r.URL.Path) {
			f.serveAPI(w, r)
			return
		}
		f.serveConsole(w, r)
	case endpointSite:
		if !f.limitBody(w, r, f.cfg.Limits.EndpointBodyBytes) {
			return
		}
		f.cfg.Endpoints.ServeHTTP(w, r)
	}
}

// apiPaths are the console's paths that are the API's, not its files:
// Connect's (/<package>.<Service>/<Method>), the WebSocket routes', and
// sign-in's (internal/auth/login), whose provider returns to the console's
// origin.
var apiPaths = []string{"/hangar.", "/v1/", "/auth/"}

func isAPIPath(p string) bool {
	for _, prefix := range apiPaths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// limitBody caps r's body at n bytes, answering 413 at once when it says
// it is longer. It reports whether the request may go on.
func (f *Front) limitBody(w http.ResponseWriter, r *http.Request, n int64) bool {
	if r.ContentLength > n {
		page(w, http.StatusRequestEntityTooLarge, pageBodyTooLarge)
		return false
	}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, n)
	}
	return true
}

func (f *Front) serveAPI(w http.ResponseWriter, r *http.Request) {
	if !f.limitBody(w, r, f.cfg.Limits.APIBodyBytes) {
		return
	}
	f.api.ServeHTTP(w, r)
}

// apiHost is the URL host requests to the apiserver go to; it is never
// resolved, DialAPI connects to the socket.
const apiHost = "hangar-apiserver"

func (f *Front) rewriteAPI(pr *httputil.ProxyRequest) {
	pr.SetURL(&url.URL{Scheme: "http", Host: apiHost})
	// The apiserver checks a WebSocket's Origin against the Host the
	// client asked for.
	pr.Out.Host = pr.In.Host
	pr.SetXForwarded()
	pr.Out.Header.Set("X-Forwarded-Proto", "https")
	if addr, ok := addrOf(pr.In.RemoteAddr); ok {
		pr.Out.Header.Set(auth.ClientIPHeader, addr.String())
	}
}

func (f *Front) apiError(w http.ResponseWriter, r *http.Request, err error) {
	if edge.BodyCut(r.Context(), err) {
		page(w, http.StatusRequestTimeout, pageBodyTooSlow)
		return
	}
	if errors.Is(err, context.Canceled) {
		return // the client went away
	}
	if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
		page(w, http.StatusRequestEntityTooLarge, pageBodyTooLarge)
		return
	}
	f.cfg.Log.Warn("the apiserver did not answer", "path", r.URL.Path, "error", err)
	page(w, http.StatusBadGateway, pageAPIDown)
}

// redirectHandler is :80's: a request for one of the front door's sites is
// sent to https, anything else is not found. ACME is DNS-01 only, so
// nothing is served here.
func (f *Front) redirectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.siteOf(r.Host) == noSite {
			page(w, http.StatusNotFound, pageNoSuchHost)
			return
		}
		redirect(w, edge.Hostname(r.Host), r.URL.RequestURI())
	})
}

// Listeners are the front door's sockets: :443 and :80.
type Listeners struct {
	HTTPS, HTTP net.Listener
}

// Serve serves the front door on ls until ctx is done, with TLS from
// tlsConfig, then lets the requests in flight finish for a while.
func (f *Front) Serve(ctx context.Context, ls Listeners, tlsConfig *tls.Config) error {
	l := f.cfg.Limits
	summary := newRefusals()
	refused := func(addr netip.Addr, why string) {
		f.cfg.Log.Debug("refused a connection", "client", addr.String(), "reason", why)
		summary.note(why)
	}
	// One line every refusalEvery at most, with the counts by reason since
	// the last, and none while nothing is refused.
	summed := make(chan struct{})
	defer close(summed)
	go func() {
		tick := time.NewTicker(f.refusalEvery)
		defer tick.Stop()
		for {
			select {
			case <-summed:
				return
			case <-tick.C:
				if by := summary.take(); by != nil {
					f.cfg.Log.Warn("refused connections past the limits", "by_reason", by)
				}
			}
		}
	}()
	errorLog := slog.NewLogLogger(f.cfg.Log.Handler(), slog.LevelDebug)
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	tc := tlsConfig.Clone()
	tc.MinVersion = tls.VersionTLS12
	tc.NextProtos = []string{"h2", "http/1.1"}
	https := &http.Server{
		Handler:           l.boundWrites(l.limitBodyRate(f.counts.limitRequests(f))),
		ConnState:         f.counts.connState,
		TLSConfig:         tc,
		Protocols:         protocols,
		ReadHeaderTimeout: l.ReadHeaderTimeout,
		IdleTimeout:       l.IdleTimeout,
		MaxHeaderBytes:    l.MaxHeaderBytes,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: l.HTTP2MaxStreams,
			SendPingTimeout:      l.HTTP2SendPingTimeout,
			PingTimeout:          l.HTTP2PingTimeout,
			WriteByteTimeout:     l.HTTP2WriteByteTimeout,
		},
		ErrorLog: errorLog,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	plain := &http.Server{
		Handler:           l.boundWrites(l.limitBodyRate(f.counts.limitRequests(f.redirectHandler()))),
		ConnState:         f.counts.connState,
		ReadHeaderTimeout: l.ReadHeaderTimeout,
		IdleTimeout:       l.IdleTimeout,
		MaxHeaderBytes:    l.MaxHeaderBytes,
		ErrorLog:          errorLog,
	}
	errc := make(chan error, 2)
	f.cfg.Log.Info("serving the front door", "https", ls.HTTPS.Addr().String(), "http", ls.HTTP.Addr().String(),
		"api", "api."+f.cfg.CPDomain, "console", "console."+f.cfg.CPDomain, "endpoints", "*."+f.cfg.UserDomain)
	go func() {
		errc <- https.ServeTLS(&listener{Listener: ls.HTTPS, counts: f.counts, refused: refused}, "", "")
	}()
	go func() {
		errc <- plain.Serve(&listener{Listener: ls.HTTP, counts: f.counts, refused: refused})
	}()
	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range []*http.Server{https, plain} {
		if serr := s.Shutdown(shutdown); serr != nil {
			_ = s.Close()
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
