// Package cftest is an in-memory fake of the Cloudflare API v4 endpoints
// package cloudflare calls, served over httptest. It keeps the API's wire
// shapes (its own types, not the client's), its envelope, status codes,
// paging and the validation that matters to callers, and it rejects request
// fields the API does not document. It is not a model of Cloudflare beyond
// those endpoints.
package cftest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Zone is a DNS zone.
type Zone struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Account struct {
		ID string `json:"id"`
	} `json:"account"`
}

// Tunnel is a cfd_tunnel.
type Tunnel struct {
	ID        string `json:"id"`
	AccountID string `json:"account_tag"`
	Name      string `json:"name"`
	ConfigSrc string `json:"config_src,omitempty"`
	// RemoteConfig is the deprecated field older responses carry, alone or
	// beside config_src.
	RemoteConfig bool       `json:"remote_config"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	DeletedAt    *time.Time `json:"deleted_at"`
	TunType      string     `json:"tun_type"`
	// Token is what GET .../token returns; never part of the tunnel's JSON.
	Token string `json:"-"`
}

// IngressRule is one ingress entry of a tunnel configuration.
type IngressRule struct {
	Hostname      string          `json:"hostname,omitempty"`
	Path          string          `json:"path,omitempty"`
	Service       string          `json:"service"`
	OriginRequest json.RawMessage `json:"originRequest,omitempty"`
}

// TunnelConfig is the config object of a tunnel configuration.
type TunnelConfig struct {
	Ingress       []IngressRule   `json:"ingress"`
	OriginRequest json.RawMessage `json:"originRequest,omitempty"`
	WarpRouting   json.RawMessage `json:"warp-routing,omitempty"`
}

// TunnelConfiguration is what .../configurations returns.
type TunnelConfiguration struct {
	AccountID string       `json:"account_id"`
	TunnelID  string       `json:"tunnel_id"`
	Version   int          `json:"version"`
	Source    string       `json:"source"`
	Config    TunnelConfig `json:"config"`
	CreatedAt time.Time    `json:"created_at"`
}

// DNSRecord is a DNS record.
type DNSRecord struct {
	ID       string          `json:"id"`
	ZoneID   string          `json:"zone_id"`
	Type     string          `json:"type"`
	Name     string          `json:"name"`
	Content  string          `json:"content"`
	Proxied  bool            `json:"proxied"`
	TTL      int             `json:"ttl"`
	Comment  string          `json:"comment"`
	Tags     []string        `json:"tags"`
	Settings json.RawMessage `json:"settings"`
}

// AppDestination is one destination of an application.
type AppDestination struct {
	Type string `json:"type"`
	URI  string `json:"uri,omitempty"`
}

// PolicyLink attaches a reusable policy to an application.
type PolicyLink struct {
	ID         string `json:"id"`
	Precedence int    `json:"precedence"`
}

// App is a self-hosted Access application.
type App struct {
	ID                     string           `json:"id"`
	AUD                    string           `json:"aud"`
	Name                   string           `json:"name"`
	Domain                 string           `json:"domain"`
	Type                   string           `json:"type"`
	SessionDuration        string           `json:"session_duration"`
	AllowedIdPs            []string         `json:"allowed_idps"`
	AutoRedirectToIdentity bool             `json:"auto_redirect_to_identity"`
	AppLauncherVisible     bool             `json:"app_launcher_visible"`
	Destinations           []AppDestination `json:"destinations"`
	Tags                   []string         `json:"tags"`
	SelfHostedDomains      []string         `json:"self_hosted_domains"`

	OptionsPreflightBypass   bool            `json:"options_preflight_bypass"`
	AllowAuthenticateViaWarp bool            `json:"allow_authenticate_via_warp"`
	AllowIframe              bool            `json:"allow_iframe"`
	CORSHeaders              json.RawMessage `json:"cors_headers,omitempty"`
	HTTPOnlyCookieAttribute  bool            `json:"http_only_cookie_attribute"`
	SameSiteCookieAttribute  string          `json:"same_site_cookie_attribute,omitempty"`
	ClientlessIsolationURL   bool            `json:"use_clientless_isolation_app_launcher_url"`
	// Links are the attached policies; the API expands them in responses.
	Links     []PolicyLink `json:"-"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// Policy is a reusable Access policy.
type Policy struct {
	ID              string            `json:"id"`
	AccountID       string            `json:"account_id"`
	Name            string            `json:"name"`
	Decision        string            `json:"decision"`
	Include         []json.RawMessage `json:"include"`
	Exclude         []json.RawMessage `json:"exclude"`
	Require         []json.RawMessage `json:"require"`
	SessionDuration string            `json:"session_duration,omitempty"`
	Reusable        bool              `json:"reusable"`
	AppCount        int               `json:"app_count"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// IdentityProvider is a login method.
type IdentityProvider struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
}

// ServiceToken is an Access service token.
type ServiceToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ClientID  string    `json:"client_id"`
	Duration  string    `json:"duration"`
	Enabled   bool      `json:"enabled"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Secret is returned once, by the create call.
	Secret string `json:"-"`
}

// Organization is the Zero Trust organization.
type Organization struct {
	Name            string `json:"name"`
	AuthDomain      string `json:"auth_domain"`
	SessionDuration string `json:"session_duration"`
}

// State is everything the fake holds. Tests change it through Fake.Mutate
// to stand in for edits made elsewhere (the dashboard, another tool).
type State struct {
	Zones         []*Zone
	Tunnels       map[string]*Tunnel
	Configs       map[string]*TunnelConfiguration
	DNS           map[string]*DNSRecord
	Apps          map[string]*App
	Policies      map[string]*Policy
	IdPs          []*IdentityProvider
	ServiceTokens map[string]*ServiceToken
	Org           *Organization
}

// Request is one request the fake received.
type Request struct {
	Method string
	Path   string
	Query  string
	// Auth is the Authorization header, kept so tests can check what was
	// sent; it is never printed by the fake.
	Auth string
}

// Fake is the fake API. Its zero value is not usable; call New.
type Fake struct {
	Server    *httptest.Server
	Token     string
	AccountID string
	// MaxPerPage, when set, caps every list's page size, so tests can make
	// short lists span pages.
	MaxPerPage int

	mu       sync.Mutex
	st       *State
	requests []Request
	faults   []fault
	mux      *http.ServeMux
}

type fault struct {
	method     string // empty matches any
	status     int
	retryAfter string
}

// New starts a fake for one account accepting one API token. It holds a
// zone, a Google login method named "Google", the One-time PIN method and an
// organization with the team domain "hangar-test.cloudflareaccess.com".
func New(token, accountID, zone string) *Fake {
	f := &Fake{Token: token, AccountID: accountID, mux: http.NewServeMux()}
	z := &Zone{ID: hexID(16), Name: zone, Status: "active"}
	z.Account.ID = accountID
	f.st = &State{
		Zones:         []*Zone{z},
		Tunnels:       map[string]*Tunnel{},
		Configs:       map[string]*TunnelConfiguration{},
		DNS:           map[string]*DNSRecord{},
		Apps:          map[string]*App{},
		Policies:      map[string]*Policy{},
		ServiceTokens: map[string]*ServiceToken{},
		IdPs: []*IdentityProvider{
			{ID: uuid(), Name: "Google", Type: "google", Config: json.RawMessage(`{"client_id":"x.apps.googleusercontent.com"}`)},
			{ID: uuid(), Name: "One-time PIN", Type: "onetimepin", Config: json.RawMessage(`{}`)},
		},
		Org: &Organization{Name: "hangar-test", AuthDomain: "hangar-test.cloudflareaccess.com", SessionDuration: "24h"},
	}
	f.routes()
	f.Server = httptest.NewServer(f)
	return f
}

// URL is the API root to give the client.
func (f *Fake) URL() string { return f.Server.URL + "/client/v4" }

// Close stops the server.
func (f *Fake) Close() { f.Server.Close() }

// Mutate runs fn on the state under the fake's lock.
func (f *Fake) Mutate(fn func(*State)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.st)
}

// Requests returns every request received so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// Writes returns the requests that were not GETs.
func (f *Fake) Writes() []Request {
	var out []Request
	for _, r := range f.Requests() {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

// Fail makes the next n requests with method (any, if empty) answer status,
// with a Retry-After header when retryAfter is set.
func (f *Fake) Fail(method string, status, n int, retryAfter string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for range n {
		f.faults = append(f.faults, fault{method: method, status: status, retryAfter: retryAfter})
	}
}

// AddTunnel creates a tunnel as something other than the tool would, and
// returns it.
func (f *Fake) AddTunnel(name, configSrc string) *Tunnel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addTunnel(name, configSrc)
}

func (f *Fake) addTunnel(name, configSrc string) *Tunnel {
	t := &Tunnel{ID: uuid(), AccountID: f.AccountID, Name: name, ConfigSrc: configSrc, Status: "inactive", CreatedAt: now(), TunType: "cfd_tunnel"}
	t.Token = "tunnel-token-" + hexID(24)
	f.st.Tunnels[t.ID] = t
	return t
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")})
	if i := slices.IndexFunc(f.faults, func(x fault) bool { return x.method == "" || x.method == r.Method }); i >= 0 {
		ft := f.faults[i]
		f.faults = slices.Delete(f.faults, i, i+1)
		f.mu.Unlock()
		if ft.retryAfter != "" {
			w.Header().Set("Retry-After", ft.retryAfter)
		}
		fail(w, ft.status, 10000+ft.status, http.StatusText(ft.status))
		return
	}
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		fail(w, http.StatusUnauthorized, 10000, "Authentication error")
		return
	}
	f.mux.ServeHTTP(w, r)
}

type handler func(w http.ResponseWriter, r *http.Request, st *State)

func (f *Fake) handle(pattern string, h handler) {
	f.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if acct := r.PathValue("account"); acct != "" && acct != f.AccountID {
			fail(w, http.StatusForbidden, 9109, "Unauthorized to access requested resource")
			return
		}
		if z := r.PathValue("zone"); z != "" && !slices.ContainsFunc(f.st.Zones, func(x *Zone) bool { return x.ID == z }) {
			fail(w, http.StatusNotFound, 7003, "Could not route to /zones/"+z+", perhaps your object identifier is invalid?")
			return
		}
		h(w, r, f.st)
	})
}

const v4 = "/client/v4"

func (f *Fake) routes() {
	f.handle("GET "+v4+"/zones", f.listZones)

	acct := v4 + "/accounts/{account}"
	f.handle("GET "+acct+"/cfd_tunnel", f.listTunnels)
	f.handle("POST "+acct+"/cfd_tunnel", f.createTunnel)
	f.handle("GET "+acct+"/cfd_tunnel/{id}", f.getTunnel)
	f.handle("GET "+acct+"/cfd_tunnel/{id}/configurations", f.getConfig)
	f.handle("PUT "+acct+"/cfd_tunnel/{id}/configurations", f.putConfig)
	f.handle("GET "+acct+"/cfd_tunnel/{id}/token", f.getToken)

	zone := v4 + "/zones/{zone}"
	f.handle("GET "+zone+"/dns_records", f.listDNS)
	f.handle("POST "+zone+"/dns_records", f.createDNS)
	f.handle("GET "+zone+"/dns_records/{id}", f.getDNS)
	f.handle("PUT "+zone+"/dns_records/{id}", f.putDNS)

	f.handle("GET "+acct+"/access/apps", f.listApps)
	f.handle("POST "+acct+"/access/apps", f.createApp)
	f.handle("GET "+acct+"/access/apps/{id}", f.getApp)
	f.handle("PUT "+acct+"/access/apps/{id}", f.putApp)

	f.handle("GET "+acct+"/access/policies", f.listPolicies)
	f.handle("POST "+acct+"/access/policies", f.createPolicy)
	f.handle("GET "+acct+"/access/policies/{id}", f.getPolicy)
	f.handle("PUT "+acct+"/access/policies/{id}", f.putPolicy)

	f.handle("GET "+acct+"/access/identity_providers", f.listIdPs)

	f.handle("GET "+acct+"/access/service_tokens", f.listServiceTokens)
	f.handle("POST "+acct+"/access/service_tokens", f.createServiceToken)
	f.handle("GET "+acct+"/access/service_tokens/{id}", f.getServiceToken)

	f.handle("GET "+acct+"/access/organizations", f.getOrg)

	f.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotFound, 7003, "No route for the URI")
	})
}

// Responses.

type message struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func reply(w http.ResponseWriter, status int, result any, info map[string]int) {
	body := map[string]any{"success": true, "errors": []message{}, "messages": []message{}, "result": result}
	if info != nil {
		body["result_info"] = info
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status, code int, msg string) {
	body := map[string]any{"success": false, "errors": []message{{code, msg}}, "messages": []message{}, "result": nil}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// decode reads a JSON body, refusing fields outside the documented request
// schema.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, 10001, "Malformed request body: "+err.Error())
		return false
	}
	return true
}

// page answers a list with the API's paging. totalPages says whether
// result_info carries total_pages, which the Access endpoints leave out.
func page[T any](f *Fake, w http.ResponseWriter, r *http.Request, items []T, defPer int, totalPages bool) {
	p, per := 1, defPer
	if s := r.URL.Query().Get("page"); s != "" {
		p, _ = strconv.Atoi(s)
	}
	if s := r.URL.Query().Get("per_page"); s != "" {
		per, _ = strconv.Atoi(s)
	}
	if p < 1 || per < 1 {
		fail(w, http.StatusBadRequest, 10001, "invalid paging")
		return
	}
	if f.MaxPerPage > 0 && per > f.MaxPerPage {
		per = f.MaxPerPage
	}
	start := min((p-1)*per, len(items))
	end := min(start+per, len(items))
	out := items[start:end]
	if out == nil {
		out = []T{}
	}
	info := map[string]int{"page": p, "per_page": per, "count": len(out), "total_count": len(items)}
	if totalPages {
		info["total_pages"] = (len(items) + per - 1) / per
	}
	reply(w, http.StatusOK, out, info)
}

func sorted[T any](m map[string]*T, key func(*T) string) []*T {
	out := make([]*T, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b *T) int { return strings.Compare(key(a), key(b)) })
	return out
}

// Zones.

func (f *Fake) listZones(w http.ResponseWriter, r *http.Request, st *State) {
	q := r.URL.Query()
	var out []*Zone
	for _, z := range st.Zones {
		if n := q.Get("name"); n != "" && z.Name != n {
			continue
		}
		if a := q.Get("account.id"); a != "" && z.Account.ID != a {
			continue
		}
		out = append(out, z)
	}
	page(f, w, r, out, 20, true)
}

// Tunnels.

func (f *Fake) listTunnels(w http.ResponseWriter, r *http.Request, st *State) {
	q := r.URL.Query()
	var out []*Tunnel
	for _, t := range sorted(st.Tunnels, func(t *Tunnel) string { return t.CreatedAt.Format(time.RFC3339Nano) + t.ID }) {
		if n := q.Get("name"); n != "" && t.Name != n {
			continue
		}
		switch q.Get("is_deleted") {
		case "false":
			if t.DeletedAt != nil {
				continue
			}
		case "true":
			if t.DeletedAt == nil {
				continue
			}
		}
		out = append(out, t)
	}
	page(f, w, r, out, 20, true)
}

func (f *Fake) createTunnel(w http.ResponseWriter, r *http.Request, st *State) {
	var in struct {
		Name         string `json:"name"`
		ConfigSrc    string `json:"config_src"`
		TunnelSecret string `json:"tunnel_secret"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Name == "" {
		fail(w, http.StatusBadRequest, 1003, "tunnel name is required")
		return
	}
	if in.ConfigSrc == "" {
		in.ConfigSrc = "local"
	}
	if in.ConfigSrc != "local" && in.ConfigSrc != "cloudflare" {
		fail(w, http.StatusBadRequest, 1003, "invalid config_src")
		return
	}
	for _, t := range st.Tunnels {
		if t.Name == in.Name && t.DeletedAt == nil {
			fail(w, http.StatusConflict, 1013, "You already have a tunnel with this name.")
			return
		}
	}
	reply(w, http.StatusOK, f.addTunnel(in.Name, in.ConfigSrc), nil)
}

func (f *Fake) tunnel(w http.ResponseWriter, r *http.Request, st *State) *Tunnel {
	t := st.Tunnels[r.PathValue("id")]
	if t == nil {
		fail(w, http.StatusNotFound, 1003, "tunnel not found")
	}
	return t
}

func (f *Fake) getTunnel(w http.ResponseWriter, r *http.Request, st *State) {
	if t := f.tunnel(w, r, st); t != nil {
		reply(w, http.StatusOK, t, nil)
	}
}

func (f *Fake) getConfig(w http.ResponseWriter, r *http.Request, st *State) {
	t := f.tunnel(w, r, st)
	if t == nil {
		return
	}
	c := st.Configs[t.ID]
	if c == nil {
		c = &TunnelConfiguration{AccountID: f.AccountID, TunnelID: t.ID, Version: 0, Source: t.ConfigSrc, Config: TunnelConfig{Ingress: []IngressRule{}}, CreatedAt: now()}
	}
	reply(w, http.StatusOK, c, nil)
}

func (f *Fake) putConfig(w http.ResponseWriter, r *http.Request, st *State) {
	t := f.tunnel(w, r, st)
	if t == nil {
		return
	}
	// warp-routing is read-only, so it is not part of the request schema.
	var in struct {
		Config struct {
			Ingress       []IngressRule   `json:"ingress"`
			OriginRequest json.RawMessage `json:"originRequest,omitempty"`
		} `json:"config"`
	}
	if !decode(w, r, &in) {
		return
	}
	if t.ConfigSrc == "local" || t.ConfigSrc == "" && !t.RemoteConfig {
		fail(w, http.StatusBadRequest, 1055, "This tunnel is not configured remotely.")
		return
	}
	if err := checkIngress(in.Config.Ingress); err != "" {
		fail(w, http.StatusBadRequest, 1056, err)
		return
	}
	old := st.Configs[t.ID]
	v := 1
	var warp json.RawMessage
	if old != nil {
		v, warp = old.Version+1, old.Config.WarpRouting
	}
	cfg := TunnelConfig{Ingress: in.Config.Ingress, OriginRequest: in.Config.OriginRequest, WarpRouting: warp}
	c := &TunnelConfiguration{AccountID: f.AccountID, TunnelID: t.ID, Version: v, Source: "cloudflare", Config: cfg, CreatedAt: now()}
	st.Configs[t.ID] = c
	reply(w, http.StatusOK, c, nil)
}

// checkIngress applies cloudflared's ingress rules: at least one rule, every
// rule has a service, and the last one matches everything.
func checkIngress(rules []IngressRule) string {
	if len(rules) == 0 {
		return "At least one ingress rule needs to be defined for the tunnel."
	}
	for _, r := range rules {
		if r.Service == "" {
			return "Ingress rule is missing a service."
		}
		var or struct {
			Access *struct {
				AudTag   []string `json:"audTag"`
				TeamName string   `json:"teamName"`
			} `json:"access"`
		}
		if len(r.OriginRequest) > 0 {
			if err := json.Unmarshal(r.OriginRequest, &or); err != nil {
				return "Invalid originRequest."
			}
		}
		if a := or.Access; a != nil && (len(a.AudTag) == 0 || a.TeamName == "") {
			return "originRequest.access needs audTag and teamName."
		}
	}
	if last := rules[len(rules)-1]; last.Hostname != "" || last.Path != "" {
		return "The last ingress rule must match all URLs (i.e. it should not have a hostname or path filter)"
	}
	return ""
}

func (f *Fake) getToken(w http.ResponseWriter, r *http.Request, st *State) {
	if t := f.tunnel(w, r, st); t != nil {
		reply(w, http.StatusOK, t.Token, nil)
	}
}

// DNS.

func (f *Fake) listDNS(w http.ResponseWriter, r *http.Request, st *State) {
	q := r.URL.Query()
	var out []*DNSRecord
	for _, d := range sorted(st.DNS, func(d *DNSRecord) string { return d.Name + d.ID }) {
		if d.ZoneID != r.PathValue("zone") {
			continue
		}
		if n := q.Get("name.exact"); n != "" && d.Name != n {
			continue
		}
		if n := q.Get("name"); n != "" && d.Name != n {
			continue
		}
		out = append(out, d)
	}
	page(f, w, r, out, 100, true)
}

type dnsInput struct {
	Type     string          `json:"type"`
	Name     string          `json:"name"`
	Content  string          `json:"content"`
	Proxied  bool            `json:"proxied"`
	TTL      int             `json:"ttl"`
	Comment  string          `json:"comment"`
	Tags     []string        `json:"tags"`
	Settings json.RawMessage `json:"settings"`
}

func (f *Fake) zoneName(id string) string {
	for _, z := range f.st.Zones {
		if z.ID == id {
			return z.Name
		}
	}
	return ""
}

func (f *Fake) checkDNS(w http.ResponseWriter, zone, self string, in dnsInput, st *State) bool {
	zn := f.zoneName(zone)
	switch {
	case in.Type == "" || in.Name == "" || in.Content == "":
		fail(w, http.StatusBadRequest, 9000, "DNS record type, name and content are required.")
		return false
	case in.Name != zn && !strings.HasSuffix(in.Name, "."+zn):
		fail(w, http.StatusBadRequest, 9005, "Record name must be within the zone.")
		return false
	case in.TTL != 0 && in.TTL != 1 && (in.TTL < 60 || in.TTL > 86400):
		fail(w, http.StatusBadRequest, 9021, "Invalid TTL.")
		return false
	case in.Type == "CNAME" && in.Content == in.Name:
		fail(w, http.StatusBadRequest, 9007, "Content for CNAME record is invalid.")
		return false
	}
	for _, d := range st.DNS {
		if d.ID == self || d.ZoneID != zone || d.Name != in.Name {
			continue
		}
		if in.Type == "CNAME" || d.Type == "CNAME" {
			fail(w, http.StatusBadRequest, 81053, "An A, AAAA, or CNAME record with that host already exists.")
			return false
		}
	}
	return true
}

func (f *Fake) fromInput(id, zone string, in dnsInput) *DNSRecord {
	ttl := in.TTL
	if ttl == 0 || in.Proxied {
		ttl = 1
	}
	settings := in.Settings
	if settings == nil {
		settings = json.RawMessage(`{}`)
	}
	tags := in.Tags
	if tags == nil {
		tags = []string{}
	}
	return &DNSRecord{ID: id, ZoneID: zone, Type: in.Type, Name: in.Name, Content: in.Content, Proxied: in.Proxied, TTL: ttl, Comment: in.Comment, Tags: tags, Settings: settings}
}

func (f *Fake) createDNS(w http.ResponseWriter, r *http.Request, st *State) {
	var in dnsInput
	if !decode(w, r, &in) {
		return
	}
	zone := r.PathValue("zone")
	if !f.checkDNS(w, zone, "", in, st) {
		return
	}
	d := f.fromInput(hexID(16), zone, in)
	st.DNS[d.ID] = d
	reply(w, http.StatusOK, d, nil)
}

func (f *Fake) getDNS(w http.ResponseWriter, r *http.Request, st *State) {
	d := st.DNS[r.PathValue("id")]
	if d == nil || d.ZoneID != r.PathValue("zone") {
		fail(w, http.StatusNotFound, 81044, "Record does not exist.")
		return
	}
	reply(w, http.StatusOK, d, nil)
}

func (f *Fake) putDNS(w http.ResponseWriter, r *http.Request, st *State) {
	zone, id := r.PathValue("zone"), r.PathValue("id")
	if d := st.DNS[id]; d == nil || d.ZoneID != zone {
		fail(w, http.StatusNotFound, 81044, "Record does not exist.")
		return
	}
	var in dnsInput
	if !decode(w, r, &in) {
		return
	}
	if !f.checkDNS(w, zone, id, in, st) {
		return
	}
	d := f.fromInput(id, zone, in)
	st.DNS[id] = d
	reply(w, http.StatusOK, d, nil)
}

// Access applications.

type appInput struct {
	Name                   string            `json:"name"`
	Domain                 string            `json:"domain"`
	Type                   string            `json:"type"`
	SessionDuration        string            `json:"session_duration"`
	AllowedIdPs            []string          `json:"allowed_idps"`
	AutoRedirectToIdentity bool              `json:"auto_redirect_to_identity"`
	AppLauncherVisible     *bool             `json:"app_launcher_visible"`
	Destinations           []AppDestination  `json:"destinations"`
	Tags                   []string          `json:"tags"`
	SelfHostedDomains      []string          `json:"self_hosted_domains"`
	Policies               []json.RawMessage `json:"policies"`

	OptionsPreflightBypass   bool            `json:"options_preflight_bypass"`
	AllowAuthenticateViaWarp bool            `json:"allow_authenticate_via_warp"`
	AllowIframe              bool            `json:"allow_iframe"`
	CORSHeaders              json.RawMessage `json:"cors_headers"`
	HTTPOnlyCookieAttribute  *bool           `json:"http_only_cookie_attribute"`
	SameSiteCookieAttribute  string          `json:"same_site_cookie_attribute"`
	// Nil when the body leaves it out: the real API refuses the field, even
	// false, on an application with only public destinations.
	ClientlessIsolationURL *bool `json:"use_clientless_isolation_app_launcher_url"`
}

// appPolicy is a policy as an application response carries it.
type appPolicy struct {
	Policy
	Precedence int `json:"precedence"`
}

func (f *Fake) appJSON(a *App, st *State) any {
	pols := []appPolicy{}
	for _, l := range a.Links {
		if p := st.Policies[l.ID]; p != nil {
			pols = append(pols, appPolicy{Policy: *p, Precedence: l.Precedence})
		}
	}
	return struct {
		*App
		Policies []appPolicy `json:"policies"`
	}{a, pols}
}

func (f *Fake) listApps(w http.ResponseWriter, r *http.Request, st *State) {
	q := r.URL.Query()
	exact := q.Get("exact") == "true"
	var out []any
	for _, a := range sorted(st.Apps, func(a *App) string { return a.CreatedAt.Format(time.RFC3339Nano) + a.ID }) {
		if d := q.Get("domain"); d != "" && !matches(a.Domain, d, exact) {
			continue
		}
		if n := q.Get("name"); n != "" && !matches(a.Name, n, exact) {
			continue
		}
		out = append(out, f.appJSON(a, st))
	}
	page(f, w, r, out, 1000, false)
}

// matches is the apps list's filter: equality with exact=true, a substring
// otherwise.
func matches(have, want string, exact bool) bool {
	if exact {
		return have == want
	}
	return strings.Contains(have, want)
}

// hasPrivateDestination reports whether any destination is a private one
// (a network or hostname reached through WARP).
func hasPrivateDestination(ds []AppDestination) bool {
	for _, d := range ds {
		if d.Type == "private" {
			return true
		}
	}
	return false
}

func (f *Fake) appFromInput(w http.ResponseWriter, in appInput, st *State, a *App) bool {
	if in.Type != "self_hosted" {
		fail(w, http.StatusBadRequest, 12130, "only self_hosted applications are supported by this fake")
		return false
	}
	if in.Domain == "" {
		fail(w, http.StatusBadRequest, 12130, "domain is required")
		return false
	}
	if in.ClientlessIsolationURL != nil && !hasPrivateDestination(in.Destinations) {
		fail(w, http.StatusBadRequest, 12130, "use_clientless_isolation_app_launcher_url can only be enabled for apps with private destinations")
		return false
	}
	if in.SessionDuration != "" {
		if _, err := time.ParseDuration(in.SessionDuration); err != nil {
			fail(w, http.StatusBadRequest, 12130, "invalid session_duration")
			return false
		}
	} else {
		in.SessionDuration = "24h"
	}
	for _, id := range in.AllowedIdPs {
		if !slices.ContainsFunc(st.IdPs, func(p *IdentityProvider) bool { return p.ID == id }) {
			fail(w, http.StatusBadRequest, 12130, "identity provider "+id+" does not exist")
			return false
		}
	}
	if in.AutoRedirectToIdentity && len(in.AllowedIdPs) != 1 {
		fail(w, http.StatusBadRequest, 12130, "auto_redirect_to_identity requires exactly one identity provider")
		return false
	}
	if in.OptionsPreflightBypass && len(in.CORSHeaders) > 0 && string(in.CORSHeaders) != "null" {
		fail(w, http.StatusBadRequest, 12130, "options_preflight_bypass cannot be on while cors_headers is set")
		return false
	}
	var links []PolicyLink
	seen := map[int]bool{}
	for i, raw := range in.Policies {
		var l PolicyLink
		if err := json.Unmarshal(raw, &l); err != nil {
			var id string
			if json.Unmarshal(raw, &id) != nil {
				fail(w, http.StatusBadRequest, 12130, "invalid policy reference")
				return false
			}
			l = PolicyLink{ID: id}
		}
		if l.Precedence == 0 {
			l.Precedence = i + 1
		}
		p := st.Policies[l.ID]
		if p == nil || !p.Reusable {
			fail(w, http.StatusBadRequest, 12130, "policy "+l.ID+" does not exist or is not reusable")
			return false
		}
		if seen[l.Precedence] {
			fail(w, http.StatusBadRequest, 12130, "policy precedence must be unique")
			return false
		}
		seen[l.Precedence] = true
		links = append(links, l)
	}
	a.Name, a.Domain, a.Type, a.SessionDuration = in.Name, in.Domain, in.Type, in.SessionDuration
	a.AllowedIdPs = in.AllowedIdPs
	if a.AllowedIdPs == nil {
		a.AllowedIdPs = []string{}
	}
	a.AutoRedirectToIdentity = in.AutoRedirectToIdentity
	a.AppLauncherVisible = in.AppLauncherVisible == nil || *in.AppLauncherVisible
	a.Destinations = in.Destinations
	if len(a.Destinations) == 0 {
		a.Destinations = []AppDestination{{Type: "public", URI: in.Domain}}
	}
	a.Tags = in.Tags
	if a.Tags == nil {
		a.Tags = []string{}
	}
	a.SelfHostedDomains = in.SelfHostedDomains
	if a.SelfHostedDomains == nil {
		a.SelfHostedDomains = []string{}
	}
	// A replace resets every setting the body leaves out to its default.
	a.OptionsPreflightBypass = in.OptionsPreflightBypass
	a.AllowAuthenticateViaWarp = in.AllowAuthenticateViaWarp
	a.AllowIframe = in.AllowIframe
	a.CORSHeaders = in.CORSHeaders
	a.HTTPOnlyCookieAttribute = in.HTTPOnlyCookieAttribute == nil || *in.HTTPOnlyCookieAttribute
	a.SameSiteCookieAttribute = in.SameSiteCookieAttribute
	a.ClientlessIsolationURL = in.ClientlessIsolationURL != nil && *in.ClientlessIsolationURL
	a.Links = links
	a.UpdatedAt = now()
	return true
}

func (f *Fake) recount(st *State) {
	for _, p := range st.Policies {
		p.AppCount = 0
	}
	for _, a := range st.Apps {
		for _, l := range a.Links {
			if p := st.Policies[l.ID]; p != nil {
				p.AppCount++
			}
		}
	}
}

func (f *Fake) createApp(w http.ResponseWriter, r *http.Request, st *State) {
	var in appInput
	if !decode(w, r, &in) {
		return
	}
	a := &App{ID: uuid(), AUD: hexID(32), CreatedAt: now()}
	if !f.appFromInput(w, in, st, a) {
		return
	}
	st.Apps[a.ID] = a
	f.recount(st)
	reply(w, http.StatusCreated, f.appJSON(a, st), nil)
}

func (f *Fake) getApp(w http.ResponseWriter, r *http.Request, st *State) {
	a := st.Apps[r.PathValue("id")]
	if a == nil {
		fail(w, http.StatusNotFound, 12130, "access.api.error.not_found")
		return
	}
	reply(w, http.StatusOK, f.appJSON(a, st), nil)
}

func (f *Fake) putApp(w http.ResponseWriter, r *http.Request, st *State) {
	a := st.Apps[r.PathValue("id")]
	if a == nil {
		fail(w, http.StatusNotFound, 12130, "access.api.error.not_found")
		return
	}
	var in appInput
	if !decode(w, r, &in) {
		return
	}
	next := *a
	if !f.appFromInput(w, in, st, &next) {
		return
	}
	*a = next
	f.recount(st)
	reply(w, http.StatusOK, f.appJSON(a, st), nil)
}

// Reusable policies.

type policyInput struct {
	Name            string            `json:"name"`
	Decision        string            `json:"decision"`
	Include         []json.RawMessage `json:"include"`
	Exclude         []json.RawMessage `json:"exclude"`
	Require         []json.RawMessage `json:"require"`
	SessionDuration string            `json:"session_duration"`
}

func (f *Fake) listPolicies(w http.ResponseWriter, r *http.Request, st *State) {
	out := sorted(st.Policies, func(p *Policy) string { return p.CreatedAt.Format(time.RFC3339Nano) + p.ID })
	page(f, w, r, out, 100, false)
}

func (f *Fake) policyFromInput(w http.ResponseWriter, in policyInput, p *Policy) bool {
	switch {
	case in.Name == "":
		fail(w, http.StatusBadRequest, 12130, "name is required")
		return false
	case !slices.Contains([]string{"allow", "deny", "non_identity", "bypass"}, in.Decision):
		fail(w, http.StatusBadRequest, 12130, "invalid decision")
		return false
	case len(in.Include) == 0:
		fail(w, http.StatusBadRequest, 12130, "include must have at least one rule")
		return false
	}
	for _, rule := range slices.Concat(in.Include, in.Exclude, in.Require) {
		var m map[string]json.RawMessage
		if json.Unmarshal(rule, &m) != nil || len(m) != 1 {
			fail(w, http.StatusBadRequest, 12130, "a rule is an object with exactly one key")
			return false
		}
	}
	if in.SessionDuration != "" {
		if _, err := time.ParseDuration(in.SessionDuration); err != nil {
			fail(w, http.StatusBadRequest, 12130, "invalid session_duration")
			return false
		}
	}
	nz := func(l []json.RawMessage) []json.RawMessage {
		if l == nil {
			return []json.RawMessage{}
		}
		return l
	}
	p.Name, p.Decision, p.SessionDuration = in.Name, in.Decision, in.SessionDuration
	p.Include, p.Exclude, p.Require = nz(in.Include), nz(in.Exclude), nz(in.Require)
	p.UpdatedAt = now()
	return true
}

func (f *Fake) createPolicy(w http.ResponseWriter, r *http.Request, st *State) {
	var in policyInput
	if !decode(w, r, &in) {
		return
	}
	p := &Policy{ID: uuid(), AccountID: f.AccountID, Reusable: true, CreatedAt: now()}
	if !f.policyFromInput(w, in, p) {
		return
	}
	st.Policies[p.ID] = p
	reply(w, http.StatusCreated, p, nil)
}

func (f *Fake) getPolicy(w http.ResponseWriter, r *http.Request, st *State) {
	p := st.Policies[r.PathValue("id")]
	if p == nil {
		fail(w, http.StatusNotFound, 12130, "access.api.error.not_found")
		return
	}
	reply(w, http.StatusOK, p, nil)
}

func (f *Fake) putPolicy(w http.ResponseWriter, r *http.Request, st *State) {
	p := st.Policies[r.PathValue("id")]
	if p == nil {
		fail(w, http.StatusNotFound, 12130, "access.api.error.not_found")
		return
	}
	var in policyInput
	if !decode(w, r, &in) {
		return
	}
	next := *p
	if !f.policyFromInput(w, in, &next) {
		return
	}
	*p = next
	reply(w, http.StatusOK, p, nil)
}

// Identity providers, service tokens and the organization.

func (f *Fake) listIdPs(w http.ResponseWriter, r *http.Request, st *State) {
	page(f, w, r, st.IdPs, 200, false)
}

func (f *Fake) listServiceTokens(w http.ResponseWriter, r *http.Request, st *State) {
	q := r.URL.Query()
	var out []*ServiceToken
	for _, t := range sorted(st.ServiceTokens, func(t *ServiceToken) string { return t.CreatedAt.Format(time.RFC3339Nano) + t.ID }) {
		if n := q.Get("name"); n != "" && t.Name != n {
			continue
		}
		out = append(out, t)
	}
	page(f, w, r, out, 1000, false)
}

func (f *Fake) createServiceToken(w http.ResponseWriter, r *http.Request, st *State) {
	var in struct {
		Name                string `json:"name"`
		Duration            string `json:"duration"`
		ClientSecretVersion int    `json:"client_secret_version"`
		Enabled             *bool  `json:"enabled"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Name == "" {
		fail(w, http.StatusBadRequest, 12130, "name is required")
		return
	}
	if in.Duration == "" {
		in.Duration = "8760h"
	}
	exp := now().Add(100 * 365 * 24 * time.Hour)
	if in.Duration != "forever" {
		d, err := time.ParseDuration(in.Duration)
		if err != nil {
			fail(w, http.StatusBadRequest, 12130, "invalid duration")
			return
		}
		exp = now().Add(d)
	}
	t := f.addServiceToken(in.Name, in.Duration, exp)
	reply(w, http.StatusCreated, struct {
		*ServiceToken
		ClientSecret string `json:"client_secret"`
	}{t, t.Secret}, nil)
}

// AddServiceToken creates a service token as something other than the tool
// would, and returns it.
func (f *Fake) AddServiceToken(name string) *ServiceToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addServiceToken(name, "8760h", now().Add(8760*time.Hour))
}

func (f *Fake) addServiceToken(name, duration string, exp time.Time) *ServiceToken {
	t := &ServiceToken{ID: uuid(), Name: name, ClientID: hexID(16) + ".access", Duration: duration, Enabled: true, ExpiresAt: exp, CreatedAt: now(), UpdatedAt: now()}
	t.Secret = hexID(32)
	f.st.ServiceTokens[t.ID] = t
	return t
}

func (f *Fake) getServiceToken(w http.ResponseWriter, r *http.Request, st *State) {
	t := st.ServiceTokens[r.PathValue("id")]
	if t == nil {
		fail(w, http.StatusNotFound, 12130, "access.api.error.not_found")
		return
	}
	reply(w, http.StatusOK, t, nil)
}

func (f *Fake) getOrg(w http.ResponseWriter, r *http.Request, st *State) {
	if st.Org == nil {
		fail(w, http.StatusNotFound, 12130, "access.api.error.not_found")
		return
	}
	reply(w, http.StatusOK, st.Org, nil)
}

var clock = struct {
	sync.Mutex
	t time.Time
}{t: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}

// now is strictly increasing, so lists ordered by creation time are stable.
func now() time.Time {
	clock.Lock()
	defer clock.Unlock()
	clock.t = clock.t.Add(time.Millisecond)
	return clock.t
}

func hexID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func uuid() string {
	h := hexID(16)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}
