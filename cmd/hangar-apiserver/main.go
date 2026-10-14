// Command hangar-apiserver serves the Hangar API on three listeners: a TCP
// address behind Cloudflare Access for people and agents, a unix socket for
// the node agent, and a unix socket for root on the host. See
// docs/decisions/authentication.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/sshca"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

type flags struct {
	db             string
	listen         string
	socket         string
	adminSocket    string
	nodeUID        uint
	nodeName       string
	authMode       string
	cfTeamDomain   string
	cfAudience     string
	cfCertsURL     string
	bootstrapAdmin string
	sshCAKey       string
}

func main() {
	var f flags
	flag.StringVar(&f.db, "db", "hangar.db", "path of the SQLite database")
	flag.StringVar(&f.listen, "listen", "127.0.0.1:8080", "address for the public API (behind cloudflared)")
	flag.StringVar(&f.socket, "socket", "", "unix socket for the node agent; empty disables it")
	flag.StringVar(&f.adminSocket, "admin-socket", "", "unix socket for root on this host, as platform.root; empty disables it")
	flag.UintVar(&f.nodeUID, "node-uid", 0, "uid accepted as the node agent on the socket")
	flag.StringVar(&f.nodeName, "node-name", "", "name of the Node the agent on the socket runs; required with --socket")
	flag.StringVar(&f.authMode, "auth-mode", "cloudflare", "how public callers authenticate: cloudflare or dev")
	flag.StringVar(&f.cfTeamDomain, "cf-team-domain", "", "Access team domain, such as https://team.cloudflareaccess.com")
	flag.StringVar(&f.cfAudience, "cf-audience", "", "AUD tag of the Access application")
	flag.StringVar(&f.cfCertsURL, "cf-certs-url", "", "override for the Access signing keys URL")
	flag.StringVar(&f.bootstrapAdmin, "bootstrap-admin", "", "email granted platform.root if no one holds it")
	flag.StringVar(&f.sshCAKey, "ssh-ca-key", "", "the platform SSH CA's private key, an OpenSSH key file only its owner reads; missing, hangar ssh cannot sign")
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
	socks, err := inheritSockets()
	if err != nil {
		return err
	}
	defer socks.close()
	return serveAll(ctx, f, log, socks)
}

func serveAll(ctx context.Context, f flags, log *slog.Logger, socks *sockets) error {
	public, err := publicChain(ctx, f)
	if err != nil {
		return err
	}
	st, err := sqlite.Open(ctx, f.db)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

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

	var sock, admin net.Listener
	if f.socket != "" {
		if sock, err = socks.listen(nodeSocketName, f.socket, 0o660); err != nil {
			return err
		}
		defer func() { _ = sock.Close() }()
	}
	if f.adminSocket != "" {
		if admin, err = socks.listen(adminSocketName, f.adminSocket, 0o600); err != nil {
			return err
		}
		defer func() { _ = admin.Close() }()
	}
	if err := socks.unused(); err != nil {
		return err
	}
	// Keepalive and a user timeout drop a dead client under an exec whose
	// data is backed up, which no ping can tell (internal/execws).
	ln, err := (&net.ListenConfig{KeepAliveConfig: execws.KeepAlive, Control: execws.Control}).Listen(ctx, "tcp", f.listen)
	if err != nil {
		return err
	}

	ca, err := loadSSHCA(f.sshCAKey, log)
	if err != nil {
		return err
	}
	srv := apiserver.New(apiserver.Config{
		Store: st, Logger: log, BootstrapAdmin: strings.ToLower(f.bootstrapAdmin), Nodes: nodes, SSHCA: ca,
	})
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(ctx) })
	g.Go(func() error {
		log.Info("serving the public API", "address", ln.Addr().String(), "auth", f.authMode)
		return serve(ctx, &http.Server{Handler: srv.Handler(public), Protocols: protocols()}, ln)
	})
	if sock != nil {
		node := auth.Chain{auth.PeerCred{NodeUIDs: []uint32{uint32(f.nodeUID)}, NodeName: f.nodeName}}
		g.Go(func() error {
			log.Info("serving the node socket", "path", f.socket)
			return serve(ctx, &http.Server{Handler: srv.Handler(node), Protocols: protocols(), ConnContext: auth.ConnContext}, sock)
		})
	}
	if admin != nil {
		// Nothing but LocalRoot, which reads only AdminConnContext's record:
		// no header can say who is calling here.
		root := auth.Chain{auth.LocalRoot{Host: host, UID: adminUID}}
		g.Go(func() error {
			log.Info("serving the admin socket", "path", f.adminSocket)
			h := auditAdmin(log, root, srv.Handler(root))
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

func publicChain(ctx context.Context, f flags) (auth.Chain, error) {
	switch f.authMode {
	case "cloudflare":
		cf, err := auth.NewCloudflare(ctx, auth.CloudflareConfig{
			TeamDomain: f.cfTeamDomain, Audience: f.cfAudience, CertsURL: f.cfCertsURL,
		})
		if err != nil {
			return nil, err
		}
		return auth.Chain{cf}, nil
	case "dev":
		if !loopback(f.listen) {
			return nil, fmt.Errorf("dev auth trusts a plain header, so it only runs on a loopback address, not %q", f.listen)
		}
		return auth.Chain{auth.Dev{}}, nil
	}
	return nil, fmt.Errorf("unknown auth mode %q; want cloudflare or dev", f.authMode)
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// protocols serves HTTP/1.1 and cleartext HTTP/2 on the same port, so
// streaming works whichever cloudflared uses to reach the origin.
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
