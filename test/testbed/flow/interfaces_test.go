//go:build testbed

package flow

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// onNetworksYAML is an idling busybox container on the networks given.
func onNetworksYAML(name string, networks ...string) string {
	var nics []string
	for _, n := range networks {
		nics = append(nics, "{network: "+n+"}")
	}
	return idlerYAML(name) + "  interfaces: [" + strings.Join(nics, ", ") + "]\n"
}

// TestAContainerOnTwoNetworks: a container on two networks reaches each
// network's machines by its interface there and the internet by its first;
// a container on one network neither reaches nor resolves the other's.
func TestAContainerOnTwoNetworks(t *testing.T) {
	c := startCluster(t)
	c.applyYAML(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: Network
metadata: {name: backend}
spec: {ipv4Cidr: 10.1.0.0/24}
`)
	c.applyYAML(onNetworksYAML("both", "backend", "default"))
	c.applyYAML(onNetworksYAML("peer", "backend"))
	c.applyYAML(idlerYAML("home"))
	for _, n := range []string{"both", "peer", "home"} {
		c.waitReady(n, 3*time.Minute)
	}
	addr := func(name string, i int) string {
		a, _, _ := strings.Cut(c.machine(name).GetSpec().GetInterfaces()[i].GetIpv4(), "/")
		return a
	}
	reaches := func(from, to string) (bool, string) {
		out, errOut, code := execCLI(t, nil, "ct", from, "--", "ping", "-c", "1", "-W", "2", to)
		return code == 0, fmt.Sprintf("exit %d: %s%s", code, out, errOut)
	}
	for _, to := range []string{addr("peer", 0), addr("home", 0)} {
		eventually(t, 30*time.Second, "both reaching "+to, func() (bool, string) { return reaches("both", to) })
	}
	out, _, code := execCLI(t, nil, "ct", "both", "--", "wget", "-q", "-T", "10", "-O-", "http://cloudflare.com/cdn-cgi/trace")
	if code != 0 || !strings.Contains(out, "ip=") {
		t.Errorf("both reaching the internet by its first network (exit %d): %s", code, out)
	}
	if ok, last := reaches("home", addr("peer", 0)); ok {
		t.Errorf("home, on default alone, reached peer on backend: %s", last)
	}
	out, _, _ = execCLI(t, nil, "ct", "home", "--", "nslookup", "peer."+project+".hangar.internal")
	if !strings.Contains(out, "NXDOMAIN") {
		t.Errorf("home resolving peer, on another network:\n%s", out)
	}
}
