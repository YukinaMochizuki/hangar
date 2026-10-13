//go:build testbed

package flow

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// idlerYAML is a busybox container named name that idles.
func idlerYAML(name string) string {
	return `apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata: {name: ` + name + `}
spec:
  image: ` + busyboxRef + `
  command: ["/bin/sh", "-c", ` + fmt.Sprintf("%q", idleScript) + `]
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi}
`
}

// TestContainersResolveEachOther: a container's resolver is its network's,
// at its gateway, which knows the containers of the network by name and
// forwards the rest.
func TestContainersResolveEachOther(t *testing.T) {
	c := startCluster(t)
	c.applyYAML(idlerYAML("alpha"))
	c.applyYAML(idlerYAML("beta"))
	c.waitReady("alpha", 3*time.Minute)
	c.waitReady("beta", 3*time.Minute)
	beta, _, _ := strings.Cut(c.machine("beta").GetSpec().GetInterfaces()[0].GetIpv4(), "/")
	zone := project + ".hangar.internal"

	out, errOut, code := execCLI(t, nil, "ct", "alpha", "--", "cat", "/etc/resolv.conf")
	if code != 0 || !strings.Contains(out, "nameserver 10.0.0.1") || !strings.Contains(out, "search "+zone) {
		t.Errorf("resolv.conf (exit %d):\n%s%s", code, out, errOut)
	}
	// busybox's nslookup asks the first nameserver of resolv.conf.
	for _, name := range []string{"beta." + zone, "beta"} {
		var last string
		eventually(t, 30*time.Second, "alpha resolving "+name, func() (bool, string) {
			out, errOut, code := execCLI(t, nil, "ct", "alpha", "--", "sh", "-c", "ping -c 1 -W 2 "+name+" 2>&1 | head -1")
			last = fmt.Sprintf("exit %d: %s%s", code, out, errOut)
			return strings.Contains(out, "("+beta+")"), last
		})
	}
	out, _, _ = execCLI(t, nil, "ct", "alpha", "--", "nslookup", "nobody."+zone)
	if !strings.Contains(out, "NXDOMAIN") {
		t.Errorf("an unknown internal name:\n%s", out)
	}
	out, _, code = execCLI(t, nil, "ct", "alpha", "--", "nslookup", "cloudflare.com")
	if code != 0 || !strings.Contains(out, "Address") {
		t.Errorf("an external name (exit %d):\n%s", code, out)
	}
}
