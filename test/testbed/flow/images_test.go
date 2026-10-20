//go:build testbed

package flow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// catalogueFile is the platform's image catalogue, from this package.
const catalogueFile = "../../../deploy/catalog/images.yaml"

// The root disk each catalogue VM gets: larger than any image's own size,
// so that the guest growing its root filesystem shows.
const (
	catalogueDiskGi = 20
	// catalogueRootSlackGi is what the root filesystem may lack of the
	// disk: the partitions before it (EFI, /boot) and the filesystem's own.
	catalogueRootSlackGi = 2.5
)

// TestTheImageCatalogue boots each image of deploy/catalog/images.yaml
// that HANGAR_TESTBED_IMAGES names (a comma-separated list of names, or
// "all"; unset, it skips), one after the other, on one cluster with the
// platform's SSH CA, as docs/runbook/vm-images.md has a new or bumped
// image checked. Each file is downloaded from its URL into the testbed's
// cache once and checked against its sha256, then served to the agent
// over HTTPS. For each: a VM of it, with a 20Gi root disk and a static
// key, is Ready; its serial console has the boot; hangar ssh logs in as
// the image's defaultUser with a certificate, and so does OpenSSH with
// the static key; cloud-init is done, gave the VM its name as hostname
// and the key to the default user; the root filesystem has grown to the
// disk; hangar exec (the console terminal's :exec) runs a command and
// returns its exit code; and hangar stop presses the power button, which
// the guest powers off on, within the grace.
func TestTheImageCatalogue(t *testing.T) {
	sel := os.Getenv("HANGAR_TESTBED_IMAGES")
	if sel == "" {
		t.Skip("HANGAR_TESTBED_IMAGES names no image of the catalogue (names, comma-separated, or all)")
	}
	if installed {
		t.Skip("the installed agent trusts no test server's certificate")
	}
	images := chosenImages(t, catalogue(t), sel)
	srv := diskimgtest.New(t)
	files := map[string]string{}
	for _, img := range images {
		s := img.GetSpec()
		files[img.GetMetadata().GetName()] = diskimgtest.Cached(t, s.GetSha256()[:12]+"-"+path.Base(s.GetUrl()), s.GetUrl(), s.GetSha256())
	}
	fw, certs := diskimgtest.Firmware(t), srv.CertFile(t)
	c := startClusterWith(t, func(c *cluster) {
		key := filepath.Join(apiDir, "ssh-ca")
		writeCAKey(t, key)
		c.apiArgs = []string{"--ssh-ca-key", key}
		c.nodeArgs = []string{"--vm-firmware", fw}
		c.nodeEnv = []string{"SSL_CERT_FILE=" + certs}
	})
	for _, img := range images {
		name := img.GetMetadata().GetName()
		t.Run(name, func(t *testing.T) {
			// The cluster's helpers fail the test they hold: this one.
			parent := c.t
			c.t = t
			defer func() { c.t = parent }()
			bootCatalogueImage(t, c, srv, img, files[name])
		})
	}
}

// bootCatalogueImage checks one image of the catalogue, served from file;
// TestTheImageCatalogue says what.
func bootCatalogueImage(t *testing.T, c *cluster, srv *diskimgtest.Server, img *storagev1alpha1.Image, file string) {
	name, user := img.GetMetadata().GetName(), img.GetSpec().GetDefaultUser()
	served := proto.Clone(img).(*storagev1alpha1.Image)
	served.Spec.Url, _ = srv.Serve(t, path.Base(img.GetSpec().GetUrl()), file)
	doc, err := protojson.Marshal(served)
	if err != nil {
		t.Fatal(err)
	}
	c.applyYAML(string(doc))

	vm := "cat-" + name
	keyFile, pub := staticKey(t)
	c.applyYAML(fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: %s}
spec:
  rootDisk: {fromImage: {image: {name: %s}, size: %dGi}}
  resources: {cpu: 2, memory: 2Gi}
  sshAuthorizedKeys: [%q]
`, vm, name, catalogueDiskGi, pub))
	start := time.Now()
	since := func() string { return time.Since(start).Round(time.Second).String() }
	defer func() {
		if t.Failed() {
			if m, ok := get(c, "machine", "vm-"+vm, &computev1alpha1.Machine{}); ok {
				dumpVM(t, m.GetMetadata().GetUid())
			}
		}
		_, _ = c.tryCLI(nil, "delete", "vm", vm)
		_, _ = c.tryCLI(nil, "wait", "vm", vm, "--for=delete", "--timeout=3m")
		_, _ = c.tryCLI(nil, "delete", "image", name)
	}()
	if _, err := c.tryCLI(nil, "wait", "vm", vm, "--for=condition=Ready", "--timeout=15m"); err != nil {
		out, _ := c.tryCLI(nil, "get", "image", name, "-o", "yaml")
		t.Fatalf("VM never Ready: %v\n%s", err, out)
	}
	t.Logf("Ready %s after the apply", since())
	m, ok := get(c, "machine", "vm-"+vm, &computev1alpha1.Machine{})
	if !ok {
		t.Fatal("no machine vm-" + vm)
	}
	uid := m.GetMetadata().GetUid()
	var image storagev1alpha1.Image
	if _, ok := get(c, "image", name, &image); ok {
		t.Logf("image status: disk size %s, conditions %v", image.GetStatus().GetDiskSize(), image.GetStatus().GetConditions())
	}

	// The serial console has the boot, which hangar logs reads.
	eventually(t, 5*time.Minute, "the serial console's first lines", func() (bool, string) {
		out, err := c.tryCLI(nil, "logs", "vm", vm)
		return err == nil && strings.Count(out, "\n") >= 5, fmt.Sprintf("%q %v", out, err)
	})

	// hangar ssh, by the CA, as the image's default user.
	sshOut := func(args ...string) (string, error) {
		return c.tryCLI(nil, append([]string{"ssh", "vm", vm, "--"}, args...)...)
	}
	eventually(t, 15*time.Minute, "hangar ssh logging in as "+user, func() (bool, string) {
		out, err := sshOut("id -un")
		return err == nil && out == user+"\n", fmt.Sprintf("%q %v", out, err)
	})
	t.Logf("hangar ssh logged in as %s %s after the apply", user, since())
	run := func(cmd string) string {
		t.Helper()
		out, err := sshOut(cmd)
		if err != nil {
			t.Errorf("%s: %v: %q", cmd, err, out)
		}
		return out
	}
	t.Logf("guest: %s", strings.Join(strings.Fields(run(
		`. /etc/os-release; echo "$PRETTY_NAME;" kernel $(uname -r)";" $(cloud-init --version 2>&1)";" selinux $(getenforce 2>/dev/null || echo none)`)), " "))

	// cloud-init is done, and applied what the platform gave it.
	status := run("cloud-init status --wait --long")
	t.Logf("cloud-init status:\n%s", status)
	if !strings.Contains(status, "status: done") {
		t.Errorf("cloud-init is not done")
	}
	if host := strings.TrimSpace(run("hostname")); host != vm {
		t.Errorf("hostname %q, not %q", host, vm)
	}
	if keys := run("cat ~/.ssh/authorized_keys"); !strings.Contains(keys, pub) {
		t.Errorf("the default user's authorized_keys lack the VM's key: %q", keys)
	}
	eventually(t, 2*time.Minute, "OpenSSH logging in with the static key", func() (bool, string) {
		cmd := exec.Command("ssh", "-i", keyFile, "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
			"-o", "ProxyCommand="+vmtest.Artifacts().CLI+" ssh-proxy vm "+vm,
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", user+"@"+vm, "echo", "by", "the", "static", "key")
		cmd.Env = append(os.Environ(), "HANGAR_CONFIG="+filepath.Join(workDir, "cli.yaml"))
		out, err := cmd.CombinedOutput()
		return err == nil && strings.HasSuffix(string(out), "by the static key\n"), fmt.Sprintf("%q %v", out, err)
	})

	// The root filesystem grew to the disk.
	df := strings.Fields(run("df -B1 --output=size,source,fstype / | tail -n 1"))
	if len(df) != 3 {
		t.Fatalf("df: %q", df)
	}
	size, _ := strconv.ParseInt(df[0], 10, 64)
	t.Logf("root filesystem: %s %s, %.1f GiB of a %d GiB disk", df[1], df[2], float64(size)/(1<<30), catalogueDiskGi)
	if float64(size) < (catalogueDiskGi-catalogueRootSlackGi)*(1<<30) {
		t.Errorf("the root filesystem is %.1f GiB: not grown to the %d GiB disk\n%s", float64(size)/(1<<30), catalogueDiskGi, run("lsblk; sudo journalctl -b --no-pager | grep -iE 'growpart|resize|growfs' | tail -20"))
	}

	// hangar exec, the console terminal's :exec, runs a command as the
	// default user and gives its exit code.
	out, errOut, code := execCLI(t, nil, "vm", vm, "--", "sh", "-c", "id -un; echo to stderr >&2; exit 7")
	if out != user+"\n" || errOut != "to stderr\n" || code != 7 {
		t.Errorf("hangar exec: stdout %q, stderr %q, exit %d; want %q, %q, 7", out, errOut, code, user+"\n", "to stderr\n")
	}

	// hangar stop presses the power button: the guest powers off, within
	// the grace, and the platform's stop ends the boot.
	before := len(exits(t, uid))
	stopped := time.Now()
	c.cli("stop", "vm", vm)
	var last []console.ExitStatus
	eventually(t, 3*time.Minute, "the boot ended by the stop", func() (bool, string) {
		last = exits(t, uid)
		return len(last) > before, fmt.Sprintf("%v", last)
	})
	took := time.Since(stopped)
	st := last[len(last)-1]
	t.Logf("stopped: %+v, %v after hangar stop", st, took.Round(time.Second))
	if st.Code != 0 || st.Signal != driver.StoppedSignal {
		t.Errorf("the boot ended with %+v, not a power-off the platform asked for; the serial console's end:\n%s", st, consoleTail(t, uid))
	}
	if took >= chvm.DefaultFirmwareStopGrace {
		t.Errorf("powered off %v after the stop: past the grace", took)
	}
}

// consoleTail is the last lines of a firmware VM's serial console.
func consoleTail(t *testing.T, uid string) string {
	t.Helper()
	return tailLines(consoleText(t, uid), 40)
}

// tailLines is the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// catalogue reads the images of deploy/catalog/images.yaml, as hangar apply
// does: strictly, an unknown field an error.
func catalogue(t *testing.T) []*storagev1alpha1.Image {
	t.Helper()
	b, err := os.ReadFile(catalogueFile)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var out []*storagev1alpha1.Image
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("%s: %v", catalogueFile, err)
		}
		js, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		img := &storagev1alpha1.Image{}
		if err := protojson.Unmarshal(js, img); err != nil {
			t.Fatalf("%s: %v: %s", catalogueFile, err, js)
		}
		out = append(out, img)
	}
}

// chosenImages is the images sel names, in the catalogue's order.
func chosenImages(t *testing.T, all []*storagev1alpha1.Image, sel string) []*storagev1alpha1.Image {
	t.Helper()
	if sel == "all" {
		return all
	}
	var names []string
	for _, img := range all {
		names = append(names, img.GetMetadata().GetName())
	}
	want := strings.Split(sel, ",")
	for _, n := range want {
		if !slices.Contains(names, strings.TrimSpace(n)) {
			t.Fatalf("HANGAR_TESTBED_IMAGES names %q, which the catalogue lacks; it has %s", n, strings.Join(names, ", "))
		}
	}
	var out []*storagev1alpha1.Image
	for _, img := range all {
		if slices.ContainsFunc(want, func(n string) bool { return strings.TrimSpace(n) == img.GetMetadata().GetName() }) {
			out = append(out, img)
		}
	}
	return out
}
