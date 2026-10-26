package apiserver

import (
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/coder/websocket"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/portforwardws"
	"github.com/YukinaMochizuki/hangar/internal/sshws"
)

// noForwardRole holds getting VMs, exec and ssh, but not port forwards.
const noForwardRole = "test.no-forward"

// forwardAs opens a port forward to port 8080 of the VM name as principal
// p, asking for subprotocols, returning the error a refusal before the
// upgrade says.
func forwardAs(t *testing.T, s *Server, p auth.Principal, name string, subprotocols ...string) error {
	t.Helper()
	srv := httptest.NewServer(s.Handler(auth.Chain{fixedAuth{p, true}}))
	t.Cleanup(srv.Close)
	c, resp, err := websocket.Dial(t.Context(), srv.URL+portforwardws.Path("web", "virtualmachines", name, 8080), &websocket.DialOptions{Subprotocols: subprotocols})
	if err == nil {
		_ = c.CloseNow()
		return nil
	}
	if resp == nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return execws.ReadError(resp)
}

// TestThePortForwardNeedsItsPermission: a member who may get the VM, exec
// in it and ssh to it, but not forward its ports, is refused, as is one who
// may only get it; its owner gets as far as the node.
func TestThePortForwardNeedsItsPermission(t *testing.T) {
	t.Parallel()
	s, system, m := sshTunnelWorld(t)
	applied, err := s.backend(registry.Machine).Apply(system, m)
	if err != nil {
		t.Fatal(err)
	}
	reportMachine(t, s, system, applied.Object.(*computev1alpha1.Machine), computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, 1)
	for who, want := range map[string]connect.Code{
		"dan@example.com":   connect.CodePermissionDenied,
		"carol@example.com": connect.CodePermissionDenied,
		"bob@example.com":   connect.CodePermissionDenied,
		"alice@example.com": connect.CodeUnavailable, // n1 is not connected
	} {
		err := forwardAs(t, s, auth.Principal{Type: auth.User, ID: who}, "web", portforwardws.Subprotocol)
		wantCodeOf(t, err, want, who+" forwarding web's port")
	}
	// dan may ssh, which the forward is not.
	wantCodeOf(t, tunnelAs(t, s, auth.Principal{Type: auth.User, ID: "dan@example.com"}, "web"), connect.CodeUnavailable, "dan opening web's SSH tunnel")
}

// TestThePortForwardNeedsItsSubprotocol: a caller who may forward the
// running container's port, whose node is connected, is refused before the
// upgrade for asking for no subprotocol or another capability's, and let in
// with its own.
func TestThePortForwardNeedsItsSubprotocol(t *testing.T) {
	t.Parallel()
	w := newStreamWorld(t)
	path := w.url + portforwardws.Path("web", "containers", "api", 8080)
	for what, subprotocols := range map[string][]string{
		"no subprotocol":    nil,
		"ssh's subprotocol": {sshws.Subprotocol},
	} {
		c, resp, err := websocket.Dial(t.Context(), path, &websocket.DialOptions{HTTPHeader: w.asBob(), Subprotocols: subprotocols})
		if err == nil {
			_ = c.CloseNow()
			t.Errorf("a forward asking for %s was let in", what)
			continue
		}
		if resp != nil {
			err = execws.ReadError(resp)
			_ = resp.Body.Close()
		}
		wantCodeOf(t, err, connect.CodeInvalidArgument, "a forward asking for "+what)
	}
	_ = w.dial(t, w.asBob(), portforwardws.Path("web", "containers", "api", 8080), portforwardws.Subprotocol, false)
}
