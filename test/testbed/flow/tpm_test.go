//go:build testbed

package flow

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// flowTPMKeys is where this run's agent keeps TPM keys.
const flowTPMKeys = "/run/hangar-testbed-flow-vtpm"

// TestAVMsTPMThroughTheAPI: a VirtualMachine applied with spec.tpm.enabled
// boots through the real apiserver and agent with a TPM: /dev/tpm0 in the
// guest, an NV index and a persistent key the guest makes there, the same
// after the VM is stopped and started with hangar, and after its host
// reboots. The key the agent got
// is a file only root reads while the VM may run, gone once it is
// stopped; neither hangar get, the services' logs nor anything in the
// VM's directory holds it; the TPM's state goes with the VM.
func TestAVMsTPMThroughTheAPI(t *testing.T) {
	if installed {
		t.Skip("the installed services run with their own key-encryption key; this test makes its own")
	}
	bundle := vmtest.Artifacts().Swtpm
	if _, err := os.Stat(filepath.Join(bundle, "bin/swtpm")); err != nil {
		t.Fatalf("the swtpm bundle: %v (make e2e-host-flow builds it)", err)
	}
	probe, err := os.ReadFile("../vms/tpmprobe.py")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(flowTPMKeys)
	t.Cleanup(func() { _ = os.RemoveAll(flowTPMKeys) })
	srv := diskimgtest.New(t)
	url, _ := srv.Serve(t, diskimgtest.UbuntuName, diskimgtest.Ubuntu(t))
	fw, certs := diskimgtest.Firmware(t), srv.CertFile(t)
	c := startClusterWith(t, func(c *cluster) {
		kek := filepath.Join(apiDir, "secrets-kek")
		if err := os.WriteFile(kek, []byte(secrets.NewKEK("flow").String()), 0o600); err != nil {
			t.Fatal(err)
		}
		nobody := lookupUID(t, "nobody")
		if err := os.Chown(kek, nobody, nobody); err != nil {
			t.Fatal(err)
		}
		c.apiArgs = append(c.apiArgs, "--secrets-kek", kek)
		c.nodeArgs = []string{"--vm-firmware", fw, "--vm-swtpm", bundle, "--vm-tpm-keys", flowTPMKeys}
		c.nodeEnv = []string{"SSL_CERT_FILE=" + certs}
	})
	c.applyYAML(fmt.Sprintf(`apiVersion: storage.hangarlab.cloud/v1alpha1
kind: Image
metadata: {name: noble}
spec: {url: %q, sha256: %s, os: ubuntu, version: "24.04"}
`, url, diskimgtest.UbuntuSHA256))
	// Every boot, the first included, says what the probe reads.
	perBoot := "#!/bin/sh\nn=$(cat /var/lib/hangar-boots 2>/dev/null || echo 0); n=$((n+1)); echo $n > /var/lib/hangar-boots\n" +
		"{ ls -l /dev/tpm0; /usr/local/bin/tpmprobe boot-$n; } > /dev/ttyS0 2>&1\n"
	c.applyYAML(fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: tpmy}
spec:
  rootDisk: {fromImage: {image: {name: noble}}}
  resources: {cpu: 2, memory: 1Gi}
  tpm: {enabled: true}
  userData: |
    #cloud-config
    write_files:
    - path: /usr/local/bin/tpmprobe
      permissions: "0755"
      encoding: b64
      content: %s
    - path: /var/lib/cloud/scripts/per-boot/tpm.sh
      permissions: "0755"
      encoding: b64
      content: %s
`, base64.StdEncoding.EncodeToString(probe), base64.StdEncoding.EncodeToString([]byte(perBoot))))
	start := time.Now()
	c.cli("wait", "vm", "tpmy", "--for=condition=Ready", "--timeout=10m")
	t.Logf("Ready %v after the apply", time.Since(start).Round(time.Second))
	m, ok := get(c, "machine", "vm-tpmy", &computev1alpha1.Machine{})
	if !ok || !m.GetSpec().GetFirmware().GetTpm() {
		t.Fatalf("the VM's machine has no TPM: %v", m.GetSpec().GetFirmware())
	}
	uid := m.GetMetadata().GetUid()

	first := tpmBoot(t, c, 1)
	if !strings.Contains(first, "nv-write=0x0") || !strings.Contains(first, "evict=0x0") {
		t.Fatalf("the probe could not make its index and key: %s", first)
	}
	if out := c.cli("logs", "vm", "tpmy"); !regexp.MustCompile(`crw-rw---- 1 tss root +10, +224 .* /dev/tpm0`).MatchString(out) {
		t.Error("the guest has no /dev/tpm0")
	}
	want := tpmIdentity(t, first)

	// The key, as the agent keeps it.
	keyFile := filepath.Join(flowTPMKeys, uid)
	var st syscall.Stat_t
	if err := syscall.Stat(keyFile, &st); err != nil || st.Uid != 0 || st.Mode&0o7777 != 0o600 {
		t.Fatalf("the TPM key file: %v, uid %d, mode %o", err, st.Uid, st.Mode&0o7777)
	}
	key, err := os.ReadFile(keyFile)
	if err != nil || len(key) != 32 {
		t.Fatalf("the TPM key file holds %d bytes: %v", len(key), err)
	}
	checkKeyNowhere(t, c, uid, key)

	c.cli("stop", "vm", "tpmy")
	c.cli("wait", "vm", "tpmy", "--for=condition=Stopped", "--timeout=5m")
	eventually(t, time.Minute, "the key gone with the VM's stop", func() (bool, string) {
		_, err := os.Stat(keyFile)
		return os.IsNotExist(err), fmt.Sprint(err)
	})
	if err := syscall.Lstat(filepath.Join(vmDir, uid, "tpm", "tpm2-00.permall"), &st); err != nil || st.Uid != 0 {
		t.Errorf("a stopped VM's TPM state: %v, uid %d; want it kept, root's", err, st.Uid)
	}
	c.cli("start", "vm", "tpmy")
	c.cli("wait", "vm", "tpmy", "--for=condition=Ready", "--timeout=10m")
	if got := tpmIdentity(t, tpmBoot(t, c, 2)); got != want {
		t.Errorf("after the VM's stop and start the TPM holds %s, not %s", got, want)
	}
	checkKeyNowhere(t, c, uid, key)

	// The host reboots: the agent goes, then the VM's unit, as systemd
	// orders a shutdown, and /run is empty after. The agent back asks for
	// the key again, and the VM boots with the same TPM.
	bootTime := func() time.Time {
		m, _ := get(c, "machine", "vm-tpmy", &computev1alpha1.Machine{})
		return m.GetStatus().GetBootTime().AsTime()
	}
	boot := bootTime()
	c.killNode()
	mustRun(t, "systemctl", "stop", unitOf(uid))
	if err := os.RemoveAll(flowTPMKeys); err != nil {
		t.Fatal(err)
	}
	c.startNode()
	eventually(t, 3*time.Minute, "the VM booted again after the host's reboot", func() (bool, string) {
		b := bootTime()
		return b.After(boot) && unitActive(uid), fmt.Sprintf("boot time %v, was %v", b, boot)
	})
	if got := tpmIdentity(t, tpmBoot(t, c, 3)); got != want {
		t.Errorf("after the host's reboot the TPM holds %s, not %s", got, want)
	}
	if again, err := os.ReadFile(keyFile); err != nil || !bytes.Equal(again, key) {
		t.Errorf("the key after the host's reboot: %v, the same: %v", err, bytes.Equal(again, key))
	}

	c.cli("delete", "vm", "tpmy")
	c.cli("wait", "vm", "tpmy", "--for=delete", "--timeout=5m")
	eventually(t, 3*time.Minute, "the VM's TPM state and key gone", func() (bool, string) {
		_, errState := os.Stat(filepath.Join(vmDir, uid))
		_, errKey := os.Stat(keyFile)
		return os.IsNotExist(errState) && os.IsNotExist(errKey), fmt.Sprintf("directory: %v, key: %v", errState, errKey)
	})
}

// tpmBoot waits for boot n's line of the probe in the VM's log.
func tpmBoot(t *testing.T, c *cluster, n int) string {
	t.Helper()
	re := regexp.MustCompile(`hangar-tpm:\[boot-` + fmt.Sprint(n) + `\] ([^\r\n]*)`)
	var line string
	eventually(t, 12*time.Minute, fmt.Sprintf("boot %d's TPM probe", n), func() (bool, string) {
		out := c.cli("logs", "vm", "tpmy")
		if m := re.FindStringSubmatch(out); m != nil {
			line = m[1]
			return true, ""
		}
		lines := strings.Split(out, "\n")
		return false, strings.Join(lines[max(0, len(lines)-10):], "\n")
	})
	t.Logf("boot %d: %s", n, line)
	return line
}

// tpmIdentity is what the probe read of the index and the persistent key.
func tpmIdentity(t *testing.T, line string) string {
	t.Helper()
	got := strings.Join(regexp.MustCompile(`\b(nv|persistent)=\S+`).FindAllString(line, -1), " ")
	if !strings.HasPrefix(got, "nv=hangar01 persistent=") || strings.Contains(got, "rc=") {
		t.Errorf("the probe read %q", line)
	}
	return got
}

// checkKeyNowhere checks the TPM key is in no answer about the VM, no log
// of the apiserver's or the agent's, and no file of the VM's directory,
// its TPM's state encrypted; and that the apiserver said it gave the key.
func checkKeyNowhere(t *testing.T, c *cluster, uid string, key []byte) {
	t.Helper()
	needles := [][]byte{key, []byte(hex.EncodeToString(key)), []byte(base64.StdEncoding.EncodeToString(key)), []byte(base64.RawStdEncoding.EncodeToString(key))}
	holds := func(b []byte) bool {
		for _, n := range needles {
			if bytes.Contains(b, n) {
				return true
			}
		}
		return false
	}
	out := c.cli("get", "vm", "tpmy", "-o", "json")
	if holds([]byte(out)) || strings.Contains(out, "sealed") {
		t.Errorf("hangar get vm holds the key or its sealed form:\n%s", out)
	}
	if !strings.Contains(out, `"enabled": true`) && !strings.Contains(out, `"enabled":true`) {
		t.Errorf("hangar get vm does not say the VM has a TPM:\n%s", out)
	}
	logs, _ := filepath.Glob(filepath.Join(workDir, "*.log"))
	var given bool
	for _, f := range logs {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if holds(b) {
			t.Errorf("%s holds the TPM key", f)
		}
		given = given || bytes.Contains(b, []byte("a TPM key given"))
	}
	if !given {
		t.Error("no log says the apiserver gave the TPM key")
	}
	err := filepath.WalkDir(filepath.Join(vmDir, uid), func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if holds(b) {
			t.Errorf("%s holds the TPM key", p)
		}
		if d.Name() == "tpm2-00.permall" && (len(b) < 6 || b[5] != 9) {
			t.Errorf("the TPM's state is not encrypted: % x", b[:min(len(b), 8)])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
