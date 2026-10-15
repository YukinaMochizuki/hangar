// Command hangar-edge is the edge proxy for public endpoints: cloudflared
// sends it every request for a hostname under the endpoint domain, and it
// routes each by Host to its endpoint's machine. It runs as its own
// unprivileged user, reads the API through the apiserver's edge socket, and
// reaches machines through the node agent's edge dial socket. See
// docs/decisions/public-endpoints.md.
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
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/edge"
)

type flags struct {
	listen, apiSocket, dialSocket, domain string
	cfTeamDomain, cfAudience, cfCertsURL  string
}

func main() {
	var f flags
	flag.StringVar(&f.listen, "listen", "127.0.0.1:8090", "loopback address cloudflared sends public endpoints' requests to")
	flag.StringVar(&f.apiSocket, "api-socket", "/run/hangar/edge.sock", "the apiserver's edge socket")
	flag.StringVar(&f.dialSocket, "dial-socket", "/run/hangar/edge-dial.sock", "the node agent's edge dial socket")
	flag.StringVar(&f.domain, "domain", "hangarlab.dev", "the domain public endpoints' hostnames are in; the apiserver's --endpoint-domain")
	flag.StringVar(&f.cfTeamDomain, "cf-team-domain", "", "Access team domain, such as https://team.cloudflareaccess.com")
	flag.StringVar(&f.cfAudience, "cf-audience", "", "AUD tag of the endpoint domain's wildcard Access application")
	flag.StringVar(&f.cfCertsURL, "cf-certs-url", "", "override for the Access signing keys URL")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, f, log); err != nil {
		log.Error("edge stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, f flags, log *slog.Logger) error {
	if os.Geteuid() == 0 {
		return errors.New("hangar-edge runs as its own unprivileged user, never root")
	}
	if !loopback(f.listen) {
		return fmt.Errorf("--listen %q is not a loopback address: only cloudflared may reach the edge", f.listen)
	}
	access, err := edge.NewAccessVerifier(ctx, edge.AccessConfig{TeamDomain: f.cfTeamDomain, Audience: f.cfAudience, CertsURL: f.cfCertsURL})
	if err != nil {
		return err
	}
	e := edge.New(edge.Config{
		API: client.New(client.UnixSocket(f.apiSocket), client.UnixSocketURL),
		DialNode: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", f.dialSocket)
		},
		Domain: f.domain,
		Access: access,
		Log:    log,
	})
	ln, err := net.Listen("tcp", f.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: e, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return e.Run(ctx) })
	g.Go(func() error {
		log.Info("serving public endpoints", "address", ln.Addr().String(), "domain", f.domain)
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
	return g.Wait()
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
