// Package metadata is the metadata service the node serves each network at
// 169.254.169.254, in the form cloud-init's NoCloud datasource reads
// (docs/decisions/networking.md). It knows a machine by the address a
// request comes from, which is sound only because each interface can send
// from no address but its own.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/netutil"
)

// Prefix is where the NoCloud documents are, as the guest is told with
// ds=nocloud;s=http://169.254.169.254/nocloud/.
const Prefix = "/nocloud/"

// Instance is what the service says of one machine.
type Instance struct {
	// UID is the instance id cloud-init is given, which runs its first
	// boot once per id: the one the machine's cloud-init data names, such
	// as a VM's uid, so the same instance however often its machine is
	// made; else the machine's uid, so a machine made again boots anew.
	UID      string
	Hostname string
	// UserData and VendorData are served as they are; empty is an empty
	// answer.
	UserData, VendorData string
}

// Config configures a Server; zero values take defaults.
type Config struct {
	// MaxConns bounds a network's connections; zero means 16. Past it, a
	// connection waits to be accepted. MaxConnsPerSource bounds one
	// address's; zero means 4. Past it, a connection is closed.
	MaxConns, MaxConnsPerSource int
	Log                         *slog.Logger
}

// Server serves the networks' machines. It is safe for concurrent use.
type Server struct {
	cfg Config

	mu   sync.RWMutex
	nets map[string]map[netip.Addr]Instance // by network uid, then address
}

// New returns a server knowing no network.
func New(cfg Config) *Server {
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 16
	}
	if cfg.MaxConnsPerSource == 0 {
		cfg.MaxConnsPerSource = 4
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Server{cfg: cfg, nets: map[string]map[netip.Addr]Instance{}}
}

// Set replaces a network's machines, by their addresses on it.
func (s *Server) Set(networkUID string, byAddr map[netip.Addr]Instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nets[networkUID] = byAddr
}

// Forget drops a network's machines.
func (s *Server) Forget(networkUID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nets, networkUID)
}

// Serve answers a network's requests on l until ctx is done or l is closed,
// then shuts down, closing its connections.
func (s *Server) Serve(ctx context.Context, networkUID string, l net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(networkUID),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          slog.NewLogLogger(s.cfg.Log.Handler(), slog.LevelDebug),
	}
	done := make(chan error, 1)
	l = limitPerSource(netutil.LimitListener(l, s.cfg.MaxConns), s.cfg.MaxConnsPerSource)
	go func() { done <- srv.Serve(l) }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = srv.Shutdown(sctx)
		cancel()
		_ = srv.Close()
		err = <-done
	}
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Handler answers a network's requests, each for the machine it comes from.
func (s *Server) Handler(networkUID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		inst, ok := s.instance(networkUID, r.RemoteAddr)
		if !ok {
			http.NotFound(w, r)
			return
		}
		var body string
		switch strings.TrimPrefix(r.URL.Path, Prefix) {
		case "meta-data":
			body = fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", inst.UID, inst.Hostname)
		case "user-data":
			body = inst.UserData
		case "vendor-data":
			body = inst.VendorData
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(body))
	})
}

func (s *Server) instance(networkUID, remote string) (Instance, bool) {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return Instance{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	inst, ok := s.nets[networkUID][ap.Addr().Unmap()]
	return inst, ok
}
