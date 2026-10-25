package apiserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/consolews"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// dialConsoleAs opens web's serial console as user, failing as the
// apiserver refuses it before the upgrade.
func dialConsoleAs(t *testing.T, url, user string) (*websocket.Conn, error) {
	t.Helper()
	c, resp, err := websocket.Dial(t.Context(), url+consolews.Path("web", "virtualmachines", "web"), &websocket.DialOptions{
		HTTPHeader:   http.Header{auth.DevUserHeader: []string{user}},
		Subprotocols: []string{consolews.Subprotocol},
	})
	if err != nil {
		if resp == nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		return nil, execws.ReadError(resp)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c, nil
}

// closedWith reads c until it closes, and returns the error its close says.
func closedWith(t *testing.T, c *websocket.Conn) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testutil.Wait)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the console stayed open")
			}
			return execws.ErrorOf(err)
		}
	}
}

// TestTheConsoleNeedsExecNotSSH: a VM's serial console is let in by
// compute.machines.exec: eve, who may exec but not ssh, attaches; sam, who
// may ssh but not exec, is refused before the upgrade.
func TestTheConsoleNeedsExecNotSSH(t *testing.T) {
	w := newVMExecWorld(t)
	if _, err := dialConsoleAs(t, w.url, "eve@example.com"); err != nil {
		t.Errorf("eve, who may exec: %v", err)
	}
	_, err := dialConsoleAs(t, w.url, "sam@example.com")
	wantCodeOf(t, err, connect.CodePermissionDenied, "sam, who may ssh but not exec")
}

// TestAConsoleWithoutAStartEndsSayingWhy: a client that sends no start
// within the start timeout is told so, InvalidArgument, as an exec's is;
// one whose access is taken away before its start is told that, as its
// close's code, and the audit line says it too.
func TestAConsoleWithoutAStartEndsSayingWhy(t *testing.T) {
	t.Run("no start", func(t *testing.T) {
		w := newStreamWorld(t)
		w.s.cfg.ExecStart = 200 * time.Millisecond
		c := w.dial(t, w.asBob(), consolews.Path("web", "virtualmachines", "web"), consolews.Subprotocol, false)
		wantCodeOf(t, closedWith(t, c), connect.CodeInvalidArgument, "a console with no start")
	})
	t.Run("revoked before the start", func(t *testing.T) {
		w := newStreamWorld(t)
		c := w.dial(t, w.asBob(), consolews.Path("web", "virtualmachines", "web"), consolews.Subprotocol, false)
		// A keepalive: the console waits on, for its start.
		if b, _ := proto.Marshal(&computev1alpha1.MachineConsoleRequest{}); c.Write(t.Context(), websocket.MessageBinary, b) != nil {
			t.Fatal("writing a keepalive")
		}
		if _, err := w.s.backend(registry.RoleBinding).Delete(w.system, resource.Key{Name: "bob-web"}, nil); err != nil {
			t.Fatal(err)
		}
		w.clock.Advance(auth.RecheckInterval)
		wantCodeOf(t, closedWith(t, c), connect.CodePermissionDenied, "a console whose binding went before its start")
	})
}
