//go:build testbed

package flow

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
)

// open6YAML lets ICMP in from any IPv6 address.
const open6YAML = `apiVersion: network.hangarlab.cloud/v1alpha1
kind: SecurityGroup
metadata: {name: open6}
spec:
  network: default
  ingress:
  - {protocol: SECURITY_GROUP_PROTOCOL_ICMP, from: {cidr: "::/0"}}
`

// addressYAML binds Address name to an interface of a kind's object.
func addressYAML(name, kind, target string) string {
	return fmt.Sprintf(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: Address
metadata: {name: %s}
spec: {target: {%s: %s}}
`, name, kind, target)
}

// addressOf is the reserved address of Address name and the machine
// holding it, once it has both.
func (c *cluster) addressOf(t *testing.T, name string) (addr, machine string) {
	t.Helper()
	eventually(t, 60*time.Second, "address "+name+" reserved and bound", func() (bool, string) {
		var a struct {
			Status struct{ IPv6, Machine string }
		}
		out := c.cli("get", "address", name, "-o", "json")
		if err := json.Unmarshal([]byte(out), &a); err != nil {
			return false, err.Error()
		}
		addr, _, _ = strings.Cut(a.Status.IPv6, "/")
		machine = a.Status.Machine
		return addr != "" && machine != "", out
	})
	return addr, machine
}

// reachesFromUpstream says whether the upstream gets an answer to a ping of
// addr.
func reachesFromUpstream(addr string) bool {
	for range 2 {
		if exec.Command("ip", "netns", "exec", upstream6NS, "ping", "-6", "-c", "1", "-W", "1", addr).Run() == nil {
			return true
		}
	}
	return false
}

// startIPv6Cluster is a cluster whose agent has the IPv6 upstream as its
// uplink and its prefix, adjusted further by configure if given.
func startIPv6Cluster(t *testing.T, configure func(*cluster)) *cluster {
	t.Helper()
	if installed {
		t.Skip("the installed agent's uplink is the host's own")
	}
	return startClusterWith(t, func(c *cluster) {
		upstream6(t)
		c.uplink = upstream6Link
		c.nodeArgs = []string{"--public-ipv6-prefix", hostPrefix6}
		if configure != nil {
			configure(c)
		}
	})
}

// TestAContainerTakesAMovedAddressLive: an Address moved from one running
// container to another reaches the second within seconds, told by its
// guest agent, and leaves the first, whose tap no ping to it reaches once
// the second answers; neither starts again.
func TestAContainerTakesAMovedAddressLive(t *testing.T) {
	c := startIPv6Cluster(t, nil)
	c.applyYAML(open6YAML)
	for _, n := range []string{"one", "two"} {
		c.applyYAML(onNetworksGroupsYAML(n, "default", "open6"))
	}
	for _, n := range []string{"one", "two"} {
		c.waitReady(n, 3*time.Minute)
	}
	booted := map[string]string{}
	for _, n := range []string{"one", "two"} {
		booted[n] = c.machine(n).GetStatus().GetBootTime().String()
	}
	holds := func(n, addr string) bool {
		return strings.Contains(mustExec(t, c.machine(n).GetMetadata().GetUid(), "ip -6 -o addr show dev eth0 scope global"), addr+"/128")
	}

	c.applyYAML(addressYAML("pub", "container", "one"))
	addr, machine := c.addressOf(t, "pub")
	if machine != "ct-one" {
		t.Fatalf("address pub is held by %s, not one's machine", machine)
	}
	eventually(t, 30*time.Second, "one holding the address and answering on it", func() (bool, string) {
		return holds("one", addr) && reachesFromUpstream(addr), "no"
	})

	moved := time.Now()
	c.applyYAML(addressYAML("pub", "container", "two"))
	eventually(t, 30*time.Second, "two holding the address and answering on it", func() (bool, string) {
		return holds("two", addr) && reachesFromUpstream(addr), "no"
	})
	t.Logf("two answers on the address %v after the move", time.Since(moved).Round(100*time.Millisecond))
	// One's guest is told in the reconcile that let go of its tap's,
	// which may end after two answers.
	eventually(t, 10*time.Second, "one's guest letting go of the address", func() (bool, string) {
		return !holds("one", addr), "still holds it"
	})
	if n := echoesOn(t, c.machine("two").GetMetadata().GetUid(), addr, 3*time.Second); n == 0 {
		t.Error("the pings answered on the address do not reach two")
	}
	if n := echoesOn(t, c.machine("one").GetMetadata().GetUid(), addr, 3*time.Second); n > 0 {
		t.Errorf("%d pings to the address reach one, which it left", n)
	}
	for _, n := range []string{"one", "two"} {
		if now := c.machine(n).GetStatus().GetBootTime().String(); now != booted[n] {
			t.Errorf("%s started again as the address moved: booted %s, now %s", n, booted[n], now)
		}
	}
}

// echoesOn counts, for d, the pings to addr that reach the tap of machine
// uid, while the upstream pings addr.
func echoesOn(t *testing.T, uid, addr string, d time.Duration) int {
	t.Helper()
	tp := tapsOf(t, uid)
	if len(tp) == 0 {
		t.Fatalf("machine %s has no tap", uid)
	}
	secs := strconv.Itoa(int(d / time.Second))
	dump := exec.Command("ip", "netns", "exec", tp[0].netns, "timeout", secs, "tcpdump", "-n", "-l", "-i", tp[0].name,
		"icmp6 and ip6[40] == 128 and dst "+addr)
	pings := exec.Command("ip", "netns", "exec", upstream6NS, "ping", "-6", "-c", secs, "-i", "1", "-W", "1", addr)
	var out strings.Builder
	dump.Stdout = &out
	if err := dump.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	_ = pings.Run()
	_ = dump.Wait()
	return strings.Count(out.String(), "ICMP6, echo request")
}

// TestAVMTakesAMovedAddress: an Address moved from one running Ubuntu VM to
// another stops reaching the first at once, as the host moves its route and
// proxy entry, and reaches the second within its DHCPv6 renewal; neither
// starts again.
func TestAVMTakesAMovedAddress(t *testing.T) {
	srv := diskimgtest.New(t)
	url, _ := srv.Serve(t, diskimgtest.UbuntuName, diskimgtest.Ubuntu(t))
	fw, certs := diskimgtest.Firmware(t), srv.CertFile(t)
	c := startIPv6Cluster(t, func(c *cluster) {
		c.nodeArgs = append(c.nodeArgs, "--vm-firmware", fw)
		c.nodeEnv = []string{"SSL_CERT_FILE=" + certs}
	})
	c.applyYAML(fmt.Sprintf(`apiVersion: storage.hangarlab.cloud/v1alpha1
kind: Image
metadata: {name: noble}
spec: {url: %q, sha256: %s, os: ubuntu, version: "24.04"}
`, url, diskimgtest.UbuntuSHA256))
	c.applyYAML(open6YAML)
	for _, n := range []string{"alpha", "beta"} {
		c.applyYAML(fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: %s}
spec:
  rootDisk: {fromImage: {image: {name: noble}}}
  resources: {cpu: 1, memory: 1Gi}
  interfaces: [{network: default, securityGroups: [default, open6]}]
`, n))
	}
	for _, n := range []string{"alpha", "beta"} {
		c.cli("wait", "vm", n, "--for=condition=Ready", "--timeout=10m")
	}
	uid := func(vm string) string {
		var m struct{ Metadata struct{ UID string } }
		if err := json.Unmarshal([]byte(c.cli("get", "machine", "vm-"+vm, "-o", "json")), &m); err != nil {
			t.Fatal(err)
		}
		return m.Metadata.UID
	}
	one, two := uid("alpha"), uid("beta")
	started := map[string]string{}
	for _, u := range []string{one, two} {
		started[u] = unitProps(t, u, "ActiveEnterTimestamp")["ActiveEnterTimestamp"]
	}

	c.applyYAML(addressYAML("pub", "virtualMachine", "alpha"))
	addr, _ := c.addressOf(t, "pub")
	eventually(t, 5*time.Minute, "alpha leasing the address and answering on it", func() (bool, string) {
		return reachesFromUpstream(addr), "no answer"
	})

	c.applyYAML(addressYAML("pub", "virtualMachine", "beta"))
	moved := time.Now()
	eventually(t, 30*time.Second, "the address bound to beta", func() (bool, string) {
		_, m := c.addressOf(t, "pub")
		return m == "vm-beta", m
	})
	if n := echoesOn(t, one, addr, 8*time.Second); n > 0 {
		t.Errorf("%d pings to the address reached alpha after it moved away", n)
	}
	eventually(t, 6*time.Minute, "beta answering on the address", func() (bool, string) {
		return reachesFromUpstream(addr), "no answer"
	})
	t.Logf("beta answers on the address %v after the move (DHCPv6 T1 2m)", time.Since(moved).Round(time.Second))
	if n := echoesOn(t, two, addr, 4*time.Second); n == 0 {
		t.Error("the pings answered on the address do not reach beta")
	}
	if n := echoesOn(t, one, addr, 4*time.Second); n > 0 {
		t.Errorf("%d pings to the address still reach alpha", n)
	}
	for _, u := range []string{one, two} {
		if now := unitProps(t, u, "ActiveEnterTimestamp")["ActiveEnterTimestamp"]; now != started[u] {
			t.Errorf("machine %s's VM started again as the address moved: %s, now %s", u, started[u], now)
		}
	}
}
