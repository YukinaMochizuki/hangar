//go:build testbed

package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare/cftest"
	hangaredge "github.com/YukinaMochizuki/hangar/internal/edge"
	"github.com/YukinaMochizuki/hangar/internal/edge/accesstest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

const edgeDomain = "hangarlab.test"

// edge is hangar-edge running as nobody in front of the cluster, and the
// Access issuer it trusts.
type edge struct {
	addr   string
	issuer *accesstest.Issuer
}

// startEdge runs hangar-edge, unprivileged, on loopback, reading the API on
// the apiserver's edge socket and dialling through the agent's.
func startEdge(t *testing.T, iss *accesstest.Issuer) *edge {
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
		"--cf-team-domain", iss.URL,
		"--cf-audience", iss.Audience,
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
	return &edge{addr: addr, issuer: iss}
}

// get asks the edge for host's "/", with a token if set.
func (e *edge) get(t *testing.T, host, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+e.addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if token != "" {
		req.Header.Set(hangaredge.AccessJWTHeader, token)
	}
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
// privately only with an Access token of someone the tenant allows, and
// publicly by anyone; the gateway's way in to its port exists while an
// endpoint does, and goes with the last one.
func TestPublicEndpointsOnARealHost(t *testing.T) {
	if installed {
		t.Skip("the installed apiserver's Cloudflare and Access are real")
	}
	const cfToken, cfAccount = "cf-token-for-the-testbed", "0123456789abcdef0123456789abcdef"
	fake := cftest.New(cfToken, cfAccount, edgeDomain)
	t.Cleanup(fake.Close)
	cf, err := cloudflare.New(cfToken, cloudflare.WithBaseURL(fake.URL()))
	if err != nil {
		t.Fatal(err)
	}
	bypass, err := cf.CreateAccessPolicy(t.Context(), cfAccount, cloudflare.AccessPolicy{
		Name: "bypass", Decision: "bypass", Include: []json.RawMessage{cloudflare.EveryoneRule()},
	})
	if err != nil {
		t.Fatal(err)
	}
	nobody := lookupUID(t, "nobody")
	c := startClusterWith(t, func(c *cluster) {
		tokenFile := filepath.Join(apiDir, "cloudflare-api-token")
		if err := os.WriteFile(tokenFile, []byte(cfToken), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(tokenFile, nobody, nobody); err != nil {
			t.Fatal(err)
		}
		c.apiArgs = []string{
			"--edge-socket", filepath.Join(apiDir, "edge.sock"), "--edge-uid", strconv.Itoa(nobody),
			"--endpoint-domain", edgeDomain,
			"--cloudflare-api-token-file", tokenFile, "--cloudflare-account-id", cfAccount,
			"--cloudflare-bypass-policy-id", bypass.ID, "--cloudflare-api-url", fake.URL(),
		}
		c.nodeArgs = []string{"--edge-dial-socket", filepath.Join(apiDir, "edge-dial.sock"), "--edge-user", "nobody"}
	})
	iss := accesstest.NewIssuer(t)
	e := startEdge(t, iss)

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

	// Private: Access's sign-in, and a grant in the tenant.
	admin := iss.UserToken(t, admin)
	if body := e.waitStatus(t, private, admin, http.StatusOK); !strings.Contains(body, "Welcome to nginx") {
		t.Errorf("the private endpoint answered %.200q", body)
	}
	if status, body := e.get(t, private, ""); status != http.StatusUnauthorized {
		t.Errorf("without a token: %d %.200s", status, body)
	}
	if status, body := e.get(t, private, iss.UserToken(t, "mallory@testbed.invalid")); status != http.StatusForbidden {
		t.Errorf("a stranger: %d %.200s", status, body)
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
	ep, _ := get(c, "publicendpoint", "pub", &networkv1alpha1.PublicEndpoint{})
	if id := ep.GetStatus().GetAccessApplicationId(); id == "" {
		t.Errorf("the public endpoint has no Access application: %v", ep.GetStatus())
	} else if _, err := cf.GetAccessApp(t.Context(), cfAccount, id); err != nil {
		t.Errorf("its Access application %s: %v", id, err)
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
	e.waitStatus(t, private, admin, http.StatusNotFound)
	e.waitStatus(t, public, "", http.StatusNotFound)
	eventually(t, 30*time.Second, "the gateway's way in to close", func() (bool, string) {
		return !fromGatewayTo(nw.GetMetadata().GetUid(), gateway, netip.AddrPortFrom(addr, 80)), ""
	})
	if _, err := cf.GetAccessApp(context.Background(), cfAccount, ep.GetStatus().GetAccessApplicationId()); !cloudflare.IsNotFound(err) {
		t.Errorf("the public endpoint's Access application outlived it: %v", err)
	}
	c.cli("delete", "container", "site")
	c.cli("wait", "container", "site", "--for=delete", "--timeout=2m")
}
