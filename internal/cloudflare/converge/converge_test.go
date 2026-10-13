package converge

import (
	"context"
	"encoding/json"
	"errors"
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
	testHost    = "api.hangar.example"
)

type env struct {
	t   *testing.T
	f   *cftest.Fake
	cfg Config
	dir string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := cftest.New(testToken, testAccount, testZone)
	t.Cleanup(f.Close)
	cfg := Config{
		AccountID:         testAccount,
		Zone:              testZone,
		Hostname:          testHost,
		TunnelName:        "hangar",
		AllowedEmails:     []string{"Owner@Example.com", "ops@example.com"},
		IdentityProviders: IdPConfig{Google: "Google"},
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, f: f, cfg: cfg, dir: filepath.Join(t.TempDir(), "out")}
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

// writesDuring returns the non-GET requests fn caused.
func (e *env) writesDuring(fn func()) []cftest.Request {
	before := len(e.f.Writes())
	fn()
	return e.f.Writes()[before:]
}

// snapshot returns every file in the output directory with its content.
func (e *env) snapshot() map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(e.dir)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		e.t.Fatal(err)
	}
	for _, ent := range entries {
		b, err := os.ReadFile(filepath.Join(e.dir, ent.Name()))
		if err != nil {
			e.t.Fatal(err)
		}
		out[ent.Name()] = string(b)
	}
	return out
}

func (e *env) state() State {
	s, err := loadState(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func objects(res *Result) []string {
	var out []string
	for _, c := range res.Changes {
		out = append(out, c.Action+" "+c.Object)
	}
	return out
}

func TestPlanFromEmptyChangesNothing(t *testing.T) {
	e := newEnv(t)
	var res *Result
	writes := e.writesDuring(func() { res = e.mustRun(false) })
	if len(writes) != 0 {
		t.Fatalf("plan sent writes: %v", writes)
	}
	if _, err := os.Stat(e.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan created the output directory: %v", err)
	}
	want := []string{
		`create Access policy "` + testHost + `: allow listed emails"`,
		`create Access policy "` + testHost + `: service tokens"`,
		"create Access application " + testHost,
		`create tunnel "hangar"`,
		`update tunnel "hangar" configuration`,
		"create DNS record " + testHost,
		`create service token "hangar-smoke"`,
		"write output " + SmokeTokenEnv,
		"write output " + CloudflaredEnv,
		"write output " + AccessEnv,
	}
	if got := objects(res); !slices.Equal(got, want) {
		t.Fatalf("plan:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestApplyFromEmpty(t *testing.T) {
	e := newEnv(t)
	res := e.mustRun(true)
	if len(res.Changes) != 10 {
		t.Fatalf("apply made %d changes: %v", len(res.Changes), objects(res))
	}
	st := e.state()
	var google, otp string
	e.f.Mutate(func(s *cftest.State) {
		for _, p := range s.IdPs {
			if p.Type == "google" {
				google = p.ID
			} else {
				otp = p.ID
			}
		}
		if len(s.Tunnels) != 1 {
			t.Fatalf("tunnels: %d", len(s.Tunnels))
		}
		tun := s.Tunnels[st.TunnelID]
		if tun == nil || tun.Name != "hangar" || tun.ConfigSrc != "cloudflare" {
			t.Fatalf("tunnel: %+v", tun)
		}
		ing := s.Configs[tun.ID].Config.Ingress
		if len(ing) != 2 || ing[0].Hostname != testHost || ing[0].Service != "http://127.0.0.1:8080" ||
			ing[1].Hostname != "" || ing[1].Service != "http_status:404" || len(ing[1].OriginRequest) != 0 {
			t.Fatalf("ingress: %+v", ing)
		}
		wantAccess := `{"access":{"required":true,"teamName":"hangar-test","audTag":["` + s.Apps[st.AppID].AUD + `"]}}`
		if got := string(ing[0].OriginRequest); got != wantAccess {
			t.Fatalf("originRequest %s, want %s", got, wantAccess)
		}
		d := s.DNS[st.DNSRecordID]
		if d == nil || d.Type != "CNAME" || d.Name != testHost || d.Content != tun.ID+".cfargotunnel.com" || !d.Proxied {
			t.Fatalf("dns: %+v", d)
		}
		app := s.Apps[st.AppID]
		if app == nil || app.Type != "self_hosted" || app.Domain != testHost || app.SessionDuration != "24h" ||
			app.AutoRedirectToIdentity || !slices.Equal(app.AllowedIdPs, []string{google, otp}) {
			t.Fatalf("app: %+v", app)
		}
		want := []cftest.PolicyLink{{ID: st.Policies[emailsPolicy], Precedence: 1}, {ID: st.Policies[tokensPolicy], Precedence: 2}}
		if !slices.Equal(app.Links, want) {
			t.Fatalf("app policies %v, want %v", app.Links, want)
		}
		emails := s.Policies[st.Policies[emailsPolicy]]
		if emails.Decision != "allow" || len(emails.Include) != 2 ||
			string(emails.Include[0]) != `{"email":{"email":"owner@example.com"}}` || len(emails.Exclude)+len(emails.Require) != 0 {
			t.Fatalf("emails policy: %+v", emails)
		}
		tokens := s.Policies[st.Policies[tokensPolicy]]
		if tokens.Decision != "non_identity" || len(tokens.Include) != 1 || string(tokens.Include[0]) != `{"any_valid_service_token":{}}` {
			t.Fatalf("service token policy: %+v", tokens)
		}
		if len(s.ServiceTokens) != 1 || s.ServiceTokens[st.ServiceTokenID] == nil {
			t.Fatalf("service tokens: %v", s.ServiceTokens)
		}
	})

	files := e.snapshot()
	var tunnelToken, aud, clientID, secret string
	e.f.Mutate(func(s *cftest.State) {
		tunnelToken = s.Tunnels[st.TunnelID].Token
		aud = s.Apps[st.AppID].AUD
		clientID = s.ServiceTokens[st.ServiceTokenID].ClientID
		secret = s.ServiceTokens[st.ServiceTokenID].Secret
	})
	wantFiles := map[string]string{
		CloudflaredEnv: "TUNNEL_TOKEN=" + tunnelToken + "\n",
		AccessEnv:      "HANGAR_CF_TEAM_DOMAIN=https://hangar-test.cloudflareaccess.com\nHANGAR_CF_AUDIENCE=" + aud + "\n",
		SmokeTokenEnv:  "HANGAR_CF_CLIENT_ID=" + clientID + "\nHANGAR_CF_CLIENT_SECRET=" + secret + "\n",
	}
	for name, want := range wantFiles {
		if files[name] != want {
			t.Errorf("%s = %q, want %q", name, files[name], want)
		}
	}
	if len(files) != 4 {
		t.Errorf("output directory holds %v, want the three outputs and the state", slices.Sorted(mapKeys(files)))
	}
	for name := range files {
		fi, err := os.Stat(filepath.Join(e.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", name, fi.Mode().Perm())
		}
	}
	if fi, _ := os.Stat(e.dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("output directory has mode %v, want 0700", fi.Mode().Perm())
	}
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func TestSecondApplyHasNoChanges(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	before := e.snapshot()
	var res *Result
	writes := e.writesDuring(func() { res = e.mustRun(true) })
	if len(res.Changes) != 0 || len(writes) != 0 {
		t.Fatalf("second apply: changes %v, writes %v", objects(res), writes)
	}
	if after := e.snapshot(); !mapsEqual(before, after) {
		t.Fatal("second apply changed the output directory")
	}
	if res := e.mustRun(false); len(res.Changes) != 0 || len(res.Notes) != 0 {
		t.Fatalf("verify after apply: %v %v", objects(res), res.Notes)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// drifts are edits made outside the tool, each with the change verify must
// report.
var drifts = []struct {
	name   string
	edit   func(e *env, st State, s *cftest.State)
	object string
	field  string
}{
	{"ingress service changed", func(e *env, st State, s *cftest.State) {
		s.Configs[st.TunnelID].Config.Ingress[0].Service = "http://127.0.0.1:9999"
	}, `tunnel "hangar" configuration`, "ingress"},
	{"ingress rule added", func(e *env, st State, s *cftest.State) {
		c := s.Configs[st.TunnelID]
		c.Config.Ingress = slices.Insert(c.Config.Ingress, 1, cftest.IngressRule{Hostname: "admin." + testZone, Service: "unix:///run/hangar/admin.sock"})
	}, `tunnel "hangar" configuration`, "ingress"},
	{"ingress origin request set", func(e *env, st State, s *cftest.State) {
		s.Configs[st.TunnelID].Config.Ingress[0].OriginRequest = json.RawMessage(`{"httpHostHeader":"other"}`)
	}, `tunnel "hangar" configuration`, "ingress"},
	{"origin access removed", func(e *env, st State, s *cftest.State) {
		s.Configs[st.TunnelID].Config.Ingress[0].OriginRequest = nil
	}, `tunnel "hangar" configuration`, "ingress"},
	{"origin access not required", func(e *env, st State, s *cftest.State) {
		r := &s.Configs[st.TunnelID].Config.Ingress[0]
		r.OriginRequest = json.RawMessage(strings.Replace(string(r.OriginRequest), `"required":true`, `"required":false`, 1))
	}, `tunnel "hangar" configuration`, "ingress"},
	{"origin access other aud", func(e *env, st State, s *cftest.State) {
		s.Configs[st.TunnelID].Config.Ingress[0].OriginRequest = json.RawMessage(`{"access":{"required":true,"teamName":"hangar-test","audTag":["other"]}}`)
	}, `tunnel "hangar" configuration`, "ingress"},
	{"origin access other team", func(e *env, st State, s *cftest.State) {
		r := &s.Configs[st.TunnelID].Config.Ingress[0]
		r.OriginRequest = json.RawMessage(strings.Replace(string(r.OriginRequest), `"hangar-test"`, `"elsewhere"`, 1))
	}, `tunnel "hangar" configuration`, "ingress"},
	{"dns points elsewhere", func(e *env, st State, s *cftest.State) {
		s.DNS[st.DNSRecordID].Content = "elsewhere.example.net"
	}, "DNS record " + testHost, "content"},
	{"dns not proxied", func(e *env, st State, s *cftest.State) {
		s.DNS[st.DNSRecordID].Proxied = false
	}, "DNS record " + testHost, "proxied"},
	{"dns type changed", func(e *env, st State, s *cftest.State) {
		d := s.DNS[st.DNSRecordID]
		d.Type, d.Content = "A", "192.0.2.1"
	}, "DNS record " + testHost, "type"},
	{"policy rule added", func(e *env, st State, s *cftest.State) {
		p := s.Policies[st.Policies[emailsPolicy]]
		p.Include = append(p.Include, json.RawMessage(`{"email":{"email":"intruder@example.org"}}`))
	}, `Access policy "` + testHost + `: allow listed emails"`, "include"},
	{"policy rule removed", func(e *env, st State, s *cftest.State) {
		p := s.Policies[st.Policies[emailsPolicy]]
		p.Include = p.Include[:1]
	}, `Access policy "` + testHost + `: allow listed emails"`, "include"},
	{"policy widened to everyone", func(e *env, st State, s *cftest.State) {
		p := s.Policies[st.Policies[emailsPolicy]]
		p.Include = []json.RawMessage{json.RawMessage(`{"everyone":{}}`)}
	}, `Access policy "` + testHost + `: allow listed emails"`, "include"},
	{"policy exclude added", func(e *env, st State, s *cftest.State) {
		p := s.Policies[st.Policies[tokensPolicy]]
		p.Exclude = []json.RawMessage{json.RawMessage(`{"ip":{"ip":"192.0.2.0/24"}}`)}
	}, `Access policy "` + testHost + `: service tokens"`, "exclude"},
	{"policy require added", func(e *env, st State, s *cftest.State) {
		p := s.Policies[st.Policies[emailsPolicy]]
		p.Require = []json.RawMessage{json.RawMessage(`{"geo":{"country_code":"US"}}`)}
	}, `Access policy "` + testHost + `: allow listed emails"`, "require"},
	{"policy decision changed", func(e *env, st State, s *cftest.State) {
		s.Policies[st.Policies[tokensPolicy]].Decision = "bypass"
	}, `Access policy "` + testHost + `: service tokens"`, "decision"},
	{"policy session changed", func(e *env, st State, s *cftest.State) {
		s.Policies[st.Policies[emailsPolicy]].SessionDuration = "720h"
	}, `Access policy "` + testHost + `: allow listed emails"`, "session_duration"},
	{"extra policy on app", func(e *env, st State, s *cftest.State) {
		p := &cftest.Policy{ID: "11111111-2222-3333-4444-555555555555", Name: "someone else's", Decision: "bypass",
			Include: []json.RawMessage{json.RawMessage(`{"everyone":{}}`)}, Reusable: true}
		s.Policies[p.ID] = p
		a := s.Apps[st.AppID]
		a.Links = append(a.Links, cftest.PolicyLink{ID: p.ID, Precedence: 3})
	}, "Access application " + testHost, "policies"},
	{"policy unlinked from app", func(e *env, st State, s *cftest.State) {
		a := s.Apps[st.AppID]
		a.Links = a.Links[1:]
	}, "Access application " + testHost, "policies"},
	{"policy precedence swapped", func(e *env, st State, s *cftest.State) {
		a := s.Apps[st.AppID]
		a.Links[0].Precedence, a.Links[1].Precedence = 2, 1
	}, "Access application " + testHost, "policies"},
	{"app session changed", func(e *env, st State, s *cftest.State) {
		s.Apps[st.AppID].SessionDuration = "720h"
	}, "Access application " + testHost, "session_duration"},
	{"app idp removed", func(e *env, st State, s *cftest.State) {
		a := s.Apps[st.AppID]
		a.AllowedIdPs = a.AllowedIdPs[:1]
	}, "Access application " + testHost, "allowed_idps"},
	{"app auto redirect on", func(e *env, st State, s *cftest.State) {
		s.Apps[st.AppID].AutoRedirectToIdentity = true
	}, "Access application " + testHost, "auto_redirect_to_identity"},
	{"app destination added", func(e *env, st State, s *cftest.State) {
		a := s.Apps[st.AppID]
		a.Destinations = append(a.Destinations, cftest.AppDestination{Type: "public", URI: "other." + testZone})
	}, "Access application " + testHost, "destinations"},
	{"app self-hosted domain added", func(e *env, st State, s *cftest.State) {
		s.Apps[st.AppID].SelfHostedDomains = []string{testHost, "other." + testZone}
	}, "Access application " + testHost, "self_hosted_domains"},
	{"app options preflight bypass", func(e *env, st State, s *cftest.State) {
		s.Apps[st.AppID].OptionsPreflightBypass = true
	}, "Access application " + testHost, "options_preflight_bypass"},
	{"app authenticate via warp", func(e *env, st State, s *cftest.State) {
		s.Apps[st.AppID].AllowAuthenticateViaWarp = true
	}, "Access application " + testHost, "allow_authenticate_via_warp"},
	{"app iframe allowed", func(e *env, st State, s *cftest.State) {
		s.Apps[st.AppID].AllowIframe = true
	}, "Access application " + testHost, "allow_iframe"},
	{"app cors headers", func(e *env, st State, s *cftest.State) {
		s.Apps[st.AppID].CORSHeaders = json.RawMessage(`{"allow_all_origins":true,"allow_credentials":true}`)
	}, "Access application " + testHost, "cors_headers"},
	{"app cookie not http-only", func(e *env, st State, s *cftest.State) {
		s.Apps[st.AppID].HTTPOnlyCookieAttribute = false
	}, "Access application " + testHost, "http_only_cookie_attribute"},
	{"app cookie samesite none", func(e *env, st State, s *cftest.State) {
		s.Apps[st.AppID].SameSiteCookieAttribute = "none"
	}, "Access application " + testHost, "same_site_cookie_attribute"},
	{"app clientless isolation", func(e *env, st State, s *cftest.State) {
		s.Apps[st.AppID].ClientlessIsolationURL = true
	}, "Access application " + testHost, "use_clientless_isolation_app_launcher_url"},
	{"tunnel deleted", func(e *env, st State, s *cftest.State) {
		now := time.Now()
		s.Tunnels[st.TunnelID].DeletedAt = &now
	}, `tunnel "hangar"`, ""},
	{"dns record deleted", func(e *env, st State, s *cftest.State) {
		delete(s.DNS, st.DNSRecordID)
	}, "DNS record " + testHost, ""},
	{"policy deleted", func(e *env, st State, s *cftest.State) {
		delete(s.Policies, st.Policies[tokensPolicy])
		a := s.Apps[st.AppID]
		a.Links = a.Links[:1]
	}, `Access policy "` + testHost + `: service tokens"`, ""},
	{"app deleted", func(e *env, st State, s *cftest.State) {
		delete(s.Apps, st.AppID)
	}, "Access application " + testHost, ""},
	{"service token deleted", func(e *env, st State, s *cftest.State) {
		delete(s.ServiceTokens, st.ServiceTokenID)
	}, `service token "hangar-smoke"`, ""},
	{"tunnel token output removed", func(e *env, st State, s *cftest.State) {
		if err := os.Remove(filepath.Join(e.dir, CloudflaredEnv)); err != nil {
			e.t.Fatal(err)
		}
	}, "output " + CloudflaredEnv, ""},
	{"access output edited", func(e *env, st State, s *cftest.State) {
		if err := os.WriteFile(filepath.Join(e.dir, AccessEnv), []byte("HANGAR_CF_AUDIENCE=stale\n"), 0o600); err != nil {
			e.t.Fatal(err)
		}
	}, "output " + AccessEnv, ""},
}

func TestVerifyFindsDriftAndApplyFixesIt(t *testing.T) {
	for _, d := range drifts {
		t.Run(d.name, func(t *testing.T) {
			e := newEnv(t)
			e.mustRun(true)
			st := e.state()
			e.f.Mutate(func(s *cftest.State) { d.edit(e, st, s) })

			before := e.snapshot()
			var res *Result
			writes := e.writesDuring(func() { res = e.mustRun(false) })
			if len(writes) != 0 {
				t.Fatalf("verify sent writes: %v", writes)
			}
			if !mapsEqual(before, e.snapshot()) {
				t.Fatal("verify changed the output directory")
			}
			i := slices.IndexFunc(res.Changes, func(c Change) bool { return c.Object == d.object })
			if i < 0 {
				t.Fatalf("verify did not report %s; it reported %v", d.object, objects(res))
			}
			if d.field != "" && !slices.ContainsFunc(res.Changes[i].Fields, func(f Field) bool { return f.Name == d.field }) {
				t.Fatalf("verify reported %s without field %s: %+v", d.object, d.field, res.Changes[i].Fields)
			}

			e.mustRun(true)
			if res := e.mustRun(false); len(res.Changes) != 0 {
				t.Fatalf("drift left after apply: %v", objects(res))
			}
			e.f.Mutate(func(s *cftest.State) {
				if n := len(s.ServiceTokens); n != 1 {
					t.Errorf("%d service tokens after apply", n)
				}
			})
		})
	}
}

func TestRecreatedTunnelRepointsDNSAndOutputs(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	old := e.state().TunnelID
	e.f.Mutate(func(s *cftest.State) {
		now := time.Now()
		s.Tunnels[old].DeletedAt = &now
	})
	e.mustRun(true)
	st := e.state()
	if st.TunnelID == old {
		t.Fatal("tunnel was not recreated")
	}
	var token string
	e.f.Mutate(func(s *cftest.State) {
		if c := s.DNS[st.DNSRecordID].Content; c != st.TunnelID+".cfargotunnel.com" {
			t.Errorf("DNS still points at %s", c)
		}
		token = s.Tunnels[st.TunnelID].Token
	})
	if got := e.snapshot()[CloudflaredEnv]; got != "TUNNEL_TOKEN="+token+"\n" {
		t.Errorf("cloudflared.env not rewritten for the new tunnel")
	}
}

func TestRefusesObjectsItDidNotCreate(t *testing.T) {
	cases := []struct {
		name string
		add  func(f *cftest.Fake) string
	}{
		{"tunnel", func(f *cftest.Fake) string { return f.AddTunnel("hangar", "cloudflare").ID }},
		{"locally managed tunnel", func(f *cftest.Fake) string { return f.AddTunnel("hangar", "local").ID }},
		{"dns record", func(f *cftest.Fake) string {
			var id string
			f.Mutate(func(s *cftest.State) {
				id = "ffffffffffffffffffffffffffffffff"
				s.DNS[id] = &cftest.DNSRecord{ID: id, ZoneID: s.Zones[0].ID, Type: "A", Name: testHost, Content: "192.0.2.10", TTL: 1}
			})
			return id
		}},
		{"access app", func(f *cftest.Fake) string {
			id := "aaaaaaaa-0000-0000-0000-000000000000"
			f.Mutate(func(s *cftest.State) {
				s.Apps[id] = &cftest.App{ID: id, Name: "theirs", Domain: testHost, Type: "self_hosted", SessionDuration: "24h"}
			})
			return id
		}},
		{"access policy", func(f *cftest.Fake) string {
			id := "bbbbbbbb-0000-0000-0000-000000000000"
			f.Mutate(func(s *cftest.State) {
				s.Policies[id] = &cftest.Policy{ID: id, Name: testHost + ": service tokens", Decision: "allow", Reusable: true,
					Include: []json.RawMessage{json.RawMessage(`{"everyone":{}}`)}}
			})
			return id
		}},
		{"service token", func(f *cftest.Fake) string { return f.AddServiceToken("hangar-smoke").ID }},
		{"access app with the hostname as a destination", func(f *cftest.Fake) string {
			return addApp(f, "aaaaaaaa-0000-0000-0000-000000000001", "other."+testZone, cftest.AppDestination{Type: "public", URI: testHost})
		}},
		{"access app on a path under the hostname", func(f *cftest.Fake) string {
			return addApp(f, "aaaaaaaa-0000-0000-0000-000000000002", testHost+"/admin")
		}},
		{"access app with a destination under the hostname", func(f *cftest.Fake) string {
			return addApp(f, "aaaaaaaa-0000-0000-0000-000000000003", "other."+testZone, cftest.AppDestination{Type: "public", URI: strings.ToUpper(testHost) + "/debug/*"})
		}},
		{"access app with the hostname as a legacy self-hosted domain", func(f *cftest.Fake) string {
			id := addApp(f, "aaaaaaaa-0000-0000-0000-000000000004", "other."+testZone)
			f.Mutate(func(s *cftest.State) { s.Apps[id].SelfHostedDomains = []string{testHost} })
			return id
		}},
	}
	for _, c := range cases {
		t.Run("before first apply/"+c.name, func(t *testing.T) {
			e := newEnv(t)
			id := c.add(e.f)
			var err error
			writes := e.writesDuring(func() { _, err = e.run(true) })
			if !errors.Is(err, ErrNotOwned) || !strings.Contains(err.Error(), id) {
				t.Fatalf("apply: %v, want ErrNotOwned naming %s", err, id)
			}
			if len(writes) != 0 {
				t.Fatalf("apply sent writes before refusing: %v", writes)
			}
			if _, err := e.run(false); !errors.Is(err, ErrNotOwned) {
				t.Fatalf("plan: %v, want ErrNotOwned", err)
			}
		})
		t.Run("after apply/"+c.name, func(t *testing.T) {
			e := newEnv(t)
			if c.name == "dns record" {
				// A second record at the name cannot coexist with the CNAME.
				t.Skip("the API refuses a record beside a CNAME")
			}
			e.mustRun(true)
			before := e.snapshot()
			id := c.add(e.f)
			var err error
			writes := e.writesDuring(func() { _, err = e.run(true) })
			if !errors.Is(err, ErrNotOwned) || !strings.Contains(err.Error(), id) {
				t.Fatalf("apply: %v, want ErrNotOwned naming %s", err, id)
			}
			if len(writes) != 0 || !mapsEqual(before, e.snapshot()) {
				t.Fatalf("apply changed something before refusing: %v", writes)
			}
		})
	}
}

func TestLostStateRefusesEverything(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	if err := os.Remove(filepath.Join(e.dir, StateFile)); err != nil {
		t.Fatal(err)
	}
	var err error
	writes := e.writesDuring(func() { _, err = e.run(true) })
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("apply without state: %v", err)
	}
	for _, kind := range []string{"tunnel", "DNS CNAME record", "Access policy", "Access application", "service token"} {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("refusal does not mention the %s: %v", kind, err)
		}
	}
	if len(writes) != 0 {
		t.Fatalf("writes: %v", writes)
	}
}

func TestSmokeSecretWrittenOnce(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	first := e.snapshot()[SmokeTokenEnv]
	if !strings.Contains(first, "HANGAR_CF_CLIENT_SECRET=") {
		t.Fatalf("smoke-token.env: %q", first)
	}
	e.mustRun(true)
	e.mustRun(true)
	if got := e.snapshot()[SmokeTokenEnv]; got != first {
		t.Fatal("a later apply rewrote smoke-token.env")
	}
	creates := 0
	for _, w := range e.f.Writes() {
		if strings.HasSuffix(w.Path, "/access/service_tokens") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("service token created %d times", creates)
	}

	// Lost secret: the token stays, and a note says the secret cannot be had.
	if err := os.Remove(filepath.Join(e.dir, SmokeTokenEnv)); err != nil {
		t.Fatal(err)
	}
	res := e.mustRun(true)
	if len(res.Changes) != 0 || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], SmokeTokenEnv) {
		t.Fatalf("after losing the secret: changes %v notes %v", objects(res), res.Notes)
	}
}

func TestSmokeTokenNotCreatedWhenSecretCannotBeSaved(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	e.f.Mutate(func(s *cftest.State) { clear(s.ServiceTokens) })
	errFull := errors.New("no space left on device")
	createTemp = func(dir, pattern string) (*os.File, error) {
		if strings.Contains(pattern, SmokeTokenEnv) {
			return nil, errFull
		}
		return os.CreateTemp(dir, pattern)
	}
	t.Cleanup(func() { createTemp = os.CreateTemp })
	if _, err := e.run(true); !errors.Is(err, errFull) {
		t.Fatalf("apply: %v, want the write failure", err)
	}
	e.f.Mutate(func(s *cftest.State) {
		if len(s.ServiceTokens) != 0 {
			t.Fatal("a service token was created whose secret could not be saved")
		}
	})
}

func TestStateFromAnotherAccount(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	e.cfg.AccountID = strings.Repeat("e", 32)
	if _, err := e.run(false); err == nil || !strings.Contains(err.Error(), "belongs to account") {
		t.Fatalf("plan with another account's state: %v", err)
	}
}

func TestMissingIdentityProvider(t *testing.T) {
	e := newEnv(t)
	e.cfg.IdentityProviders.Google = "Google Workspace"
	_, err := e.run(false)
	if err == nil || !strings.Contains(err.Error(), `no google login method is named "Google Workspace"`) {
		t.Fatalf("plan: %v", err)
	}
	e = newEnv(t)
	e.f.Mutate(func(s *cftest.State) { s.IdPs = s.IdPs[:1] })
	if _, err := e.run(false); err == nil || !strings.Contains(err.Error(), "no onetimepin login method") {
		t.Fatalf("plan without OTP: %v", err)
	}
}

func TestListsSpanPages(t *testing.T) {
	e := newEnv(t)
	e.f.MaxPerPage = 1
	e.f.Mutate(func(s *cftest.State) {
		for i := range 5 {
			id := "cccccccc-0000-0000-0000-00000000000" + string(rune('0'+i))
			s.Policies[id] = &cftest.Policy{ID: id, Name: "other " + id, Decision: "allow", Reusable: true,
				Include: []json.RawMessage{json.RawMessage(`{"everyone":{}}`)}}
		}
	})
	e.mustRun(true)
	if res := e.mustRun(true); len(res.Changes) != 0 {
		t.Fatalf("second apply with paging: %v", objects(res))
	}
	// A same-named policy that sorts last must still be found.
	id := "dddddddd-0000-0000-0000-000000000000"
	e.f.Mutate(func(s *cftest.State) {
		s.Policies[id] = &cftest.Policy{ID: id, Name: testHost + ": allow listed emails", Decision: "allow", Reusable: true,
			Include: []json.RawMessage{json.RawMessage(`{"everyone":{}}`)}, CreatedAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}
	})
	if _, err := e.run(false); !errors.Is(err, ErrNotOwned) || !strings.Contains(err.Error(), id) {
		t.Fatalf("plan: %v, want the policy on the last page refused", err)
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	e := newEnv(t)
	e.f.Fail("GET", 503, 2, "")
	e.f.Fail("POST", 429, 1, "1")
	e.mustRun(true)
	if res := e.mustRun(false); len(res.Changes) != 0 {
		t.Fatalf("after retries: %v", objects(res))
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
zone: Hangar.Example
hostname: API.hangar.example.
tunnel_name: hangar
allowed_emails: [" Me@Example.com ", me@example.com]
identity_providers:
  google: Google
`
	c, err := LoadConfig(write(good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Zone != "hangar.example" || c.Hostname != "api.hangar.example" || !slices.Equal(c.AllowedEmails, []string{"me@example.com"}) {
		t.Fatalf("normalized: %+v", c)
	}
	for name, bad := range map[string]string{
		"unknown key":      good + "api_token: x\n",
		"host outside":     strings.Replace(good, "API.hangar.example.", "api.other.example", 1),
		"bad email":        strings.Replace(good, `" Me@Example.com ", me@example.com`, `"Me <me@example.com>"`, 1),
		"bad account":      strings.Replace(good, "0123456789abcdef0123456789abcdef", "nope", 1),
		"no google":        strings.Replace(good, "  google: Google\n", "", 1),
		"no emails":        strings.Replace(good, `[" Me@Example.com ", me@example.com]`, "[]", 1),
		"wildcard host":    strings.Replace(good, "API.hangar.example.", "*.hangar.example", 1),
		"empty tunnelname": strings.Replace(good, "tunnel_name: hangar", "tunnel_name: ''", 1),
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
	if c.Zone != "example.com" || c.Hostname != "api.example.com" {
		t.Fatalf("example config holds more than placeholders: %+v", c)
	}
}

func TestZeroJSON(t *testing.T) {
	for raw, want := range map[string]bool{
		``: true, `null`: true, `{}`: true, `{"enabled":false}`: true, `{"a":{"b":0,"c":""}}`: true, `[]`: true,
		`{"enabled":true}`: false, `{"noTLSVerify":true}`: false, `{"httpHostHeader":"x"}`: false, `{"connectTimeout":30}`: false,
	} {
		if got := zeroJSON(json.RawMessage(raw)); got != want {
			t.Errorf("zeroJSON(%s) = %t", raw, got)
		}
	}
}
