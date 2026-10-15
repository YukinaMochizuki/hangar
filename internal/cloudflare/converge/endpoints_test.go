package converge

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare/cftest"
)

const (
	testEndpointZone = "hangar-user.example"
	testWildcard     = "*." + testEndpointZone
)

// newEndpointsEnv is newEnv with public endpoints in a zone of their own.
func newEndpointsEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.f.AddZone(testEndpointZone)
	e.cfg.Endpoints = &EndpointsConfig{Zone: testEndpointZone}
	if err := e.cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEndpointsApplyFromEmpty(t *testing.T) {
	e := newEndpointsEnv(t)
	e.mustRun(true)
	st := e.state()
	if st.EndpointsZone != testEndpointZone || st.EndpointsAppID == "" || st.EndpointsDNSRecordID == "" || st.Policies[bypassPolicy] == "" {
		t.Fatalf("state: %+v", st)
	}
	e.f.Mutate(func(s *cftest.State) {
		app := s.Apps[st.EndpointsAppID]
		if app == nil || app.Domain != testWildcard || len(app.Links) != 2 ||
			app.Links[0].ID != st.Policies[emailsPolicy] || app.Links[1].ID != st.Policies[tokensPolicy] {
			t.Errorf("the wildcard application: %+v", app)
		}
		bypass := s.Policies[st.Policies[bypassPolicy]]
		if bypass == nil || bypass.Decision != "bypass" || len(bypass.Include) != 1 || string(bypass.Include[0]) != `{"everyone":{}}` {
			t.Errorf("the bypass policy: %+v", bypass)
		}
		if bypass.AppCount != 0 {
			t.Errorf("the bypass policy is on %d applications; only the apiserver's PUBLIC endpoints take it", bypass.AppCount)
		}
		ing := s.Configs[st.TunnelID].Config.Ingress
		if len(ing) != 3 || ing[0].Hostname != testHost || ing[1].Hostname != testWildcard || ing[1].Service != EdgeOrigin ||
			len(ing[1].OriginRequest) != 0 || ing[2].Hostname != "" {
			t.Errorf("ingress: %+v", ing)
		}
		d := s.DNS[st.EndpointsDNSRecordID]
		if d == nil || d.ZoneID != st.EndpointsZoneID || d.Name != testWildcard || d.Type != "CNAME" || !d.Proxied ||
			d.Content != st.TunnelID+".cfargotunnel.com" {
			t.Errorf("the wildcard record: %+v", d)
		}
		var aud string
		if app != nil {
			aud = app.AUD
		}
		b, err := os.ReadFile(filepath.Join(e.dir, EdgeEnv))
		want := "HANGAR_EDGE_DOMAIN=" + testEndpointZone + "\nHANGAR_EDGE_CF_AUDIENCE=" + aud + "\nHANGAR_CF_ACCOUNT_ID=" + testAccount +
			"\nHANGAR_CF_BYPASS_POLICY_ID=" + st.Policies[bypassPolicy] + "\n"
		if err != nil || string(b) != want {
			t.Errorf("%s: %q, %v; want %q", EdgeEnv, b, err, want)
		}
	})
	if res := e.mustRun(false); len(res.Changes) != 0 {
		t.Errorf("a second run finds changes: %v", objects(res))
	}
}

// TestEndpointsCoveredBeforeRouted: the wildcard application comes before
// the ingress that routes the wildcard, and the record last.
func TestEndpointsCoveredBeforeRouted(t *testing.T) {
	e := newEndpointsEnv(t)
	var zoneID string
	e.f.Mutate(func(s *cftest.State) { zoneID = s.Zones[1].ID })
	writes := e.writesDuring(func() { e.mustRun(true) })
	at := func(match func(cftest.Request) bool) int {
		return slices.IndexFunc(writes, match)
	}
	app := at(func(r cftest.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/access/apps")
	})
	// The API's application is made first, the wildcard's second.
	wildcardApp := -1
	for i := app + 1; i < len(writes); i++ {
		if writes[i].Method == http.MethodPost && strings.HasSuffix(writes[i].Path, "/access/apps") {
			wildcardApp = i
			break
		}
	}
	ingress := at(func(r cftest.Request) bool {
		return r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/configurations")
	})
	record := at(func(r cftest.Request) bool {
		return r.Method == http.MethodPost && strings.Contains(r.Path, "/zones/"+zoneID+"/dns_records")
	})
	if app < 0 || app >= wildcardApp || wildcardApp >= ingress || ingress >= record {
		t.Fatalf("writes in the wrong order (app %d, wildcard app %d, ingress %d, record %d): %v", app, wildcardApp, ingress, record, writes)
	}
}

// TestEndpointsLeaveTheApiserversApplicationsAlone: the apiserver's
// applications, one hostname below the wildcard each, are neither its to
// change nor a conflict; another one there is a note, and one on the
// wildcard itself a conflict.
func TestEndpointsLeaveTheApiserversApplicationsAlone(t *testing.T) {
	e := newEndpointsEnv(t)
	e.mustRun(true)
	host := "web-demo." + testEndpointZone
	addApp(e.f, "aaaaaaaa-0000-0000-0000-00000000000a", host)
	e.f.Mutate(func(s *cftest.State) {
		s.Apps["aaaaaaaa-0000-0000-0000-00000000000a"].Name = cloudflare.EndpointAppPrefix + host
	})
	var res *Result
	writes := e.writesDuring(func() { res = e.mustRun(true) })
	if len(writes) != 0 || len(res.Changes) != 0 || len(res.Notes) != 0 {
		t.Fatalf("with an apiserver's application: writes %v, changes %v, notes %v", writes, objects(res), res.Notes)
	}

	addApp(e.f, "aaaaaaaa-0000-0000-0000-00000000000b", "other-demo."+testEndpointZone)
	res = e.mustRun(false)
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "other-demo."+testEndpointZone) {
		t.Errorf("another application below the wildcard: notes %v", res.Notes)
	}

	addApp(e.f, "aaaaaaaa-0000-0000-0000-00000000000c", testWildcard)
	if _, err := e.run(false); !errors.Is(err, ErrNotOwned) || !strings.Contains(err.Error(), "aaaaaaaa-0000-0000-0000-00000000000c") {
		t.Errorf("another application on the wildcard: %v", err)
	}
}

func TestEndpointsRefuseObjectsItDidNotCreate(t *testing.T) {
	for name, add := range map[string]func(f *cftest.Fake){
		"wildcard record": func(f *cftest.Fake) {
			f.Mutate(func(s *cftest.State) {
				s.DNS["ffff"] = &cftest.DNSRecord{ID: "ffff", ZoneID: s.Zones[1].ID, Type: "A", Name: testWildcard, Content: "192.0.2.1", TTL: 1}
			})
		},
		"bypass policy": func(f *cftest.Fake) {
			f.Mutate(func(s *cftest.State) {
				s.Policies["bbbb"] = &cftest.Policy{ID: "bbbb", Name: testEndpointZone + ": public endpoints, everyone", Decision: "bypass",
					Reusable: true, Include: []json.RawMessage{json.RawMessage(`{"everyone":{}}`)}}
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEndpointsEnv(t)
			add(e.f)
			var err error
			writes := e.writesDuring(func() { _, err = e.run(true) })
			if !errors.Is(err, ErrNotOwned) || len(writes) != 0 {
				t.Fatalf("apply: %v, writes %v", err, writes)
			}
		})
	}
}

// TestEndpointsZoneIsFixed: the zone of a state's endpoints neither
// changes nor goes.
func TestEndpointsZoneIsFixed(t *testing.T) {
	e := newEndpointsEnv(t)
	e.mustRun(true)
	e.f.AddZone("elsewhere.example")
	for what, ep := range map[string]*EndpointsConfig{"another zone": {Zone: "elsewhere.example"}, "none": nil} {
		e.cfg.Endpoints = ep
		var err error
		writes := e.writesDuring(func() { _, err = e.run(true) })
		if err == nil || !strings.Contains(err.Error(), testEndpointZone) || len(writes) != 0 {
			t.Errorf("%s: %v, writes %v", what, err, writes)
		}
	}
}

func TestEndpointsZoneIsApartFromTheControlZone(t *testing.T) {
	for _, zone := range []string{testZone, "user." + testZone, "*.user.example", ""} {
		c := newEnv(t).cfg
		c.Endpoints = &EndpointsConfig{Zone: zone}
		if err := c.normalize(); err == nil {
			t.Errorf("endpoints zone %q accepted beside control zone %q", zone, testZone)
		}
	}
}
