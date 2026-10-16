// Package cftest is an in-memory fake of the Cloudflare API v4 endpoints
// package cloudflare calls, served over httptest: zones and DNS records. It
// keeps the API's wire shapes (its own types, not the client's), its
// envelope, status codes, paging and the validation that matters to
// callers, and it rejects request fields the API does not document. It is
// not a model of Cloudflare beyond those endpoints.
package cftest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// State is everything the fake holds. Tests change it through Fake.Mutate
// to stand in for edits made elsewhere (the dashboard, another tool).
type State struct {
	Zones []*Zone
	DNS   map[string]*DNSRecord
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
	// RefuseTypeChange makes a PUT that changes a record's type fail, as
	// the API might: the fake accepts it otherwise.
	RefuseTypeChange bool

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

// New starts a fake for one account accepting one API token, holding one
// zone.
func New(token, accountID, zone string) *Fake {
	f := &Fake{Token: token, AccountID: accountID, mux: http.NewServeMux()}
	z := &Zone{ID: hexID(16), Name: zone, Status: "active"}
	z.Account.ID = accountID
	f.st = &State{Zones: []*Zone{z}, DNS: map[string]*DNSRecord{}}
	f.routes()
	f.Server = httptest.NewServer(f)
	return f
}

// AddZone adds another active zone of the account, and returns it.
func (f *Fake) AddZone(name string) *Zone {
	f.mu.Lock()
	defer f.mu.Unlock()
	z := &Zone{ID: hexID(16), Name: name, Status: "active"}
	z.Account.ID = f.AccountID
	f.st.Zones = append(f.st.Zones, z)
	return z
}

// AddRecord creates a record as something other than the tool would, such
// as the dashboard, without the API's checks, and returns it.
func (f *Fake) AddRecord(zoneID string, d DNSRecord) *DNSRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	d.ID, d.ZoneID = hexID(16), zoneID
	if d.TTL == 0 {
		d.TTL = 1
	}
	if d.Tags == nil {
		d.Tags = []string{}
	}
	if d.Settings == nil {
		d.Settings = json.RawMessage(`{}`)
	}
	f.st.DNS[d.ID] = &d
	return &d
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

	zone := v4 + "/zones/{zone}"
	f.handle("GET "+zone+"/dns_records", f.listDNS)
	f.handle("POST "+zone+"/dns_records", f.createDNS)
	f.handle("GET "+zone+"/dns_records/{id}", f.getDNS)
	f.handle("PUT "+zone+"/dns_records/{id}", f.putDNS)

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

// page answers a list with the API's paging.
func page[T any](f *Fake, w http.ResponseWriter, r *http.Request, items []T, defPer int) {
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
	info := map[string]int{"page": p, "per_page": per, "count": len(out), "total_count": len(items), "total_pages": (len(items) + per - 1) / per}
	reply(w, http.StatusOK, out, info)
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
	page(f, w, r, out, 20)
}

// DNS.

func (f *Fake) listDNS(w http.ResponseWriter, r *http.Request, st *State) {
	q := r.URL.Query()
	var out []*DNSRecord
	for _, d := range st.DNS {
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
	slices.SortFunc(out, func(a, b *DNSRecord) int { return strings.Compare(a.Name+a.ID, b.Name+b.ID) })
	page(f, w, r, out, 100)
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
	addr, addrErr := netip.ParseAddr(in.Content)
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
	case in.Type == "A" && (addrErr != nil || !addr.Is4()):
		fail(w, http.StatusBadRequest, 9005, "Content for A record must be a valid IPv4 address.")
		return false
	case in.Type == "AAAA" && (addrErr != nil || !addr.Is6() || addr.Is4In6()):
		fail(w, http.StatusBadRequest, 9006, "Content for AAAA record must be a valid IPv6 address.")
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
		if d.Type == in.Type && d.Content == in.Content {
			fail(w, http.StatusBadRequest, 81058, "An identical record already exists.")
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
	old := st.DNS[id]
	if old == nil || old.ZoneID != zone {
		fail(w, http.StatusNotFound, 81044, "Record does not exist.")
		return
	}
	var in dnsInput
	if !decode(w, r, &in) {
		return
	}
	if f.RefuseTypeChange && in.Type != old.Type {
		fail(w, http.StatusBadRequest, 9000, "DNS record type cannot be changed.")
		return
	}
	if !f.checkDNS(w, zone, id, in, st) {
		return
	}
	d := f.fromInput(id, zone, in)
	st.DNS[id] = d
	reply(w, http.StatusOK, d, nil)
}

func hexID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
