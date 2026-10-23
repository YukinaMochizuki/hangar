//go:build testbed

package flow

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
)

// customImageMarker is the file the source VM writes, which every VM of
// its image must hold.
const customImageMarker = "/var/lib/hangar-golden"

// TestACustomImageOfAnUbuntuVM: an Ubuntu VM from the catalogue writes a
// marker, is cleaned for capture as docs/runbook/vm-images.md says, and
// stops; its root disk is captured as a CustomImage, a copy on the pool
// keyed by the image. A VM of the image boots, holds the marker, has its
// own hostname, instance id and machine id, and takes hangar ssh and
// hangar exec; it restarts fine with the source VM and its disk gone. The
// image deleted, its base stays while that VM's disk is cloned from it,
// and goes with the VM (minutes, as the cloud image tests).
func TestACustomImageOfAnUbuntuVM(t *testing.T) {
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
	start := time.Now()
	since := func() string { return time.Since(start).Round(time.Second).String() }
	defer func() {
		for _, vm := range []string{"golden-src", "golden-copy"} {
			_, _ = c.tryCLI(nil, "delete", "vm", vm)
		}
		_, _ = c.tryCLI(nil, "delete", "cimg", "golden")
	}()

	ssh := func(vm, cmd string) (string, error) { return c.tryCLI(nil, "ssh", "vm", vm, "--", cmd) }
	run := func(vm, cmd string) string {
		t.Helper()
		out, err := ssh(vm, cmd)
		if err != nil {
			t.Fatalf("%s on %s: %v: %q", cmd, vm, err, out)
		}
		return strings.TrimSpace(out)
	}
	ready := func(vm string) {
		t.Helper()
		if _, err := c.tryCLI(nil, "wait", "vm", vm, "--for=condition=Ready", "--timeout=15m"); err != nil {
			t.Fatalf("VM %s never Ready: %v", vm, err)
		}
		eventually(t, 15*time.Minute, "hangar ssh into "+vm+" as ubuntu", func() (bool, string) {
			out, err := ssh(vm, "id -un")
			return err == nil && out == "ubuntu\n", fmt.Sprintf("%q %v", out, err)
		})
		run(vm, "cloud-init status --wait")
		t.Logf("%s Ready and logged in %s after the start", vm, since())
	}
	stopped := func(vm string) {
		t.Helper()
		eventually(t, 5*time.Minute, vm+" stopped", func() (bool, string) {
			v, _ := get(c, "vm", vm, &computev1alpha1.VirtualMachine{})
			r := conditions.Find(v.GetStatus().GetConditions(), "Ready")
			return r.GetReason() == computeapi.ReasonStopped && r.GetObservedGeneration() == v.GetMetadata().GetGeneration(), fmt.Sprintf("%v", r)
		})
	}
	identity := func(vm string) (host, instance, machineID string) {
		t.Helper()
		return run(vm, "hostname"), run(vm, "cat /run/cloud-init/.instance-id"), run(vm, "cat /etc/machine-id")
	}

	// The source: set up, cleaned for capture, stopped.
	c.applyYAML(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: golden-src}
spec:
  rootDisk: {fromImage: {image: {name: noble}, size: 10Gi}}
  resources: {cpu: 2, memory: 2Gi}
`)
	ready("golden-src")
	marker := fmt.Sprintf("golden %d", time.Now().UnixNano())
	run("golden-src", fmt.Sprintf("echo %q | sudo tee %s >/dev/null && sync", marker, customImageMarker))
	srcHost, srcInstance, srcMachineID := identity("golden-src")
	t.Logf("source: hostname %s, instance %s, machine id %s", srcHost, srcInstance, srcMachineID)
	run("golden-src", "sudo cloud-init clean --machine-id --logs && sync")
	c.cli("stop", "vm", "golden-src")
	stopped("golden-src")

	// The capture.
	c.applyYAML(`apiVersion: storage.hangarlab.cloud/v1alpha1
kind: CustomImage
metadata: {name: golden}
spec:
  source: {disk: {name: golden-src-root}}
  os: ubuntu
  version: "24.04"
  defaultUser: ubuntu
`)
	captured := time.Now()
	if _, err := c.tryCLI(nil, "wait", "cimg", "golden", "--for=condition=Ready", "--timeout=10m"); err != nil {
		out, _ := c.tryCLI(nil, "get", "cimg", "golden", "-o", "yaml")
		t.Fatalf("the image never Ready: %v\n%s", err, out)
	}
	ci, _ := get(c, "cimg", "golden", &storagev1alpha1.CustomImage{})
	t.Logf("captured in %v: %v", time.Since(captured).Round(time.Second), ci.GetStatus())
	if st := ci.GetStatus(); st.GetDiskSize() != "10Gi" || st.GetUsedSize() == "" || st.GetUsedSize() == "10Gi" {
		t.Errorf("status %v: want disk_size 10Gi and used_size what the sparse copy holds", st)
	}
	base := dataset + "/vmbases/customimage-" + ci.GetMetadata().GetUid()
	zfsGet := func(ds, props string) string {
		out, err := exec.Command("zfs", "get", "-H", "-o", "value", props, ds).CombinedOutput()
		if err != nil {
			return "missing: " + strings.TrimSpace(string(out))
		}
		return strings.Join(strings.Fields(string(out)), " ")
	}
	if got := zfsGet(base, "readonly,hangar:customimage,origin"); got != "on "+ci.GetMetadata().GetUid()+" -" {
		t.Errorf("the base %s: readonly, hangar:customimage, origin %q; want a read-only copy of no origin", base, got)
	}
	if got := zfsGet(base+"@base", "hangar:customimage"); got != ci.GetMetadata().GetUid() {
		t.Errorf("the base's snapshot: hangar:customimage %q", got)
	}
	if out, _ := exec.Command("zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-r", dataset+"/disks").CombinedOutput(); strings.Contains(string(out), "@customimage-") {
		t.Errorf("the snapshot of the source is still there:\n%s", out)
	}

	// A VM of the image: the marker, an identity of its own, ssh and exec.
	// The image is named in the project the CLI works in, the image's own.
	c.applyYAML(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: golden-copy}
spec:
  rootDisk: {fromImage: {image: {name: golden, project: ` + ci.GetMetadata().GetProject() + `}, size: 12Gi}}
  resources: {cpu: 2, memory: 2Gi}
`)
	start = time.Now()
	ready("golden-copy")
	if got := run("golden-copy", "cat "+customImageMarker); got != marker {
		t.Errorf("the copy holds %q, want the source's %q", got, marker)
	}
	copyVM, _ := get(c, "vm", "golden-copy", &computev1alpha1.VirtualMachine{})
	host, instance, machineID := identity("golden-copy")
	t.Logf("copy: hostname %s, instance %s, machine id %s", host, instance, machineID)
	if host != "golden-copy" || instance != copyVM.GetMetadata().GetUid() || instance == srcInstance || machineID == srcMachineID || machineID == "" {
		t.Errorf("the copy's hostname %q, instance %q, machine id %q; want its own, not the source's %q, %q, %q",
			host, instance, machineID, srcHost, srcInstance, srcMachineID)
	}
	out, errOut, code := execCLI(t, nil, "vm", "golden-copy", "--", "sh", "-c", "cat "+customImageMarker+"; exit 3")
	if strings.TrimSpace(out) != marker || errOut != "" || code != 3 {
		t.Errorf("hangar exec: stdout %q, stderr %q, exit %d", out, errOut, code)
	}
	root, _ := get(c, "disk", "golden-copy-root", &storagev1alpha1.Disk{})
	if got := zfsGet(dataset+"/disks/"+root.GetMetadata().GetUid(), "origin,hangar:customimage"); got != base+"@base "+ci.GetMetadata().GetUid() {
		t.Errorf("the copy's volume: origin, hangar:customimage %q; want a clone of %s@base", got, base)
	}

	// The source goes; the copy restarts.
	c.cli("delete", "vm", "golden-src")
	c.cli("wait", "vm", "golden-src", "--for=delete", "--timeout=3m")
	eventually(t, 3*time.Minute, "the source's disk gone", func() (bool, string) {
		_, ok := get(c, "disk", "golden-src-root", &storagev1alpha1.Disk{})
		return !ok, ""
	})
	c.cli("stop", "vm", "golden-copy")
	stopped("golden-copy")
	c.cli("start", "vm", "golden-copy")
	start = time.Now()
	ready("golden-copy")
	if got := run("golden-copy", "cat "+customImageMarker); got != marker {
		t.Errorf("after the restart the copy holds %q", got)
	}

	// The image goes; its base stays while the copy's disk is cloned from
	// it, over more than two of the agent's minute-long sweeps, as nothing
	// says when one has passed; then goes with it.
	c.cli("delete", "cimg", "golden")
	for deadline := time.Now().Add(150 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Second) {
		if got := zfsGet(base+"@base", "hangar:customimage"); got != ci.GetMetadata().GetUid() {
			t.Fatalf("the base went while a disk is cloned from it: %q", got)
		}
	}
	c.cli("delete", "vm", "golden-copy")
	c.cli("wait", "vm", "golden-copy", "--for=delete", "--timeout=3m")
	eventually(t, 5*time.Minute, "the image's base gone", func() (bool, string) {
		got := zfsGet(base, "name")
		return strings.HasPrefix(got, "missing"), got
	})
	c.cli("delete", "image", "noble")
}
