// Package apiserver serves the Hangar API: every resource kind through the
// generated Connect handlers, backed by one generic implementation over the
// store, plus the background work that keeps the store consistent
// (authorization view, garbage collection, compaction).
package apiserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"golang.org/x/sync/errgroup"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/servicegen"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// Config configures a Server. Zero durations and counts take defaults.
type Config struct {
	Store  store.Store
	Clock  clockwork.Clock
	Logger *slog.Logger
	// BootstrapAdmin, if set, is granted platform.root when no binding grants
	// that role yet.
	BootstrapAdmin string
	// BookmarkInterval is how often an idle watch sends a bookmark.
	BookmarkInterval time.Duration
	// EventRetention is how many revisions of history watches can resume
	// from.
	EventRetention int64
	// CompactInterval is how often history beyond the retention is dropped.
	CompactInterval time.Duration
	// SweepInterval is how often garbage collection rechecks every object,
	// catching anything it missed while following deletes.
	SweepInterval time.Duration
}

// Server is the API server. Create it with New, start its background work
// with Run, and serve Handler on each listener.
type Server struct {
	cfg      Config
	store    store.Store
	clock    clockwork.Clock
	log      *slog.Logger
	authz    *authz.Authorizer
	backends map[string]*backend
}

// New returns a server over cfg.Store.
func New(cfg Config) *Server {
	if cfg.Clock == nil {
		cfg.Clock = clockwork.NewRealClock()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.BookmarkInterval == 0 {
		cfg.BookmarkInterval = 30 * time.Second
	}
	if cfg.EventRetention == 0 {
		cfg.EventRetention = 10000
	}
	if cfg.CompactInterval == 0 {
		cfg.CompactInterval = 10 * time.Minute
	}
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = 5 * time.Minute
	}
	s := &Server{
		cfg:      cfg,
		store:    cfg.Store,
		clock:    cfg.Clock,
		log:      cfg.Logger,
		authz:    authz.New(cfg.Store),
		backends: map[string]*backend{},
	}
	for _, r := range registry.All {
		s.backends[r.Kind] = &backend{s: s, r: r, hooks: hooksFor(r)}
	}
	return s
}

func (s *Server) backend(r registry.Resource) *backend { return s.backends[r.Kind] }

// Handler serves the API, authenticating every call with chain.
func (s *Server) Handler(chain auth.Chain) http.Handler {
	mux := http.NewServeMux()
	servicegen.Mount(mux, func(r registry.Resource) resource.Backend { return s.backend(r) },
		connect.WithInterceptors(chain.Interceptor()))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// Run does the server's background work until ctx is done: it keeps the
// authorization view current, bootstraps the first admin, collects garbage
// and compacts history.
func (s *Server) Run(ctx context.Context) error {
	// Only a store nothing was ever written to gets a bootstrap admin, so
	// revoking every root binding is not undone by the next restart.
	rev, err := s.store.Revision(ctx)
	if err != nil {
		return err
	}
	firstStart := rev == 0
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.authz.Run(gctx) })
	g.Go(func() error {
		if s.cfg.BootstrapAdmin == "" || !firstStart {
			return nil
		}
		return s.bootstrap(gctx, s.cfg.BootstrapAdmin)
	})
	g.Go(func() error { return s.runGC(gctx) })
	g.Go(func() error { return s.runCompaction(gctx) })
	// Background work ends when the caller's ctx does; only a failure before
	// that is an error.
	if err := g.Wait(); ctx.Err() == nil {
		return err
	}
	return nil
}

func (s *Server) system(ctx context.Context) context.Context {
	return auth.WithPrincipal(ctx, auth.SystemPrincipal)
}

// bootstrap grants email platform.root.
func (s *Server) bootstrap(ctx context.Context, email string) error {
	_, err := s.backend(registry.RoleBinding).Apply(s.system(ctx), &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: "bootstrap-root"},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: email}},
			Role:    "platform.root",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}},
		},
	})
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	s.log.Info("granted platform.root to the bootstrap admin", "user", email)
	return nil
}

func (s *Server) runCompaction(ctx context.Context) error {
	ticker := s.clock.NewTicker(s.cfg.CompactInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.Chan():
		}
		rev, err := s.store.Revision(ctx)
		if err != nil {
			return err
		}
		if rev > s.cfg.EventRetention {
			if err := s.store.Compact(ctx, rev-s.cfg.EventRetention); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Error("compaction failed", "error", err)
			}
		}
	}
}
