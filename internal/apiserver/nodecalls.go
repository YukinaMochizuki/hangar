package apiserver

import (
	"errors"
	"fmt"
	"sync"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/tunnel"
)

// Calls to nodes open at once are capped per caller and per tenant, so no
// one takes a node's streams from the others, however many tenants they
// make, and per node below what its tunnel takes, keeping room for the
// apiserver's own calls. A call over a cap fails at once with
// ResourceExhausted; none waits (node-tunnel.md).
const (
	DefaultMaxNodeCallsPerPrincipal = 64
	DefaultMaxNodeCallsPerTenant    = 64
	DefaultMaxNodeCallsPerNode      = tunnel.MaxStreams - 100
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
	maxPrincipal int
	maxTenant    int
	maxPerNode   int
}

func newNodeCalls(maxPrincipal, maxTenant, maxPerNode int) *nodeCalls {
	if maxPrincipal == 0 {
		maxPrincipal = DefaultMaxNodeCallsPerPrincipal
	}
	if maxTenant == 0 {
		maxTenant = DefaultMaxNodeCallsPerTenant
	}
	if maxPerNode == 0 {
		maxPerNode = DefaultMaxNodeCallsPerNode
	}
	return &nodeCalls{
		perPrincipal: map[auth.Principal]int{}, perTenant: map[string]int{}, perNode: map[string]int{},
		maxPrincipal: maxPrincipal, maxTenant: maxTenant, maxPerNode: maxPerNode,
	}
}

// take takes a slot for a call by p, the authenticated caller, in tenant
// to node, and returns what gives it back.
func (n *nodeCalls) take(p auth.Principal, tenant, node string) (func(), error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.perPrincipal[p] >= n.maxPrincipal {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("%s has %d calls open to nodes, the most one may; end one first", p, n.maxPrincipal))
	}
	if n.perTenant[tenant] >= n.maxTenant {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("tenant %q has %d calls open to nodes, the most it may; end one first", tenant, n.maxTenant))
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

// callNode begins a call by p, the authenticated caller, in tenant to
// node; what names it in errors, such as the container it is for. The
// caller ends it with release. Reaching nodes some other way changes only
// this.
func (s *Server) callNode(p auth.Principal, tenant, node, what string) (*nodeCall, error) {
	t, err := s.tunnels.Node(node)
	if errors.Is(err, tunnel.ErrNoTunnel) {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s's node %s is not connected", what, node))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	release, err := s.nodeCalls.take(p, tenant, node)
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
