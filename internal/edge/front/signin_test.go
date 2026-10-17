package front_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/oidctest"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestSignInThroughTheFrontDoor: a browser signs in to the console through
// the front door, which hands the apiserver's public socket every sign-in
// request: the provider returns to the console's /auth/callback, the
// session's __Host- cookie is set for the console's origin and works there
// with that exact Origin and no other, and the sign-in's audit line has the
// browser's address.
func TestSignInThroughTheFrontDoor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "hangar.db"))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	srv := apiserver.New(apiserver.Config{
		Store: st, BootstrapAdmin: "alice@example.com", DefaultPlanLimits: &tenancyv1alpha1.Limits{},
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
	})
	p := oidctest.New(t)
	origin := "https://" + consoleHost
	chain, err := srv.EnableOIDC(apiserver.OIDCConfig{
		Issuer: p.URL, ClientID: p.ClientID, ClientSecret: token.Secret(p.ClientSecret), PublicURL: origin,
	}, st)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = st.Close()
	})

	o := defaultOptions()
	// The apiserver's public socket, as the apiserver serves it.
	o.api = auth.TrustClientIPFrom([]uint32{uint32(os.Getuid())}, srv.PublicHandler(chain))
	o.apiConnContext = auth.ProxyConnContext
	o.consoleDir = consoleFiles(t)
	d := startDoor(t, newCA(t), o)

	// A browser: the front door's names reach the front door, the
	// provider's its own address.
	tr := d.client1.Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		if strings.HasSuffix(host, ".test") {
			addr = d.https
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Transport: tr, Jar: jar}

	// The bootstrap admin is invited once the apiserver has granted her
	// the role in the background.
	var final *url.URL
	testutil.Eventually(t, "signing in ends at the console", func() (bool, string) {
		p.SignIn("alice@example.com", true)
		resp, err := browser.Get(origin + "/auth/login?return=/projects/demo")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		final = resp.Request.URL
		if final.Path != "/projects/demo" {
			return false, fmt.Sprintf("%s: %d %q", final, resp.StatusCode, b)
		}
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "placeholder console") {
			t.Fatalf("after sign-in, the console: %d %q", resp.StatusCode, b)
		}
		return true, ""
	})
	if final.Scheme != "https" || final.Host != consoleHost {
		t.Errorf("signed in back at %s; want the console's origin", final)
	}
	var cookie *http.Cookie
	for _, c := range jar.Cookies(&url.URL{Scheme: "https", Host: consoleHost, Path: "/"}) {
		if c.Name == session.CookieName {
			cookie = c
		}
	}
	if cookie == nil || !strings.HasPrefix(session.CookieName, "__Host-") {
		t.Fatalf("no %s cookie for the console after sign-in", session.CookieName)
	}

	whoami := func(origin string) error {
		c := iamv1alpha1connect.NewAccountServiceClient(browser, "https://"+consoleHost, connect.WithInterceptors(connect.UnaryInterceptorFunc(
			func(next connect.UnaryFunc) connect.UnaryFunc {
				return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
					req.Header().Set("Origin", origin)
					return next(ctx, req)
				}
			})))
		_, err := c.WhoAmI(t.Context(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
		return err
	}
	if err := whoami(origin); err != nil {
		t.Errorf("WhoAmI with the session, from the console's origin: %v", err)
	}
	for _, other := range []string{"https://api." + cpDomain, "https://" + consoleHost + ":8443", "http://" + consoleHost, "https://evil.example"} {
		if err := whoami(other); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("WhoAmI with the session, from %s: %v; want unauthenticated", other, err)
		}
	}
	if got := logs.String(); !strings.Contains(got, `msg="signed in on a browser" principal=user:alice@example.com client_ip=127.0.0.1`) {
		t.Errorf("the sign-in is not logged with the browser's address:\n%s", got)
	}
}
