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

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
)

// shareYAML is a FileShare on networks.
func shareYAML(name, size string, networks ...string) string {
	return fmt.Sprintf(`apiVersion: storage.hangarlab.cloud/v1alpha1
kind: FileShare
metadata: {name: %s}
spec: {size: %s, networks: [%s]}
`, name, size, strings.Join(networks, ", "))
}

// sharingYAML is a container that runs script, mounting share at /data.
func sharingYAML(name, share, script string) string {
	return fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata: {name: %s}
spec:
  image: %s
  command: ["/bin/sh", "-c", %q]
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi}
  volumes:
  - {source: %s, sourceKind: FileShare, target: /data}
`, name, busyboxRef, script, share)
}

// writerScript appends a line to /data/<name>.log every 200 ms, and
// idles.
func writerScript(name string) string {
	return `trap 'exit 0' TERM
i=0
while :; do i=$((i+1)); echo $i >> /data/` + name + `.log; sleep 0.2 & wait $!; done`
}

func (c *cluster) share(name string) *storagev1alpha1.FileShare {
	c.t.Helper()
	fs, ok := get(c, "fileshare", name, &storagev1alpha1.FileShare{})
	if !ok {
		c.t.Fatalf("no file share %s", name)
	}
	return fs
}

func (c *cluster) shareAddr(name, network string) string {
	c.t.Helper()
	var at string
	eventually(c.t, 3*time.Minute, "file share "+name+"'s address on "+network, func() (bool, string) {
		fs := c.share(name)
		for _, a := range fs.GetStatus().GetAddresses() {
			if a.GetNetwork() == network {
				at = a.GetAddress()
			}
		}
		return at != "", fs.GetStatus().String()
	})
	return at
}

func (c *cluster) serverMachine(name string) *computev1alpha1.Machine {
	c.t.Helper()
	m, ok := get(c, "machine", "fs-"+name, &computev1alpha1.Machine{})
	if !ok {
		c.t.Fatalf("file share %s has no server", name)
	}
	return m
}

// waitSharePhase waits for the share's phase.
func (c *cluster) waitSharePhase(name string, phase storagev1alpha1.FileSharePhase) {
	c.t.Helper()
	eventually(c.t, 3*time.Minute, fmt.Sprintf("file share %s %v", name, phase), func() (bool, string) {
		fs := c.share(name)
		return fs.GetStatus().GetPhase() == phase, fs.GetStatus().String()
	})
}

// lines counts the lines of a file in a container.
func lines(t *testing.T, ct, path string) (int, string) {
	out, errOut, code := execCLI(t, nil, "ct", ct, "--", "wc", "-l", path)
	if code != 0 {
		return -1, fmt.Sprintf("exit %d: %s%s", code, out, errOut)
	}
	n, err := strconv.Atoi(strings.Fields(out + " x")[0])
	if err != nil {
		return -1, out
	}
	return n, out
}

// keepsWriting waits for the writer's log, read from reader, to grow by
// more than n lines past what it holds now: its mount is alive.
func keepsWriting(t *testing.T, writer, reader string, n int, within time.Duration) {
	t.Helper()
	start := -1
	eventually(t, within, fmt.Sprintf("%s's writes, read by %s, going on", writer, reader), func() (bool, string) {
		now, out := lines(t, reader, "/data/"+writer+".log")
		if start < 0 {
			start = now
			return false, out
		}
		return now > start+n, fmt.Sprintf("%d lines from %d: %s", now, start, out)
	})
}

// zvolExists says whether the dataset has a share's volume.
func zvolExists(uid string) bool {
	return exec.Command("zfs", "list", "-H", dataset+"/shares/"+uid).Run() == nil
}

// TestAFileShare: a share's two containers on one network read each
// other's writes; a container declaring nothing reaches neither the
// server's 2049 nor anything else of it, while a client does; the server
// runs on a zvol of the share, on taps of a server's. Growing the share
// grows it in the running server; adding a network to it starts its server
// again; either way the clients' mounts carry on, their writes landing;
// deleting the share, once nothing declares it, destroys its zvol.
func TestAFileShare(t *testing.T) {
	c := startCluster(t)
	c.applyYAML(`apiVersion: network.hangarlab.cloud/v1alpha1
kind: Network
metadata: {name: backend}
spec: {ipv4Cidr: 10.1.0.0/24}
`)
	c.applyYAML(shareYAML("data", "1Gi", "default"))
	c.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE)
	at := c.shareAddr("data", "default")
	fs := c.share("data")
	uid := fs.GetMetadata().GetUid()
	if !zvolExists(uid) {
		t.Fatalf("no zvol %s/shares/%s for the share", dataset, uid)
	}
	srv := c.serverMachine("data")
	for _, tp := range tapsOf(t, srv.GetMetadata().GetUid()) {
		if !strings.HasPrefix(tp.name, hostnetServerTap) {
			t.Errorf("the server's tap %s is no server's", tp.name)
		}
	}

	c.applyYAML(sharingYAML("alpha", "data", writerScript("alpha")))
	c.applyYAML(sharingYAML("beta", "data", idleScript))
	c.applyYAML(idlerYAML("gamma"))
	for _, n := range []string{"alpha", "beta", "gamma"} {
		c.waitReady(n, 3*time.Minute)
	}
	c.waitSharePhase("data", storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_IN_USE)

	// Each reads the other's writes.
	keepsWriting(t, "alpha", "beta", 5, time.Minute)
	if out, errOut, code := execCLI(t, nil, "ct", "beta", "--", "sh", "-c", "echo from-beta > /data/beta.txt"); code != 0 {
		t.Fatalf("beta writing: exit %d: %s%s", code, out, errOut)
	}
	eventually(t, 30*time.Second, "alpha reading beta's write", func() (bool, string) {
		out, errOut, code := execCLI(t, nil, "ct", "alpha", "--", "cat", "/data/beta.txt")
		return code == 0 && strings.TrimSpace(out) == "from-beta", fmt.Sprintf("exit %d: %s%s", code, out, errOut)
	})
	out, _, _ := execCLI(t, nil, "ct", "alpha", "--", "cat", "/proc/mounts")
	if !strings.Contains(out, at+":/ /data nfs4") {
		t.Errorf("alpha's mounts lack %s:/ on /data:\n%s", at, out)
	}

	// Only a client reaches the server's 2049, and nothing else of it.
	connects := func(ct, port string) (bool, string) {
		out, errOut, code := execCLI(t, nil, "ct", ct, "--", "sh", "-c", "nc -w 3 "+at+" "+port+" </dev/null")
		return code == 0, fmt.Sprintf("exit %d: %s%s", code, out, errOut)
	}
	if ok, last := connects("beta", "2049"); !ok {
		t.Errorf("a client does not reach its share's 2049: %s", last)
	}
	if ok, last := connects("gamma", "2049"); ok {
		t.Errorf("a container declaring no share reaches its 2049: %s", last)
	}
	if ok, last := connects("beta", "22"); ok {
		t.Errorf("a client reaches the server's port 22: %s", last)
	}
	if out, _, code := execCLI(t, nil, "ct", "gamma", "--", "ping", "-c", "1", "-W", "2", at); code == 0 {
		t.Errorf("a container declaring no share pings the server: %s", out)
	}

	// Grown, the zvol grows under the running server, which grows its
	// filesystem, and the mounts carry on, nothing started again.
	clients := map[string]time.Time{}
	for _, n := range []string{"alpha", "beta"} {
		clients[n] = c.machine(n).GetStatus().GetBootTime().AsTime()
	}
	booted := srv.GetStatus().GetBootTime().AsTime()
	c.applyYAML(shareYAML("data", "2Gi", "default"))
	eventually(t, time.Minute, "beta seeing the grown filesystem", func() (bool, string) {
		out, _, _ := execCLI(t, nil, "ct", "beta", "--", "df", "-k", "/data")
		f := strings.Fields(out)
		size, _ := strconv.Atoi(f[min(len(f)-1, 8)])
		return size > 1<<20+(1<<19), out
	})
	keepsWriting(t, "alpha", "beta", 5, 2*time.Minute)
	if now := c.serverMachine("data").GetStatus().GetBootTime().AsTime(); !now.Equal(booted) {
		t.Errorf("the server booted again at %v to grow its share (it booted at %v)", now, booted)
	}

	// A network added: the server starts again on both, and the mounts
	// carry on.
	booted = c.serverMachine("data").GetStatus().GetBootTime().AsTime()
	c.applyYAML(shareYAML("data", "2Gi", "default", "backend"))
	c.shareAddr("data", "backend")
	eventually(t, 3*time.Minute, "the server booted again on two networks", func() (bool, string) {
		m := c.serverMachine("data")
		return len(m.GetSpec().GetInterfaces()) == 2 && m.GetStatus().GetBootTime().AsTime().After(booted) &&
			m.GetStatus().GetPhase() == computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetStatus().String()
	})
	keepsWriting(t, "alpha", "beta", 5, 2*time.Minute)
	for n, was := range clients {
		if now := c.machine(n).GetStatus().GetBootTime().AsTime(); !now.Equal(was) {
			t.Errorf("%s was started again, at %v, the share's server changing", n, now)
		}
	}

	// Deleted once nothing declares it, its zvol goes.
	for _, n := range []string{"alpha", "beta", "gamma"} {
		c.cli("delete", "container", n)
		c.cli("wait", "container", n, "--for=delete", "--timeout=3m")
	}
	c.cli("delete", "fileshare", "data")
	c.cli("wait", "fileshare", "data", "--for=delete", "--timeout=3m")
	if zvolExists(uid) {
		t.Errorf("the deleted share's zvol %s/shares/%s is still there", dataset, uid)
	}
}

// hostnetServerTap is what a file share server's taps are named from.
const hostnetServerTap = "hnts"

// shareVMUserData installs the NFS client, mounts the share with the
// console's commands, its NFSv4 identity its instance id, reads what a
// container wrote and writes for it to read, and says so on the serial
// console.
const shareVMUserData = `#cloud-config
package_update: true
packages: [nfs-common]
runcmd:
- - sh
  - -c
  - |
    {
      mkdir -p /mnt/data
      modprobe nfs
      echo $(cat /run/cloud-init/.instance-id) > /sys/module/nfs/parameters/nfs4_unique_id
      mount -t nfs4 -o nosuid,nodev,timeo=50 ADDRESS:/ /mnt/data && echo "hangar-fs mounted"
      echo "hangar-fs read: $(cat /mnt/data/from-container)"
      echo from-vm > /mnt/data/from-vm && echo "hangar-fs wrote"
      echo "hangar-fs done"
    } > /dev/ttyS0 2>&1
`

// TestAVMMountsAShareByHand: an Ubuntu VM declaring a share mounts it with
// the console's commands, reads what a container wrote, and writes what the
// container then reads.
func TestAVMMountsAShareByHand(t *testing.T) {
	if installed {
		t.Skip("the installed agent trusts no test server's certificate")
	}
	srv := diskimgtest.New(t)
	url, _ := srv.Serve(t, diskimgtest.UbuntuName, diskimgtest.Ubuntu(t))
	fw, certs := diskimgtest.Firmware(t), srv.CertFile(t)
	c := startClusterWith(t, func(c *cluster) {
		c.nodeArgs = []string{"--vm-firmware", fw}
		c.nodeEnv = []string{"SSL_CERT_FILE=" + certs}
	})
	c.applyYAML(fmt.Sprintf(`apiVersion: storage.hangarlab.cloud/v1alpha1
kind: Image
metadata: {name: noble}
spec: {url: %q, sha256: %s, os: ubuntu, version: "24.04"}
`, url, diskimgtest.UbuntuSHA256))
	c.applyYAML(shareYAML("data", "1Gi", "default"))
	at := c.shareAddr("data", "default")
	c.applyYAML(sharingYAML("writer", "data", "echo from-container > /data/from-container; "+idleScript))
	c.waitReady("writer", 3*time.Minute)

	ud, _ := json.Marshal(strings.ReplaceAll(shareVMUserData, "ADDRESS", at))
	c.applyYAML(fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: mounter}
spec:
  rootDisk: {fromImage: {image: {name: noble}}}
  resources: {cpu: 2, memory: 1Gi}
  fileShares: [{name: data}]
  userData: %s
`, ud))
	start := time.Now()
	c.cli("wait", "vm", "mounter", "--for=condition=Ready", "--timeout=10m")
	var out string
	eventually(t, 15*time.Minute, "the VM's report on its serial console", func() (bool, string) {
		out = c.cli("logs", "vm", "mounter")
		ls := strings.Split(out, "\n")
		return strings.Contains(out, "hangar-fs done"), strings.Join(ls[max(0, len(ls)-15):], "\n")
	})
	t.Logf("the VM mounted and reported %v after the apply", time.Since(start).Round(time.Second))
	for _, want := range []string{"hangar-fs mounted", "hangar-fs read: from-container", "hangar-fs wrote"} {
		if !strings.Contains(out, want) {
			t.Errorf("the serial console lacks %q", want)
		}
	}
	eventually(t, 30*time.Second, "the container reading the VM's write", func() (bool, string) {
		o, errOut, code := execCLI(t, nil, "ct", "writer", "--", "cat", "/data/from-vm")
		return code == 0 && strings.TrimSpace(o) == "from-vm", fmt.Sprintf("exit %d: %s%s", code, o, errOut)
	})
	c.cli("delete", "vm", "mounter")
	c.cli("wait", "vm", "mounter", "--for=delete", "--timeout=3m")
}
