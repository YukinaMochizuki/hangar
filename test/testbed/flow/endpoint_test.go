//go:build testbed

package flow

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

const edgeDomain = "hangarlab.test"

// edge is hangar-edge running as nobody in front of the cluster.
type edge struct {
	addr string
}

// startEdge runs hangar-edge, unprivileged, on loopback, reading the API on
// the apiserver's edge socket and dialling through the agent's.
func startEdge(t *testing.T) *edge {
	t.Helper()
	nobody := lookupUID(t, "nobody")
	bin := filepath.Join(apiDir, "hangar-edge")
	copyFile(t, vmtest.Artifacts().Edge, bin, 0o755)
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	log := openLog(t, "edge.log")
	cmd := exec.Command(bin,
		"--listen", addr,
		"--api-socket", filepath.Join(apiDir, "edge.sock"),
		"--dial-socket", filepath.Join(apiDir, "edge-dial.sock"),
		"--domain", edgeDomain,
		"--console-url", "https://console.testbed.invalid",
	)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(nobody), Gid: uint32(nobody)},
		Pdeathsig:  syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("--- edge log:\n%s", tail(filepath.Join(workDir, "edge.log")))
		}
	})
	eventually(t, 30*time.Second, "the edge to listen", func() (bool, string) {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
		}
		return err == nil, fmt.Sprint(err)
	})
	return &edge{addr: addr}
}

// get asks the edge for host's "/", with a Hangar token as the bearer if
// set. A request without one is no browser's (it asks for no page), so the
// edge refuses rather than sends it to sign in.
func (e *edge) get(t *testing.T, host, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+e.addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Sec-Fetch-Mode", "cors")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, string(b)
}

// waitStatus waits until the edge answers host with status.
func (e *edge) waitStatus(t *testing.T, host, token string, status int) string {
	t.Helper()
	var body string
	eventually(t, 60*time.Second, fmt.Sprintf("%s answering %d", host, status), func() (bool, string) {
		var got int
		got, body = e.get(t, host, token)
		return got == status, fmt.Sprintf("%d %.200s", got, body)
	})
	return body
}

// fromGatewayTo says whether a TCP connection from the network's gateway,
// as the platform's own sockets make one, reaches addr.
func fromGatewayTo(netUID string, gateway netip.Addr, addr netip.AddrPort) bool {
	script := fmt.Sprintf(`import socket
s=socket.socket(); s.bind((%q,0)); s.settimeout(2)
s.connect((%q,%d))`, gateway.String(), addr.Addr().String(), addr.Port())
	return exec.Command("ip", "netns", "exec", hostnet.NamespaceOf(netUID), "python3", "-c", script).Run() == nil
}

// TestPublicEndpointsOnARealHost: nginx in a container whose security group
// lets nothing in is reached through hangar-edge by its endpoint's hostname,
// privately only with a Hangar token the project allows, and publicly by
// anyone; the gateway's way in to its port exists while an endpoint does,
// and goes with the last one.
func TestPublicEndpointsOnARealHost(t *testing.T) {
	if installed {
		t.Skip("the installed apiserver and edge are not this test's to configure")
	}
	nobody := lookupUID(t, "nobody")
	c := startClusterWith(t, func(c *cluster) {
		c.apiArgs = []string{
			"--edge-socket", filepath.Join(apiDir, "edge.sock"), "--edge-uid", strconv.Itoa(nobody),
			"--endpoint-domain", edgeDomain,
		}
		c.nodeArgs = []string{"--edge-dial-socket", filepath.Join(apiDir, "edge-dial.sock"), "--edge-user", "nobody"}
	})
	e := startEdge(t)

	c.applyYAML(lockedGroupYAML())
	c.applyYAML(fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata: {name: site}
spec:
  image: %s
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi}
  interfaces: [{network: default, securityGroups: [locked]}]
  ports: [{name: http, port: 80, public: true}]
`, nginxRef))
	c.waitReady("site", 5*time.Minute)
	c.cli("wait", "publicendpoint", "site-http", "--for=condition=Ready", "--timeout=60s")
	private := "site-http-" + project + "." + edgeDomain

	// Private: a Hangar token, and a grant in the project.
	editor := strings.TrimSpace(c.cli("token", "create", "edge-editor", "--role", "project.editor"))
	idle := strings.TrimSpace(c.cli("token", "create", "edge-idle"))
	if body := e.waitStatus(t, private, editor, http.StatusOK); !strings.Contains(body, "Welcome to nginx") {
		t.Errorf("the private endpoint answered %.200q", body)
	}
	if status, body := e.get(t, private, ""); status != http.StatusUnauthorized {
		t.Errorf("without a token: %d %.200s", status, body)
	}
	if status, body := e.get(t, private, idle); status != http.StatusForbidden {
		t.Errorf("a token the project grants nothing: %d %.200s", status, body)
	}

	// Public: anyone.
	c.applyYAML(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: PublicEndpoint
metadata: {name: pub}
spec: {target: {container: site}, port: 80, access: PUBLIC_ENDPOINT_ACCESS_PUBLIC}
`)
	c.cli("wait", "publicendpoint", "pub", "--for=condition=Ready", "--timeout=60s")
	public := "pub-" + project + "." + edgeDomain
	if body := e.waitStatus(t, public, "", http.StatusOK); !strings.Contains(body, "Welcome to nginx") {
		t.Errorf("the public endpoint answered %.200q", body)
	}

	// The group lets nothing in, but the gateway reaches port 80 while an
	// endpoint does.
	m := c.machine("site")
	nw := c.network("default")
	addr := netip.MustParsePrefix(m.GetSpec().GetInterfaces()[0].GetIpv4()).Addr()
	gateway := netip.MustParsePrefix(nw.GetSpec().GetIpv4Cidr()).Addr().Next()
	if !fromGatewayTo(nw.GetMetadata().GetUid(), gateway, netip.AddrPortFrom(addr, 80)) {
		t.Error("the gateway does not reach the endpoint's port")
	}

	// Both endpoints gone, so is the way in, and the edge has nothing.
	c.cli("delete", "publicendpoint", "pub")
	c.applyYAML(fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata: {name: site}
spec:
  image: %s
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi}
  interfaces: [{network: default, securityGroups: [locked]}]
  ports: [{name: http, port: 80}]
`, nginxRef))
	e.waitStatus(t, private, editor, http.StatusNotFound)
	e.waitStatus(t, public, "", http.StatusNotFound)
	eventually(t, 30*time.Second, "the gateway's way in to close", func() (bool, string) {
		return !fromGatewayTo(nw.GetMetadata().GetUid(), gateway, netip.AddrPortFrom(addr, 80)), ""
	})
	c.cli("delete", "container", "site")
	c.cli("wait", "container", "site", "--for=delete", "--timeout=2m")
}
