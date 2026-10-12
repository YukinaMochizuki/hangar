package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/execws"
)

// dialExec opens an exec in a container as user, failing as the apiserver
// refuses it.
func (w *world) dialExec(user, name string) (*websocket.Conn, error) {
	w.t.Helper()
	c, resp, err := websocket.Dial(context.Background(), w.env.URL+execws.Path(project, name), &websocket.DialOptions{
		HTTPHeader:   http.Header{auth.DevUserHeader: []string{user}},
		Subprotocols: []string{execws.Subprotocol},
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
	return c, nil
}

func sendExec(t *testing.T, c *websocket.Conn, req *computev1alpha1.ContainerExecRequest) {
	t.Helper()
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageBinary, b); err != nil {
		t.Fatal(err)
	}
}

func startExec(start *computev1alpha1.ContainerExecStart) *computev1alpha1.ContainerExecRequest {
	return &computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Start{Start: start}}
}

type execOutput struct {
	stdout, stderr string
	exit           *computev1alpha1.ContainerExecExit
	err            error
}

// readExec reads an exec to its close.
func readExec(t *testing.T, c *websocket.Conn) (r execOutput) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out, errb strings.Builder
	defer func() { r.stdout, r.stderr = out.String(), errb.String() }()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("the exec did not end")
			}
			r.err = execws.ErrorOf(err)
			return r
		}
		res := &computev1alpha1.ContainerExecResponse{}
		if err := proto.Unmarshal(b, res); err != nil {
			t.Fatal(err)
		}
		switch m := res.GetMessage().(type) {
		case *computev1alpha1.ContainerExecResponse_Stdout:
			out.Write(m.Stdout)
		case *computev1alpha1.ContainerExecResponse_Stderr:
			errb.Write(m.Stderr)
		case *computev1alpha1.ContainerExecResponse_Exit:
			r.exit = m.Exit
		}
	}
}

// exec runs a command in a container as root, with stdin's messages after
// the start, to its end.
func (w *world) exec(name string, start *computev1alpha1.ContainerExecStart, input ...*computev1alpha1.ContainerExecRequest) execOutput {
	w.t.Helper()
	c, err := w.dialExec(apiservertest.Root, name)
	if err != nil {
		return execOutput{err: err}
	}
	sendExec(w.t, c, startExec(start))
	for _, in := range input {
		sendExec(w.t, c, in)
	}
	return readExec(w.t, c)
}

// TestExecInAContainer: a command runs in a running container through the
// apiserver and the node's tunnel, with the container's environment, and
// its output, stdin, terminal and exit pass through.
func TestExecInAContainer(t *testing.T) {
	w := newWorld(t)
	c := container("api", "nginx")
	c.Spec.Env = []*computev1alpha1.EnvVar{{Name: "MODE", Value: "prod"}}
	w.apply(c)
	w.waitReason("api", "Running")

	r := w.exec("api", &computev1alpha1.ContainerExecStart{Command: []string{"show", "PATH", "MODE", "EXTRA"}})
	if r.err != nil || r.stdout != "/usr/bin\nprod\n\n\n\n" || r.exit.GetCode() != 0 {
		t.Errorf("the container's environment: %+v", r)
	}
	r = w.exec("api", &computev1alpha1.ContainerExecStart{
		Command: []string{"show", "MODE", "EXTRA"}, Env: []*computev1alpha1.EnvVar{{Name: "MODE", Value: "dev"}, {Name: "EXTRA", Value: "1"}},
		User: "1000", WorkingDir: "/tmp",
	})
	if r.err != nil || r.stdout != "dev\n1\n/tmp\n1000\n" {
		t.Errorf("overridden: %+v", r)
	}
	r = w.exec("api", &computev1alpha1.ContainerExecStart{Command: []string{"exit", "3", "out", "err"}})
	if r.err != nil || r.stdout != "out" || r.stderr != "err" || r.exit.GetCode() != 3 {
		t.Errorf("an exit code: %+v", r)
	}
	r = w.exec("api", &computev1alpha1.ContainerExecStart{Command: []string{"cat"}, Stdin: true},
		&computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Stdin{Stdin: []byte("piped")}},
		&computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_StdinClose{StdinClose: &computev1alpha1.ContainerExecStdinClose{}}})
	if r.err != nil || r.stdout != "piped" || r.exit.GetCode() != 0 {
		t.Errorf("stdin: %+v", r)
	}
	r = w.exec("api", &computev1alpha1.ContainerExecStart{Command: []string{"cat"}})
	if r.err != nil || r.stdout != "" || r.exit.GetCode() != 0 {
		t.Errorf("no stdin: %+v", r)
	}
	r = w.exec("api", &computev1alpha1.ContainerExecStart{Command: []string{"size"}, Tty: true, Stdin: true, Size: &computev1alpha1.ContainerExecWindowSize{Rows: 24, Cols: 80}},
		&computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Resize{Resize: &computev1alpha1.ContainerExecWindowSize{Rows: 30, Cols: 100}}},
		&computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Stdin{Stdin: []byte("\n")}})
	if r.err != nil || r.stdout != "24 80\n30 100\n" {
		t.Errorf("a terminal: %+v", r)
	}
	r = w.exec("api", &computev1alpha1.ContainerExecStart{Command: []string{"wait", "x"}},
		&computev1alpha1.ContainerExecRequest{Message: &computev1alpha1.ContainerExecRequest_Signal{Signal: 15}})
	if r.err != nil || r.exit.GetCode() != 143 || r.exit.GetSignal() != 15 {
		t.Errorf("a signal: %+v", r)
	}
	r = w.exec("api", &computev1alpha1.ContainerExecStart{Command: []string{"no-such-program"}})
	if r.err != nil || r.exit.GetCode() != 127 {
		t.Errorf("not found: %+v", r)
	}
	if n := w.host.Execs(w.machine("api").GetMetadata().GetUid()); n != 0 {
		t.Errorf("%d execs left running", n)
	}
}

// TestAKilledClientKillsItsProcess: the process goes with the client,
// however it goes.
func TestAKilledClientKillsItsProcess(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	uid := w.machine("api").GetMetadata().GetUid()
	for _, end := range []func(*websocket.Conn){
		func(c *websocket.Conn) { _ = c.CloseNow() },
		func(c *websocket.Conn) { _ = c.Close(websocket.StatusGoingAway, "") },
	} {
		c, err := w.dialExec(apiservertest.Root, "api")
		if err != nil {
			t.Fatal(err)
		}
		sendExec(t, c, startExec(&computev1alpha1.ContainerExecStart{Command: []string{"wait", "x"}, Stdin: true}))
		w.eventually("the process running", func() (bool, string) { return w.host.Execs(uid) == 1, fmt.Sprint(w.host.Execs(uid)) })
		end(c)
		w.eventually("the process killed", func() (bool, string) { return w.host.Execs(uid) == 0, fmt.Sprint(w.host.Execs(uid)) })
	}
}

// readerRole reads containers and their logs, and nothing more.
const readerRole = "e2e.reader"

// TestExecNeedsItsOwnPermission: the tenant's owner execs; one who may
// get and read logs, a sponsor, a member and another tenant's owner do
// not, and are told so the same whether the container exists or not.
func TestExecNeedsItsOwnPermission(t *testing.T) {
	authz.Roles[readerRole] = authz.Role{Level: authz.TenantLevel, Permissions: []string{
		authz.Permission(registry.Container, authz.Get), authz.ContainerLogs,
	}}
	t.Cleanup(func() { delete(authz.Roles, readerRole) })
	w := newWorld(t)
	bind := func(name, user, role string, scope *iamv1alpha1.BindingScope) {
		t.Helper()
		if _, err := w.root.Apply(context.Background(), registry.RoleBinding, &iamv1alpha1.RoleBinding{
			Metadata: &metav1.ObjectMeta{Name: name},
			Spec: &iamv1alpha1.RoleBindingSpec{
				Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: user}}, Role: role, Scope: scope,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	platform := &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{Platform: &emptypb.Empty{}}}
	tenant := &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: apiservertest.Tenant}}
	for _, u := range []string{"owner", "other", "member", "reader"} {
		bind("member-"+u, u+"@example.com", "platform.member", platform)
	}
	bind("sponsor", "sponsor@example.com", "platform.sponsor", platform)
	bind("owner", "owner@example.com", "tenant.owner", tenant)
	bind("reader", "reader@example.com", readerRole, tenant)
	if _, err := w.root.Apply(context.Background(), registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	bind("other-owner", "other@example.com", "tenant.owner", &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: "other"}})
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")

	var c *websocket.Conn
	w.eventually("the owner's exec", func() (bool, string) {
		var err error
		c, err = w.dialExec("owner@example.com", "api")
		return err == nil, fmt.Sprint(err)
	})
	sendExec(t, c, startExec(&computev1alpha1.ContainerExecStart{Command: []string{"echo", "hi"}}))
	if r := readExec(t, c); r.err != nil || r.stdout != "hi\n" {
		t.Errorf("the owner's exec: %+v", r)
	}
	// The reader reads the container and its logs.
	if _, err := w.env.As("reader@example.com").Get(context.Background(), registry.Container, client.Key{Project: project, Name: "api"}); err != nil {
		t.Errorf("the reader getting the container: %v", err)
	}
	if _, err := readLogs(context.Background(), w.logsAs("reader@example.com"), &computev1alpha1.ContainerLogsRequest{Project: project, Name: "api"}); err != nil {
		t.Errorf("the reader reading the logs: %v", err)
	}
	for _, user := range []string{"reader@example.com", "sponsor@example.com", "member@example.com", "other@example.com", "stranger@example.com"} {
		for _, name := range []string{"api", "absent"} {
			_, err := w.dialExec(user, name)
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("%s exec in %s: %v, want denied", user, name, err)
			}
		}
	}
}

// TestExecIsRefusedClearly: a container that is not running, a node that
// is down, and a caller over the caps on calls to nodes are each told so.
func TestExecIsRefusedClearly(t *testing.T) {
	w := newIdleWorldWith(t, func(c *apiserver.Config) { c.MaxNodeCallsPerPrincipal = 1 })
	w.startAgent()
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")

	c, err := w.dialExec(apiservertest.Root, "api")
	if err != nil {
		t.Fatal(err)
	}
	sendExec(t, c, startExec(&computev1alpha1.ContainerExecStart{Command: []string{"wait", "x"}}))
	uid := w.machine("api").GetMetadata().GetUid()
	w.eventually("the first exec running", func() (bool, string) { return w.host.Execs(uid) == 1, "" })
	if _, err := w.dialExec(apiservertest.Root, "api"); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("an exec over the cap: %v", err)
	}
	_ = c.CloseNow()
	w.eventually("the first exec ended", func() (bool, string) { return w.host.Execs(uid) == 0, "" })

	w.stopAgent()
	w.eventually("the node's tunnel gone", func() (bool, string) {
		_, err := w.dialExec(apiservertest.Root, "api")
		return connect.CodeOf(err) == connect.CodeUnavailable, fmt.Sprint(err)
	})
	w.startAgent()

	ct, _ := get[*computev1alpha1.Container](w, registry.Container, "api")
	ct.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	w.apply(ct)
	w.eventually("a stopped container told it is not running", func() (bool, string) {
		_, err := w.dialExec(apiservertest.Root, "api")
		return connect.CodeOf(err) == connect.CodeFailedPrecondition && strings.Contains(fmt.Sprint(err), "not running"), fmt.Sprint(err)
	})
}
