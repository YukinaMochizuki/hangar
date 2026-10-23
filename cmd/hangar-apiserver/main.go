// Command hangar-apiserver serves the Hangar API on five listeners: a TCP
// address on loopback for people and agents, who sign in through Google
// (OIDC) and then hold Hangar's own sessions; a unix socket hangar-edge
// forwards the same public API to from :443, with each client's address;
// one for the node agent; one for hangar-edge's own reads; and one for root
// on the host. See docs/decisions/authentication.md and
// docs/decisions/front-door.md. With --metrics-listen it also serves
// /metrics, and nothing else, on a loopback address of its own
// (docs/decisions/monitoring.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/metrics"
	"github.com/YukinaMochizuki/hangar/internal/sdsocket"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
	"github.com/YukinaMochizuki/hangar/internal/sshca"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

type flags struct {
	db              string
	listen          string
	socket          string
	adminSocket     string
	nodeUID         uint
	nodeName        string
	authMode        string
	oidcIssuer      string
	oidcClientID    string
	oidcSecretFile  string
	publicURL       string
	bootstrapAdmin  string
	sshCAKey        string
	edgeSocket      string
	publicSocket    string
	edgeUID         uint
	endpointDomain  string
	domainResolvers []string
	secretsKEK      string
	previousKEKs    []string
	metricsListen   string
	// prometheusURL and alertmanagerURL are what MonitoringService asks.
	prometheusURL   string
	alertmanagerURL string
	// maxOwnedTenants is the default cap on the tenants one person or
	// service token owns.
	maxOwnedTenants int
	// maxUploadSize bounds a file uploaded for a CustomImage, a quantity.
	maxUploadSize string
}

func main() {
	var f flags
	flag.StringVar(&f.db, "db", "hangar.db", "path of the SQLite database")
	flag.StringVar(&f.listen, "listen", "127.0.0.1:8080", "address for the public API")
	flag.StringVar(&f.socket, "socket", "", "unix socket for the node agent; empty disables it")
	flag.StringVar(&f.adminSocket, "admin-socket", "", "unix socket for root on this host, as platform.root; empty disables it")
	flag.UintVar(&f.nodeUID, "node-uid", 0, "uid accepted as the node agent on the socket")
	flag.StringVar(&f.nodeName, "node-name", "", "name of the Node the agent on the socket runs; required with --socket")
	flag.StringVar(&f.authMode, "auth-mode", "oidc", "how public callers authenticate: oidc (sign-in through the OIDC provider, then Hangar's sessions) or dev")
	flag.StringVar(&f.oidcIssuer, "oidc-issuer", "https://accounts.google.com", "the OIDC provider people sign in through")
	flag.StringVar(&f.oidcClientID, "oidc-client-id", "", "the apiserver's OAuth client ID at the provider; required with oidc auth")
	flag.StringVar(&f.oidcSecretFile, "oidc-client-secret-file", "", "a file only its owner reads (0600) holding the OAuth client's secret; required with oidc auth")
	flag.StringVar(&f.publicURL, "public-url", "", "the console's origin, such as https://console.hangarlab.cloud; the provider redirects to its /auth/callback; required with oidc auth")
	flag.StringVar(&f.bootstrapAdmin, "bootstrap-admin", "", "email granted platform.root if no one holds it")
	flag.StringVar(&f.sshCAKey, "ssh-ca-key", "", "the platform SSH CA's private key, an OpenSSH key file only its owner reads; missing, hangar ssh cannot sign")
	flag.StringVar(&f.edgeSocket, "edge-socket", "", "unix socket for hangar-edge, which reads public endpoints and role bindings; empty disables it")
	flag.StringVar(&f.publicSocket, "public-socket", "", "unix socket hangar-edge forwards the public API to, from :443, with each client's address; empty disables it")
	flag.UintVar(&f.edgeUID, "edge-uid", 0, "uid accepted as hangar-edge on the edge and public sockets; required with either")
	flag.StringVar(&f.endpointDomain, "endpoint-domain", apiserver.DefaultEndpointDomain, "the domain public endpoints' hostnames are in")
	flag.Func("custom-domain-resolver", "a recursive DNS server, as host:port, that finds the nameservers custom domains' challenge CNAMEs are looked up at, and is asked itself when none of those answers; repeatable; none is the system's", func(s string) error {
		f.domainResolvers = append(f.domainResolvers, s)
		return nil
	})
	flag.StringVar(&f.secretsKEK, "secrets-kek", "", "the key-encryption key Secrets are sealed under, a file only its owner reads; missing, Secrets cannot be written, and the apiserver will not start while any are stored")
	flag.Func("secrets-kek-previous", "a previous key-encryption key, only to open what it sealed, which is sealed again under --secrets-kek at start; repeatable", func(s string) error {
		f.previousKEKs = append(f.previousKEKs, s)
		return nil
	})
	flag.StringVar(&f.metricsListen, "metrics-listen", "", "loopback address to serve /metrics on, such as 127.0.0.1:9101, apart from the API; empty disables it")
	flag.StringVar(&f.prometheusURL, "prometheus-url", "", "the host's Prometheus on loopback, such as http://127.0.0.1:9090, which MonitoringService asks; empty, it answers Unavailable")
	flag.StringVar(&f.alertmanagerURL, "alertmanager-url", "", "the host's Alertmanager on loopback, such as http://127.0.0.1:9093, which MonitoringService asks for alerts; empty, it answers Unavailable")
	flag.IntVar(&f.maxOwnedTenants, "default-max-owned-tenants", apiserver.DefaultMaxOwnedTenants,
		"how many tenants one person or service token may own, unless their User says otherwise; at least 1")
	flag.StringVar(&f.maxUploadSize, "max-upload-size", fmt.Sprintf("%dGi", apiserver.DefaultMaxUploadSize>>30),
		"the largest file a CustomImage may be uploaded from, as a quantity such as 16Gi")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, f, log); err != nil {
		log.Error("apiserver stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, f flags, log *slog.Logger) error {
	socks, err := sdsocket.Inherit()
	if err != nil {
		return err
	}
	defer socks.Close()
	return serveAll(ctx, f, log, socks)
}

// Names systemd gives the sockets it passes (FileDescriptorName=), set in
// deploy/host/steps/80-services.sh.
const (
	nodeSocketName   = "node"
	adminSocketName  = "admin"
	edgeSocketName   = "edge"
	publicSocketName = "public"
)

func serveAll(ctx context.Context, f flags, log *slog.Logger, socks *sdsocket.Sockets) error {
	if err := checkAuthFlags(f); err != nil {
		return err
	}
	if err := checkBootstrapAdmin(f.bootstrapAdmin); err != nil {
		return err
	}
	// Empty is the default.
	var maxUpload uint64
	if f.maxUploadSize != "" {
		b, err := quantity.Bytes(f.maxUploadSize)
		if err != nil || b == 0 || b > math.MaxInt64 {
			return fmt.Errorf("--max-upload-size: %q is not a size above zero", f.maxUploadSize)
		}
		maxUpload = b
	}
	st, err := sqlite.Open(ctx, f.db)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

	for _, fl := range []struct{ name, url string }{{"--prometheus-url", f.prometheusURL}, {"--alertmanager-url", f.alertmanagerURL}} {
		if fl.url == "" {
			continue
		}
		if err := apiserver.CheckMonitoringURL(fl.url); err != nil {
			return fmt.Errorf("%s: %w", fl.name, err)
		}
	}
	if f.maxOwnedTenants < 1 {
		return fmt.Errorf("--default-max-owned-tenants is %d; it must be at least 1", f.maxOwnedTenants)
	}
	if f.socket != "" && f.nodeName == "" {
		return errors.New("--socket needs --node-name: the agent on it acts as that Node")
	}
	if f.nodeName != "" {
		if err := apiserver.ValidName(f.nodeName); err != nil {
			return fmt.Errorf("--node-name: %w", err)
		}
	}
	var nodes []string
	if f.nodeName != "" {
		nodes = []string{f.nodeName}
	}
	host := f.nodeName
	if host == "" {
		if host, err = os.Hostname(); err != nil {
			return err
		}
	}

	if (f.edgeSocket != "" || f.publicSocket != "") && f.edgeUID == 0 {
		return errors.New("--edge-socket and --public-socket need --edge-uid: hangar-edge runs as its own unprivileged user, never root")
	}
	var sock, admin, edge, publicSock net.Listener
	if f.socket != "" {
		if sock, err = socks.Unix(nodeSocketName, f.socket, 0o660); err != nil {
			return err
		}
		defer func() { _ = sock.Close() }()
	}
	if f.adminSocket != "" {
		if admin, err = socks.Unix(adminSocketName, f.adminSocket, 0o600); err != nil {
			return err
		}
		defer func() { _ = admin.Close() }()
	}
	if f.edgeSocket != "" {
		if edge, err = socks.Unix(edgeSocketName, f.edgeSocket, 0o660); err != nil {
			return err
		}
		defer func() { _ = edge.Close() }()
	}
	if f.publicSocket != "" {
		if publicSock, err = socks.Unix(publicSocketName, f.publicSocket, 0o660); err != nil {
			return err
		}
		defer func() { _ = publicSock.Close() }()
	}
	if err := socks.Unused(); err != nil {
		return err
	}
	// Keepalive and a user timeout drop a dead client under an exec whose
	// data is backed up, which no ping can tell (internal/execws).
	ln, err := (&net.ListenConfig{KeepAliveConfig: execws.KeepAlive, Control: execws.Control}).Listen(ctx, "tcp", f.listen)
	if err != nil {
		return err
	}
	var metricsLn net.Listener
	if f.metricsListen != "" {
		if metricsLn, err = metrics.Listen(ctx, f.metricsListen); err != nil {
			return fmt.Errorf("--metrics-listen: %w", err)
		}
		defer func() { _ = metricsLn.Close() }()
	}

	ca, err := loadSSHCA(f.sshCAKey, log)
	if err != nil {
		return err
	}
	keys, err := loadKeyring(f.secretsKEK, f.previousKEKs, log)
	if err != nil {
		return err
	}
	srv := apiserver.New(apiserver.Config{
		Store: st, Logger: log, BootstrapAdmin: strings.ToLower(f.bootstrapAdmin), Nodes: nodes, SSHCA: ca, Secrets: keys,
		PublicEndpoints: apiserver.PublicEndpointsConfig{
			Domain: f.endpointDomain, ControlPlaneDomain: controlPlaneDomain(f.publicURL), Resolvers: f.domainResolvers,
		},
		DefaultMaxOwnedTenants: f.maxOwnedTenants, Credentials: st, MaxUploadSize: int64(maxUpload),
		Monitoring: apiserver.MonitoringConfig{PrometheusURL: f.prometheusURL, AlertmanagerURL: f.alertmanagerURL},
	})
	public, err := publicChain(f, srv, st)
	if err != nil {
		return err
	}
	// A key-encryption key that does not open what is stored stops the
	// apiserver here, before it serves anything.
	if err := srv.PrepareSecrets(ctx); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(ctx) })
	if metricsLn != nil {
		reg := metrics.NewRegistry()
		reg.MustRegister(srv.Collectors()...)
		g.Go(func() error { return metrics.Serve(ctx, metricsLn, reg, log) })
	}
	g.Go(func() error {
		log.Info("serving the public API", "address", ln.Addr().String(), "auth", f.authMode)
		return serve(ctx, &http.Server{Handler: auth.ClientIPFromConn(srv.PublicHandler(public)), Protocols: protocols()}, ln)
	})
	if publicSock != nil {
		// The public listener's own handler, sign-in included, from
		// hangar-edge alone, which says each request's client address.
		h := auth.TrustClientIPFrom([]uint32{uint32(f.edgeUID)}, srv.PublicHandler(public))
		g.Go(func() error {
			log.Info("serving the public API to hangar-edge", "path", f.publicSocket, "auth", f.authMode)
			return serve(ctx, &http.Server{Handler: h, Protocols: protocols(), ConnContext: auth.ProxyConnContext}, publicSock)
		})
	}
	if sock != nil {
		node := auth.Chain{auth.PeerCred{NodeUIDs: []uint32{uint32(f.nodeUID)}, NodeName: f.nodeName}}
		g.Go(func() error {
			log.Info("serving the node socket", "path", f.socket)
			return serve(ctx, &http.Server{Handler: srv.Handler(node), Protocols: protocols(), ConnContext: auth.ConnContext}, sock)
		})
	}
	if edge != nil {
		// Nothing but PeerEdge, which reads only EdgeConnContext's record.
		chain := auth.Chain{auth.PeerEdge{UIDs: []uint32{uint32(f.edgeUID)}, Host: host}}
		g.Go(func() error {
			log.Info("serving the edge socket", "path", f.edgeSocket)
			return serve(ctx, &http.Server{Handler: srv.EdgeHandler(chain), Protocols: protocols(), ConnContext: auth.EdgeConnContext}, edge)
		})
	}
	if admin != nil {
		// Nothing but LocalRoot, which reads only AdminConnContext's record:
		// no header can say who is calling here.
		root := auth.Chain{auth.LocalRoot{Host: host, UID: adminUID}}
		g.Go(func() error {
			log.Info("serving the admin socket", "path", f.adminSocket)
			h := auditAdmin(log, root, srv.AdminHandler(root))
			return serve(ctx, &http.Server{Handler: h, Protocols: protocols(), ConnContext: auth.AdminConnContext}, admin)
		})
	}
	return g.Wait()
}

// adminUID is the uid the admin socket accepts: root, except in tests.
var adminUID uint32

// auditAdmin logs every request on the admin socket, accepted or not, with
// the process that made it: what root does there bypasses every binding.
func auditAdmin(log *slog.Logger, chain auth.Chain, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attrs := []any{"method", r.Method, "path", r.URL.Path}
		if cred, ok := auth.AdminPeer(r.Context()); ok {
			attrs = append(attrs, "uid", cred.UID, "pid", cred.PID)
		}
		if ctx, err := chain.Authenticate(r.Context(), r.Header); err != nil {
			log.Warn("admin socket request refused", append(attrs, "error", err.Error())...)
		} else {
			p, _ := auth.FromContext(ctx)
			log.Info("admin socket request", append(attrs, "principal", p.String())...)
		}
		next.ServeHTTP(w, r)
	})
}

// checkAuthFlags refuses an auth mode that is not whole, before anything
// opens.
func checkAuthFlags(f flags) error {
	switch f.authMode {
	case "oidc":
		if f.oidcClientID == "" || f.oidcSecretFile == "" || f.publicURL == "" {
			return errors.New("oidc auth needs --oidc-client-id, --oidc-client-secret-file and --public-url")
		}
		if _, err := login.ParseOrigin(f.publicURL); err != nil {
			return fmt.Errorf("--public-url: %w", err)
		}
		return nil
	case "dev":
		if !loopback(f.listen) {
			return fmt.Errorf("dev auth trusts a plain header, so it only runs on a loopback IP such as 127.0.0.1:8080 or [::1]:8080, not %q", f.listen)
		}
		// What reaches the public socket comes from the internet.
		if f.publicSocket != "" {
			return errors.New("dev auth trusts a plain header, so it never serves --public-socket, which hangar-edge forwards the internet to")
		}
		return nil
	}
	return fmt.Errorf("unknown auth mode %q; want oidc or dev", f.authMode)
}

// checkBootstrapAdmin refuses a --bootstrap-admin that is not ASCII, before
// it is lower-cased: lower-casing some other characters makes an ASCII
// letter (U+212A KELVIN SIGN becomes k), so it could grant platform.root
// to someone else's address.
func checkBootstrapAdmin(email string) error {
	if !auth.ASCII(email) {
		return fmt.Errorf("--bootstrap-admin %q must be ASCII", email)
	}
	return nil
}

// publicChain is the public listener's authentication: in oidc auth, the
// credentials sign-in issues, kept in st, and service tokens; in dev auth,
// the dev header, then service tokens.
func publicChain(f flags, srv *apiserver.Server, st *sqlite.Store) (auth.Chain, error) {
	if f.authMode == "dev" {
		tokens, ok := srv.ServiceTokens()
		if !ok {
			return nil, errors.New("dev auth takes service tokens, which need the credentials store")
		}
		return auth.Chain{auth.Dev{}, tokens}, nil
	}
	secret, err := readSecretFile(f.oidcSecretFile)
	if err != nil {
		return nil, fmt.Errorf("--oidc-client-secret-file: %w", err)
	}
	return srv.EnableOIDC(apiserver.OIDCConfig{
		Issuer: f.oidcIssuer, ClientID: f.oidcClientID, ClientSecret: secret, PublicURL: f.publicURL,
	}, st)
}

// loopback holds when addr's host is a loopback IP (127.0.0.0/8 or ::1).
// Not localhost, which a hosts file can point anywhere.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// protocols serves HTTP/1.1 and cleartext HTTP/2 on the same port, so
// streaming works whichever a client on the host speaks; the front door
// speaks HTTP/1.1.
func protocols() *http.Protocols {
	p := &http.Protocols{}
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	// Requests run under ctx, so stopping ends open watches and Shutdown
	// does not wait for them.
	srv.BaseContext = func(net.Listener) context.Context { return ctx }
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	return nil
}

// loadKeyring loads the key-encryption keys. Without a current one, it is
// nil: Secrets cannot be written, and PrepareSecrets refuses to start over
// stored ones. A previous key's file may be missing, as it is but while a
// key is rotated (docs/runbook/host-bootstrap.md), and is then skipped. A
// key file that is there but unusable fails, and so does a previous key
// without a current one.
func loadKeyring(current string, previous []string, log *slog.Logger) (*secrets.Keyring, error) {
	if current == "" {
		if len(previous) > 0 {
			return nil, errors.New("--secrets-kek-previous needs --secrets-kek")
		}
		log.Warn("no --secrets-kek: Secrets cannot be written")
		return nil, nil
	}
	kek, err := secrets.LoadKEK(current)
	if errors.Is(err, fs.ErrNotExist) {
		for _, p := range previous {
			if _, err := os.Stat(p); err == nil {
				return nil, fmt.Errorf("--secrets-kek-previous %s is there, and --secrets-kek %s is not: a rotation needs the new key too", p, current)
			}
		}
		log.Warn("the key-encryption key file is missing: Secrets cannot be written", "secrets_kek", current)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("--secrets-kek: %w", err)
	}
	var olds []secrets.KEK
	for _, p := range previous {
		k, err := secrets.LoadKEK(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("--secrets-kek-previous: %w", err)
		}
		olds = append(olds, k)
	}
	keys, err := secrets.NewKeyring(kek, olds...)
	if err != nil {
		return nil, err
	}
	log.Info("loaded the key-encryption key", "secrets_kek", current, "id", kek.ID, "previous", len(olds))
	return keys, nil
}

// loadSSHCA loads the SSH CA's key at path. With no path or no file the
// apiserver runs without one, and says so; a key it cannot use stops it.
func loadSSHCA(path string, log *slog.Logger) (*sshca.CA, error) {
	if path == "" {
		log.Warn("no --ssh-ca-key: hangar ssh cannot sign keys; static keys still work")
		return nil, nil
	}
	ca, err := sshca.Load(path)
	if sshca.IsMissing(err) {
		log.Warn("the SSH CA's key file is missing: hangar ssh cannot sign keys; static keys still work", "ssh_ca_key", path)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("--ssh-ca-key: %w", err)
	}
	log.Info("loaded the SSH CA", "ssh_ca_key", path, "public_key", ca.PublicKey())
	return ca, nil
}

// readSecretFile reads a credential from a file only its owner may read,
// trimmed of surrounding space.
func readSecretFile(path string) (token.Secret, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is mode %v; a credential is readable by its owner only (0600)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token.Secret(secret), nil
}

// controlPlaneDomain is the domain of the console's origin, which no
// custom domain may share a site with; "" without one.
func controlPlaneDomain(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
