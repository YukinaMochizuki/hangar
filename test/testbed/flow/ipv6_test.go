//go:build testbed

package flow

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
)

// The IPv6 upstream of TestContainersHavePublicIPv6, OVH's as
// test/testbed/network has it: the host's /64 on-link at the upstream,
// which asks for each address by neighbour solicitation, and the
// upstream's own address outside it. The agent takes its link as its
// uplink, so the containers of that test have IPv6 to it and no internet.
const (
	upstream6NS   = "tbf-up6"
	upstream6Link = "tbfup0"
	hostPrefix6   = "2001:db8:0:1::/64"
	upstreamAddr6 = "2001:db8:0:ff::ffff"
)

// upstream6 makes the IPv6 upstream and sets the host up for it as
// deploy/host does: forwarding, and proxy NDP on the uplink.
func upstream6(t *testing.T) {
	t.Helper()
	for _, args := range [][]string{
		{"ip", "netns", "add", upstream6NS},
		{"ip", "link", "add", upstream6Link, "type", "veth", "peer", "name", "tbfupx", "netns", upstream6NS},
		{"ip", "addr", "add", "198.51.100.2/24", "dev", upstream6Link},
		{"ip", "link", "set", upstream6Link, "up"},
		{"ip", "-n", upstream6NS, "link", "set", "tbfupx", "up"},
		{"ip", "-n", upstream6NS, "link", "set", "lo", "up"},
		{"sysctl", "-q", "-w", "net.ipv6.conf.all.forwarding=1"},
		{"sysctl", "-q", "-w", "net.ipv6.conf." + upstream6Link + ".proxy_ndp=1"},
		{"ip", "-6", "addr", "add", "2001:db8:0:1::1/64", "dev", upstream6Link, "nodad"},
		{"ip", "-6", "route", "add", "2001:db8:0:ff::/64", "dev", upstream6Link},
		{"ip", "-n", upstream6NS, "-6", "addr", "add", upstreamAddr6 + "/64", "dev", "tbfupx", "nodad"},
		{"ip", "-n", upstream6NS, "-6", "route", "add", hostPrefix6, "dev", "tbfupx"},
	} {
		mustRun(t, args[0], args[1:]...)
	}
}

// TestContainersHavePublicIPv6: on a host with a prefix, a container's boot
// disk gives it its /128 and the default route through the gateway; it
// reaches the upstream from it, is resolved to it by name, and is reached
// at it from the upstream only once a group lets it in.
func TestContainersHavePublicIPv6(t *testing.T) {
	if installed {
		t.Skip("the installed agent's uplink is the host's own")
	}
	c := startClusterWith(t, func(c *cluster) {
		upstream6(t)
		c.uplink = upstream6Link
		c.nodeArgs = []string{"--public-ipv6-prefix", hostPrefix6}
	})
	c.applyYAML(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: SecurityGroup
metadata: {name: open6}
spec:
  network: default
  ingress:
  - {protocol: SECURITY_GROUP_PROTOCOL_ICMP, from: {cidr: "::/0"}}
`)
	c.applyYAML(idlerYAML("six"))
	c.waitReady("six", 3*time.Minute)
	nic := c.machine("six").GetSpec().GetInterfaces()[0]
	addr, _, _ := strings.Cut(nic.GetIpv6(), "/")
	if !strings.HasPrefix(addr, "2001:db8:0:1:") {
		t.Fatalf("container six has IPv6 %q, not of %s", nic.GetIpv6(), hostPrefix6)
	}

	out := mustExec(t, c.machine("six").GetMetadata().GetUid(), "ip -6 addr show dev eth0 scope global; ip -6 route show default")
	if !strings.Contains(out, addr+"/128") || !strings.Contains(out, "via fe80::1 dev eth0") {
		t.Errorf("six's IPv6 is not its boot disk's %s/128 via fe80::1:\n%s", addr, out)
	}
	ping := func(args ...string) (bool, string) {
		o, errOut, code := execCLI(t, nil, append([]string{"ct", "six", "--"}, args...)...)
		return code == 0, fmt.Sprintf("exit %d: %s%s", code, o, errOut)
	}
	eventually(t, 30*time.Second, "six reaching the upstream", func() (bool, string) {
		return ping("ping", "-6", "-c", "1", "-W", "2", upstreamAddr6)
	})
	eventually(t, 30*time.Second, "six resolving its own name to its /128", func() (bool, string) {
		o, _, _ := execCLI(t, nil, "ct", "six", "--", "nslookup", "-type=AAAA", "six."+project+".hangar.internal")
		return strings.Contains(o, addr), o
	})

	fromUpstream := func() bool {
		for range 3 {
			if exec.Command("ip", "netns", "exec", upstream6NS, "ping", "-6", "-c", "1", "-W", "1", addr).Run() == nil {
				return true
			}
		}
		return false
	}
	if fromUpstream() {
		t.Error("the upstream reaches six, whose groups let it not in")
	}
	c.applyYAML(onNetworksGroupsYAML("six", "default", "open6"))
	eventually(t, 30*time.Second, "the upstream reaching six once open6 lets it in", func() (bool, string) {
		return fromUpstream(), "no answer"
	})
	if n := mustRun(t, "ip", "-n", upstream6NS, "-6", "neigh", "show", addr); !strings.Contains(n, macOfLink(t, upstream6Link)) {
		t.Errorf("the upstream has six's address at %q, not at the host's uplink: proxy NDP did not answer for it", n)
	}
}

// vmIPv6UserData waits for the guest's own DHCPv6 address, then shows it
// and the default route on the serial console.
const vmIPv6UserData = `#cloud-config
runcmd:
- - sh
  - -c
  - |
    for i in $(seq 1 60); do ip -6 -o addr show scope global | grep -q /128 && break; sleep 1; done
    {
      ip -6 -o addr show scope global | sed 's/^/hangar-v6 addr: /'
      ip -6 route show default | sed 's/^/hangar-v6 route: /'
      journalctl -b -u systemd-networkd --no-pager 2>&1 | grep -i -E 'dhcp6|dhcpv6|ndisc' | tail -20 | sed 's/^/hangar-v6 networkd: /'
      echo "hangar-v6 done"
    } > /dev/ttyS0
`

// TestAVMLeasesItsIPv6: an Ubuntu cloud image, unchanged, configures its
// public /128 by router advertisement and DHCPv6, as its networkd does by
// default, takes its default route from the advertisement, and is reached
// at it from the upstream once a group lets it in.
func TestAVMLeasesItsIPv6(t *testing.T) {
	if installed {
		t.Skip("the installed agent's uplink is the host's own")
	}
	srv := diskimgtest.New(t)
	url, _ := srv.Serve(t, diskimgtest.UbuntuName, diskimgtest.Ubuntu(t))
	fw, certs := diskimgtest.Firmware(t), srv.CertFile(t)
	c := startClusterWith(t, func(c *cluster) {
		upstream6(t)
		c.uplink = upstream6Link
		c.nodeArgs = []string{"--vm-firmware", fw, "--public-ipv6-prefix", hostPrefix6}
		c.nodeEnv = []string{"SSL_CERT_FILE=" + certs}
	})
	c.applyYAML(fmt.Sprintf(`apiVersion: storage.hangarlab.cloud/v1alpha1
kind: Image
metadata: {name: noble}
spec: {url: %q, sha256: %s, os: ubuntu, version: "24.04"}
---
apiVersion: network.hangarlab.cloud/v1alpha1
kind: SecurityGroup
metadata: {name: open6}
spec:
  network: default
  ingress:
  - {protocol: SECURITY_GROUP_PROTOCOL_ICMP, from: {cidr: "::/0"}}
`, url, diskimgtest.UbuntuSHA256))
	ud, _ := json.Marshal(vmIPv6UserData)
	c.applyYAML(fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: sixvm}
spec:
  rootDisk: {fromImage: {image: {name: noble}}}
  resources: {cpu: 2, memory: 1Gi}
  interfaces: [{network: default, securityGroups: [default, open6]}]
  userData: %s
`, ud))
	c.cli("wait", "vm", "sixvm", "--for=condition=Ready", "--timeout=10m")
	var vm struct {
		Status struct {
			Interfaces []struct{ IPv6 string }
		}
	}
	if err := json.Unmarshal([]byte(c.cli("get", "vm", "sixvm", "-o", "json")), &vm); err != nil {
		t.Fatal(err)
	}
	if len(vm.Status.Interfaces) != 1 {
		t.Fatalf("VM sixvm's interfaces: %+v", vm.Status.Interfaces)
	}
	addr, _, _ := strings.Cut(vm.Status.Interfaces[0].IPv6, "/")
	if !strings.HasPrefix(addr, "2001:db8:0:1:") {
		t.Fatalf("VM sixvm has IPv6 %q, not of %s", vm.Status.Interfaces[0].IPv6, hostPrefix6)
	}
	var out string
	eventually(t, 12*time.Minute, "the guest's report of its IPv6 on the serial console", func() (bool, string) {
		out = c.cli("logs", "vm", "sixvm")
		lines := strings.Split(out, "\n")
		return strings.Contains(out, "hangar-v6 done"), strings.Join(lines[max(0, len(lines)-15):], "\n")
	})
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "hangar-v6") {
			t.Logf("console: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, addr+"/128") {
		t.Errorf("the guest did not configure its leased %s/128", addr)
	}
	if !strings.Contains(out, "hangar-v6 route: default via fe80::1 dev ") {
		t.Error("the guest's default route is not the router advertisement's, via fe80::1")
	}
	eventually(t, 30*time.Second, "the upstream reaching sixvm", func() (bool, string) {
		err := exec.Command("ip", "netns", "exec", upstream6NS, "ping", "-6", "-c", "1", "-W", "2", addr).Run()
		return err == nil, fmt.Sprint(err)
	})
	c.cli("delete", "vm", "sixvm")
	c.cli("wait", "vm", "sixvm", "--for=delete", "--timeout=3m")
}

func macOfLink(t *testing.T, link string) string {
	t.Helper()
	b, err := os.ReadFile("/sys/class/net/" + link + "/address")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}
