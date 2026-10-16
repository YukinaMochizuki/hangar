// Command hangar-edge is the edge proxy. With --cp-domain it is the host's
// front door (internal/edge/front): the only listener on :443 and :80,
// which systemd passes it, ending TLS with certificates it gets over ACME
// DNS-01, and routing by Host to the apiserver, the console's files and the
// public endpoints. For tests, it can also serve public endpoints over plain
// HTTP on loopback (--listen). It runs as its own
// unprivileged user, reads the API through the apiserver's edge socket,
// forwards the API through its public socket, and reaches machines through
// the node agent's edge dial socket. See docs/decisions/public-endpoints.md
// and docs/decisions/front-door.md.
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"golang.org/x/sync/errgroup"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1/networkv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/edge"
	"github.com/YukinaMochizuki/hangar/internal/edge/front"
	"github.com/YukinaMochizuki/hangar/internal/sdsocket"
)

// Names systemd gives the front door's sockets (FileDescriptorName=), set
// in deploy/host/steps/85-edge.sh.
const (
	httpsSocketName = "https"
	httpSocketName  = "http"
)

// dnsTokenCredential is the name the unit's LoadCredential= gives the
// Cloudflare DNS token, in $CREDENTIALS_DIRECTORY.
const dnsTokenCredential = "cloudflare-dns-token"

type flags struct {
	listen, apiSocket, dialSocket, domain string
	consoleURL                            string

	cpDomain, publicSocket       string
	httpsListen, httpListen      string
	certStorage                  string
	acmeCA, acmeCARoot, acmeMail string
	acmeDNS, dnsTokenFile        string
	consoleDir                   string
	endpointResponseTimeout      time.Duration
	limits                       front.Limits
}

func main() {
	var f flags
	flag.StringVar(&f.listen, "listen", "", "loopback address to serve public endpoints on over plain HTTP, for tests; empty, the default, serves none")
	flag.StringVar(&f.apiSocket, "api-socket", "/run/hangar/edge.sock", "the apiserver's edge socket")
	flag.StringVar(&f.dialSocket, "dial-socket", "/run/hangar/edge-dial.sock", "the node agent's edge dial socket")
	flag.StringVar(&f.domain, "domain", "hangarlab.dev", "the domain public endpoints' hostnames are in; the apiserver's --endpoint-domain")

	flag.StringVar(&f.consoleURL, "console-url", "", "the console's origin, whose endpoint-login signs browsers in to private endpoints; empty is https://console.<--cp-domain>")
	flag.StringVar(&f.cpDomain, "cp-domain", "", "the control plane's domain, which serves api.<it> and console.<it>; empty, the front door is off")
	flag.StringVar(&f.publicSocket, "public-socket", "/run/hangar/public.sock", "the apiserver's public socket, the API from :443 goes to")
	flag.StringVar(&f.httpsListen, "https-listen", "", "address for :443 when systemd passes no socket; under systemd its socket unit says")
	flag.StringVar(&f.httpListen, "http-listen", "", "address for :80 when systemd passes no socket")
	flag.StringVar(&f.certStorage, "cert-storage", stateDir(), "directory the certificates, their keys and the ACME account are kept in, the edge's alone")
	flag.StringVar(&f.acmeCA, "acme-ca", front.LetsEncrypt, "ACME directory URL; "+front.LetsEncryptStaging+" for a testbed")
	flag.StringVar(&f.acmeCARoot, "acme-ca-root", "", "PEM file of the only roots the ACME directory's TLS certificate is checked against, for a test CA")
	flag.StringVar(&f.acmeMail, "acme-email", "", "contact for the ACME account, told of problems with the certificates; required with --cp-domain")
	flag.StringVar(&f.acmeDNS, "acme-dns", "cloudflare", "how DNS-01 challenges are answered: cloudflare"+testDNSUsage)
	flag.StringVar(&f.dnsTokenFile, "cloudflare-dns-token-file", credential(dnsTokenCredential), "file only its owner reads holding a Cloudflare token that may edit DNS in the two domains' zones")
	flag.StringVar(&f.consoleDir, "console-dir", "", "directory of the console's files, index.html at its top; empty, the console is not installed")
	flag.DurationVar(&f.endpointResponseTimeout, "endpoint-response-header-timeout", 100*time.Second, "how long an endpoint has to start its answer")
	limitFlags(&f.limits)
	flag.Func("own-prefix", "the host's own IPv6 prefix, such as its /64, whose addresses, its machines', are each a client of the limits by itself; repeatable", func(s string) error {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return err
		}
		f.limits.OwnPrefixes = append(f.limits.OwnPrefixes, p)
		return nil
	})
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, f, log); err != nil {
		log.Error("edge stopped", "error", err)
		os.Exit(1)
	}
}

// limitFlags binds a flag to each of the front door's limits, defaulting to
// front.DefaultLimits.
func limitFlags(l *front.Limits) {
	d := front.DefaultLimits
	flag.IntVar(&l.Conns, "max-conns", d.Conns, "open connections to :443 and :80, from everyone")
	flag.IntVar(&l.ConnsPerClient, "max-conns-per-client", d.ConnsPerClient, "open connections from one client, an IPv4 address or an IPv6 /64")
	flag.IntVar(&l.ConnsPer56, "max-conns-per-56", d.ConnsPer56, "open connections from every IPv6 client in one /56")
	flag.IntVar(&l.ConnsPer48, "max-conns-per-48", d.ConnsPer48, "open connections from every IPv6 client in one /48")
	flag.Float64Var(&l.ConnRate, "conn-rate", d.ConnRate, "new connections a second from one client")
	flag.IntVar(&l.ConnBurst, "conn-burst", d.ConnBurst, "new connections one client may open at once, past --conn-rate")
	flag.Float64Var(&l.RequestRate, "request-rate", d.RequestRate, "requests a second from one client, each HTTP/2 stream one")
	flag.IntVar(&l.RequestBurst, "request-burst", d.RequestBurst, "requests one client may send at once, past --request-rate")
	flag.IntVar(&l.RequestsPerClient, "max-requests-per-client", d.RequestsPerClient, "requests in flight from one client, streams and WebSockets included")
	flag.IntVar(&l.MaxHeaderBytes, "max-header-bytes", d.MaxHeaderBytes, "size of a request's header")
	flag.Int64Var(&l.APIBodyBytes, "max-api-body-bytes", d.APIBodyBytes, "size of a request's body to the API")
	flag.Int64Var(&l.EndpointBodyBytes, "max-endpoint-body-bytes", d.EndpointBodyBytes, "size of a request's body to a public endpoint")
	flag.DurationVar(&l.ReadHeaderTimeout, "read-header-timeout", d.ReadHeaderTimeout, "time for a TLS handshake, then for each request's header")
	flag.DurationVar(&l.IdleTimeout, "idle-timeout", d.IdleTimeout, "time a connection may carry no request")
	flag.DurationVar(&l.ConsoleWriteTimeout, "console-write-timeout", d.ConsoleWriteTimeout, "time to send one of the console's files")
	flag.IntVar(&l.HTTP2MaxStreams, "http2-max-streams", d.HTTP2MaxStreams, "HTTP/2 streams at once on one connection")
	flag.DurationVar(&l.HTTP2SendPingTimeout, "http2-send-ping-timeout", d.HTTP2SendPingTimeout, "idle time on an HTTP/2 connection before it is pinged")
	flag.DurationVar(&l.HTTP2PingTimeout, "http2-ping-timeout", d.HTTP2PingTimeout, "time an HTTP/2 ping has to be answered before the connection is closed")
	flag.DurationVar(&l.HTTP2WriteByteTimeout, "http2-write-byte-timeout", d.HTTP2WriteByteTimeout, "time an HTTP/2 connection has to take any of what is written to it")
}

// consoleOrigin is the console's origin the edge sends browsers to, to be
// signed in to a private endpoint: --console-url, else the front door's
// console.
func consoleOrigin(f flags) (string, error) {
	if f.consoleURL == "" {
		if f.cpDomain == "" {
			return "", errors.New("private endpoints need --console-url, or --cp-domain to make it of")
		}
		return "https://console." + f.cpDomain, nil
	}
	u, err := url.Parse(f.consoleURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", fmt.Errorf("--console-url %q is not an https origin, such as https://console.example.com", f.consoleURL)
	}
	return "https://" + u.Host, nil
}

// stateDir is systemd's StateDirectory= for the unit, where the
// certificates are kept unless a flag says otherwise.
func stateDir() string {
	if d := os.Getenv("STATE_DIRECTORY"); d != "" {
		return strings.Split(d, ":")[0]
	}
	return "/var/lib/hangar-edge"
}

// credential is where systemd's LoadCredential= put name, or nothing
// outside a unit that loads credentials.
func credential(name string) string {
	if d := os.Getenv("CREDENTIALS_DIRECTORY"); d != "" {
		return filepath.Join(d, name)
	}
	return ""
}

func run(ctx context.Context, f flags, log *slog.Logger) error {
	if err := checkFlags(f); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return errors.New("hangar-edge runs as its own unprivileged user, never root")
	}
	if f.listen != "" && !loopback(f.listen) {
		return fmt.Errorf("--listen %q is not a loopback address: it serves plain HTTP, for tests on the host alone", f.listen)
	}
	socks, err := sdsocket.Inherit()
	if err != nil {
		return err
	}
	defer socks.Close()

	consoleURL, err := consoleOrigin(f)
	if err != nil {
		return err
	}
	e := edge.New(edge.Config{
		API: client.New(client.UnixSocket(f.apiSocket), client.UnixSocketURL),
		DialNode: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", f.dialSocket)
		},
		Auth:                  networkv1alpha1connect.NewEndpointAuthServiceClient(client.UnixSocket(f.apiSocket), client.UnixSocketURL),
		Domain:                f.domain,
		ConsoleURL:            consoleURL,
		ResponseHeaderTimeout: f.endpointResponseTimeout,
		Log:                   log,
	})
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return e.Run(ctx) })

	if f.cpDomain != "" {
		serve, err := frontDoor(ctx, f, log, socks, e)
		if err != nil {
			return err
		}
		g.Go(serve)
	}
	if err := socks.Unused(); err != nil {
		return err
	}
	if f.cpDomain == "" && f.listen == "" {
		return errors.New("nothing to serve: no --cp-domain and no --listen")
	}
	if f.listen != "" {
		ln, err := net.Listen("tcp", f.listen)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: e, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
		g.Go(func() error {
			log.Info("serving public endpoints over plain HTTP on loopback", "address", ln.Addr().String(), "domain", f.domain)
			err := srv.Serve(ln)
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		})
		g.Go(func() error {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return srv.Shutdown(sctx)
		})
	}
	return g.Wait()
}

// checkFlags refuses flags the edge cannot run with, before it starts
// anything.
func checkFlags(f flags) error {
	if f.cpDomain != "" && f.acmeMail == "" {
		return errors.New("the front door (--cp-domain) needs --acme-email: without it certmagic may register the ACME account as mailto:default on a first start, which the CA refuses (docs/decisions/front-door.md)")
	}
	return nil
}

// frontDoor sets up the front door on :443 and :80 and returns what serves
// it until ctx is done.
func frontDoor(ctx context.Context, f flags, log *slog.Logger, socks *sdsocket.Sockets, endpoints http.Handler) (func() error, error) {
	door, err := front.New(front.Config{
		CPDomain: f.cpDomain, UserDomain: f.domain,
		DialAPI: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", f.publicSocket)
		},
		Endpoints: endpoints, ConsoleDir: f.consoleDir, Limits: f.limits, Log: log,
	})
	if err != nil {
		return nil, err
	}
	dns, resolvers, err := dnsProvider(ctx, f, log)
	if err != nil {
		return nil, err
	}
	var roots *x509.CertPool
	if f.acmeCARoot != "" {
		pem, err := os.ReadFile(f.acmeCARoot)
		if err != nil {
			return nil, fmt.Errorf("--acme-ca-root: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--acme-ca-root %s holds no certificate", f.acmeCARoot)
		}
	}
	tc, err := front.Certificates(ctx, front.CertConfig{
		Storage: f.certStorage, CA: f.acmeCA, CARoots: roots, Email: f.acmeMail,
		DNS: dns, Resolvers: resolvers, Names: door.Names(), Log: log,
	})
	if err != nil {
		return nil, err
	}
	https, err := socks.TCP(ctx, httpsSocketName, f.httpsListen, nil)
	if err != nil {
		return nil, err
	}
	plain, err := socks.TCP(ctx, httpSocketName, f.httpListen, nil)
	if err != nil {
		_ = https.Close()
		return nil, err
	}
	return func() error {
		return door.Serve(ctx, front.Listeners{HTTPS: https, HTTP: plain}, tc)
	}, nil
}

// dnsProvider is what answers the DNS-01 challenges, as --acme-dns says,
// and the DNS servers to ask about them, none for the system's.
func dnsProvider(ctx context.Context, f flags, log *slog.Logger) (certmagic.DNSProvider, []string, error) {
	if f.acmeDNS == "cloudflare" {
		if f.dnsTokenFile == "" {
			return nil, nil, errors.New("the front door needs --cloudflare-dns-token-file, or the unit's credential " + dnsTokenCredential)
		}
		token, err := readSecretFile(f.dnsTokenFile)
		if err != nil {
			return nil, nil, fmt.Errorf("--cloudflare-dns-token-file: %w", err)
		}
		return &cloudflare.Provider{APIToken: token}, nil, nil
	}
	if p, resolvers, ok, err := testDNS(ctx, f.acmeDNS, []string{f.cpDomain, f.domain}, log); ok || err != nil {
		return p, resolvers, err
	}
	return nil, nil, fmt.Errorf("--acme-dns %q: want cloudflare%s", f.acmeDNS, testDNSUsage)
}

// readSecretFile reads a credential from a file only its owner may read,
// trimmed of surrounding space. In systemd's credentials directory, which
// only the unit can enter, newer systemd gives the file 0440: group read is
// allowed there.
func readSecretFile(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	deny := os.FileMode(0o077)
	if d := os.Getenv("CREDENTIALS_DIRECTORY"); d != "" && filepath.Dir(filepath.Clean(path)) == filepath.Clean(d) {
		deny = 0o037
	}
	if fi.Mode().Perm()&deny != 0 {
		return "", fmt.Errorf("%s is mode %v; a credential is readable by its owner only (0600)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token, nil
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
