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
	if st.EndpointsZone != testEndpointZone || st.EndpointsDNSRecordID == "" || st.EndpointsAppID != "" || st.Policies[oldBypassPolicy] != "" {
		t.Fatalf("state: %+v", st)
	}
	e.f.Mutate(func(s *cftest.State) {
		for _, a := range s.Apps {
			if a.ID != st.AppID {
				t.Errorf("an Access application besides the API's: %+v", a)
			}
		}
		for _, p := range s.Policies {
			if p.Decision == "bypass" {
				t.Errorf("a bypass policy: %+v", p)
			}
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
	})
	b, err := os.ReadFile(filepath.Join(e.dir, EdgeEnv))
	if want := "HANGAR_EDGE_DOMAIN=" + testEndpointZone + "\n"; err != nil || string(b) != want {
		t.Errorf("%s: %q, %v; want %q", EdgeEnv, b, err, want)
	}
	if res := e.mustRun(false); len(res.Changes) != 0 || len(res.Notes) != 0 {
		t.Errorf("a second run finds changes %v, notes %v", objects(res), res.Notes)
	}
}

// TestEndpointsRoutedAfterTheIngress: the ingress that routes the wildcard
// comes before the record that sends it to the tunnel.
func TestEndpointsRoutedAfterTheIngress(t *testing.T) {
	e := newEndpointsEnv(t)
	var zoneID string
	e.f.Mutate(func(s *cftest.State) { zoneID = s.Zones[1].ID })
	writes := e.writesDuring(func() { e.mustRun(true) })
	at := func(match func(cftest.Request) bool) int {
		return slices.IndexFunc(writes, match)
	}
	ingress := at(func(r cftest.Request) bool {
		return r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/configurations")
	})
	record := at(func(r cftest.Request) bool {
		return r.Method == http.MethodPost && strings.Contains(r.Path, "/zones/"+zoneID+"/dns_records")
	})
	if ingress < 0 || ingress >= record {
		t.Fatalf("writes in the wrong order (ingress %d, record %d): %v", ingress, record, writes)
	}
}

// TestOldEndpointAccessIsNoted: the Access objects an earlier version made
// for public endpoints, the wildcard application and the bypass policy, and
// the apiserver's applications for PUBLIC endpoints, are no longer used:
// a run names the first two, to be deleted by hand, and changes none of
// them, nor counts any as a conflict.
func TestOldEndpointAccessIsNoted(t *testing.T) {
	e := newEndpointsEnv(t)
	e.mustRun(true)
	const oldApp, oldPolicy, publicApp = "aaaaaaaa-0000-0000-0000-00000000000a", "bbbbbbbb-0000-0000-0000-00000000000b", "cccccccc-0000-0000-0000-00000000000c"
	addApp(e.f, oldApp, testWildcard)
	addApp(e.f, publicApp, "web-demo."+testEndpointZone)
	e.f.Mutate(func(s *cftest.State) {
		s.Apps[publicApp].Name = cloudflare.EndpointAppPrefix + "web-demo." + testEndpointZone
		s.Policies[oldPolicy] = &cftest.Policy{ID: oldPolicy, Name: testEndpointZone + ": public endpoints, everyone", Decision: "bypass",
			Reusable: true, Include: []json.RawMessage{json.RawMessage(`{"everyone":{}}`)}}
	})
	st := e.state()
	st.EndpointsAppID = oldApp
	st.Policies[oldBypassPolicy] = oldPolicy
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, StateFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	var res *Result
	writes := e.writesDuring(func() { res = e.mustRun(true) })
	if len(writes) != 0 || len(res.Changes) != 0 {
		t.Errorf("with the old objects: writes %v, changes %v", writes, objects(res))
	}
	notes := strings.Join(res.Notes, "\n")
	if len(res.Notes) != 2 || !strings.Contains(notes, oldApp) || !strings.Contains(notes, oldPolicy) || !strings.Contains(notes, "delete it by hand") {
		t.Errorf("notes %q; want the old application and policy named", res.Notes)
	}
	e.f.Mutate(func(s *cftest.State) {
		if s.Apps[oldApp] == nil || s.Apps[publicApp] == nil || s.Policies[oldPolicy] == nil {
			t.Error("an old object went")
		}
	})
	if got := e.state(); got.EndpointsAppID != oldApp || got.Policies[oldBypassPolicy] != oldPolicy {
		t.Errorf("the state forgot the old objects: %+v", got)
	}
}

func TestEndpointsRefuseARecordItDidNotCreate(t *testing.T) {
	e := newEndpointsEnv(t)
	e.f.Mutate(func(s *cftest.State) {
		s.DNS["ffff"] = &cftest.DNSRecord{ID: "ffff", ZoneID: s.Zones[1].ID, Type: "A", Name: testWildcard, Content: "192.0.2.1", TTL: 1}
	})
	var err error
	writes := e.writesDuring(func() { _, err = e.run(true) })
	if !errors.Is(err, ErrNotOwned) || len(writes) != 0 {
		t.Fatalf("apply: %v, writes %v", err, writes)
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
