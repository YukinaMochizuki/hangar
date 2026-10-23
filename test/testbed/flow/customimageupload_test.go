//go:build testbed

package flow

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// uploadedFrom is the first count a run of hangar image upload printed:
// where it took the upload up from.
var uploadedFrom = regexp.MustCompile(`uploading [a-z0-9-]+: ([0-9.]+ [KMG]?i?B) of`)

// TestACustomImageUploadedFromAQcow2: the CLI uploads the cached Ubuntu
// qcow2, is killed part-way, and run again goes on from where the node
// is; the real converter VM converts it, and a VM boots from the image,
// finishes cloud-init and takes hangar ssh. That VM's disk is captured as
// a new CustomImage, which becomes Ready. Raw files ending in a VHD footer
// and starting with VHDX's signature are attached as raw, whatever the
// converter then makes of them. A crafted qcow2 naming a data
// file, and one holding a disk over the bound, are each refused, leaving
// no base (minutes, as the cloud image tests).
func TestACustomImageUploadedFromAQcow2(t *testing.T) {
	if installed {
		t.Skip("the installed agent's converter is the release's, and its bounds the host's")
	}
	if _, err := os.Stat(vmtest.Artifacts().ConverterInitramfs); err != nil {
		t.Fatalf("%v (make guest-converter builds it)", err)
	}
	fw := diskimgtest.Firmware(t)
	c := startClusterWith(t, func(c *cluster) {
		key := filepath.Join(apiDir, "ssh-ca")
		writeCAKey(t, key)
		c.apiArgs = []string{"--ssh-ca-key", key}
		c.nodeArgs = []string{"--vm-firmware", fw, "--max-image-disk-size", "20Gi"}
	})
	defer func() {
		for _, vm := range []string{"uploaded-vm"} {
			_, _ = c.tryCLI(nil, "delete", "vm", vm)
		}
		for _, img := range []string{"noble-up", "noble-again", "datafile", "huge", "vhdfooter", "vhdx"} {
			_, _ = c.tryCLI(nil, "delete", "cimg", img)
		}
	}()
	image := func(name string) *storagev1alpha1.CustomImage {
		t.Helper()
		ci, _ := get(c, "cimg", name, &storagev1alpha1.CustomImage{})
		return ci
	}
	baseOf := func(ci *storagev1alpha1.CustomImage) string {
		return dataset + "/vmbases/customimage-" + ci.GetMetadata().GetUid()
	}
	exists := func(ds string) bool { return exec.Command("zfs", "list", "-H", ds).Run() == nil }

	// The upload, killed part-way.
	qcow := diskimgtest.Ubuntu(t)
	fi, err := os.Stat(qcow)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("uploading %s, %d bytes", qcow, fi.Size())
	upload := exec.Command(vmtest.Artifacts().CLI, "image", "upload", qcow, "--name", "noble-up",
		"--os", "ubuntu", "--version", "24.04", "--default-user", "ubuntu")
	upload.Env = append(os.Environ(), "HANGAR_CONFIG="+filepath.Join(workDir, "cli.yaml"))
	var firstErr bytes.Buffer
	upload.Stderr = &firstErr
	if err := upload.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Minute, "a chunk counted", func() (bool, string) {
		st := image("noble-up").GetStatus()
		return st.GetReceivedBytes() > 0, fmt.Sprint(st)
	})
	_ = upload.Process.Signal(syscall.SIGKILL)
	_ = upload.Wait()
	held := image("noble-up").GetStatus().GetReceivedBytes()
	if held >= fi.Size() {
		t.Fatalf("the upload was whole, %d bytes, before it was killed", held)
	}
	t.Logf("killed the upload with %d of %d bytes in", held, fi.Size())
	start := time.Now()
	r, err := c.tryCLIStderr("image", "upload", qcow, "--name", "noble-up", "--os", "ubuntu", "--version", "24.04", "--default-user", "ubuntu")
	if err != nil {
		t.Fatalf("running the upload again: %v", err)
	}
	m := uploadedFrom.FindStringSubmatch(r)
	if m == nil || strings.HasPrefix(m[1], "0 B") {
		t.Fatalf("the upload run again went on from %v, want from where it was (%d bytes): %q", m, held, r)
	}
	t.Logf("the rest uploaded in %v, from %s; the first run said: %q", time.Since(start).Round(time.Second), m[1], lastLine(firstErr.String()))
	start = time.Now()
	if _, err := c.tryCLI(nil, "wait", "cimg", "noble-up", "--for=condition=Ready", "--timeout=15m"); err != nil {
		out, _ := c.tryCLI(nil, "get", "cimg", "noble-up", "-o", "yaml")
		t.Fatalf("the image never Ready: %v\n%s", err, out)
	}
	ci := image("noble-up")
	st := ci.GetStatus()
	t.Logf("converted in %v: %v", time.Since(start).Round(time.Second), st)
	if !conditions.IsTrue(st.GetConditions(), computeapi.CustomImageUploaded) || st.GetSha256() != diskimgtest.UbuntuSHA256 || st.GetReceivedBytes() != fi.Size() {
		t.Errorf("status %v; want Uploaded with the file's sha256 %s", st, diskimgtest.UbuntuSHA256)
	}
	if st.GetDiskSize() != "4Gi" {
		t.Errorf("disk_size %q, want the cloud image's 3.5 GiB disk in whole Gi", st.GetDiskSize())
	}
	if got := zfsProps(baseOf(ci), "readonly,hangar:customimage,origin"); got != "on "+ci.GetMetadata().GetUid()+" -" {
		t.Errorf("the base: readonly, hangar:customimage, origin %q; want a read-only base of no origin", got)
	}
	if out, err := exec.Command("zfs", "get", "-H", "-o", "value,source", "hangar:customimage", baseOf(ci)+"@base").CombinedOutput(); err != nil ||
		strings.Join(strings.Fields(string(out)), " ") != ci.GetMetadata().GetUid()+" local" {
		t.Errorf("the base's snapshot: hangar:customimage %q %v; want it set locally, finished as a captured base", out, err)
	}
	if left, _ := os.ReadDir(filepath.Join(imageDir, "uploads")); len(left) != 0 {
		t.Errorf("the staged file is still there: %v", left)
	}
	if out, _ := exec.Command("systemctl", "list-units", "--all", "--no-legend", "hangar-vm-convert-*").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("a converter VM's unit is left: %s", out)
	}

	// A VM of it: cloud-init, and hangar ssh.
	c.applyYAML(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: uploaded-vm}
spec:
  rootDisk: {fromImage: {image: {name: noble-up, project: ` + ci.GetMetadata().GetProject() + `}, size: 10Gi}}
  resources: {cpu: 2, memory: 2Gi}
`)
	start = time.Now()
	if _, err := c.tryCLI(nil, "wait", "vm", "uploaded-vm", "--for=condition=Ready", "--timeout=15m"); err != nil {
		t.Fatalf("the VM never Ready: %v", err)
	}
	ssh := func(cmd string) (string, error) { return c.tryCLI(nil, "ssh", "vm", "uploaded-vm", "--", cmd) }
	eventually(t, 15*time.Minute, "hangar ssh into the VM as ubuntu", func() (bool, string) {
		out, err := ssh("id -un")
		return err == nil && out == "ubuntu\n", fmt.Sprintf("%q %v", out, err)
	})
	if out, err := ssh("cloud-init status --wait"); err != nil || !strings.Contains(out, "done") {
		t.Errorf("cloud-init: %q %v", out, err)
	}
	if out, err := ssh("hostname"); err != nil || strings.TrimSpace(out) != "uploaded-vm" {
		t.Errorf("hostname %q %v", out, err)
	}
	t.Logf("the VM of the uploaded image Ready and logged in %v after its start", time.Since(start).Round(time.Second))

	// Its disk captured again, as any disk of a VM.
	c.cli("stop", "vm", "uploaded-vm")
	eventually(t, 5*time.Minute, "the VM stopped", func() (bool, string) {
		v, _ := get(c, "vm", "uploaded-vm", &computev1alpha1.VirtualMachine{})
		r := conditions.Find(v.GetStatus().GetConditions(), "Ready")
		return r.GetReason() == computeapi.ReasonStopped && r.GetObservedGeneration() == v.GetMetadata().GetGeneration(), fmt.Sprintf("%v", r)
	})
	c.applyYAML(`apiVersion: storage.hangarlab.cloud/v1alpha1
kind: CustomImage
metadata: {name: noble-again}
spec:
  source: {disk: {name: uploaded-vm-root}}
  defaultUser: ubuntu
`)
	if _, err := c.tryCLI(nil, "wait", "cimg", "noble-again", "--for=condition=Ready", "--timeout=10m"); err != nil {
		out, _ := c.tryCLI(nil, "get", "cimg", "noble-again", "-o", "yaml")
		t.Fatalf("the capture of the uploaded image's VM never Ready: %v\n%s", err, out)
	}
	t.Logf("captured again: %v", image("noble-again").GetStatus())

	// Raw files whose ends look like another format's, which Cloud
	// Hypervisor would take for one were it given them: attached as raw all
	// the same, the staging's zeros at both ends, and judged by qemu-img in
	// the converter.
	dir := t.TempDir()
	raw := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name+".raw")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	vhdFooter := append(bytes.Repeat([]byte("hangar\n"), 1<<17), []byte("conectix")...)
	vhdFooter = append(vhdFooter, make([]byte, 504)...)
	if out, err := c.tryCLIStderr("image", "upload", raw("vhdfooter", vhdFooter), "--name", "vhdfooter"); err != nil {
		t.Fatalf("uploading a raw file ending in a VHD footer: %v: %s", err, out)
	}
	if _, err := c.tryCLI(nil, "wait", "cimg", "vhdfooter", "--for=condition=Ready", "--timeout=10m"); err != nil {
		t.Errorf("a raw file ending in a VHD footer never Ready: %v: %v", err, image("vhdfooter").GetStatus())
	} else {
		t.Logf("a raw file ending in a VHD footer converted as raw: %v", image("vhdfooter").GetStatus())
	}
	vhdx := append([]byte("vhdxfile"), bytes.Repeat([]byte{9}, 1<<20)...)
	if out, err := c.tryCLIStderr("image", "upload", raw("vhdx", vhdx), "--name", "vhdx"); err != nil {
		t.Fatalf("uploading a raw file starting with VHDX's signature: %v: %s", err, out)
	}
	eventually(t, 10*time.Minute, "the VHDX-looking file judged", func() (bool, string) {
		ci := image("vhdx")
		return computeapi.CustomImageFailed(ci) || computeapi.CustomImageReadyNow(ci), fmt.Sprint(ci.GetStatus())
	})
	vr := conditions.Find(image("vhdx").GetStatus().GetConditions(), computeapi.CustomImageReady)
	t.Logf("a raw file starting with VHDX's signature: Ready %s %s: %s", vr.GetStatus(), vr.GetReason(), vr.GetMessage())
	if strings.Contains(vr.GetMessage(), "DiskImageTypeMismatch") || strings.Contains(vr.GetMessage(), "stopped before its agent answered") ||
		(vr.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE && vr.GetReason() != computeapi.ReasonInvalidImage) {
		t.Errorf("a raw file starting with VHDX's signature: %s %s; want it converted, or refused by qemu-img in the converter", vr.GetReason(), vr.GetMessage())
	}

	// Crafted files, refused, no base left.
	crafted := func(name, size string, opts ...string) string {
		t.Helper()
		path := filepath.Join(dir, name+".qcow2")
		if out, err := exec.Command("qemu-img", append(append([]string{"create", "-q", "-f", "qcow2"}, opts...), path, size)...).CombinedOutput(); err != nil {
			t.Fatalf("qemu-img create: %v: %s", err, out)
		}
		return path
	}
	for _, tt := range []struct {
		name, file, reason string
	}{
		{"datafile", crafted("datafile", "64M", "-o", "data_file="+filepath.Join(dir, "data.raw")), computeapi.ReasonInvalidImage},
		{"huge", crafted("huge", "30G"), computeapi.ReasonImageTooLarge},
	} {
		out, err := c.tryCLIStderr("image", "upload", tt.file, "--name", tt.name)
		if err != nil {
			t.Fatalf("uploading %s: %v: %s", tt.name, err, out)
		}
		eventually(t, 10*time.Minute, tt.name+" refused", func() (bool, string) {
			return computeapi.CustomImageFailed(image(tt.name)), fmt.Sprint(image(tt.name).GetStatus())
		})
		ci := image(tt.name)
		ready := conditions.Find(ci.GetStatus().GetConditions(), computeapi.CustomImageReady)
		if ready.GetReason() != tt.reason {
			t.Errorf("%s: Ready %s (%s), want %s", tt.name, ready.GetReason(), ready.GetMessage(), tt.reason)
		}
		t.Logf("%s refused: %s: %s", tt.name, ready.GetReason(), ready.GetMessage())
		if exists(baseOf(ci)) {
			t.Errorf("%s left its base %s", tt.name, baseOf(ci))
		}
	}
	if left, _ := os.ReadDir(filepath.Join(imageDir, "uploads")); len(left) != 0 {
		t.Errorf("staged files left: %v", left)
	}

	c.cli("delete", "vm", "uploaded-vm")
	c.cli("wait", "vm", "uploaded-vm", "--for=delete", "--timeout=3m")
	for _, img := range []string{"noble-up", "noble-again", "datafile", "huge", "vhdfooter", "vhdx"} {
		c.cli("delete", "cimg", img)
	}
	eventually(t, 5*time.Minute, "the images' bases gone", func() (bool, string) {
		return !exists(baseOf(ci)), ""
	})
}

// lastLine is the last of what a progress line wrote, each update after a
// carriage return.
func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// zfsProps is a dataset's properties' values, space-separated, or
// "missing: ..." for none.
func zfsProps(ds, props string) string {
	out, err := exec.Command("zfs", "get", "-H", "-o", "value", props, ds).CombinedOutput()
	if err != nil {
		return "missing: " + strings.TrimSpace(string(out))
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// tryCLIStderr runs the CLI, as tryCLI does, and returns what it wrote to
// stderr and stdout, together.
func (c *cluster) tryCLIStderr(args ...string) (string, error) {
	cmd := exec.Command(vmtest.Artifacts().CLI, args...)
	cmd.Env = append(os.Environ(), "HANGAR_CONFIG="+filepath.Join(workDir, "cli.yaml"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}
