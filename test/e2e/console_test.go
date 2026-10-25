package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/consolews"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// dialConsole attaches to the serial console of the object of plural named
// name as user, failing as the apiserver refuses it, and sends start.
func (w *world) dialConsole(user, plural, name string, start *computev1alpha1.MachineConsoleStart) (*websocket.Conn, error) {
	w.t.Helper()
	c, resp, err := websocket.Dial(context.Background(), w.env.URL+consolews.Path(project, plural, name), &websocket.DialOptions{
		HTTPHeader: http.Header{auth.DevUserHeader: []string{user}}, Subprotocols: []string{consolews.Subprotocol},
	})
	if err != nil {
		if resp == nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, execws.ReadError(resp)
		}
		return nil, err
	}
	w.t.Cleanup(func() { _ = c.CloseNow() })
	sendConsole(w.t, c, &computev1alpha1.MachineConsoleRequest{Message: &computev1alpha1.MachineConsoleRequest_Start{Start: start}})
	return c, nil
}

func sendConsole(t *testing.T, c *websocket.Conn, req *computev1alpha1.MachineConsoleRequest) {
	t.Helper()
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageBinary, b); err != nil {
		t.Fatal(err)
	}
}

// nextConsole reads the console's next message; the error is how it ended.
func nextConsole(t *testing.T, c *websocket.Conn) (*computev1alpha1.MachineConsoleResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, execws.ErrorOf(err)
	}
	res := &computev1alpha1.MachineConsoleResponse{}
	if err := proto.Unmarshal(b, res); err != nil {
		t.Fatal(err)
	}
	return res, nil
}

// consoleState reads the console's next message, which must be a state.
func consoleState(t *testing.T, c *websocket.Conn) *computev1alpha1.MachineConsoleState {
	t.Helper()
	res, err := nextConsole(t, c)
	if err != nil || res.GetState() == nil {
		t.Fatalf("the console sent %v, %v, not a state", res, err)
	}
	return res.GetState()
}

// consoleOutput reads the console's output until it holds want.
func consoleOutput(t *testing.T, c *websocket.Conn, want string) {
	t.Helper()
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		res, err := nextConsole(t, c)
		if err != nil {
			t.Fatalf("the console ended, %v, before sending %q; it sent %q", err, want, got.String())
		}
		got.Write(res.GetOutput())
	}
}

// TestAVirtualMachinesSerialConsole: a VM's serial console, through the
// apiserver and its node, tells the first client it holds the input at
// 80x24, draws it the screen, sends it what the guest writes, and takes its
// typing to the guest; a second client watches, and is told when the
// holder resizes the screen. A client that leaves is let go on the node,
// what it typed having reached nothing if it watched; once the holder
// leaves, the next to attach holds the input. Stopping the VM ends the
// console cleanly.
func TestAVirtualMachinesSerialConsole(t *testing.T) {
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("web", 0)
	w.waitVMReason("web", "Running")
	uid := w.machine2(vmMachineOf("web")).GetMetadata().GetUid()
	if err := w.host.ConsoleOutput(uid, []byte("web login:")); err != nil {
		t.Fatal(err)
	}

	a, err := w.dialConsole(apiservertest.Root, registry.VirtualMachine.Plural, "web", &computev1alpha1.MachineConsoleStart{})
	if err != nil {
		t.Fatal(err)
	}
	if st := consoleState(t, a); !st.GetInput() || st.GetSize().GetColumns() != 80 || st.GetSize().GetRows() != 24 {
		t.Errorf("the first client is told %v, want the input at 80x24", st)
	}
	consoleOutput(t, a, "web login:")
	b, err := w.dialConsole(apiservertest.Root, registry.VirtualMachine.Plural, "web", &computev1alpha1.MachineConsoleStart{})
	if err != nil {
		t.Fatal(err)
	}
	if st := consoleState(t, b); st.GetInput() {
		t.Error("the second client holds the input")
	}
	consoleOutput(t, b, "web login:")

	sendConsole(t, a, &computev1alpha1.MachineConsoleRequest{Message: &computev1alpha1.MachineConsoleRequest_Input{Input: []byte("ubuntu\r")}})
	testutil.Eventually(t, "the holder's typing at the guest", func() (bool, string) {
		got := string(w.host.ConsoleTyped(uid))
		return got == "ubuntu\r", got
	})
	if err := w.host.ConsoleOutput(uid, []byte("Password: ")); err != nil {
		t.Fatal(err)
	}
	consoleOutput(t, a, "Password: ")
	consoleOutput(t, b, "Password: ")

	sendConsole(t, a, &computev1alpha1.MachineConsoleRequest{Message: &computev1alpha1.MachineConsoleRequest_Resize{Resize: &computev1alpha1.MachineConsoleSize{Columns: 160, Rows: 48}}})
	for name, c := range map[string]*websocket.Conn{"holder": a, "watcher": b} {
		if st := consoleState(t, c); st.GetSize().GetColumns() != 160 || st.GetSize().GetRows() != 48 {
			t.Errorf("the %s is told %v after the resize", name, st)
		}
	}

	// The watcher types, then leaves: once the node has let it go, all it
	// sent was taken, and none of it reached the guest.
	sendConsole(t, b, &computev1alpha1.MachineConsoleRequest{Message: &computev1alpha1.MachineConsoleRequest_Input{Input: []byte("intruder\r")}})
	_ = b.Close(websocket.StatusNormalClosure, "")
	testutil.Eventually(t, "the watcher let go on the node", func() (bool, string) {
		n := w.host.ConsoleClients(uid)
		return n == 1, fmt.Sprint(n, " clients")
	})
	if got := string(w.host.ConsoleTyped(uid)); got != "ubuntu\r" {
		t.Errorf("the guest was typed %q; the watcher's typing reached it", got)
	}
	_ = a.Close(websocket.StatusNormalClosure, "")
	testutil.Eventually(t, "the holder let go on the node", func() (bool, string) {
		n := w.host.ConsoleClients(uid)
		return n == 0, fmt.Sprint(n, " clients")
	})
	c, err := w.dialConsole(apiservertest.Root, registry.VirtualMachine.Plural, "web", &computev1alpha1.MachineConsoleStart{})
	if err != nil {
		t.Fatal(err)
	}
	if st := consoleState(t, c); !st.GetInput() {
		t.Error("once the holder left, the next client to attach does not hold the input")
	}

	w.applyVM("web", computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED)
	if err := consoleEnd(t, c); err != nil {
		t.Errorf("the console ended with %v, want a clean close", err)
	}
}

// consoleEnd reads the console until it closes, and returns how it did:
// nil for a clean close.
func consoleEnd(t *testing.T, c *websocket.Conn) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the console went on")
			}
			return execws.ErrorOf(err)
		}
	}
}

// TestTheSerialConsoleIsRefusedClearly: a container's console, and anyone's
// without compute.machines.exec, are refused before the upgrade, saying
// why, a caller without the permission told the same whether the VM
// exists; a VM started before consoles were interactive ends the console
// after it, FailedPrecondition, saying to start the VM again.
func TestTheSerialConsoleIsRefusedClearly(t *testing.T) {
	w := newIdleWorldWith(t, func(c *apiserver.Config) {
		c.ExtraRoles = map[string]authz.Role{readerRole: authz.NewRole(authz.TenantLevel,
			authz.Permission(registry.VirtualMachine, authz.Get), authz.MachineLogs)}
	})
	w.startAgent()
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.applyVM("web", 0)
	w.waitVMReason("web", "Running")
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	w.env.AddMember("reader@example.com", "platform.member")
	if _, err := w.root.Apply(context.Background(), registry.RoleBinding, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: "reader"},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "reader@example.com"}}, Role: readerRole,
			Scope: &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: apiservertest.Tenant}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.dialConsole(apiservertest.Root, registry.Container.Plural, "api", &computev1alpha1.MachineConsoleStart{}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a container's console: %v, want failed precondition", err)
	}
	for _, name := range []string{"web", "absent"} {
		if _, err := w.dialConsole("reader@example.com", registry.VirtualMachine.Plural, name, &computev1alpha1.MachineConsoleStart{}); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("one who may read %s and its logs, attaching: %v, want denied", name, err)
		}
	}

	w.host.StartedWithoutConsole(w.machine2(vmMachineOf("web")).GetMetadata().GetUid())
	c, err := w.dialConsole(apiservertest.Root, registry.VirtualMachine.Plural, "web", &computev1alpha1.MachineConsoleStart{})
	if err != nil {
		t.Fatal(err)
	}
	if err := consoleEnd(t, c); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "started again") {
		t.Errorf("an older VM's console: %v, want failed precondition saying it takes a start", err)
	}
}
