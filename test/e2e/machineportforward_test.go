package e2e

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/cli"
	"github.com/YukinaMochizuki/hangar/internal/execws"
	"github.com/YukinaMochizuki/hangar/internal/portforwardws"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// forwardTo opens a port forward to port of the object of kind named name
// as user; query, if set, replaces the port's query.
func (w *world) forwardTo(user string, kind registry.Resource, name string, port uint16, query string) (*websocket.Conn, error) {
	w.t.Helper()
	path := portforwardws.Path(project, kind.Plural, name, port)
	if query != "" {
		path = path[:strings.IndexByte(path, '?')] + query
	}
	c, resp, err := websocket.Dial(context.Background(), w.env.URL+path, &websocket.DialOptions{
		HTTPHeader: http.Header{auth.DevUserHeader: []string{user}}, Subprotocols: []string{portforwardws.Subprotocol},
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

// TestThePortForwardReachesAnyPortOfItsMachine: a container's port forward
// reaches the port it names of the container's first interface, whatever
// its security groups say, and a virtual machine's likewise; one without
// compute.machines.portforward, or naming no port or a port out of range,
// is refused before anything is dialled; a port nothing answers at closes
// the WebSocket as unavailable; a stopped container is not reached.
func TestThePortForwardReachesAnyPortOfItsMachine(t *testing.T) {
	t.Parallel()
	w, _ := sshWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	w.applyVMOn("db")
	def := w.networkUID("default")
	api, db := w.ipv4Of("api"), w.vmAddrs("db")["default"]
	w.host.ServeGuest(def, netip.AddrPortFrom(api, 8080), banner("api 8080"))
	w.host.ServeGuest(def, netip.AddrPortFrom(api, 9000), banner("api 9000"))
	w.host.ServeGuest(def, netip.AddrPortFrom(db, 5432), banner("db 5432"))

	// Refused before anything is dialled.
	if _, err := w.forwardTo("mallory@example.com", registry.Container, "api", 8080, ""); err == nil || !strings.Contains(err.Error(), "permission_denied") {
		t.Errorf("mallory forwarding to api: %v", err)
	}
	for _, q := range []string{"?", "?port=0", "?port=65536", "?port=http", "?port=1&port=2"} {
		if _, err := w.forwardTo(apiservertest.Root, registry.Container, "api", 0, q); err == nil || !strings.Contains(err.Error(), "invalid_argument") {
			t.Errorf("a forward with %s: %v", q, err)
		}
	}
	if dials := w.host.Dials(); len(dials) != 0 {
		t.Errorf("refused, yet dialled %v", dials)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reach := func(kind registry.Resource, name string, port uint16, want string) {
		t.Helper()
		c, err := w.forwardTo(apiservertest.Root, kind, name, port, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, b, err := c.Read(ctx); err != nil || string(b) != want+"\n" {
			t.Errorf("%s %s's port %d reached %q %v", kind.Kind, name, port, b, err)
		}
		if err := c.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
			t.Fatal(err)
		}
		if _, b, err := c.Read(ctx); err != nil || string(b) != "ping" {
			t.Errorf("echoed %q %v", b, err)
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
	reach(registry.Container, "api", 8080, "api 8080")
	reach(registry.Container, "api", 9000, "api 9000")
	reach(registry.VirtualMachine, "db", 5432, "db 5432")
	want := []string{def + " " + api.String() + ":8080", def + " " + api.String() + ":9000", def + " " + db.String() + ":5432"}
	testutil.Eventually(t, "the dials", func() (bool, string) {
		got := w.host.Dials()
		return slices.Equal(got, want), strings.Join(got, ", ")
	})

	// A port nothing answers at.
	if c, err := w.forwardTo(apiservertest.Root, registry.Container, "api", 7000, ""); err != nil {
		t.Errorf("a forward to a closed port refused before the upgrade: %v", err)
	} else if _, _, err := c.Read(ctx); err == nil || !strings.Contains(execws.ErrorOf(err).Error(), "port 7000") {
		t.Errorf("a forward to a closed port: %v", err)
	}

	// Stopped, it is not reached.
	stopped := container("api", "nginx")
	stopped.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	w.apply(stopped)
	w.waitReason("api", "Stopped")
	if _, err := w.forwardTo(apiservertest.Root, registry.Container, "api", 8080, ""); err == nil || !strings.Contains(err.Error(), "failed_precondition") {
		t.Errorf("a stopped container's forward: %v", err)
	}
}

// TestHangarPortForward: hangar port-forward listens on a local port and
// relays each connection to the container's port.
func TestHangarPortForward(t *testing.T) {
	t.Parallel()
	w := newIdleWorld(t)
	w.startAgent()
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	w.host.ServeGuest(w.networkUID("default"), netip.AddrPortFrom(w.ipv4Of("api"), 8080), banner("api 8080"))

	config := t.TempDir() + "/config.yaml"
	env := func(k string) string {
		if k == "HANGAR_CONFIG" {
			return config
		}
		return os.Getenv(k)
	}
	run := func(ctx context.Context, args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := cli.Run(ctx, args, cli.IO{In: strings.NewReader(""), Out: &out, Err: &errb, Env: env}, cli.Options{})
		return code, errb.String()
	}
	if code, e := run(context.Background(), "config", "set-context", "dev", "--endpoint", w.env.URL, "--auth", "dev", "--user", apiservertest.Root, "--project", project, "--use"); code != 0 {
		t.Fatal(e)
	}
	if code, e := run(context.Background(), "port-forward", "secret", "x", "80"); code == 0 || !strings.Contains(e, "only containers and virtual machines") {
		t.Errorf("a forward to a secret: exit %d: %s", code, e)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	local := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() {
		code, _ := run(ctx, "port-forward", "container", "api", strconv.Itoa(local)+":8080")
		done <- code
	}()
	var conn net.Conn
	testutil.Eventually(t, "the local port listens", func() bool {
		conn, err = net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(local))
		return err == nil
	})
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	if line, err := r.ReadString('\n'); err != nil || line != "api 8080\n" {
		t.Errorf("reached %q %v", line, err)
	}
	_ = conn.Close()
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("hangar port-forward exited %d once interrupted", code)
	}
}
