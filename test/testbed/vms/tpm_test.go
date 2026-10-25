//go:build testbed

package vms

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// tpmprobe.py speaks TPM 2.0 to the guest's /dev/tpmrm0: Ubuntu's cloud
// image has no tpm2-tools, and the test's guest no network to fetch them.
//
//go:embed tpmprobe.py
var tpmProbe []byte

// tpmKeys is where the driver keeps the test's VMs' keys.
const tpmKeys = "/run/hangar-testbed-vtpm"

// TestAVMsTPMKeepsItsState: an Ubuntu VM with a TPM sees /dev/tpm0, and the
// NV index and the persistent key it makes there are the same after the
// guest reboots, and after the VM is stopped and started again, the state
// made its user's again, whichever it is then. The state on the host is
// encrypted, root's while the VM is stopped, and goes with its logs. swtpm
// runs confined as Cloud Hypervisor is.
func TestAVMsTPMKeepsItsState(t *testing.T) {
	const uid = "tpmvm"
	bundle := vmtest.Artifacts().Swtpm
	if _, err := os.Stat(filepath.Join(bundle, "bin/swtpm")); err != nil {
		t.Fatalf("the swtpm bundle: %v (make e2e-host-vms builds it)", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tpmKeys) })

	// Each boot prints its number and what the probe reads; the first, from
	// the user-data, makes the index and the key, then reboots.
	script := "echo " + base64.StdEncoding.EncodeToString(tpmProbe) + ` | base64 -d > /usr/local/bin/tpmprobe
chmod +x /usr/local/bin/tpmprobe
ls -l /dev/tpm0 /dev/tpmrm0
mkdir -p /var/lib/cloud/scripts/per-boot
cat > /var/lib/cloud/scripts/per-boot/tpm.sh <<'EOF'
#!/bin/sh
n=$(cat /var/lib/hangar-boots 2>/dev/null || echo 0); n=$((n+1)); echo $n > /var/lib/hangar-boots
{ ls /dev/tpm0 && /usr/local/bin/tpmprobe boot-$n; dmesg | grep -i 'tpm.*error' ; } > /dev/ttyS0 2>&1
EOF
chmod +x /var/lib/cloud/scripts/per-boot/tpm.sh
/var/lib/cloud/scripts/per-boot/tpm.sh
systemd-run --on-active=3 systemctl reboot`
	// The key the agent would have asked the apiserver for.
	putKey := func(hv *chvm.Hypervisor) {
		if err := hv.PutTPMKey(context.Background(), uid, key); err != nil {
			t.Fatal(err)
		}
	}
	out, hv, spec := bootWithSeedConfig(t, uid, script,
		func(c *chvm.Config) { c.Swtpm, c.TPMKeys = bundle, tpmKeys }, putKey,
		func(_ *host, s *driver.VMSpec) { s.SystemSerialNumber, s.TPM = uid, true })
	if !regexp.MustCompile(`crw-rw---- 1 tss root +10, +224 .* /dev/tpm0`).MatchString(out) {
		t.Errorf("the guest has no /dev/tpm0:\n%s", tail(out))
	}
	first := tpmBoot(t, uid, 1)
	if !strings.Contains(first, "nv-write=0x0") || !strings.Contains(first, "evict=0x0") {
		t.Fatalf("the probe could not make its index and key: %s", first)
	}
	want := tpmIdentity(t, first)
	user := vmUserOf(t, uid)
	checkSwtpmConfined(t, uid, user)
	checkTPMState(t, uid, user)

	if got := tpmIdentity(t, tpmBoot(t, uid, 2)); got != want {
		t.Errorf("after the guest's reboot the TPM holds %s, not %s", got, want)
	}

	ctx := context.Background()
	if err := hv.Stop(ctx, uid); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(vmDir, uid, "tpm", "tpm2-00.permall"), &st); err != nil || st.Uid != 0 {
		t.Errorf("a stopped VM's TPM state is %d's (%v), want root's", st.Uid, err)
	}
	if err := hv.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got := tpmIdentity(t, tpmBoot(t, uid, 3)); got != want {
		t.Errorf("after the VM's stop and start the TPM holds %s, not %s", got, want)
	}
	checkSwtpmConfined(t, uid, vmUserOf(t, uid))
	checkTPMState(t, uid, vmUserOf(t, uid))

	if err := hv.Stop(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if err := hv.DeleteLogs(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(vmDir, uid, "tpm")); !os.IsNotExist(err) {
		t.Errorf("the TPM's state outlived DeleteLogs: %v", err)
	}
}

// tpmBoot waits for boot n's line of the probe on the serial console.
func tpmBoot(t *testing.T, uid string, n int) string {
	t.Helper()
	re := regexp.MustCompile(`hangar-tpm:\[boot-` + strconv.Itoa(n) + `\] ([^\r\n]*)`)
	deadline := time.Now().Add(10 * time.Minute)
	for {
		out := serialOf(t, uid)
		if m := re.FindStringSubmatch(out); m != nil {
			t.Logf("boot %d: %s", n, m[1])
			return m[1]
		}
		if time.Now().After(deadline) {
			dump(t, uid)
			t.Fatalf("no boot %d in 10 minutes:\n%s", n, tail(out))
		}
		// A second, not Eventually's 50ms: each look decodes the whole
		// console log, and a boot through the firmware takes minutes.
		time.Sleep(time.Second)
	}
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

// swtpmPID is the machine's swtpm, run under the bundle's loader.
func swtpmPID(t *testing.T, uid string) int {
	t.Helper()
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if bytes.Contains(cmd, []byte("/bin/swtpm\x00socket\x00")) && bytes.Contains(cmd, []byte("/"+uid+"/")) {
			return pid
		}
	}
	t.Fatalf("no swtpm for %s", uid)
	return 0
}

// checkSwtpmConfined checks the machine's swtpm runs as Cloud Hypervisor
// does: as the VM's user, with no group else, no capability and
// no_new_privs, in the VM's unit, with AppArmor's swtpm profile, Ubuntu's,
// unattached; that it holds no fd but its sockets, pipes, /dev/null and
// what is in its state directory, and may hold the host's nr_open of them;
// and that its control socket is root's and the VM's group's alone, which
// another VM's user cannot connect to.
func checkSwtpmConfined(t *testing.T, uid string, user int) {
	t.Helper()
	pid := swtpmPID(t, uid)
	st := statusOf(t, pid)
	id := strconv.Itoa(user)
	for k, v := range map[string]string{
		"Uid": strings.Repeat(id+" ", 3) + id, "Gid": strings.Repeat(id+" ", 3) + id,
		"CapInh": "0000000000000000", "CapPrm": "0000000000000000", "CapEff": "0000000000000000",
		"CapBnd": "0000000000000000", "CapAmb": "0000000000000000", "NoNewPrivs": "1", "Groups": "",
	} {
		if st[k] != v {
			t.Errorf("swtpm's %s is %q, want %q", k, st[k], v)
		}
	}
	t.Logf("swtpm: Uid %s, CapEff %s, CapBnd %s, NoNewPrivs %s, Seccomp %s", st["Uid"], st["CapEff"], st["CapBnd"], st["NoNewPrivs"], st["Seccomp"])
	cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil || !strings.Contains(string(cg), "/hangar-vm-"+uid+".service") {
		t.Errorf("swtpm's cgroup %q is not its VM's unit's (%v)", cg, err)
	}
	label, err := os.ReadFile(fmt.Sprintf("/proc/%d/attr/current", pid))
	if err != nil || strings.TrimSpace(string(label)) != "unconfined" {
		t.Errorf("swtpm's security label is %q (%v), want unconfined", label, err)
	}
	if out, err := exec.Command("aa-status").CombinedOutput(); err == nil {
		t.Logf("AppArmor's swtpm profile loaded on the host: %t", regexp.MustCompile(`(?m)^\s+swtpm$`).Match(out))
	}
	state := filepath.Join(vmDir, uid, "tpm") + "/"
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		t.Fatal(err)
	}
	var held []string
	for _, fd := range fds {
		link, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
		held = append(held, fd.Name()+"="+link)
		switch {
		case strings.HasPrefix(link, "socket:"), strings.HasPrefix(link, "pipe:"), strings.HasPrefix(link, "anon_inode:"),
			link == "/dev/null", strings.HasPrefix(link, state):
		default:
			t.Errorf("swtpm holds fd %s, %s", fd.Name(), link)
		}
	}
	t.Logf("swtpm's fds: %s", strings.Join(held, " "))
	limits, _ := os.ReadFile(fmt.Sprintf("/proc/%d/limits", pid))
	nrOpen, _ := os.ReadFile("/proc/sys/fs/nr_open")
	n := strings.TrimSpace(string(nrOpen))
	if !regexp.MustCompile(`Max open files\s+` + n + `\s+` + n + `\s`).Match(limits) {
		t.Errorf("swtpm's limits, want %s open files:\n%s", n, limits)
	}

	sock := filepath.Join(vmDir, uid, "tpm.sock")
	var sst syscall.Stat_t
	if err := syscall.Lstat(sock, &sst); err != nil {
		t.Fatal(err)
	}
	if sst.Mode&syscall.S_IFMT != syscall.S_IFSOCK || sst.Mode&0o7777 != 0o660 || sst.Uid != 0 || sst.Gid != uint32(user) {
		t.Errorf("the TPM's socket: mode %o, %d:%d; want a socket, 0660, root's and %d's", sst.Mode, sst.Uid, sst.Gid, user)
	}
	other := strconv.Itoa(user + 1000)
	out, err := exec.Command("setpriv", "--reuid", other, "--regid", other, "--clear-groups", "--",
		"python3", "-c", "import socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1])", sock).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "PermissionError") {
		t.Errorf("another user connecting to the TPM's socket: %v, %s; want it refused", err, out)
	}
}

// checkTPMState checks the running VM's TPM state is its user's alone, in
// swtpm's encrypted format, the index's value nowhere in it.
func checkTPMState(t *testing.T, uid string, user int) {
	t.Helper()
	dir := filepath.Join(vmDir, uid, "tpm")
	var st syscall.Stat_t
	if err := syscall.Lstat(dir, &st); err != nil || st.Uid != uint32(user) || st.Mode&0o7777 != 0o700 {
		t.Errorf("the TPM's state directory: %d's, mode %o (%v); want %d's, 0700", st.Uid, st.Mode&0o7777, err, user)
	}
	b, err := os.ReadFile(filepath.Join(dir, "tpm2-00.permall"))
	if err != nil {
		t.Fatal(err)
	}
	// swtpm's state header: version 2, a header of 10 bytes, then its flags,
	// 9 for encrypted with a 256-bit key (BLOB_FLAG_ENCRYPTED and
	// BLOB_FLAG_ENCRYPTED_256BIT_KEY); a plain state's are 0.
	if len(b) < 8 || b[4] != 0 || b[5] != 9 {
		t.Errorf("the TPM's state is not encrypted: % x", b[:min(len(b), 16)])
	}
	if bytes.Contains(b, []byte("hangar01")) {
		t.Error("the index's value is in the TPM's state as it is")
	}
	t.Logf("the TPM's state: %d bytes, starting % x", len(b), b[:8])
}
