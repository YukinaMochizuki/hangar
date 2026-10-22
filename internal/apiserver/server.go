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
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"golang.org/x/sync/errgroup"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/discovery/v1alpha1/discoveryv1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/monitoring/v1alpha1/monitoringv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/servicegen"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/login"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
	"github.com/YukinaMochizuki/hangar/internal/sshca"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/tunnel"
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
	// Nodes are the Node names of the agents this apiserver accepts; each
	// gets a Node object if it has none.
	Nodes []string
	// NodeGracePeriod is how long a node may go without reporting before it
	// is marked Ready=Unknown. Node agents report every ten seconds, so the
	// default tolerates three missed reports.
	NodeGracePeriod time.Duration
	// NodeMonitorInterval is how often heartbeats are checked.
	NodeMonitorInterval time.Duration
	// TunnelLiveness is the node tunnels'; zero means
	// tunnel.DefaultLiveness.
	TunnelLiveness tunnel.Liveness
	// MaxNodeCallsPerPrincipal, MaxNodeCallsPerTenant and
	// MaxNodeCallsPerNode cap the calls to nodes open at once; zero means
	// their Default.
	MaxNodeCallsPerPrincipal, MaxNodeCallsPerTenant, MaxNodeCallsPerNode int
	// ExecPing, ExecPong and ExecStart time a container's exec: how often
	// its client is pinged, how long a pong may take, and how long the
	// start may; zero means DefaultExecPing, DefaultExecPong and
	// DefaultExecStart.
	ExecPing, ExecPong, ExecStart time.Duration
	// ExtraRoles are roles beyond the built-in ones, by name, as tests
	// need; none replaces a built-in role.
	ExtraRoles map[string]authz.Role
	// SSHCA signs keys for hangar ssh, and its public key goes to every
	// VM's guest; nil is none, and signing fails (docs/decisions/vm-ssh.md).
	SSHCA *sshca.CA
	// PublicEndpoints configures PublicEndpoints: their domain.
	PublicEndpoints PublicEndpointsConfig
	// Secrets seals and opens Secrets' data; nil is no key-encryption key,
	// and writing a Secret fails (docs/decisions/secrets.md). Call
	// PrepareSecrets before serving.
	Secrets *secrets.Keyring
	// DisableControllers leaves out the controllers, as tests of the generic
	// machinery do: they use real kinds as examples and need nothing else
	// writing to them. A real apiserver always runs them.
	DisableControllers bool
	// DefaultMaxOwnedTenants is how many tenants someone may own whose User
	// sets no limit of its own; zero means DefaultMaxOwnedTenants.
	DefaultMaxOwnedTenants int
	// DefaultPlanLimits are what the default Plan is made with, the first
	// time the apiserver starts on a store; nil means DefaultLimits.
	DefaultPlanLimits *tenancyv1alpha1.Limits
	// Credentials keeps the credentials the apiserver issues: service
	// tokens' secrets in any auth mode, and with EnableOIDC the sessions of
	// people who sign in. Nil issues none, and ServiceTokenIssuerService
	// fails.
	Credentials store.Credentials
	// Monitoring is where MonitoringService asks; unset, its calls fail
	// with Unavailable.
	Monitoring MonitoringConfig
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
	// heartbeats is when each node agent was last heard from.
	heartbeats heartbeats
	// tunnels are the node agents' tunnels, through which the apiserver
	// calls them.
	tunnels *tunnel.Hub
	// nodeCalls caps the calls to nodes open at once.
	nodeCalls *nodeCalls
	// capacity serializes every decision that reserves memory or disk on a node:
	// placing a machine, and growing a placed one.
	capacity sync.Mutex
	// ipam serializes every decision that hands out an address (a machine's
	// interface addresses, a network's transit link) and the release of a
	// network. Whoever holds both takes ipam first.
	ipam sync.Mutex
	// attach serializes every apply and delete of machines, containers,
	// virtual machines, disks, file shares and networks, from the admission
	// checks to the write, so a disk is attached to one machine at a time
	// and never to one being deleted, and a share never leaves a network
	// what declares it mounts it from nor stays on one deleted. The backend
	// takes it under ipam and capacity; onboarding, whose transaction writes
	// a network, takes it after users and plans and before the transaction,
	// which every other write waits for.
	attach sync.Mutex
	// customDomains serializes every apply and delete of a CustomDomain and
	// every apply of a PublicEndpoint with spec.hostnames, so no two
	// tenants' domains overlap, no two endpoints hold one hostname, and no
	// domain goes while an endpoint serves under it.
	customDomains sync.Mutex
	// addressBinding serializes every apply and delete of an Address, so
	// no two bind one interface.
	addressBinding sync.Mutex
	// users serializes every apply and delete of a User or an Invitation,
	// so no two Users have one email and no Invitation is for an email with
	// a User. Whoever holds it and plans takes users first.
	users sync.Mutex
	// quota serializes every write that can raise what a tenant holds, but
	// the apiserver's own, from counting what the tenant holds to the write,
	// so concurrent writes cannot together overshoot a limit. The backend
	// takes it after the kind's writeLock. It holds only while one apiserver
	// process writes the store; see docs/decisions/tenancy-and-iam.md.
	quota sync.Mutex
	// plans serializes every apply and delete of a Plan, a Tenant, a User
	// or an Invitation, so none comes to name a plan being deleted. Whoever
	// holds it and quota takes plans first.
	plans sync.Mutex
	// nameSuffix makes the random part of the name signing up gives a
	// person's first tenant and project.
	nameSuffix func() string
	// owners serializes every apply and delete of a RoleBinding but the
	// apiserver's own, so no two writes each leave the other's tenant owner
	// as the last one and together leave none (checkOwnerLeft).
	owners sync.Mutex
	// sessions issues and checks credentials, over Config.Credentials; nil
	// without them. login is set by EnableOIDC: sign-in through the
	// provider; nil without it.
	sessions *session.Manager
	login    *login.Flow
	// tokenSecrets serializes issuing each service token's secrets, so its
	// status names the secret that works. A delete does not take it; see
	// issueTokenSecret. Keyed by <project>/<name>.
	tokenSecrets keyLocks
	// tokenBindings serializes, for each service token, making a
	// RoleBinding that names it, from the check that the token stands to
	// the write, with the token's cascade, from looking for the bindings it
	// owns to letting the token go: so no binding is written for a token
	// that is going, unseen by its cascade. Keyed by <project>/<name>.
	tokenBindings keyLocks
	// tokenUses queues service tokens' uses to record in their status.
	tokenUses chan tokenUse
	// controllers are what Run runs, unless the config disables them; gc
	// is how far garbage collection has come.
	controllers []*controller.Controller
	gc          gcProgress
	// rpc counts the Connect calls served; boots follows machines' boot
	// times, through bootWatch, for the crash-loop gauge. bootWatch writes
	// nothing, so it runs even when the config disables the controllers.
	rpc       *rpcMetrics
	boots     *bootTracker
	bootWatch *controller.Controller
	// prometheus and alertmanager are what MonitoringService asks.
	prometheus, alertmanager *monitoringBackend
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
	if cfg.NodeGracePeriod == 0 {
		cfg.NodeGracePeriod = 40 * time.Second
	}
	if cfg.NodeMonitorInterval == 0 {
		cfg.NodeMonitorInterval = 10 * time.Second
	}
	if cfg.DefaultMaxOwnedTenants == 0 {
		cfg.DefaultMaxOwnedTenants = DefaultMaxOwnedTenants
	}
	s := &Server{
		cfg:        cfg,
		store:      txStore{cfg.Store},
		nameSuffix: randomNameSuffix,
		clock:      cfg.Clock,
		log:        cfg.Logger,
		authz:      authz.New(cfg.Store, cfg.ExtraRoles),
		backends:   map[string]*backend{},
		tunnels:    &tunnel.Hub{Liveness: cfg.TunnelLiveness, Log: cfg.Logger},
		nodeCalls:  newNodeCalls(cfg.MaxNodeCallsPerPrincipal, cfg.MaxNodeCallsPerTenant, cfg.MaxNodeCallsPerNode),
		tokenUses:  make(chan tokenUse, 256),
		rpc:        newRPCMetrics(),
		boots:      newBootTracker(cfg.Clock),
	}
	s.bootWatch = &controller.Controller{
		Name: "machineboots", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
		Reconciler: controller.ReconcilerFunc(s.reconcileBoots),
		Watches:    []controller.Watch{{Kind: registry.Machine.Kind, Map: controller.Self}},
	}
	if cfg.Credentials != nil {
		s.sessions = s.newSessions(cfg.Credentials)
	}
	s.newMonitoring(cfg.Monitoring)
	for _, r := range registry.All {
		s.backends[r.Kind] = &backend{s: s, r: r, hooks: hooksFor(r)}
	}
	if !cfg.DisableControllers {
		s.controllers = s.newControllers()
	}
	return s
}

// ControllerStatus reports what each controller and the garbage collector
// are doing.
func (s *Server) ControllerStatus() []controller.Status {
	st := []controller.Status{s.gc.status(), s.bootWatch.Status()}
	for _, c := range s.controllers {
		st = append(st, c.Status())
	}
	return st
}

func (s *Server) backend(r registry.Resource) *backend { return s.backends[r.Kind] }

// Handler serves the API, authenticating every call with chain. It takes
// node agents' tunnels too, from callers chain authenticates as nodes.
func (s *Server) Handler(chain auth.Chain) http.Handler {
	return s.mux(chain)
}

func (s *Server) mux(chain auth.Chain) *http.ServeMux {
	mux := http.NewServeMux()
	servicegen.Mount(mux, func(r registry.Resource) resource.Backend { return s.backend(r) },
		s.interceptors(chain))
	mux.Handle(computev1alpha1connect.NewMachineCapabilityServiceHandler(machineCapabilities{s}, s.interceptors(chain)))
	mux.HandleFunc(execws.Pattern, s.machineSocket(chain))
	mux.Handle(nodev1connect.NewImagePullCredentialsServiceHandler(imagePullCredentials{s}, s.interceptors(chain)))
	mux.HandleFunc(tunnel.Path, s.openTunnel(chain))
	mux.Handle(tenancyv1alpha1connect.NewNameServiceHandler(names{s}, s.interceptors(chain)))
	mux.Handle(discoveryv1alpha1connect.NewDiscoveryServiceHandler(newDiscovery(s), s.interceptors(chain)))
	mux.Handle(iamv1alpha1connect.NewAccountServiceHandler(account{s}, s.interceptors(chain)))
	mux.Handle(iamv1alpha1connect.NewMemberServiceHandler(members{s}, s.interceptors(chain)))
	mux.Handle(iamv1alpha1connect.NewServiceTokenIssuerServiceHandler(tokenIssuer{s}, s.interceptors(chain)))
	mux.Handle(monitoringv1alpha1connect.NewMonitoringServiceHandler(monitoring{s}, s.interceptors(chain)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// interceptors count the Connect calls a listener takes, authenticate them
// with chain, and recheck its streams on the server's clock. Counting comes
// first, so calls refused for their credentials are counted too.
func (s *Server) interceptors(chain auth.Chain) connect.HandlerOption {
	return connect.WithInterceptors(s.rpc, chain.InterceptorOn(s.clock, s.log))
}

// openTunnel takes a node agent's tunnel (internal/tunnel). The tunnel is
// the node the listener's chain authenticates the request as, which on the
// node socket is the name PeerCred is configured with; nothing the request
// says names the node.
func (s *Server) openTunnel(chain auth.Chain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, err := chain.Authenticate(r.Context(), r.Header)
		if err != nil {
			http.Error(w, "unauthenticated: "+err.Error(), http.StatusUnauthorized)
			return
		}
		p, _ := auth.FromContext(ctx)
		if p.Type != auth.Node || !slices.Contains(s.cfg.Nodes, p.ID) {
			http.Error(w, p.String()+" is not a node agent this apiserver accepts", http.StatusForbidden)
			return
		}
		s.tunnels.Accept(w, r, p.ID)
	}
}

// Run does the server's background work until ctx is done: it keeps the
// authorization view current, makes the default plan, bootstraps the first
// admin, registers nodes, watches their heartbeats, runs the controllers,
// collects garbage and compacts history.
func (s *Server) Run(ctx context.Context) error {
	// Before anything else, so a tenant opened as soon as the first admin
	// can has its plan.
	if err := s.ensureDefaultPlan(ctx); err != nil {
		return err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		<-gctx.Done()
		s.tunnels.Close()
		return nil
	})
	g.Go(func() error { return s.authz.Run(gctx) })
	g.Go(func() error {
		if s.cfg.BootstrapAdmin == "" {
			return nil
		}
		return s.bootstrap(gctx, s.cfg.BootstrapAdmin)
	})
	g.Go(func() error { return s.ensureNodes(gctx) })
	g.Go(func() error { return s.runNodeMonitor(gctx) })
	for _, c := range s.controllers {
		g.Go(func() error { return c.Run(gctx) })
	}
	g.Go(func() error { return s.bootWatch.Run(gctx) })
	g.Go(func() error { return s.runGC(gctx) })
	g.Go(func() error { return s.runCompaction(gctx) })
	if s.sessions != nil {
		g.Go(func() error { return s.sessions.Run(gctx) })
		g.Go(func() error { return s.recordTokenUses(gctx) })
	}
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

// bootstrapMarker records that a bootstrap admin was granted root. It is not
// an API kind; nothing serves or watches it.
var bootstrapMarker = store.Key{Kind: "BootstrapMarker", Name: "done"}

// bootstrap grants email platform.root, once in the life of the store, so
// revoking every root binding is not undone by the next restart.
func (s *Server) bootstrap(ctx context.Context, email string) error {
	_, err := s.store.Get(ctx, bootstrapMarker)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_, err = s.backend(registry.RoleBinding).Apply(s.system(ctx), &iamv1alpha1.RoleBinding{
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
	// A crash before the marker is written grants the same binding again on
	// the next start, which changes nothing.
	if _, err := s.store.Create(ctx, bootstrapMarker, nil); err != nil && !errors.Is(err, store.ErrExists) {
		return fmt.Errorf("recording the bootstrap: %w", err)
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
