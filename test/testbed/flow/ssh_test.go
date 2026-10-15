//go:build testbed

package flow

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// lockedGroupYAML is the group "locked", which lets nothing in but rules.
func lockedGroupYAML(rules ...string) string {
	doc := "apiVersion: network.hangarlab.cloud/v1alpha1\nkind: SecurityGroup\nmetadata: {name: locked}\nspec:\n  network: default\n"
	if len(rules) > 0 {
		doc += "  ingress:\n"
		for _, r := range rules {
			doc += "  - " + r + "\n"
		}
	}
	return doc
}

// TestHangarSSHIntoAVM: hangar ssh logs in to an Ubuntu VM that has no
// static key, with a certificate the apiserver signs, as the image's
// default user, or -l another made by the user-data, never root; its
// security group lets nothing in, yet the platform's SSH path passes, while
// another guest of the network is still dropped at port 22. Both are shown
// by mutation: a rule letting the other guest in lets it reach sshd, and
// with the platform's rule deleted the gateway's path, hangar ssh's, is
// dropped too, until the agent writes the firewall again. A VM with a
// static key takes it from OpenSSH through hangar ssh-proxy, and the CA's
// certificate beside it.
func TestHangarSSHIntoAVM(t *testing.T) {
	if installed {
		t.Skip("the installed agent trusts no test server's certificate")
	}
	srv := diskimgtest.New(t)
	url, _ := srv.Serve(t, diskimgtest.UbuntuName, diskimgtest.Ubuntu(t))
	fw, certs := diskimgtest.Firmware(t), srv.CertFile(t)
	c := startClusterWith(t, func(c *cluster) {
		key := filepath.Join(apiDir, "ssh-ca")
		writeCAKey(t, key)
		c.apiArgs = []string{"--ssh-ca-key", key}
		c.nodeArgs = []string{"--vm-firmware", fw}
		c.nodeEnv = []string{"SSL_CERT_FILE=" + certs}
	})
	c.applyYAML(fmt.Sprintf(`apiVersion: storage.hangarlab.cloud/v1alpha1
kind: Image
metadata: {name: noble}
spec: {url: %q, sha256: %s, os: ubuntu, version: "24.04", defaultUser: ubuntu}
`, url, diskimgtest.UbuntuSHA256))
	c.applyYAML(lockedGroupYAML())
	c.applyYAML(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: sshy}
spec:
  rootDisk: {fromImage: {image: {name: noble}}}
  resources: {cpu: 2, memory: 1Gi}
  interfaces: [{network: default, securityGroups: [locked]}]
  userData: "#cloud-config\nusers: [default, {name: alice, shell: /bin/bash}]\n"
`)
	// A VM with a static key too, beside the CA.
	keyFile, pub := staticKey(t)
	c.applyYAML(fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: keyed}
spec:
  rootDisk: {fromImage: {image: {name: noble}}}
  resources: {cpu: 1, memory: 1Gi}
  sshAuthorizedKeys: [%q]
`, pub))
	c.applyYAML(idlerYAML("prober"))
	start := time.Now()
	c.cli("wait", "vm", "sshy", "--for=condition=Ready", "--timeout=10m")
	c.waitReady("prober", 3*time.Minute)

	// As with ssh, the remote command is one line, which the user's shell
	// runs.
	sshOut := func(args ...string) (string, error) {
		return c.tryCLI(nil, append([]string{"ssh"}, args...)...)
	}
	var last string
	eventually(t, 12*time.Minute, "hangar ssh logging in", func() (bool, string) {
		out, err := sshOut("vm", "sshy", "--", "echo hello from $(hostname) as $(id -un)")
		last = fmt.Sprintf("%q %v", out, err)
		return err == nil && out == "hello from sshy as ubuntu\n", last
	})
	t.Logf("hangar ssh logged in %v after the apply", time.Since(start).Round(time.Second))
	if out, err := sshOut("vm", "sshy", "--", "cat ~/.ssh/authorized_keys 2>/dev/null; true"); err != nil || strings.Contains(out, "ssh-") {
		t.Errorf("the default user has static keys: %q %v", out, err)
	}
	if out, err := sshOut("-l", "alice", "vm", "sshy", "--", "id", "-un"); err != nil || out != "alice\n" {
		t.Errorf("-l alice: %q %v", out, err)
	}
	if out, err := sshOut("-l", "root", "vm", "sshy", "--", "id", "-un"); err == nil {
		t.Errorf("logged in as root: %q", out)
	}
	// The static key logs in with OpenSSH alone, hangar ssh-proxy its
	// ProxyCommand, and so does hangar ssh, by the CA, on the same VM.
	c.cli("wait", "vm", "keyed", "--for=condition=Ready", "--timeout=10m")
	eventually(t, 5*time.Minute, "ssh with the static key", func() (bool, string) {
		cmd := exec.Command("ssh", "-i", keyFile, "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
			"-o", "ProxyCommand="+vmtest.Artifacts().CLI+" ssh-proxy vm keyed",
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "ubuntu@keyed", "echo", "by", "the", "static", "key")
		cmd.Env = append(os.Environ(), "HANGAR_CONFIG="+filepath.Join(workDir, "cli.yaml"))
		out, err := cmd.CombinedOutput()
		return err == nil && strings.HasSuffix(string(out), "by the static key\n"), fmt.Sprintf("%q %v", out, err)
	})
	// The CA's script runs as cloud-init's first boot ends, after the
	// static key is given.
	ok := false
	for deadline := time.Now().Add(2 * time.Minute); !ok && time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		out, err := sshOut("vm", "keyed", "--", "echo by the CA")
		ok = err == nil && out == "by the CA\n"
	}
	if !ok {
		logs, _ := c.tryCLI(nil, "logs", "vm", "keyed")
		lines := strings.Split(logs, "\n")
		t.Errorf("hangar ssh beside a static key failed; keyed's console:\n%s", strings.Join(lines[max(0, len(lines)-40):], "\n"))
	}
	c.cli("delete", "vm", "keyed")

	known, err := os.ReadFile(filepath.Join(workDir, "known_hosts"))
	if err != nil || !strings.Contains(string(known), " ssh-ed25519 ") {
		t.Errorf("known_hosts %q %v", known, err)
	}

	// Another guest of the network does not reach port 22 ...
	vmm, ok := get(c, "machine", "vm-sshy", &computev1alpha1.Machine{})
	if !ok {
		t.Fatal("no machine vm-sshy")
	}
	addr := strings.Split(vmm.GetSpec().GetInterfaces()[0].GetIpv4(), "/")[0]
	probe := func() (bool, string) {
		out, errOut, code := execCLI(t, nil, "ct", "prober", "--", "sh", "-c", "nc -w 3 "+addr+" 22 </dev/null | head -c 64")
		return strings.Contains(out, "SSH-2.0"), fmt.Sprintf("exit %d: %q %s", code, out, errOut)
	}
	if ok, last := probe(); ok {
		t.Errorf("another guest reached the locked VM's sshd: %s", last)
	}
	// ... until the group lets it in: the probe can tell.
	c.applyYAML(lockedGroupYAML("{protocol: SECURITY_GROUP_PROTOCOL_TCP, ports: \"22\", from: {group: default}}"))
	eventually(t, 30*time.Second, "the prober reaching sshd once the group lets it in", probe)
	c.applyYAML(lockedGroupYAML())
	eventually(t, 30*time.Second, "the prober dropped again", func() (bool, string) {
		ok, last := probe()
		return !ok, last
	})

	// The platform's path is the namespace's own, from the gateway: with
	// its rule it reaches sshd ...
	var n struct {
		Metadata struct{ UID string }
	}
	if err := json.Unmarshal([]byte(c.cli("get", "network", "default", "-o", "json")), &n); err != nil {
		t.Fatal(err)
	}
	ns := hostnet.NamespaceOf(n.Metadata.UID)
	fromGateway := func() (bool, string) {
		out, err := exec.Command("ip", "netns", "exec", ns, "timeout", "5", "bash", "-c",
			"exec 3<>/dev/tcp/"+addr+"/22 && head -c 7 <&3").CombinedOutput()
		return string(out) == "SSH-2.0", fmt.Sprintf("%q %v", out, err)
	}
	if ok, last := fromGateway(); !ok {
		t.Errorf("the gateway does not reach the locked VM's sshd: %s", last)
	}
	// ... and without it, it is dropped as any other. The agent writes a
	// firewall that drifted at once, so it is stopped meanwhile; the VM
	// runs on without it.
	c.killNode()
	listing := mustRun(t, "ip", "netns", "exec", ns, "nft", "-a", "list", "chain", "bridge", hostnet.FirewallTable, "output")
	rule := regexp.MustCompile(`platform's SSH[^\n]*# handle (\d+)`).FindStringSubmatch(listing)
	if rule == nil {
		t.Fatalf("no platform's SSH rule in the namespace's output chain:\n%s", listing)
	}
	mustRun(t, "ip", "netns", "exec", ns, "nft", "delete", "rule", "bridge", hostnet.FirewallTable, "output", "handle", rule[1])
	if ok, last := fromGateway(); ok {
		t.Errorf("the gateway reached the locked VM's sshd without the platform's rule: %s", last)
	} else {
		t.Logf("without the platform's rule, from the gateway: %s", last)
	}
	// Back, the agent writes the rule again, and hangar ssh logs in.
	c.startNode()
	eventually(t, time.Minute, "the gateway reaching sshd once the agent is back", fromGateway)
	eventually(t, time.Minute, "hangar ssh once the agent is back", func() (bool, string) {
		out, err := sshOut("vm", "sshy", "--", "echo", "back")
		return err == nil && out == "back\n", fmt.Sprintf("%q %v", out, err)
	})

	c.cli("delete", "vm", "sshy")
	c.cli("delete", "container", "prober")
	c.cli("wait", "vm", "sshy", "--for=delete", "--timeout=3m")
}

// writeCAKey writes a new SSH CA key at path, the apiserver's, as the
// runbook installs it.
func writeCAKey(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "hangar-ssh-ca")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	nobody := lookupUID(t, "nobody")
	if err := os.Chown(path, nobody, nobody); err != nil {
		t.Fatal(err)
	}
}

// staticKey makes a user key, written to a file for ssh -i, and returns the
// file and the public key's authorized_keys line.
func staticKey(t *testing.T) (string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "static")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return path, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " static"
}
