// Command hangar-apiserver serves the Hangar API on two listeners: a TCP
// address behind Cloudflare Access for people and agents, and a unix socket
// for the node agent. See docs/decisions/authentication.md.
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
)

type flags struct {
	db             string
	listen         string
	socket         string
	nodeUID        uint
	authMode       string
	cfTeamDomain   string
	cfAudience     string
	cfCertsURL     string
	bootstrapAdmin string
}

func main() {
	var f flags
	flag.StringVar(&f.db, "db", "hangar.db", "path of the SQLite database")
	flag.StringVar(&f.listen, "listen", "127.0.0.1:8080", "address for the public API (behind cloudflared)")
	flag.StringVar(&f.socket, "socket", "", "unix socket for the node agent; empty disables it")
	flag.UintVar(&f.nodeUID, "node-uid", 0, "uid accepted as the node agent on the socket")
	flag.StringVar(&f.authMode, "auth-mode", "cloudflare", "how public callers authenticate: cloudflare or dev")
	flag.StringVar(&f.cfTeamDomain, "cf-team-domain", "", "Access team domain, such as https://team.cloudflareaccess.com")
	flag.StringVar(&f.cfAudience, "cf-audience", "", "AUD tag of the Access application")
	flag.StringVar(&f.cfCertsURL, "cf-certs-url", "", "override for the Access signing keys URL")
	flag.StringVar(&f.bootstrapAdmin, "bootstrap-admin", "", "email granted platform.root if no one holds it")
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
	public, err := publicChain(ctx, f)
	if err != nil {
		return err
	}
	st, err := sqlite.Open(ctx, f.db)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

	srv := apiserver.New(apiserver.Config{Store: st, Logger: log, BootstrapAdmin: strings.ToLower(f.bootstrapAdmin)})
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(ctx) })

	ln, err := net.Listen("tcp", f.listen)
	if err != nil {
		return err
	}
	g.Go(func() error {
		log.Info("serving the public API", "address", ln.Addr().String(), "auth", f.authMode)
		return serve(ctx, &http.Server{Handler: srv.Handler(public), Protocols: protocols()}, ln)
	})

	if f.socket != "" {
		sock, err := listenSocket(f.socket)
		if err != nil {
			return err
		}
		node := auth.Chain{auth.PeerCred{NodeUIDs: []uint32{uint32(f.nodeUID)}}}
		g.Go(func() error {
			log.Info("serving the node socket", "path", f.socket)
			return serve(ctx, &http.Server{Handler: srv.Handler(node), Protocols: protocols(), ConnContext: auth.ConnContext}, sock)
		})
	}
	return g.Wait()
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

func listenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
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
