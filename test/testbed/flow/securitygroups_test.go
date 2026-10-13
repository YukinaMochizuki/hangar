//go:build testbed

package flow

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSecurityGroups: groups applied through the API hold on running
// guests: a sandbox locked to one peer reaches it and nothing else, not
// even the internet, but still resolves names; a guest in a group of its
// own takes in only what the group lets in; and a rule changing applies to
// guests already running.
func TestSecurityGroups(t *testing.T) {
	c := startCluster(t)
	c.applyYAML(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: SecurityGroup
metadata: {name: api}
spec:
  network: default
  ingress:
  - {protocol: SECURITY_GROUP_PROTOCOL_ICMP, from: {group: sandbox}}
---
apiVersion: network.hangarlab.cloud/v1alpha1
kind: SecurityGroup
metadata: {name: sandbox}
spec:
  network: default
  egress:
  - {protocol: SECURITY_GROUP_PROTOCOL_ICMP, to: {group: api}}
`)
	c.applyYAML(onNetworksGroupsYAML("api", "api"))
	c.applyYAML(onNetworksGroupsYAML("sandbox", "sandbox"))
	c.applyYAML(idlerYAML("plain"))
	for _, n := range []string{"api", "sandbox", "plain"} {
		c.waitReady(n, 3*time.Minute)
	}
	addr := func(name string) string {
		a, _, _ := strings.Cut(c.machine(name).GetSpec().GetInterfaces()[0].GetIpv4(), "/")
		return a
	}
	pings := func(from, to string) (bool, string) {
		out, errOut, code := execCLI(t, nil, "ct", from, "--", "ping", "-c", "1", "-W", "2", to)
		return code == 0, fmt.Sprintf("exit %d: %s%s", code, out, errOut)
	}
	eventually(t, 30*time.Second, "sandbox reaching api", func() (bool, string) { return pings("sandbox", addr("api")) })
	for _, c := range []struct{ from, to, why string }{
		{"sandbox", "plain", "sandbox sends only to api"},
		{"plain", "api", "api lets in sandbox alone"},
		{"api", "sandbox", "sandbox lets nothing in"},
	} {
		if ok, last := pings(c.from, addr(c.to)); ok {
			t.Errorf("%s reached %s: %s (%s)", c.from, c.to, c.why, last)
		}
	}
	out, _, code := execCLI(t, nil, "ct", "sandbox", "--", "wget", "-q", "-T", "5", "-O-", "http://cloudflare.com/cdn-cgi/trace")
	if code == 0 {
		t.Errorf("sandbox reached the internet: %s", out)
	}
	out, _, _ = execCLI(t, nil, "ct", "sandbox", "--", "nslookup", "api."+project+".hangar.internal")
	if !strings.Contains(out, addr("api")) {
		t.Errorf("sandbox cannot resolve, though the network's resolver is always reachable:\n%s", out)
	}

	// The rule letting plain's pings in arrives on a running api.
	c.applyYAML(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: SecurityGroup
metadata: {name: api}
spec:
  network: default
  ingress:
  - {protocol: SECURITY_GROUP_PROTOCOL_ICMP, from: {group: sandbox}}
  - {protocol: SECURITY_GROUP_PROTOCOL_ICMP, from: {group: default}}
`)
	eventually(t, 30*time.Second, "plain reaching api once its group lets default in", func() (bool, string) { return pings("plain", addr("api")) })
}

// onNetworksGroupsYAML is an idling busybox container on the default
// network, in the groups given.
func onNetworksGroupsYAML(name string, groups ...string) string {
	return idlerYAML(name) + "  interfaces: [{network: default, securityGroups: [" + strings.Join(groups, ", ") + "]}]\n"
}
