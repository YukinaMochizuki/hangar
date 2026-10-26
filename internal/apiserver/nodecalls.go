package apiserver

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/tunnel"
)

// Caps on the calls to nodes open at once (docs/decisions/node-tunnel.md).
const (
	// DefaultMaxNodeCallsOfNoTenant caps each caller of no tenant
	// (ofNoTenant); a tenant's callers are capped by its machine_calls.
	DefaultMaxNodeCallsOfNoTenant = 64
	DefaultMaxNodeCallsPerNode    = tunnel.MaxStreams - 100
	// DefaultMachineCalls is the machine_calls of a Plan that sets none.
	DefaultMachineCalls = 64
	// MaxMachineCalls is the most machine_calls a Plan may set: below
	// DefaultMaxNodeCallsPerNode, so no one tenant fills a node.
	MaxMachineCalls = 512
)

// maxNodeMessage bounds a message from a node: a batch of log records is
// about driver.MaxLogBatch, and one record at most 64 KiB more.
const maxNodeMessage = 1 << 20

// nodeCalls counts the calls to nodes open, by caller, by tenant and by
// node.
type nodeCalls struct {
	mu           sync.Mutex
	perPrincipal map[auth.Principal]int
	perTenant    map[string]int
	perNode      map[string]int
	// maxNoTenant caps each caller of no tenant.
	maxNoTenant int
	maxPerNode  int
}

func newNodeCalls(maxNoTenant, maxPerNode int) *nodeCalls {
	if maxNoTenant == 0 {
		maxNoTenant = DefaultMaxNodeCallsOfNoTenant
	}
	if maxPerNode == 0 {
		maxPerNode = DefaultMaxNodeCallsPerNode
	}
	return &nodeCalls{
		perPrincipal: map[auth.Principal]int{}, perTenant: map[string]int{}, perNode: map[string]int{},
		maxNoTenant: maxNoTenant, maxPerNode: maxPerNode,
	}
}

// ofNoTenant reports whether p is a principal no tenant can have as a
// member, the apiserver or root on the host, which no tenant's
// machine_calls caps. A person or service token, whatever its role, is
// capped by the tenant each call is in.
func ofNoTenant(p auth.Principal) bool {
	return p.Type == auth.System || p.Type == auth.Local
}

// take takes a slot for a call by p, the authenticated caller, in tenant
// to node, where tenant's Plan allows maxTenant calls, and returns what
// gives it back.
func (n *nodeCalls) take(p auth.Principal, tenant string, maxTenant int, node string) (func(), error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ofNoTenant(p) {
		if n.perPrincipal[p] >= n.maxNoTenant {
			return nil, connect.NewError(connect.CodeResourceExhausted,
				fmt.Errorf("%s has %d calls open to nodes, the most it may; end one first", p, n.maxNoTenant))
		}
	} else if n.perPrincipal[p] >= maxTenant {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("%s has %d calls open to nodes, and tenant %q's plan lets a caller hold %d (machine_calls); end one first",
				p, n.perPrincipal[p], tenant, maxTenant))
	}
	if n.perTenant[tenant] >= maxTenant {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("tenant %q has %d calls open to nodes, the most its plan allows (machine_calls); end one first", tenant, maxTenant))
	}
	if n.perNode[node] >= n.maxPerNode {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("node %s has %d calls open, the most it takes; try again later", node, n.maxPerNode))
	}
	n.perPrincipal[p]++
	n.perTenant[tenant]++
	n.perNode[node]++
	var once sync.Once
	return func() {
		once.Do(func() {
			n.mu.Lock()
			defer n.mu.Unlock()
			if n.perPrincipal[p]--; n.perPrincipal[p] == 0 {
				delete(n.perPrincipal, p)
			}
			if n.perTenant[tenant]--; n.perTenant[tenant] == 0 {
				delete(n.perTenant, tenant)
			}
			if n.perNode[node]--; n.perNode[node] == 0 {
				delete(n.perNode, node)
			}
		})
	}, nil
}

// nodeCall is a call to a node under way: its NodeService over the
// node's tunnel, and its slot.
type nodeCall struct {
	nodes   nodev1connect.NodeServiceClient
	tunnel  *tunnel.Tunnel
	what    string
	release func()
}

// machineCallsOf is how many calls to nodes tenant's Plan lets it hold
// open, read at each call so a change to the Plan holds from the next;
// what names the call in errors. A tenant whose Plan is missing has the
// default, as one whose Plan sets none.
func (s *Server) machineCallsOf(ctx context.Context, tenant, what string) (int, error) {
	obj, err := s.get(ctx, registry.Tenant, resource.Key{Name: tenant})
	if err != nil {
		return 0, err
	}
	if obj == nil {
		return 0, failedPrecondition("%s is in tenant %q, which does not exist", what, tenant)
	}
	_, ls, err := s.planOf(ctx, obj.(*tenancyv1alpha1.Tenant))
	if err != nil && !isNoPlan(err) {
		return 0, err
	}
	if ls.machineCallsSet {
		return int(ls.machineCalls), nil
	}
	return s.cfg.DefaultMachineCalls, nil
}

// callNode begins a call by p, the authenticated caller, in tenant to
// node; what names it in errors, such as the container it is for. The
// caller ends it with release. Reaching nodes some other way changes only
// this.
func (s *Server) callNode(ctx context.Context, p auth.Principal, tenant, node, what string) (*nodeCall, error) {
	t, err := s.tunnels.Node(node)
	if errors.Is(err, tunnel.ErrNoTunnel) {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s's node %s is not connected", what, node))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	maxTenant, err := s.machineCallsOf(ctx, tenant, what)
	if err != nil {
		return nil, apiError(err)
	}
	release, err := s.nodeCalls.take(p, tenant, maxTenant, node)
	if err != nil {
		return nil, err
	}
	return &nodeCall{
		nodes:   nodev1connect.NewNodeServiceClient(t.HTTPClient(), "http://"+node, connect.WithReadMaxBytes(maxNodeMessage)),
		tunnel:  t,
		what:    fmt.Sprintf("%s's node %s", what, node),
		release: release,
	}, nil
}

// failed is what the call fails with when the node's answer is err: one
// the tunnel's drop failed is Unavailable.
func (c *nodeCall) failed(err error) error {
	code, msg := connect.CodeUnknown, err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) {
		code, msg = ce.Code(), ce.Message()
	}
	if c.tunnel.Closed() {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s dropped its connection: %s", c.what, msg))
	}
	return connect.NewError(code, fmt.Errorf("%s: %s", c.what, msg))
}
