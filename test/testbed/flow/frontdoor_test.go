//go:build testbed

package flow

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/letsencrypt/pebble/v2/ca"
	"github.com/letsencrypt/pebble/v2/db"
	"github.com/letsencrypt/pebble/v2/va"
	"github.com/letsencrypt/pebble/v2/wfe"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/auth/oidctest"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	edgeproxy "github.com/YukinaMochizuki/hangar/internal/edge"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// The control plane's domain the front door serves api. and console. of,
// and the console's origin, sign-in's public URL.
const (
	doorCP      = "flowcp.test"
	doorConsole = "https://console." + doorCP
	doorAPI     = "https://api." + doorCP
)

// startDoorPebble runs Let's Encrypt's test CA in this process, validating
// DNS-01 against resolver, and returns its directory, the PEM file its
// directory's certificate chains to, and the roots what it issues chains
// to.
func startDoorPebble(t *testing.T, resolver string) (directory, directoryRoot string, roots *x509.CertPool) {
	t.Helper()
	t.Setenv("PEBBLE_VA_NOSLEEP", "1")
	t.Setenv("PEBBLE_WFE_NONCEREJECT", "0")
	t.Setenv("PEBBLE_AUTHZREUSE", "0")
	logger := log.New(io.Discard, "", 0)
	store := db.NewMemoryStore()
	authority := ca.New(logger, store, "", "ecdsa", 0, 1, map[string]ca.Profile{"default": {Description: "testbed"}})
	validator := va.New(logger, 0, 0, false, resolver, store)
	front := wfe.New(logger, store, validator, authority, []string{"pebble.letsencrypt.org"}, false, false, 1, 1)
	srv := httptest.NewTLSServer(front.Handler())
	t.Cleanup(srv.Close)
	directoryRoot = filepath.Join(workDir, "pebble-directory.pem")
	if err := os.WriteFile(directoryRoot, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	if !roots.AppendCertsFromPEM(authority.GetRootCert(0).PEM()) {
		t.Fatal("pebble's root does not parse")
	}
	return srv.URL + wfe.DirectoryPath, directoryRoot, roots
}

// lowPorts lets anyone bind :80 and :443 until the test ends, so the edge,
// never root, takes them as systemd's socket units give them on a host.
func lowPorts(t *testing.T) {
	t.Helper()
	const knob = "/proc/sys/net/ipv4/ip_unprivileged_port_start"
	before, err := os.ReadFile(knob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knob, []byte("80"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(knob, before, 0) })
}

// startFrontDoor runs hangar-edge as nobody, the front door on
// 127.0.0.1:443 and :80, with certificates from pebble over DNS-01 it
// answers itself (a hangar-edge built with the testbed tag), and returns
// the roots its certificates chain to.
func startFrontDoor(t *testing.T) *x509.CertPool {
	t.Helper()
	nobody := lookupUID(t, "nobody")
	dnsAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	directory, directoryRoot, roots := startDoorPebble(t, dnsAddr)
	storage := filepath.Join(workDir, "edge-certs")
	if err := os.MkdirAll(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(storage, nobody, nobody); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(apiDir, "hangar-edge")
	copyFile(t, vmtest.Artifacts().Edge, bin, 0o755)
	logf := openLog(t, "front-door.log")
	cmd := exec.Command(bin,
		"--api-socket", filepath.Join(apiDir, "edge.sock"),
		"--dial-socket", filepath.Join(apiDir, "edge-dial.sock"),
		"--public-socket", filepath.Join(apiDir, "public.sock"),
		"--domain", edgeDomain,
		"--cp-domain", doorCP,
		"--https-listen", "127.0.0.1:443",
		"--http-listen", "127.0.0.1:80",
		"--cert-storage", storage,
		"--acme-ca", directory,
		"--acme-ca-root", directoryRoot,
		"--acme-email", "flow@example.com",
		"--acme-dns", "testbed:"+dnsAddr,
	)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(nobody), Gid: uint32(nobody)},
		Pdeathsig:  syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("--- front door log:\n%s", tail(filepath.Join(workDir, "front-door.log")))
		}
	})
	return roots
}

// doorBrowser is a browser whose names under .test reach the front door,
// trusting its certificates; others, the provider's, their own address.
// It records every URL it is redirected to.
func doorBrowser(roots *x509.CertPool, visited *[]string) *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			if strings.HasSuffix(host, ".test") {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: tr, Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
		if visited != nil {
			*visited = append(*visited, r.URL.String())
		}
		return nil
	}}
}

// browse has b get u, and returns the status, the final URL and the body.
func browse(t *testing.T, b *http.Client, u string) (int, *url.URL, string) {
	t.Helper()
	resp, err := b.Get(u)
	if err != nil {
		return 0, nil, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, resp.Request.URL, string(body)
}

// withOrigin sends Origin on every call, as a browser does on the
// console's own pages.
func withOrigin(origin string) connect.Option {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Origin", origin)
			return next(ctx, req)
		}
	}))
}

// withBearer sends a Hangar token, as the CLI and agents do.
func withBearer(token string) connect.Option {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}))
}

// TestTheDirectPath is the platform with nothing in front of it, as a host
// runs it after the cutover from Cloudflare: the apiserver signs people in
// through a provider (a fake one here) and the front door, hangar-edge as
// nobody on :443, is all there is between them and the internet. A person
// signs in to the console through the front door and signs up; an agent's
// token reaches the API at api.; a machine's PRIVATE endpoint hands the
// signed-in browser off from the console and serves it, and a PUBLIC one
// serves anyone.
func TestTheDirectPath(t *testing.T) {
	if installed {
		t.Skip("the installed services are deploy/host's; test/testbed/host-bootstrap.sh tests their front door")
	}
	lowPorts(t)
	for _, port := range []string{"80", "443"} {
		if ln, err := net.Listen("tcp", "127.0.0.1:"+port); err != nil {
			t.Fatalf("the front door needs 127.0.0.1:%s: %v", port, err)
		} else {
			_ = ln.Close()
		}
	}
	provider := oidctest.New(t)
	provider.SignIn(admin, true)
	nobody := lookupUID(t, "nobody")
	c := startClusterWith(t, func(c *cluster) {
		secret := filepath.Join(apiDir, "oidc-client-secret")
		if err := os.WriteFile(secret, []byte(provider.ClientSecret+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(secret, nobody, nobody); err != nil {
			t.Fatal(err)
		}
		c.authArgs = []string{
			"--auth-mode", "oidc", "--oidc-issuer", provider.URL, "--oidc-client-id", provider.ClientID,
			"--oidc-client-secret-file", secret, "--public-url", doorConsole,
		}
		c.apiArgs = []string{
			"--edge-socket", filepath.Join(apiDir, "edge.sock"), "--public-socket", filepath.Join(apiDir, "public.sock"),
			"--edge-uid", strconv.Itoa(nobody), "--endpoint-domain", edgeDomain,
		}
		c.nodeArgs = []string{"--edge-dial-socket", filepath.Join(apiDir, "edge-dial.sock"), "--edge-user", "nobody"}
	})
	roots := startFrontDoor(t)

	// The certificates come, one for each name, over DNS-01.
	b := doorBrowser(roots, nil)
	eventually(t, 2*time.Minute, "the front door's certificates", func() (bool, string) {
		var last []string
		ok := true
		for _, u := range []string{doorAPI + "/healthz", doorConsole + "/", "https://nothing." + edgeDomain + "/"} {
			status, _, body := browse(t, b, u)
			if status == 0 {
				ok = false
			}
			last = append(last, fmt.Sprintf("%s: %d %.80s", u, status, body))
		}
		return ok, strings.Join(last, "; ")
	})
	if status, _, body := browse(t, b, doorAPI+"/healthz"); status != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Fatalf("the API's health through the front door: %d %q", status, body)
	}

	t.Run("a person signs in to the console through the front door, and signs up", func(t *testing.T) {
		var visited []string
		b = doorBrowser(roots, &visited)
		// The bootstrap admin is invited once the apiserver has granted the
		// role, in the background.
		eventually(t, 30*time.Second, "signing in", func() (bool, string) {
			visited = nil
			status, at, body := browse(t, b, doorConsole+"/auth/login?return=/projects/flow")
			return at != nil && at.String() == doorConsole+"/projects/flow", fmt.Sprintf("%d at %v: %.200s", status, at, body)
		})
		if !strings.Contains(strings.Join(visited, " "), provider.URL) {
			t.Errorf("signed in without the provider: %v", visited)
		}
		var cookie bool
		for _, ck := range b.Jar.Cookies(&url.URL{Scheme: "https", Host: "console." + doorCP, Path: "/"}) {
			cookie = cookie || ck.Name == session.CookieName
		}
		if !cookie {
			t.Fatalf("no %s cookie for the console after signing in", session.CookieName)
		}
		accounts := iamv1alpha1connect.NewAccountServiceClient(b, doorConsole, withOrigin(doorConsole))
		if _, err := accounts.SignUp(t.Context(), connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: "flow-admin"})); err != nil {
			t.Fatalf("signing up through the console: %v", err)
		}
		who, err := accounts.WhoAmI(t.Context(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
		if err != nil || !strings.Contains(who.Msg.String(), admin) {
			t.Fatalf("WhoAmI through the console: %v %v", who, err)
		}
		// The session counts on the console's own origin alone.
		other := iamv1alpha1connect.NewAccountServiceClient(b, doorConsole, withOrigin("https://evil.example"))
		if _, err := other.WhoAmI(t.Context(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("WhoAmI from another origin: %v; want unauthenticated", err)
		}
	})

	t.Run("an agent's token reaches the API through the front door", func(t *testing.T) {
		secret := strings.TrimSpace(c.cli("token", "create", "door-agent", "--role", "project.viewer"))
		api := iamv1alpha1connect.NewAccountServiceClient(doorBrowser(roots, nil), doorAPI, withBearer(secret))
		var who *connect.Response[iamv1alpha1.WhoAmIResponse]
		eventually(t, 30*time.Second, "the token to work", func() (bool, string) {
			var err error
			who, err = api.WhoAmI(t.Context(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
			return err == nil, fmt.Sprint(err)
		})
		if who.Msg.GetType() != "serviceToken" || who.Msg.GetId() != project+"/door-agent" {
			t.Errorf("WhoAmI with the token at api.: %v", who.Msg)
		}
		bad := iamv1alpha1connect.NewAccountServiceClient(doorBrowser(roots, nil), doorAPI, withBearer("hgr_"+strings.Repeat("A", 43)))
		if _, err := bad.WhoAmI(t.Context(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("a made-up token at api.: %v; want unauthenticated", err)
		}
	})

	c.applyYAML(lockedGroupYAML())
	c.applyYAML(fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata: {name: door-site}
spec:
  image: %s
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi}
  interfaces: [{network: default, securityGroups: [locked]}]
  ports: [{name: http, port: 80, public: true}]
`, nginxRef))
	t.Cleanup(func() {
		_, _ = c.tryCLI(nil, "delete", "container", "door-site")
		_, _ = c.tryCLI(nil, "wait", "container", "door-site", "--for=delete", "--timeout=2m")
	})
	c.waitReady("door-site", 5*time.Minute)
	c.cli("wait", "publicendpoint", "door-site-http", "--for=condition=Ready", "--timeout=60s")
	private := "https://door-site-http-" + project + "." + edgeDomain

	t.Run("a private endpoint hands the signed-in browser off from the console", func(t *testing.T) {
		var visited []string
		hb := &http.Client{Transport: b.Transport, Jar: b.Jar, Timeout: 30 * time.Second, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
			visited = append(visited, r.URL.String())
			return nil
		}}
		// Every attempt's redirects are kept: one handoff can end on a 502,
		// while nginx starts, and the next attempt carries its session.
		var body string
		eventually(t, 60*time.Second, "the private endpoint through the handoff", func() (bool, string) {
			var status int
			var at *url.URL
			status, at, body = browse(t, hb, private+"/")
			return status == http.StatusOK && strings.Contains(body, "Welcome to nginx"), fmt.Sprintf("%d at %v via %v: %.200s", status, at, visited, body)
		})
		if !strings.Contains(strings.Join(visited, " "), doorConsole+"/auth/endpoint-login") {
			t.Errorf("not handed off through the console: %v", visited)
		}
		if strings.Contains(strings.Join(visited, " "), provider.URL) {
			t.Errorf("signed in at the provider again, with a console session: %v", visited)
		}
		var names []string
		for _, ck := range hb.Jar.Cookies(&url.URL{Scheme: "https", Host: strings.TrimPrefix(private, "https://"), Path: "/"}) {
			names = append(names, ck.Name)
		}
		if len(names) != 1 || names[0] != edgeproxy.EndpointCookie {
			t.Errorf("cookies on the endpoint: %v; want its session alone", names)
		}
		// A stranger's browser goes to sign in, as someone not invited, and
		// gets no further.
		provider.SignIn("stranger@example.com", true)
		status, at, body := browse(t, doorBrowser(roots, nil), private+"/")
		provider.SignIn(admin, true)
		if strings.Contains(body, "Welcome to nginx") || at == nil || at.Host == strings.TrimPrefix(private, "https://") {
			t.Errorf("a stranger's browser ended at %v with %d: %.200s", at, status, body)
		}
	})

	t.Run("a public endpoint serves anyone", func(t *testing.T) {
		c.applyYAML(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: PublicEndpoint
metadata: {name: door-pub}
spec: {target: {container: door-site}, port: 80, access: PUBLIC_ENDPOINT_ACCESS_PUBLIC}
`)
		c.cli("wait", "publicendpoint", "door-pub", "--for=condition=Ready", "--timeout=60s")
		public := "https://door-pub-" + project + "." + edgeDomain + "/"
		eventually(t, 60*time.Second, "the public endpoint", func() (bool, string) {
			status, _, body := browse(t, doorBrowser(roots, nil), public)
			return status == http.StatusOK && strings.Contains(body, "Welcome to nginx"), fmt.Sprintf("%d %.200s", status, body)
		})
		c.cli("delete", "publicendpoint", "door-pub")
	})
}
