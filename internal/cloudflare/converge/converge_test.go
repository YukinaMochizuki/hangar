package converge

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare/cftest"
)

const (
	testToken   = "test-api-token-5f1c2d"
	testAccount = "0123456789abcdef0123456789abcdef"
	testZone    = "hangar.example"
	testEPZone  = "hangar-user.example"
	testIPv4    = "192.0.2.10"
	testIPv6    = "2001:db8:1::1"
)

type env struct {
	t      *testing.T
	f      *cftest.Fake
	epZone *cftest.Zone
	cfg    Config
	dir    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := cftest.New(testToken, testAccount, testZone)
	t.Cleanup(f.Close)
	ep := f.AddZone(testEPZone)
	cfg := Config{
		AccountID: testAccount, Zone: testZone,
		Addresses: Addresses{IPv4: testIPv4, IPv6: testIPv6},
		Endpoints: &EndpointsConfig{Zone: testEPZone},
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, f: f, epZone: ep, cfg: cfg, dir: filepath.Join(t.TempDir(), "out")}
}

func (e *env) zoneID() string {
	var id string
	e.f.Mutate(func(s *cftest.State) { id = s.Zones[0].ID })
	return id
}

func (e *env) client(readOnly bool) *cloudflare.Client {
	opts := []cloudflare.Option{
		cloudflare.WithBaseURL(e.f.URL()),
		cloudflare.WithSleep(func(context.Context, time.Duration) error { return nil }),
	}
	if readOnly {
		opts = append(opts, cloudflare.ReadOnly())
	}
	c, err := cloudflare.New(testToken, opts...)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) run(apply bool) (*Result, error) {
	e.t.Helper()
	eng := &Engine{Client: e.client(!apply), Config: e.cfg, Dir: e.dir, Apply: apply}
	return eng.Run(context.Background())
}

func (e *env) mustRun(apply bool) *Result {
	e.t.Helper()
	res, err := e.run(apply)
	if err != nil {
		e.t.Fatalf("run(apply=%t): %v", apply, err)
	}
	return res
}

func (e *env) state() State {
	e.t.Helper()
	s, _, err := loadState(e.dir, e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// records returns the fake's records as "name type content proxied ttl".
func (e *env) records() []string {
	var out []string
	e.f.Mutate(func(s *cftest.State) {
		for _, d := range s.DNS {
			out = append(out, strings.Join([]string{d.Name, d.Type, d.Content, map[bool]string{true: "proxied", false: "dns-only"}[d.Proxied], formatTTL(d.TTL)}, " "))
		}
	})
	slices.Sort(out)
	return out
}

func objects(res *Result) []string {
	var out []string
	for _, c := range res.Changes {
		out = append(out, c.Action+" "+c.Object)
	}
	return out
}

var wantRecords = []string{
	"*.hangar-user.example A 192.0.2.10 dns-only auto",
	"*.hangar-user.example AAAA 2001:db8:1::1 dns-only auto",
	"api.hangar.example A 192.0.2.10 dns-only auto",
	"api.hangar.example AAAA 2001:db8:1::1 dns-only auto",
	"console.hangar.example A 192.0.2.10 dns-only auto",
	"console.hangar.example AAAA 2001:db8:1::1 dns-only auto",
}

func TestPlanFromEmptyChangesNothing(t *testing.T) {
	e := newEnv(t)
	res := e.mustRun(false)
	want := []string{
		"create DNS A record api.hangar.example", "create DNS AAAA record api.hangar.example",
		"create DNS A record console.hangar.example", "create DNS AAAA record console.hangar.example",
		"create DNS A record *.hangar-user.example", "create DNS AAAA record *.hangar-user.example",
	}
	if got := objects(res); !slices.Equal(got, want) {
		t.Fatalf("plan:\n%v\nwant\n%v", got, want)
	}
	if w := e.f.Writes(); len(w) != 0 {
		t.Fatalf("plan wrote: %v", w)
	}
	if _, err := os.Stat(e.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan made the output directory: %v", err)
	}
}

func TestApplyFromEmptyThenNoChanges(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	if got := e.records(); !slices.Equal(got, wantRecords) {
		t.Fatalf("records:\n%v\nwant\n%v", got, wantRecords)
	}
	var comments []string
	e.f.Mutate(func(s *cftest.State) {
		for _, d := range s.DNS {
			comments = append(comments, d.Comment)
		}
	})
	for _, c := range comments {
		if c != Marker {
			t.Fatalf("comment %q", c)
		}
	}
	st := e.state()
	if len(st.Records) != 6 || st.Version != 2 || st.Zone != testZone || st.EndpointsZone != testEPZone {
		t.Fatalf("state: %+v", st)
	}
	fi, err := os.Stat(filepath.Join(e.dir, StateFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", fi, err)
	}
	before := len(e.f.Writes())
	if res := e.mustRun(true); len(res.Changes) != 0 {
		t.Fatalf("second apply: %v", objects(res))
	}
	if n := len(e.f.Writes()) - before; n != 0 {
		t.Fatalf("second apply sent %d writes", n)
	}
	if res := e.mustRun(false); len(res.Changes) != 0 {
		t.Fatalf("verify after apply: %v", objects(res))
	}
}

func TestWithoutEndpointsOnlyTheControlZone(t *testing.T) {
	e := newEnv(t)
	e.cfg.Endpoints = nil
	e.mustRun(true)
	if got := e.records(); !slices.Equal(got, wantRecords[2:]) {
		t.Fatalf("records: %v", got)
	}
}

// v1 is a state the tool wrote while it ran the tunnel and Access.
func (e *env) writeV1(apiID, epID string) {
	e.t.Helper()
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	s := `{
  "version": 1,
  "account_id": "` + testAccount + `",
  "tunnel_id": "11111111-2222-3333-4444-555555555555",
  "dns_record_id": "` + apiID + `",
  "zone_id": "` + e.zoneID() + `",
  "hostname": "api.hangar.example",
  "app_id": "aaaaaaaa-2222-3333-4444-555555555555",
  "policies": {"emails": "pe", "service_tokens": "pt"},
  "service_token_id": "ssssssss-2222-3333-4444-555555555555"`
	if epID != "" {
		s += `,
  "endpoints_zone": "` + testEPZone + `",
  "endpoints_zone_id": "` + e.epZone.ID + `",
  "endpoints_dns_record_id": "` + epID + `"`
	}
	s += "\n}\n"
	if err := os.WriteFile(filepath.Join(e.dir, StateFile), []byte(s), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func TestTakesOverTheTunnelsCNAMEsInPlace(t *testing.T) {
	e := newEnv(t)
	tunnelName := "11111111-2222-3333-4444-555555555555.cfargotunnel.com"
	api := e.f.AddRecord(e.zoneID(), cftest.DNSRecord{Type: "CNAME", Name: "api.hangar.example", Content: tunnelName, Proxied: true, Comment: Marker})
	ep := e.f.AddRecord(e.epZone.ID, cftest.DNSRecord{Type: "CNAME", Name: "*.hangar-user.example", Content: tunnelName, Proxied: true, Comment: Marker})
	e.writeV1(api.ID, ep.ID)
	before, _ := os.ReadFile(filepath.Join(e.dir, StateFile))

	res := e.mustRun(false)
	got := objects(res)
	want := []string{
		"write state.json",
		"update DNS A record api.hangar.example", "create DNS AAAA record api.hangar.example",
		"create DNS A record console.hangar.example", "create DNS AAAA record console.hangar.example",
		"update DNS A record *.hangar-user.example", "create DNS AAAA record *.hangar-user.example",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("plan:\n%v\nwant\n%v", got, want)
	}
	if after, _ := os.ReadFile(filepath.Join(e.dir, StateFile)); string(after) != string(before) {
		t.Fatal("plan rewrote the v1 state")
	}
	// The retired tunnel and Access objects are named, to be deleted by hand.
	notes := strings.Join(res.Notes, "\n")
	for _, id := range []string{"11111111-2222-3333-4444-555555555555", "aaaaaaaa-2222-3333-4444-555555555555", "ssssssss-2222-3333-4444-555555555555", "pe", "pt"} {
		if !strings.Contains(notes, id) {
			t.Errorf("notes miss %s:\n%s", id, notes)
		}
	}

	e.mustRun(true)
	if got := e.records(); !slices.Equal(got, wantRecords) {
		t.Fatalf("records:\n%v\nwant\n%v", got, wantRecords)
	}
	st := e.state()
	if st.Records["api.hangar.example A"] != api.ID || st.Records["*.hangar-user.example A"] != ep.ID {
		t.Fatalf("the CNAMEs were not changed in place: %+v", st.Records)
	}
	if st.Retired["tunnel"] == "" || st.Retired["access_app"] == "" {
		t.Fatalf("retired lost: %+v", st.Retired)
	}
	if res := e.mustRun(false); len(res.Changes) != 0 || len(res.Notes) == 0 {
		t.Fatalf("verify after the takeover: %v, notes %v", objects(res), res.Notes)
	}
	// Nothing is ever deleted.
	for _, w := range e.f.Writes() {
		if w.Method == http.MethodDelete {
			t.Fatalf("deleted: %+v", w)
		}
	}
}

func TestRefusedTypeChangeLeavesTheRecord(t *testing.T) {
	e := newEnv(t)
	e.cfg.Endpoints = nil
	api := e.f.AddRecord(e.zoneID(), cftest.DNSRecord{Type: "CNAME", Name: "api.hangar.example", Content: "t.cfargotunnel.com", Proxied: true, Comment: Marker})
	e.writeV1(api.ID, "")
	e.f.RefuseTypeChange = true
	_, err := e.run(true)
	if err == nil || !strings.Contains(err.Error(), "from CNAME to A") || !strings.Contains(err.Error(), "docs/runbook/cloudflare.md") {
		t.Fatalf("apply: %v", err)
	}
	if got := e.records(); !slices.Equal(got, []string{"api.hangar.example CNAME t.cfargotunnel.com proxied auto"}) {
		t.Fatalf("records: %v", got)
	}
	// The fallback: the CNAME deleted by hand, the next apply makes the A.
	e.f.Mutate(func(s *cftest.State) { delete(s.DNS, api.ID) })
	e.mustRun(true)
	if got := e.records(); !slices.Equal(got, wantRecords[2:]) {
		t.Fatalf("records after the fallback: %v", got)
	}
}

func TestVerifyFindsDriftAndApplyFixesIt(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	st := e.state()
	e.f.Mutate(func(s *cftest.State) {
		d := s.DNS[st.Records["api.hangar.example A"]]
		d.Proxied, d.Content = true, "198.51.100.1"
		s.DNS[st.Records["console.hangar.example AAAA"]].TTL = 3600
		s.DNS[st.Records["*.hangar-user.example A"]].Comment = ""
		delete(s.DNS, st.Records["*.hangar-user.example AAAA"])
	})
	res := e.mustRun(false)
	want := []string{
		"update DNS A record api.hangar.example", "update DNS AAAA record console.hangar.example",
		"update DNS A record *.hangar-user.example", "create DNS AAAA record *.hangar-user.example",
	}
	if got := objects(res); !slices.Equal(got, want) {
		t.Fatalf("verify:\n%v\nwant\n%v", got, want)
	}
	var fields []string
	for _, f := range res.Changes[0].Fields {
		fields = append(fields, f.Name)
	}
	if !slices.Equal(fields, []string{"content", "proxied"}) {
		t.Fatalf("fields %v", fields)
	}
	e.mustRun(true)
	if got := e.records(); !slices.Equal(got, wantRecords) {
		t.Fatalf("records: %v", got)
	}
}

func TestRefusesRecordsItDidNotCreate(t *testing.T) {
	for _, typ := range []string{"A", "AAAA", "CNAME"} {
		t.Run(typ, func(t *testing.T) {
			e := newEnv(t)
			content := map[string]string{"A": "198.51.100.7", "AAAA": "2001:db8::7", "CNAME": "elsewhere.example"}[typ]
			foreign := e.f.AddRecord(e.zoneID(), cftest.DNSRecord{Type: typ, Name: "console.hangar.example", Content: content})
			_, err := e.run(true)
			if !errors.Is(err, ErrNotOwned) || !strings.Contains(err.Error(), foreign.ID) {
				t.Fatalf("apply: %v", err)
			}
			if w := e.f.Writes(); len(w) != 0 {
				t.Fatalf("wrote before refusing: %v", w)
			}
		})
	}
	// Records of other types at the names are not the tool's business.
	e := newEnv(t)
	e.f.AddRecord(e.zoneID(), cftest.DNSRecord{Type: "TXT", Name: "api.hangar.example", Content: "v=spf1 -all"})
	e.mustRun(true)
}

func TestLostStateRefusesEverything(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	if err := os.Remove(filepath.Join(e.dir, StateFile)); err != nil {
		t.Fatal(err)
	}
	_, err := e.run(false)
	if !errors.Is(err, ErrNotOwned) || strings.Count(err.Error(), "exists but is not in") != 6 {
		t.Fatalf("plan without state: %v", err)
	}
}

func TestStateMismatches(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	for name, mut := range map[string]func(*Config){
		"account":        func(c *Config) { c.AccountID = strings.Repeat("e", 32) },
		"endpoints zone": func(c *Config) { c.Endpoints = nil },
	} {
		cfg := e.cfg
		ep := *e.cfg.Endpoints
		cfg.Endpoints = &ep
		mut(&cfg)
		eng := &Engine{Client: e.client(true), Config: cfg, Dir: e.dir}
		if _, err := eng.Run(context.Background()); err == nil {
			t.Errorf("%s changed: accepted", name)
		}
	}
	// A v1 state for another API hostname is refused.
	e = newEnv(t)
	e.writeV1("x", "")
	b, _ := os.ReadFile(filepath.Join(e.dir, StateFile))
	_ = os.WriteFile(filepath.Join(e.dir, StateFile), []byte(strings.Replace(string(b), `"hostname": "api.hangar.example"`, `"hostname": "hangar-api.hangar.example"`, 1)), 0o600)
	if _, err := e.run(false); err == nil || !strings.Contains(err.Error(), "hangar-api.hangar.example") {
		t.Fatalf("v1 for another hostname: %v", err)
	}
}

func TestOpenOutputDirectoryIsRefused(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(false); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("plan: %v", err)
	}
	if n := len(e.f.Requests()); n != 0 {
		t.Fatalf("%d requests before refusing", n)
	}
}

func TestListsSpanPagesAndRetries(t *testing.T) {
	e := newEnv(t)
	e.f.MaxPerPage = 1
	for i := range 3 {
		e.f.AddRecord(e.zoneID(), cftest.DNSRecord{Type: "TXT", Name: "api.hangar.example", Content: "t" + string(rune('0'+i))})
	}
	e.f.Fail("GET", 503, 2, "")
	e.f.Fail("POST", 429, 1, "1")
	e.mustRun(true)
	// A foreign record that sorts last, past several pages, is still found.
	foreign := "ffffffffffffffffffffffffffffffff"
	e.f.Mutate(func(s *cftest.State) {
		s.DNS[foreign] = &cftest.DNSRecord{ID: foreign, ZoneID: e.epZone.ID, Type: "AAAA", Name: "*.hangar-user.example", Content: "2001:db8::99", TTL: 1}
	})
	if _, err := e.run(false); !errors.Is(err, ErrNotOwned) || !strings.Contains(err.Error(), foreign) {
		t.Fatalf("plan: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := `account_id: 0123456789abcdef0123456789abcdef
zone: Hangar.Example.
addresses:
  ipv4: 192.0.2.10
  ipv6: 2001:DB8:1::1
endpoints:
  zone: Hangar-User.Example
`
	c, err := LoadConfig(write(good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Zone != "hangar.example" || c.Addresses.IPv6 != "2001:db8:1::1" || c.Endpoints.Zone != "hangar-user.example" || c.APIHostname() != "api.hangar.example" {
		t.Fatalf("normalized: %+v", c)
	}
	for name, bad := range map[string]string{
		"unknown key":     good + "api_token: x\n",
		"old tunnel key":  good + "tunnel_name: hangar\n",
		"bad account":     strings.Replace(good, "0123456789abcdef0123456789abcdef", "nope", 1),
		"no ipv4":         strings.Replace(good, "  ipv4: 192.0.2.10\n", "", 1),
		"private ipv4":    strings.Replace(good, "192.0.2.10", "10.0.0.1", 1),
		"ipv6 as ipv4":    strings.Replace(good, "192.0.2.10", "2001:db8::1", 1),
		"no ipv6":         strings.Replace(good, "  ipv6: 2001:DB8:1::1\n", "", 1),
		"ula ipv6":        strings.Replace(good, "2001:DB8:1::1", "fd00::1", 1),
		"same zone":       strings.Replace(good, "Hangar-User.Example", "user.hangar.example", 1),
		"wildcard zone":   strings.Replace(good, "zone: Hangar.Example.", "zone: '*.hangar.example'", 1),
		"no zone":         strings.Replace(good, "zone: Hangar.Example.\n", "", 1),
		"empty endpoints": strings.Replace(good, "  zone: Hangar-User.Example\n", "  zone: ''\n", 1),
	} {
		if _, err := LoadConfig(write(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	c, err := LoadConfig("../../../deploy/cloudflare/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Zone != "example.com" || c.Endpoints == nil || c.Endpoints.Zone != "example.dev" {
		t.Fatalf("example config holds more than placeholders: %+v", c)
	}
}
