package chvm

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// fakeSwtpmEnv, set to stubborn, makes fakeSwtpm ignore SIGTERM.
const fakeSwtpmEnv = "CHVM_FAKE_SWTPM"

// fakeSwtpm plays the bundle's loader running swtpm: it records its
// arguments, its fds from 3 to 9, the key it reads from fd 4, whether fd 3
// listens, and its RLIMIT_NOFILE, in swtpm.json in its state directory;
// writes a line to its log, fd 5; accepts on fd 3; and on SIGTERM leaves
// "terminated" there and exits 0.
func fakeSwtpm(args []string) int {
	var state string
	for _, a := range args {
		if s, ok := strings.CutPrefix(a, "dir="); ok {
			state = s
		}
	}
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	fds := openFDs()
	key := make([]byte, 64)
	n, _ := syscall.Read(4, key)
	listens, _ := unix.GetsockoptInt(3, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	var lim syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim)
	b, _ := json.Marshal(map[string]any{
		"args": args, "fds": fds, "key": hex.EncodeToString(key[:max(n, 0)]), "listens": listens == 1, "nofile": lim.Cur,
	})
	_ = os.WriteFile(filepath.Join(state, "swtpm.json"), b, 0o600)
	_, _ = syscall.Write(5, []byte("swtpm: hello from the log\n"))
	go func() {
		for {
			if nfd, _, err := syscall.Accept(3); err == nil {
				defer func() { _ = syscall.Close(nfd) }()
			} else {
				return
			}
		}
	}()
	for {
		<-terms
		if os.Getenv(fakeSwtpmEnv) == "stubborn" {
			continue
		}
		_ = os.WriteFile(filepath.Join(state, "terminated"), nil, 0o600)
		return 0
	}
}

// swtpmBundle is a bundle whose loader is this test binary.
func swtpmBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"lib", "bin"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(os.Args[0], filepath.Join(dir, swtpmLoader)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, swtpmBinary), []byte("swtpm"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

var testTPMKey = bytes.Repeat([]byte{0xa5}, tpmKeySize)

// tpmLauncher is firmwareDir with the VM given a TPM, its key among the unit's
// credentials, and its state made its user's by a recorder.
func tpmLauncher(t *testing.T, mode string, key []byte) (*Launcher, string, *[]string) {
	t.Helper()
	l, dir := firmwareDir(t, mode, func(s *driver.VMSpec) { s.TPM = true })
	ln, err := readLaunch(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln.Swtpm = swtpmBundle(t)
	b, err := json.Marshal(ln)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, specFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	creds := t.TempDir()
	if key != nil {
		if err := os.WriteFile(filepath.Join(creds, tpmCredential), key, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	l.Credentials = creds
	var owned []string
	l.OwnTPM = func(path string, uid int) error {
		owned = append(owned, fmt.Sprintf("%s:%d", path, uid))
		return os.MkdirAll(path, 0o700)
	}
	return l, dir, &owned
}

type swtpmRecord struct {
	Args    []string
	FDs     []int
	Key     string
	Listens bool
	NoFile  uint64
}

func swtpmOf(t *testing.T, dir string) swtpmRecord {
	t.Helper()
	var r swtpmRecord
	b, err := os.ReadFile(filepath.Join(dir, tpmDir, "swtpm.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestATPMVMRunsSwtpm: a TPM VM's launcher makes its state its user's, runs
// swtpm on it before Cloud Hypervisor, under the bundle's loader, with the
// control socket listening on fd 3, the key on a pipe on fd 4 and its log
// on fd 5, never --terminate; Cloud Hypervisor connects to the socket,
// root's and the VM's group's; swtpm's log reaches the launcher's, and
// swtpm is terminated once Cloud Hypervisor has exited.
func TestATPMVMRunsSwtpm(t *testing.T) {
	l, dir, owned := tpmLauncher(t, "exit:0", testTPMKey)
	logs := &lockedBuffer{}
	l.Log = slog.New(slog.NewTextHandler(logs, nil))
	if got := runFor(t, l, t.Context(), "m1"); got != 0 {
		t.Fatalf("exited %d", got)
	}
	if want := []string{filepath.Join(dir, tpmDir) + ":0"}; !slices.Equal(*owned, want) {
		t.Errorf("owned %q, want %q", *owned, want)
	}
	var ch struct {
		Args []string
		TPM  string
	}
	b, _ := os.ReadFile(filepath.Join(dir, chDir, "args.json"))
	if err := json.Unmarshal(b, &ch); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(ch.Args, " "); !strings.Contains(joined, "--tpm socket="+filepath.Join(dir, tpmSocket)) {
		t.Errorf("Cloud Hypervisor's arguments %q lack the TPM's socket", joined)
	}
	if ch.TPM != "connected" {
		t.Errorf("Cloud Hypervisor connecting to the TPM's socket: %s", ch.TPM)
	}
	sw := swtpmOf(t, dir)
	ln, _ := readLaunch(dir)
	want := []string{
		filepath.Join(ln.Swtpm, "lib"), filepath.Join(ln.Swtpm, swtpmBinary),
		"socket", "--tpm2", "--tpmstate", "dir=" + filepath.Join(dir, tpmDir),
		"--ctrl", "type=unixio,fd=3", "--key", "fd=4,format=binary,mode=aes-256-cbc",
		"--flags", "not-need-init", "--log", "fd=5,level=1",
	}
	if !slices.Equal(sw.Args, want) {
		t.Errorf("swtpm's arguments %q, want %q", sw.Args, want)
	}
	if !slices.Equal(sw.FDs, []int{3, 4, 5}) || !sw.Listens {
		t.Errorf("swtpm's fds %v, fd 3 listening %t; want 3 to 5, listening", sw.FDs, sw.Listens)
	}
	if sw.Key != hex.EncodeToString(testTPMKey) {
		t.Errorf("swtpm read the key %s", sw.Key)
	}
	fi, err := os.Lstat(filepath.Join(dir, tpmSocket))
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o660 {
		t.Errorf("the TPM's socket: %v %v; want a socket, 0660", fi, err)
	}
	if !exists(filepath.Join(dir, tpmDir, "terminated")) {
		t.Error("swtpm was not terminated")
	}
	if !strings.Contains(string(logs.Bytes()), "swtpm: hello from the log") {
		t.Errorf("swtpm's log is not in the launcher's:\n%s", logs.Bytes())
	}
}

// TestASwtpmThatStaysUpIsKilled: swtpm ignoring SIGTERM is killed after
// the launcher's grace, and the launcher returns.
func TestASwtpmThatStaysUpIsKilled(t *testing.T) {
	t.Setenv(fakeSwtpmEnv, "stubborn")
	l, dir, _ := tpmLauncher(t, "exit:0", testTPMKey)
	l.KillGrace = 200 * time.Millisecond
	start := time.Now()
	if got := runFor(t, l, t.Context(), "m1"); got != 0 {
		t.Fatalf("exited %d", got)
	}
	if exists(filepath.Join(dir, tpmDir, "terminated")) {
		t.Error("a stubborn swtpm says it was terminated")
	}
	if d := time.Since(start); d < l.KillGrace {
		t.Errorf("returned in %v, before the grace %v", d, l.KillGrace)
	}
}

// TestATPMVMWithoutItsKeyDoesNotStart: no key among the credentials, or one
// of another size, fails the start, recorded, with neither swtpm nor Cloud
// Hypervisor run.
func TestATPMVMWithoutItsKeyDoesNotStart(t *testing.T) {
	for name, key := range map[string][]byte{"none": nil, "short": testTPMKey[:16], "long": append(slices.Clone(testTPMKey), 0)} {
		t.Run(name, func(t *testing.T) {
			l, dir, _ := tpmLauncher(t, "exit:0", key)
			if got := runFor(t, l, t.Context(), "m1"); got != ExitFailed {
				t.Fatalf("exited %d, want %d", got, ExitFailed)
			}
			rec, ok := exitOf(t, dir)
			if !ok || !strings.Contains(rec.Error, "key") {
				t.Errorf("exit record %+v %t, want the key's error", rec, ok)
			}
			if exists(filepath.Join(dir, tpmDir, "swtpm.json")) || exists(filepath.Join(dir, chDir, "args.json")) {
				t.Error("swtpm or Cloud Hypervisor ran without a key")
			}
		})
	}
}

// TestSwtpmRunsAsTheVMsUser: with a VM user, swtpm runs through vmm-exec as
// it, with its three fds and the host's nr_open as its RLIMIT_NOFILE.
func TestSwtpmRunsAsTheVMsUser(t *testing.T) {
	l := &Launcher{}
	l.defaults()
	ln := launch{VMUser: 200007, Swtpm: "/b", NoFile: 1 << 20}
	got := l.swtpmCommand(ln, "/v/m1/tpm").Args
	want := []string{
		"/proc/self/exe", "vmm-exec", "--uid", "200007", "--gid", "200007", "--fds", "3", "--nofile", "1048576",
		"--", "/b/lib/ld-musl-x86_64.so.1", "--library-path", "/b/lib", "/b/bin/swtpm",
	}
	if !slices.Equal(got[:len(want)], want) {
		t.Errorf("swtpm runs as %q, want %q...", got, want)
	}
	if c, argv, err := parseExec(got[2:]); err != nil || c.nofile != 1<<20 || c.fds != 3 || argv[0] != "/b/lib/ld-musl-x86_64.so.1" {
		t.Errorf("vmm-exec reads %+v %q %v", c, argv, err)
	}
	if slices.Contains(got, "--terminate") {
		t.Error("swtpm is told to --terminate, and would end with the guest's first reboot")
	}
}

// tpmDriver is a driver with a swtpm bundle, a directory of keys, and an
// nr_open of 1048576.
func tpmDriver(t *testing.T, s *fakeSystemd) (*Hypervisor, string, *[]string) {
	t.Helper()
	fw := filepath.Join(t.TempDir(), "CLOUDHV.fd")
	if err := os.WriteFile(fw, []byte("fw"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, procSys := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(procSys, "fs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procSys, "fs/nr_open"), []byte("1048576\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var disowned []string
	bundle := swtpmBundle(t)
	h := newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) {
		c.Firmware, c.FirmwareStopGrace = fw, testGrace
		c.Swtpm, c.TPMKeys, c.procSys = bundle, keys, procSys
		c.disownTPM = func(path string) error {
			disowned = append(disowned, path)
			return nil
		}
	})
	return h, keys, &disowned
}

func tpmSpec(uid string) driver.VMSpec {
	spec := testSpec(uid, driver.RestartOnFailure)
	spec.Root, spec.Run, spec.Firmware, spec.TPM = driver.Disk{}, driver.RunSpec{}, true, true
	spec.Disks = []driver.Volume{{DiskUID: "root", Path: "/dev/zvol/hangar/disks/root", Size: 10 << 30}}
	return spec
}

// TestStartWritesATPMVM: a TPM VM's launch names the bundle and nr_open;
// its unit binds the bundle, loads the key as its credential, takes
// nr_open fds at most and 1024 unless raised, and has tasks, memory and
// time to stop for swtpm. Without the bundle, without a key, or without
// firmware, it is refused.
func TestStartWritesATPMVM(t *testing.T) {
	s := newFakeSystemd()
	h, keys, _ := tpmDriver(t, s)
	spec := tpmSpec("m1")
	if err := h.Start(t.Context(), spec); err == nil {
		t.Error("a TPM VM with no key started")
	}
	key := filepath.Join(keys, "m1")
	if err := os.WriteFile(key, testTPMKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	ln, err := readLaunch(h.dir("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if ln.Swtpm != h.cfg.Swtpm || ln.NoFile != 1048576 || !ln.Spec.TPM {
		t.Errorf("launch %+v", ln)
	}
	props := s.props["hangar-vm-m1.service"]
	if got, want := prop(props, "LoadCredential"), []pathPair{{tpmCredential, key}}; !reflect.DeepEqual(got, want) {
		t.Errorf("LoadCredential=%v, want %v", got, want)
	}
	if binds := prop(props, "BindReadOnlyPaths").([]bindPath); binds[len(binds)-1].Source != h.cfg.Swtpm {
		t.Errorf("BindReadOnlyPaths=%v, want the bundle last", binds)
	}
	bare := spec
	bare.TPM = false
	for name, want := range map[string]any{
		"LimitNOFILE":     uint64(1048576),
		"LimitNOFILESoft": uint64(unitNoFileSoft),
		"TasksMax":        unitTasks(bare) + tpmTasks,
		"MemoryMax":       vmMemoryMax(spec.MemoryMiB) + tpmMemory,
		"TimeoutStopUSec": usec(stopTimeout(testGrace, testSlack, bare) + killGrace),
	} {
		if got := prop(props, name); got != want {
			t.Errorf("%s=%v, want %v", name, got, want)
		}
	}
	// A VM without a TPM has none of it.
	plain := tpmSpec("m3")
	plain.TPM = false
	if err := h.Start(t.Context(), plain); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"LoadCredential", "LimitNOFILE", "LimitNOFILESoft"} {
		if got := prop(s.props["hangar-vm-m3.service"], name); got != nil {
			t.Errorf("a VM without a TPM has %s=%v", name, got)
		}
	}

	noFirmware := tpmSpec("m2")
	noFirmware.Firmware, noFirmware.Disks = false, nil
	noFirmware.Root = driver.Disk{MachineUID: "m2", Path: "/dev/zvol/hangar/roots/m2"}
	noFirmware.Run = driver.RunSpec{Command: []string{"true"}}
	if err := os.WriteFile(filepath.Join(keys, "m2"), testTPMKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), noFirmware); err == nil || !strings.Contains(err.Error(), "TPM") {
		t.Errorf("a container VM with a TPM: %v", err)
	}
	bundleless, _ := firmwareDriverWithKeys(t, keys)
	if err := bundleless.Start(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "swtpm") {
		t.Errorf("a host with no swtpm started a TPM VM: %v", err)
	}
}

// firmwareDriverWithKeys is a firmware driver with no swtpm bundle.
func firmwareDriverWithKeys(t *testing.T, keys string) (*Hypervisor, *fakeSystemd) {
	t.Helper()
	s := newFakeSystemd()
	fw := filepath.Join(t.TempDir(), "CLOUDHV.fd")
	if err := os.WriteFile(fw, []byte("fw"), 0o644); err != nil {
		t.Fatal(err)
	}
	return newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) { c.Firmware, c.TPMKeys = fw, keys }), s
}

// TestATPMsStateOutlivesItsVMsStop: Stop keeps a TPM VM's state, made
// root's before the spec file goes and frees the VM's id, and the
// directory with it, though no log is left; the sweep's Logs lists such a
// machine, and DeleteLogs removes the state.
func TestATPMsStateOutlivesItsVMsStop(t *testing.T) {
	s := newFakeSystemd()
	h, keys, disowned := tpmDriver(t, s)
	if err := os.WriteFile(filepath.Join(keys, "m1"), testTPMKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), tpmSpec("m1")); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(h.dir("m1"), tpmDir)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "tpm2-00.permall"), []byte("sealed"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{tpmSocket, chDir} {
		if err := os.MkdirAll(filepath.Join(h.dir("m1"), f), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h.disownTPM = func(path string) error {
		*disowned = append(*disowned, path)
		if !exists(filepath.Join(filepath.Dir(path), specFile)) {
			t.Error("the TPM's state was made root's after the spec file went")
		}
		return nil
	}
	if err := h.Stop(t.Context(), "m1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*disowned, []string{state}) {
		t.Errorf("made root's %q, want %q", *disowned, state)
	}
	if b, err := os.ReadFile(filepath.Join(state, "tpm2-00.permall")); err != nil || string(b) != "sealed" {
		t.Errorf("the TPM's state after Stop: %q %v", b, err)
	}
	entries, _ := os.ReadDir(h.dir("m1"))
	if len(entries) != 1 || entries[0].Name() != tpmDir {
		t.Errorf("the directory holds %v after Stop, want %s alone", entries, tpmDir)
	}
	logs, err := h.Logs(t.Context())
	if err != nil || !slices.Equal(logs, []string{"m1"}) {
		t.Errorf("Logs: %q %v, want m1, whose TPM's state is kept", logs, err)
	}
	if vm, ok := listed(t, h, "m1"); ok {
		t.Errorf("a stopped TPM VM is listed: %+v", vm)
	}
	if err := h.DeleteLogs(t.Context(), "m1"); err != nil {
		t.Fatal(err)
	}
	if exists(h.dir("m1")) {
		t.Error("DeleteLogs left the TPM's state")
	}
}

// TestOwnTPMDirRefusesALink: a link in place of the state directory is
// refused, never followed.
func TestOwnTPMDirRefusesALink(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, tpmDir)); err != nil {
		t.Fatal(err)
	}
	if err := ownTPMDir(filepath.Join(dir, tpmDir), os.Getuid()); err == nil {
		t.Error("a link was taken for the TPM's state")
	}
}

// TestOwnTPMDirGivesTheStateToTheVMsUser: as root, the state, made if need
// be, is root's 0700 and then the VM user's, with its directories and
// regular files; a symbolic link, a FIFO and a hard link to a file
// elsewhere are left as they are, and so is that file.
func TestOwnTPMDirGivesTheStateToTheVMsUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to give files away")
	}
	dir := t.TempDir()
	state := filepath.Join(dir, tpmDir)
	if err := ownTPMDir(state, 200001); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "tpm2-00.permall"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(state, "link")); err != nil {
		t.Fatal(err)
	}
	// A hard link the VM's user made of a file elsewhere, and a FIFO.
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(elsewhere, 4242, 4242); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(elsewhere, filepath.Join(state, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(state, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ownTPMDir(state, 200002); err != nil {
		t.Fatal(err)
	}
	for path, uid := range map[string]uint32{
		state: 200002, filepath.Join(state, "tpm2-00.permall"): 200002,
		filepath.Join(state, "link"): 0, outside: 0, filepath.Join(state, "fifo"): 0,
		filepath.Join(state, "hard"): 4242, elsewhere: 4242,
	} {
		var st syscall.Stat_t
		if err := syscall.Lstat(path, &st); err != nil || st.Uid != uid {
			t.Errorf("%s is %d's (%v), want %d's", path, st.Uid, err, uid)
		}
	}
	if fi, _ := os.Stat(state); fi.Mode().Perm() != 0o700 {
		t.Errorf("the state is %v", fi.Mode())
	}
	if err := disownTPMDir(state); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(state, "tpm2-00.permall"), &st); err != nil || st.Uid != 0 {
		t.Errorf("disowned, the state is %d's (%v)", st.Uid, err)
	}
	if err := syscall.Lstat(elsewhere, &st); err != nil || st.Uid != 4242 {
		t.Errorf("disowned, the hard link's file is %d's (%v), want 4242's", st.Uid, err)
	}
}
