package front_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestCustomHostsReachTheEndpoints: a hostname the endpoints serve under a
// custom domain, whose certificates the front door keeps once delegated,
// reaches the endpoints over TLS and is redirected to on :80; another under
// the same domain, or a control plane name the endpoints claim, is no site.
// A domain no longer delegated loses its certificates.
func TestCustomHostsReachTheEndpoints(t *testing.T) {
	c := newCA(t)
	o := defaultOptions()
	served := "app." + tenantDomain
	o.customHost = func(h string) bool { return h == served || h == "evil."+cpDomain }
	d := startDoor(t, c, o)
	challenge := "k3x9.acme." + userDomain
	c.dns.SetCNAME("_acme-challenge."+tenantDomain, challenge)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan struct{}, 1)
	want := map[string]string{tenantDomain: challenge}
	current := make(chan map[string]string, 1)
	current <- want
	kept := make(chan struct{})
	go func() {
		d.certs.KeepDelegated(ctx, changes, func() map[string]string {
			m := <-current
			current <- m
			return m
		})
		close(kept)
	}()
	d.waitCertificates(t, served)

	if code, body, _ := do(t, d.client, http.MethodGet, "https://"+served+"/", nil, nil); code != http.StatusOK || !strings.HasPrefix(body, "endpoints ") {
		t.Errorf("a custom hostname served: %d %q", code, body)
	}
	if code, _, h := do(t, d.client1, http.MethodGet, "http://"+served+"/x?y=1", nil, nil); code != http.StatusPermanentRedirect || h.Get("Location") != "https://"+served+"/x?y=1" {
		t.Errorf("a custom hostname on :80: %d %q", code, h.Get("Location"))
	}
	if code, body, _ := do(t, d.client, http.MethodGet, "https://other."+tenantDomain+"/", nil, nil); code != http.StatusNotFound {
		t.Errorf("a hostname no endpoint serves under the domain: %d %q", code, body)
	}
	for _, u := range []string{"http://other." + tenantDomain + "/", "http://evil." + cpDomain + "/"} {
		if code, _, _ := do(t, d.client1, http.MethodGet, u, nil, nil); code != http.StatusNotFound {
			t.Errorf("%s on :80: %d, want 404", u, code)
		}
	}
	// A connection opened for the API carries none of the endpoints'.
	req, err := http.NewRequest(http.MethodGet, "https://"+apiHost+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = served
	resp, err := d.client1.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("a custom hostname on a connection for the API: %d", resp.StatusCode)
	}

	<-current
	current <- map[string]string{}
	changes <- struct{}{}
	testutil.EventuallyWithin(t, 10*time.Second, "the certificates of a domain no longer delegated to go", func() (bool, string) {
		conn, err := d.handshake(served)
		if err != nil {
			return true, ""
		}
		names := conn.ConnectionState().PeerCertificates[0].DNSNames
		_ = conn.Close()
		return false, strings.Join(names, ",")
	})
	if conn, err := d.handshake(apiHost); err != nil || !slices.Equal(conn.ConnectionState().PeerCertificates[0].DNSNames, []string{apiHost}) {
		t.Errorf("the API's certificate after a domain went: %v", err)
	} else {
		_ = conn.Close()
	}
	cancel()
	<-kept
}
