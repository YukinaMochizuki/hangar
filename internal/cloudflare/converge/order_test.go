package converge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/cloudflare/cftest"
)

// addApp adds a self-hosted application the tool did not create.
func addApp(f *cftest.Fake, id, domain string, dests ...cftest.AppDestination) string {
	f.Mutate(func(s *cftest.State) {
		s.Apps[id] = &cftest.App{ID: id, Name: "theirs " + id, Domain: domain, Type: "self_hosted", SessionDuration: "24h",
			Destinations: dests, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	})
	return id
}

// TestFirstApplyCoversBeforeRouting holds the order that keeps the hostname
// from ever reaching the origin without Access in front: policies, then the
// application, then the tunnel and its ingress, and the DNS record last.
func TestFirstApplyCoversBeforeRouting(t *testing.T) {
	e := newEnv(t)
	writes := e.writesDuring(func() { e.mustRun(true) })
	var got []string
	for _, w := range writes {
		p := strings.TrimPrefix(w.Path, "/client/v4")
		parts := strings.Split(p, "/")
		// Name each call by its path with IDs dropped.
		var kept []string
		for i, s := range parts {
			if i > 0 && (strings.Count(s, "-") == 4 || len(s) == 32) {
				continue
			}
			kept = append(kept, s)
		}
		got = append(got, w.Method+" "+strings.Join(kept, "/"))
	}
	want := []string{
		"POST /accounts/access/policies",
		"POST /accounts/access/policies",
		"POST /accounts/access/apps",
		"POST /accounts/cfd_tunnel",
		"PUT /accounts/cfd_tunnel/configurations",
		"POST /zones/dns_records",
		"POST /accounts/access/service_tokens",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("write order:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestWarpRoutingIsANoteNotDrift(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	st := e.state()
	e.f.Mutate(func(s *cftest.State) { s.Configs[st.TunnelID].Config.WarpRouting = json.RawMessage(`{"enabled":true}`) })
	res := e.mustRun(false)
	if len(res.Changes) != 0 {
		t.Fatalf("warp-routing reported as drift: %v", objects(res))
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "warp-routing") {
		t.Fatalf("notes %v", res.Notes)
	}
	// Ingress drift alongside: the fix is sent without warp-routing, which
	// the fake, like the API's schema, does not accept in a request.
	e.f.Mutate(func(s *cftest.State) { s.Configs[st.TunnelID].Config.Ingress[0].Service = "http://127.0.0.1:1" })
	e.mustRun(true)
	if res := e.mustRun(false); len(res.Changes) != 0 {
		t.Fatalf("after apply: %v", objects(res))
	}
}

func TestRemoteConfigWithoutConfigSrcIsRemote(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	st := e.state()
	e.f.Mutate(func(s *cftest.State) {
		tun := s.Tunnels[st.TunnelID]
		tun.ConfigSrc, tun.RemoteConfig = "", true
		s.Configs[st.TunnelID].Config.Ingress[0].Service = "http://127.0.0.1:1"
	})
	e.mustRun(true)
	if res := e.mustRun(false); len(res.Changes) != 0 {
		t.Fatalf("after apply: %v", objects(res))
	}

	e.f.Mutate(func(s *cftest.State) { s.Tunnels[st.TunnelID].ConfigSrc = "local" })
	var err error
	writes := e.writesDuring(func() { _, err = e.run(true) })
	if err == nil || !strings.Contains(err.Error(), "locally managed") {
		t.Fatalf("apply on a local tunnel: %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("writes: %v", writes)
	}
}

func TestHostnameChangeIsRefused(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	e.cfg.Hostname = "api2." + testZone
	for _, apply := range []bool{false, true} {
		var err error
		writes := e.writesDuring(func() { _, err = e.run(apply) })
		if err == nil || !strings.Contains(err.Error(), "changing the hostname is not supported") || !strings.Contains(err.Error(), testHost) {
			t.Fatalf("apply=%t: %v", apply, err)
		}
		if len(writes) != 0 {
			t.Fatalf("writes: %v", writes)
		}
	}
	if st := e.state(); st.Hostname != testHost {
		t.Fatalf("state hostname %q", st.Hostname)
	}
}

func TestAppsNotCoveringTheHostnameAreNotConflicts(t *testing.T) {
	e := newEnv(t)
	addApp(e.f, "eeeeeeee-0000-0000-0000-000000000001", testHost+".other.example")
	addApp(e.f, "eeeeeeee-0000-0000-0000-000000000002", "x."+testHost)
	addApp(e.f, "eeeeeeee-0000-0000-0000-000000000003", "other."+testZone, cftest.AppDestination{Type: "public", URI: testHost + "2/x"})
	e.mustRun(true)
	if res := e.mustRun(false); len(res.Changes) != 0 {
		t.Fatalf("second run: %v", objects(res))
	}
}

func TestOutputDirectoryMode(t *testing.T) {
	t.Run("created 0700 under a loose umask", func(t *testing.T) {
		e := newEnv(t)
		old := syscall.Umask(0)
		defer syscall.Umask(old)
		e.mustRun(true)
		if fi, err := os.Stat(e.dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("mode %v %v", fi.Mode().Perm(), err)
		}
	})
	t.Run("existing 0700 is used", func(t *testing.T) {
		e := newEnv(t)
		if err := os.Mkdir(e.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		e.mustRun(true)
	})
	for _, mode := range []os.FileMode{0o755, 0o750, 0o701, 0o707} {
		t.Run("existing "+mode.String()+" is refused", func(t *testing.T) {
			e := newEnv(t)
			if err := os.Mkdir(e.dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(e.dir, mode); err != nil {
				t.Fatal(err)
			}
			for _, apply := range []bool{false, true} {
				var err error
				writes := e.writesDuring(func() { _, err = e.run(apply) })
				if err == nil || !strings.Contains(err.Error(), "chmod 700 "+e.dir) {
					t.Fatalf("apply=%t: %v", apply, err)
				}
				if len(e.f.Requests()) != 0 || len(writes) != 0 {
					t.Fatal("the API was called before the directory was checked")
				}
			}
			if entries, _ := os.ReadDir(e.dir); len(entries) != 0 {
				t.Fatalf("files written: %v", entries)
			}
		})
	}
	t.Run("a file is refused", func(t *testing.T) {
		e := newEnv(t)
		if err := os.WriteFile(e.dir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.run(false); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("plan: %v", err)
		}
	})
}

func TestTeamDomainMustBeCloudflareAccess(t *testing.T) {
	e := newEnv(t)
	e.f.Mutate(func(s *cftest.State) { s.Org.AuthDomain = "login.example.com" })
	if _, err := e.run(false); err == nil || !strings.Contains(err.Error(), "<team>.cloudflareaccess.com") {
		t.Fatalf("plan: %v", err)
	}
}

func TestStateRecordsHostname(t *testing.T) {
	e := newEnv(t)
	e.mustRun(true)
	b, err := os.ReadFile(filepath.Join(e.dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil || s.Hostname != testHost {
		t.Fatalf("state %s %v", b, err)
	}
}
