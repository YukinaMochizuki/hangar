package edge_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/edge"
	"github.com/YukinaMochizuki/hangar/internal/edge/front/acmetest"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const (
	tenantDomain = "example.test"
	customHost   = "www." + tenantDomain
)

// customWorld is a world whose apiserver looks custom domains' challenge
// CNAMEs up in dns, holding custom domain "example" of the tenant, not yet
// verified.
func customWorld(t *testing.T) (*world, *acmetest.Server) {
	t.Helper()
	dns, err := acmetest.Start("127.0.0.1:0", tenantDomain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dns.Close)
	env := apiservertest.Start(t, func(c *apiserver.Config) { c.PublicEndpoints.Resolvers = []string{dns.Addr} })
	w := newWorldOn(t, env)
	w.customDomain(nil)
	return w, dns
}

// customDomain applies custom domain "example" with labels, which, changed,
// has the apiserver look for its challenge CNAME again.
func (w *world) customDomain(labels map[string]string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.CustomDomain, &networkv1alpha1.CustomDomain{
		Metadata: &metav1.ObjectMeta{Name: "example", Labels: labels},
		Spec:     &networkv1alpha1.CustomDomainSpec{Tenant: apiservertest.Tenant, Domain: tenantDomain},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// verify puts the domain's challenge CNAME in dns and waits for the edge to
// see the domain verified.
func (w *world) verify(dns *acmetest.Server) {
	w.t.Helper()
	var target string
	testutil.Eventually(w.t, "the domain's challenge target", func() (bool, string) {
		obj, err := w.root.Get(context.Background(), registry.CustomDomain, client.Key{Name: "example"})
		if err != nil {
			return false, err.Error()
		}
		target = obj.(*networkv1alpha1.CustomDomain).GetStatus().GetChallengeTarget()
		return target != "", "none yet"
	})
	dns.SetCNAME("_acme-challenge."+tenantDomain, target)
	w.customDomain(map[string]string{"checked": "again"})
	testutil.Eventually(w.t, "the edge to see the domain verified", func() (bool, string) {
		got := w.edge.VerifiedDomains()
		return got[tenantDomain] == target, fmt.Sprint(got)
	})
}

// publicAt applies endpoint site, PUBLIC on 8080, at hostnames.
func (w *world) publicAt(hostnames ...string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.PublicEndpoint, &networkv1alpha1.PublicEndpoint{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "site"},
		Spec: &networkv1alpha1.PublicEndpointSpec{
			Target:    &networkv1alpha1.PublicEndpointTarget{Object: &networkv1alpha1.PublicEndpointTarget_Container{Container: "api"}},
			Port:      8080,
			Access:    networkv1alpha1.PublicEndpointAccess_PUBLIC_ENDPOINT_ACCESS_PUBLIC,
			Hostnames: hostnames,
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// TestACustomHostnameServesOnceItsDomainIsVerified: an endpoint's hostname
// under a custom domain reaches it only once the domain is verified, as
// its own hostname always does, and no longer once the endpoint gives it
// up; a hostname under the domain no endpoint has is no endpoint.
func TestACustomHostnameServesOnceItsDomainIsVerified(t *testing.T) {
	w, dns := customWorld(t)
	w.publicAt(customHost)
	testutil.Eventually(w.t, "the endpoint at its own hostname", func() (bool, string) {
		a, body := w.do(http.MethodGet, host, "/", nil)
		return a.StatusCode == http.StatusOK, fmt.Sprint(a.StatusCode, body)
	})
	if a, body := w.do(http.MethodGet, customHost, "/", nil); a.StatusCode != http.StatusNotFound {
		t.Errorf("a hostname under a domain not verified: %d %q", a.StatusCode, body)
	}
	if w.edge.ServesCustomHost(customHost) {
		t.Error("the edge serves a hostname under a domain not verified")
	}

	w.verify(dns)
	testutil.Eventually(w.t, "the endpoint at its custom hostname", func() (bool, string) {
		a, body := w.do(http.MethodGet, customHost, "/", nil)
		return a.StatusCode == http.StatusOK && strings.HasPrefix(body, "port 8080 "), fmt.Sprint(a.StatusCode, body)
	})
	for _, h := range []string{strings.ToUpper(customHost) + ".", customHost + ":443"} {
		if !w.edge.ServesCustomHost(h) {
			t.Errorf("the edge does not serve %s", h)
		}
	}
	if a, body := w.do(http.MethodGet, "other."+tenantDomain, "/", nil); a.StatusCode != http.StatusNotFound {
		t.Errorf("a hostname of no endpoint under the domain: %d %q", a.StatusCode, body)
	}
	if a, body := w.do(http.MethodGet, customHost, "/.hangar/callback", nil); a.StatusCode != http.StatusNotFound {
		t.Errorf("the edge's own path at a custom hostname: %d %q", a.StatusCode, body)
	}

	w.publicAt()
	testutil.Eventually(w.t, "the custom hostname given up", func() (bool, string) {
		a, body := w.do(http.MethodGet, customHost, "/", nil)
		return a.StatusCode == http.StatusNotFound, fmt.Sprint(a.StatusCode, body)
	})
	if w.edge.ServesCustomHost(customHost) {
		t.Error("the edge serves a hostname no endpoint asks for")
	}
}

// TestCookiesAtACustomHostnameKeepADomainOfTheirOwn: at a custom hostname
// an endpoint's cookie keeps a Domain of the custom domain or under it,
// which reaches only the tenant's names, and loses any other, as a parent
// other tenants' domains can be under; Hangar's own names are still
// dropped. At the endpoint's own hostname every Domain goes, as ever.
func TestCookiesAtACustomHostnameKeepADomainOfTheirOwn(t *testing.T) {
	w, dns := customWorld(t)
	w.publicAt(customHost)
	w.verify(dns)
	var custom *answer
	testutil.Eventually(w.t, "the endpoint at its custom hostname", func() (bool, string) {
		a, body := w.do(http.MethodGet, customHost, "/set-cookie", nil)
		custom = a
		return a.StatusCode == http.StatusOK, fmt.Sprint(a.StatusCode, body)
	})
	own, _ := w.do(http.MethodGet, host, "/set-cookie", nil)
	for _, c := range []struct {
		where string
		a     *answer
	}{{"a custom hostname", custom}, {"its own hostname", own}} {
		// The backend's Domain is the endpoint domain, no custom domain's.
		want := []string{"theme=dark", "lang=en; Path=/"}
		if got := c.a.Header.Values("Set-Cookie"); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("at %s, Set-Cookie %q; want %q", c.where, got, want)
		}
	}
	for v, want := range map[string]string{
		"a=1; domain=" + tenantDomain:                   "a=1; domain=" + tenantDomain,
		"a=1; Domain=." + strings.ToUpper(tenantDomain): "a=1; Domain=." + strings.ToUpper(tenantDomain),
		"a=1; Domain=" + customHost + "; Path=/":        "a=1; Domain=" + customHost + "; Path=/",
		"a=1; Domain=test; Path=/":                      "a=1; Path=/",
		"a=1; Domain=other.test":                        "a=1",
		"a=1; Domain=not" + tenantDomain:                "a=1",
	} {
		h := http.Header{"Set-Cookie": {v, edge.EndpointCookie + "=x"}}
		edge.ConfineCustomCookies(h, tenantDomain)
		if got := h.Values("Set-Cookie"); len(got) != 1 || got[0] != want {
			t.Errorf("%q at a custom hostname under %s: %q; want %q", v, tenantDomain, got, want)
		}
	}
}

// hold sends a request for "/hold" at h, which the backend never answers,
// and returns, once the backend has it, the status it ends with: 0 for a
// connection the edge closed.
func (w *world) hold(h string) <-chan int {
	w.t.Helper()
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(w.t.Context(), http.MethodGet, w.url+"/hold", nil)
		req.Host = h
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		_ = resp.Body.Close()
		done <- resp.StatusCode
	}()
	<-w.held
	return done
}

// serving waits for the edge to serve the endpoint at its custom hostname.
func (w *world) serving() {
	w.t.Helper()
	testutil.Eventually(w.t, "the endpoint at its custom hostname", func() (bool, string) {
		a, body := w.do(http.MethodGet, customHost, "/", nil)
		return a.StatusCode == http.StatusOK, fmt.Sprint(a.StatusCode, body)
	})
}

// nested applies custom domain "www" of the tenant, the custom hostname
// itself, not verified: the hostname is under it now, and no longer serves.
func (w *world) nested() {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.CustomDomain, &networkv1alpha1.CustomDomain{
		Metadata: &metav1.ObjectMeta{Name: "www"},
		Spec:     &networkv1alpha1.CustomDomainSpec{Tenant: apiservertest.Tenant, Domain: customHost},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// TestARequestAtACustomHostnameEndsWhenItIsGivenUp: what a custom hostname
// let in does not outlive the endpoint's asking for it.
func TestARequestAtACustomHostnameEndsWhenItIsGivenUp(t *testing.T) {
	w, dns := customWorld(t)
	w.publicAt(customHost)
	w.verify(dns)
	w.serving()
	done := w.hold(customHost)
	w.publicAt()
	if code := <-done; code != http.StatusServiceUnavailable {
		t.Errorf("a request held at a hostname given up ended %d; want 503", code)
	}
}

// TestARequestAtACustomHostnameEndsWhenItsDomainNoLongerServesIt: the
// endpoint unchanged, a domain of the tenant's made at the hostname itself,
// not verified, takes it from the verified one above: what the hostname
// let in ends with it.
func TestARequestAtACustomHostnameEndsWhenItsDomainNoLongerServesIt(t *testing.T) {
	w, dns := customWorld(t)
	w.publicAt(customHost)
	w.verify(dns)
	w.serving()
	done := w.hold(customHost)
	w.nested()
	if code := <-done; code != http.StatusServiceUnavailable {
		t.Errorf("a request held at a hostname its domain no longer serves ended %d; want 503", code)
	}
	if a, body := w.do(http.MethodGet, customHost, "/", nil); a.StatusCode != http.StatusNotFound {
		t.Errorf("the hostname after: %d %q", a.StatusCode, body)
	}
}

// TestACustomHostnameThatStopsWhileARequestRegistersIsNotServed: a request
// routed to a custom hostname that stops serving before it is registered
// among those the edge ends is not served.
func TestACustomHostnameThatStopsWhileARequestRegistersIsNotServed(t *testing.T) {
	w, dns := customWorld(t)
	w.publicAt(customHost)
	w.verify(dns)
	w.serving()
	edge.SetRegistering(w.edge, func() {
		w.nested()
		testutil.Eventually(w.t, "the hostname to stop", func() (bool, string) {
			return !w.edge.ServesCustomHost(customHost), "still serving"
		})
	})
	a, body := w.do(http.MethodGet, customHost, "/", nil)
	edge.SetRegistering(w.edge, nil)
	if a.StatusCode != http.StatusServiceUnavailable || strings.Contains(body, "port 8080") {
		t.Errorf("a request whose hostname stopped as it registered: %d %q", a.StatusCode, body)
	}
}

// TestACustomHostnameServesOnlyUnderItsOwnTenantsDomain: an endpoint whose
// tenant's domain is gone, as while a tenant being deleted empties its
// projects, still asks for a hostname under it; another tenant then
// verifies the domain. The edge does not serve the endpoint there.
func TestACustomHostnameServesOnlyUnderItsOwnTenantsDomain(t *testing.T) {
	w, dns := customWorld(t)
	w.publicAt(customHost)
	ctx := context.Background()
	// Straight from the store, as the cascade's delete is, past the
	// refusal that keeps a domain in use; read again when the controller
	// wrote the domain's status in between.
	key := store.Key{Kind: registry.CustomDomain.Kind, Name: "example"}
	for {
		o, err := w.env.Store.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = w.env.Store.Delete(ctx, key, o.Revision)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	if _, err := w.root.Apply(ctx, registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.root.Apply(ctx, registry.CustomDomain, &networkv1alpha1.CustomDomain{
		Metadata: &metav1.ObjectMeta{Name: "theirs"},
		Spec:     &networkv1alpha1.CustomDomainSpec{Tenant: "other", Domain: tenantDomain},
	}); err != nil {
		t.Fatal(err)
	}
	var target string
	testutil.Eventually(w.t, "their challenge target", func() (bool, string) {
		obj, err := w.root.Get(ctx, registry.CustomDomain, client.Key{Name: "theirs"})
		if err != nil {
			return false, err.Error()
		}
		target = obj.(*networkv1alpha1.CustomDomain).GetStatus().GetChallengeTarget()
		return target != "", "none yet"
	})
	dns.SetCNAME("_acme-challenge."+tenantDomain, target)
	if _, err := w.root.Apply(ctx, registry.CustomDomain, &networkv1alpha1.CustomDomain{
		Metadata: &metav1.ObjectMeta{Name: "theirs", Labels: map[string]string{"checked": "again"}},
		Spec:     &networkv1alpha1.CustomDomainSpec{Tenant: "other", Domain: tenantDomain},
	}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "the edge to see their domain verified", func() (bool, string) {
		got := w.edge.VerifiedDomains()
		return got[tenantDomain] == target, fmt.Sprint(got)
	})
	if w.edge.ServesCustomHost(customHost) {
		t.Error("the edge serves a hostname under another tenant's domain")
	}
	if a, body := w.do(http.MethodGet, customHost, "/", nil); a.StatusCode != http.StatusNotFound {
		t.Errorf("a hostname under another tenant's domain: %d %q", a.StatusCode, body)
	}
}
