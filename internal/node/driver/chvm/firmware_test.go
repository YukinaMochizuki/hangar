package chvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// firmwareDir is launcherDir with the launch made a firmware VM's: two
// volumes, the root first, and no root disk.
func firmwareDir(t *testing.T, mode string, mutate ...func(*driver.VMSpec)) (*Launcher, string) {
	t.Helper()
	l, dir := launcherDir(t, "m1", mode)
	ln, err := readLaunch(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln.Spec.Root = driver.Disk{}
	ln.Spec.Run = driver.RunSpec{}
	ln.Spec.Firmware = true
	ln.Spec.Disks = []driver.Volume{
		{DiskUID: "root", Path: "/dev/zvol/hangar/disks/root", Size: 10 << 30},
		{DiskUID: "data", Path: "/dev/zvol/hangar/disks/data", Size: 1 << 30},
	}
	for _, f := range mutate {
		f(&ln.Spec)
	}
	ln.Hash = ln.Spec.Hash()
	ln.Kernel, ln.Initramfs, ln.Cmdline, ln.Firmware = "", "", "", "/fw/CLOUDHV.fd"
	// Long enough for a fake guest on a busy machine to power off.
	ln.StopGrace = 10 * time.Second
	b, err := json.Marshal(ln)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, specFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, bootFile)); err != nil {
		t.Fatal(err)
	}
	var opened []string
	l.OpenDisk = func(path string) (*os.File, error) {
		opened = append(opened, path)
		return os.CreateTemp(l.Dir, "dev")
	}
	t.Cleanup(func() {
		if want := []string{ln.Spec.Disks[0].Path, ln.Spec.Disks[1].Path}; !slices.Equal(opened, want) && !t.Failed() {
			t.Errorf("opened %q, want the volumes %q alone", opened, want)
		}
	})
	return l, dir
}

func chArgsOf(t *testing.T, dir string) []string {
	t.Helper()
	var ch struct{ Args []string }
	b, _ := os.ReadFile(filepath.Join(dir, chDir, "args.json"))
	if err := json.Unmarshal(b, &ch); err != nil {
		t.Fatal(err)
	}
	return ch.Args
}

// TestTheNoCloudSerialSaysNet: the serial number, split as cloud-init
// splits it, names NoCloud, seeds it from the metadata service, and says
// dsmode net, which cloud-init 22.4 needs to keep an http seed and every
// later one honours first.
func TestTheNoCloudSerialSaysNet(t *testing.T) {
	fields := strings.Split(NoCloudSerial, ";")
	if fields[0] != "ds=nocloud" {
		t.Errorf("%q does not start with ds=nocloud", NoCloudSerial)
	}
	kv := map[string]string{}
	for _, f := range fields[1:] {
		k, v, _ := strings.Cut(f, "=")
		kv[k] = v
	}
	if want := "http://" + driver.MetadataAddr.String() + "/nocloud/"; kv["s"] != want {
		t.Errorf("seedfrom %q, not %q", kv["s"], want)
	}
	if kv["dsmode"] != "net" {
		t.Errorf("dsmode %q, not net", kv["dsmode"])
	}
}

// TestAFirmwareVMBootsItsOwnKernel: Cloud Hypervisor gets the firmware,
// its volumes, the root first, as vda and on, the NoCloud serial number, an
// API socket, its serial console, and the balloon its freed memory goes
// back through; no platform kernel, boot disk, vsock or virtio console.
func TestAFirmwareVMBootsItsOwnKernel(t *testing.T) {
	l, dir := firmwareDir(t, "exit:0")
	if got := runFor(t, l, t.Context(), "m1"); got != 0 {
		t.Fatalf("exited %d", got)
	}
	args := chArgsOf(t, dir)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--firmware /fw/CLOUDHV.fd",
		"--balloon size=0,free_page_reporting=on",
		`--platform system_serial_number="ds=nocloud;s=http://169.254.169.254/nocloud/;dsmode=net"`,
		"--disk path=" + filepath.Join(dir, volumeNode(0)) + ",image_type=raw path=" + filepath.Join(dir, volumeNode(1)) + ",image_type=raw --net",
		"--api-socket path=" + filepath.Join(dir, apiFile),
		"--console off",
		"--serial file=" + filepath.Join(dir, serialFIFO),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Cloud Hypervisor's arguments %q lack %q", joined, want)
		}
	}
	for _, not := range []string{"--kernel", "--initramfs", "--cmdline", "--vsock", bootFile} {
		if strings.Contains(joined, not) {
			t.Errorf("a firmware VM's arguments %q have %q", joined, not)
		}
	}
}

// TestQuoteOptionRoundTrips: a quoted value reads back, as Cloud
// Hypervisor's option parser reads it (option_parser's split_commas and
// dequote, v53.0, rewritten here), as itself, whatever printable ASCII it
// holds, and the option after it is still an option of its own.
func TestQuoteOptionRoundTrips(t *testing.T) {
	for _, v := range []string{
		"ARX-0001", NoCloudSerial, `a,b"c[d]=e`, `"`, `""`, ",", "[", "]]", " lead and trail ", "x=y,z=w", `\"`,
	} {
		opts := chSplitCommas(t, "system_serial_number="+quoteOption(v)+",uuid=u")
		if len(opts) != 2 || opts[1] != "uuid=u" {
			t.Errorf("%q: options %q, not the serial number and uuid=u", v, opts)
			continue
		}
		key, val, _ := strings.Cut(opts[0], "=")
		if got := chDequote(strings.TrimSpace(val)); key != "system_serial_number" || got != v {
			t.Errorf("%q reads back as %s=%q", v, key, got)
		}
	}
}

// chSplitCommas splits options as Cloud Hypervisor does: at commas outside
// double quotes and brackets.
func chSplitCommas(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	var cur strings.Builder
	quoted, brackets := false, 0
	for _, c := range strings.TrimSpace(s) {
		switch {
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '[':
			brackets++
		case c == ']':
			if brackets == 0 {
				t.Fatalf("%q: unbalanced brackets", s)
			}
			brackets--
		case c == ',' && brackets == 0:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(c)
	}
	if quoted || brackets != 0 {
		t.Fatalf("%q: unbalanced quotes or brackets", s)
	}
	return append(out, cur.String())
}

// chDequote drops double quotes as Cloud Hypervisor does, a quote right
// after a closing one kept as one.
func chDequote(s string) string {
	var out strings.Builder
	var prev rune
	quoted := false
	for _, c := range s {
		if c == '"' {
			if prev == '"' && !quoted {
				out.WriteRune('"')
			}
			quoted = !quoted
		} else {
			out.WriteRune(c)
		}
		prev = c
	}
	return out.String()
}

// TestAFirmwareVMGetsItsOwnSerialNumber: a spec's serial number replaces
// the NoCloud one, quoted; a bad one, or one on a VM that boots no
// firmware, is refused before anything starts.
func TestAFirmwareVMGetsItsOwnSerialNumber(t *testing.T) {
	l, dir := firmwareDir(t, "exit:0", func(s *driver.VMSpec) { s.SystemSerialNumber = `ARX,"01"` })
	if got := runFor(t, l, t.Context(), "m1"); got != 0 {
		t.Fatalf("exited %d", got)
	}
	joined := strings.Join(chArgsOf(t, dir), " ")
	if want := `--platform system_serial_number="ARX,""01"""`; !strings.Contains(joined, want) {
		t.Errorf("Cloud Hypervisor's arguments %q lack %q", joined, want)
	}
	if strings.Contains(joined, "nocloud") {
		t.Errorf("Cloud Hypervisor's arguments %q still point cloud-init at the platform", joined)
	}

	firmware := driver.VMSpec{MachineUID: "m1", VCPUs: 1, MemoryMiB: 256, Firmware: true, Disks: []driver.Volume{{DiskUID: "d1", Path: "/dev/zvol/d"}}}
	for name, mutate := range map[string]func(*driver.VMSpec){
		"65 bytes":  func(s *driver.VMSpec) { s.SystemSerialNumber = strings.Repeat("a", 65) },
		"a newline": func(s *driver.VMSpec) { s.SystemSerialNumber = "a\nb" },
		"UTF-8":     func(s *driver.VMSpec) { s.SystemSerialNumber = "ü" },
		"no firmware": func(s *driver.VMSpec) {
			s.Firmware, s.Disks = false, nil
			s.Root = driver.Disk{MachineUID: "m1", Path: "/dev/zvol/r"}
			s.SystemSerialNumber = "ARX-01"
		},
	} {
		spec := firmware
		mutate(&spec)
		if err := checkSpec(spec); err == nil {
			t.Errorf("%s: checkSpec takes it", name)
		}
	}
	firmware.SystemSerialNumber = strings.Repeat("~", 64)
	if err := checkSpec(firmware); err != nil {
		t.Errorf("64 bytes of printable ASCII: %v", err)
	}
}

// TestAFirmwareVMsConsoleIsItsSerial: the serial console is framed into
// the console log, numbered by boot, each boot ended by whether the guest
// powered off; nothing goes to the serial log, and what Cloud Hypervisor
// prints on its stdout is no record.
func TestAFirmwareVMsConsoleIsItsSerial(t *testing.T) {
	for _, tt := range []struct {
		mode string
		code int
	}{
		{"exit:0", 0}, // powered off: Cloud Hypervisor exits 0
		{"crash", ExitNoRecord},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			l, dir := firmwareDir(t, tt.mode)
			if got := runFor(t, l, t.Context(), "m1"); got != tt.code {
				t.Errorf("exited %d, want %d", got, tt.code)
			}
			recs := records(t, filepath.Join(dir, ConsoleLog))
			var streams []console.Stream
			var out string
			for _, r := range recs {
				streams = append(streams, r.Stream)
				if r.Stream == console.Stdout {
					out += string(r.Payload)
				}
			}
			if want := []console.Stream{console.Boot, console.Stdout, console.Exit}; !slices.Equal(streams, want) {
				t.Errorf("console log streams %v, want %v", streams, want)
			}
			if out != "kernel: hello\n" {
				t.Errorf("the serial console in the log: %q", out)
			}
			// Ended on its own, not stopped.
			if st, err := console.ExitOf(recs[len(recs)-1]); err != nil || int(st.Code) != tt.code || st.Signal != 0 {
				t.Errorf("the boot ended with %+v %v, want %d and no signal", st, err, tt.code)
			}
			if exists(filepath.Join(dir, SerialLog)) {
				t.Error("a firmware VM's serial console went to the serial log too")
			}
			if r, ok := exitOf(t, dir); !ok || r.Code != tt.code {
				t.Errorf("exit record %+v %t", r, ok)
			}
		})
	}
}

// TestAFirmwareVMStoppedBySystemdPowersOff: stopped as at host shutdown,
// the launcher presses the VM's power button, and the guest powering off
// ends the boot well, as the platform's.
func TestAFirmwareVMStoppedBySystemdPowersOff(t *testing.T) {
	l, dir := firmwareDir(t, "stoppable")
	var pressed []string
	l.PowerButton = func(_ context.Context, socket string) error {
		pressed = append(pressed, socket)
		return os.WriteFile(filepath.Join(dir, chDir, "stop"), nil, 0o600)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() { done <- l.Run(ctx, "m1") }()
	testutil.Eventually(t, "Cloud Hypervisor started", func() bool { return exists(filepath.Join(dir, chDir, "args.json")) })
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("a stopped VM's launcher exited %d", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the launcher did not return")
	}
	if want := []string{filepath.Join(dir, apiFile)}; !slices.Equal(pressed, want) {
		t.Errorf("pressed %q, want %q", pressed, want)
	}
	recs := records(t, filepath.Join(dir, ConsoleLog))
	if st, err := console.ExitOf(recs[len(recs)-1]); err != nil || st.Code != 0 || st.Signal != driver.StoppedSignal {
		t.Errorf("the boot ended with %+v %v, want a power-off the platform asked for", st, err)
	}
	if exists(filepath.Join(dir, exitFile)) {
		t.Error("a VM stopped as at host shutdown has an exit record: it would read as having powered off")
	}
}

// TestAFirmwareVMPoweredOffByTheDriversStop: a guest powering off once the
// driver, marking the VM as being stopped, pressed its power button ends
// its boot as the platform's, and its launcher waits for the stop.
func TestAFirmwareVMPoweredOffByTheDriversStop(t *testing.T) {
	l, dir := firmwareDir(t, "exit:0")
	if err := os.WriteFile(filepath.Join(dir, stopFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() { done <- l.Run(ctx, "m1") }()
	testutil.Eventually(t, "the guest powered off", func() bool { return exists(filepath.Join(dir, exitFile)) })
	select {
	case code := <-done:
		t.Fatalf("the launcher exited %d before it was stopped", code)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exited %d", code)
	}
	recs := records(t, filepath.Join(dir, ConsoleLog))
	if st, err := console.ExitOf(recs[len(recs)-1]); err != nil || st.Code != 0 || st.Signal != driver.StoppedSignal {
		t.Errorf("the boot ended with %+v %v, want a power-off the platform asked for", st, err)
	}
}

func TestPressPowerButton(t *testing.T) {
	socket := filepath.Join(shortDir(t), "api")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	if err := pressPowerButton(t.Context(), socket); err != nil {
		t.Fatal(err)
	}
	if want := []string{"PUT /api/v1/vm.power-button"}; !slices.Equal(got, want) {
		t.Errorf("asked %q, want %q", got, want)
	}
	if err := pressPowerButton(t.Context(), socket+".none"); err == nil {
		t.Error("pressed the button of no VM")
	}
	// Cloud Hypervisor may write in its directory: a link it plants where
	// its API socket goes, to another VM's, is not followed.
	planted := filepath.Join(filepath.Dir(socket), "planted")
	if err := os.Symlink(socket, planted); err != nil {
		t.Fatal(err)
	}
	if err := pressPowerButton(t.Context(), planted); !errors.Is(err, chguest.ErrNotSocket) {
		t.Errorf("through a link: %v, want ErrNotSocket", err)
	}
	if len(got) != 1 {
		t.Errorf("the socket a link leads to was asked %q", got)
	}
}

// firmwareDriver is a driver whose firmware VMs' power buttons press as
// button says, with a grace of testGrace once pressed.
func firmwareDriver(t *testing.T, s *fakeSystemd, button func(ctx context.Context, socket string) error) *Hypervisor {
	t.Helper()
	fw := filepath.Join(t.TempDir(), "CLOUDHV.fd")
	if err := os.WriteFile(fw, []byte("fw"), 0o644); err != nil {
		t.Fatal(err)
	}
	return newDriverWith(t, shortDir(t), s, &fakeGuest{}, func(c *Config) {
		c.Firmware, c.FirmwareStopGrace, c.powerButton = fw, testGrace, button
	})
}

// startFirmware starts a firmware VM for uid, with a console log.
func startFirmware(t *testing.T, h *Hypervisor, uid string) {
	t.Helper()
	spec := testSpec(uid, driver.RestartOnFailure)
	spec.Root, spec.Run, spec.Firmware = driver.Disk{}, driver.RunSpec{}, true
	spec.Disks = []driver.Volume{{DiskUID: "root", Path: "/dev/zvol/hangar/disks/root", Size: 10 << 30}}
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir(uid), ConsoleLog), []byte("log"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestOnlyAGuestPoweringOffKeepsAFirmwareVMOff: a VirtualMachine's
// firmware VM, restarted on failure, whose unit went at a host shutdown or
// with the host, no exit recorded, is not listed after the reboot, for the
// agent to start again; one whose guest powered off on its own has exited
// 0, and stays so.
func TestOnlyAGuestPoweringOffKeepsAFirmwareVMOff(t *testing.T) {
	s := newFakeSystemd()
	h := firmwareDriver(t, s, nil)
	startFirmware(t, h, "rebooted")
	startFirmware(t, h, "off")
	s.mu.Lock()
	delete(s.units, unitName("rebooted"))
	s.mu.Unlock()
	s.exit(t, h, "off", 0)
	if vm, ok := listed(t, h, "rebooted"); ok {
		t.Errorf("a VM its host's reboot ended is listed, %+v: the agent would not start it again", vm)
	}
	if vm, ok := listed(t, h, "off"); !ok || vm.State != driver.VMExited || vm.ExitCode != 0 {
		t.Errorf("a VM whose guest powered off is listed %+v %t, want exited 0", vm, ok)
	}
}

// TestStopPressesAFirmwareVMsPowerButton: the driver stops a firmware VM
// by its power button at its API socket, the VM marked as being stopped,
// never by its guest's agent; a guest that powers off is not waited out,
// one that stays up is killed after the firmware grace and the slack, and
// one whose button does not press is killed at once.
func TestStopPressesAFirmwareVMsPowerButton(t *testing.T) {
	for name, tt := range map[string]struct {
		powersOff bool
		buttonErr error
		min, max  time.Duration
	}{
		"powering off":  {powersOff: true, max: testGrace / 2},
		"staying up":    {min: testGrace + testSlack, max: testGrace + testSlack + time.Second},
		"no API socket": {buttonErr: chguest.ErrNotSocket, max: testGrace / 2},
	} {
		t.Run(name, func(t *testing.T) {
			s := newFakeSystemd()
			var h *Hypervisor
			var pressed []string
			var marked []bool
			h = firmwareDriver(t, s, func(_ context.Context, socket string) error {
				pressed = append(pressed, socket)
				marked = append(marked, exists(filepath.Join(h.dir("m1"), stopFile)))
				if tt.powersOff {
					time.AfterFunc(20*time.Millisecond, func() { writeExit(t, h, "m1", 0) })
				}
				return tt.buttonErr
			})
			g := h.guest.(*fakeGuest)
			startFirmware(t, h, "m1")
			start := time.Now()
			if err := h.Stop(t.Context(), "m1"); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < tt.min || d > tt.max {
				t.Errorf("took %v, want between %v and %v", d, tt.min, tt.max)
			}
			if want := []string{filepath.Join(h.dir("m1"), apiFile)}; !slices.Equal(pressed, want) || !marked[0] {
				t.Errorf("pressed %q, marked as stopping %v; want %q, marked", pressed, marked, want)
			}
			if len(g.asked) != 0 {
				t.Errorf("a firmware VM's guest agent was asked to stop: %v", g.asked)
			}
			stopped(t, h, s, "m1")
		})
	}
}

// TestStartWritesAFirmwareVM: a firmware VM's launch names the firmware,
// no kernel or boot disk; its unit binds the firmware and may open its
// volumes alone, and systemd waits for the firmware grace when it stops
// it.
func TestStartWritesAFirmwareVM(t *testing.T) {
	s := newFakeSystemd()
	dir := shortDir(t)
	fw := filepath.Join(t.TempDir(), "CLOUDHV.fd")
	if err := os.WriteFile(fw, []byte("fw"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newDriverWith(t, dir, s, &fakeGuest{}, func(c *Config) { c.Firmware = fw })
	spec := testSpec("m1", driver.RestartNever)
	spec.Root, spec.Run, spec.Firmware = driver.Disk{}, driver.RunSpec{}, true
	spec.Disks = []driver.Volume{{DiskUID: "root", Path: "/dev/zvol/hangar/disks/root", Size: 10 << 30}}
	if err := h.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	ln, err := readLaunch(h.dir("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if ln.Firmware != fw || ln.Kernel != "" || ln.Initramfs != "" || ln.StopGrace != DefaultFirmwareStopGrace {
		t.Errorf("launch %+v", ln)
	}
	if exists(filepath.Join(h.dir("m1"), bootFile)) {
		t.Error("a firmware VM got a boot disk")
	}
	props := s.props["hangar-vm-m1.service"]
	if got, want := prop(props, "DeviceAllow"), []pathPair{{"/dev/kvm", "rw"}, {"/dev/net/tun", "rw"}, {spec.Disks[0].Path, "rwm"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("DeviceAllow=%v, want %v", got, want)
	}
	if got, want := prop(props, "TimeoutStopUSec"), usec(stopAsk+DefaultFirmwareStopGrace+testSlack+killGrace+drainWait); got != want {
		t.Errorf("TimeoutStopUSec=%v, want %d", got, want)
	}
	// /run/netns, the launcher, Cloud Hypervisor and the firmware: no
	// kernel or initramfs.
	if binds := prop(props, "BindReadOnlyPaths").([]bindPath); len(binds) != 4 || binds[3].Source != fw {
		t.Errorf("BindReadOnlyPaths=%v, want the firmware and no kernel", binds)
	}

	// Without firmware, the host refuses such a VM.
	bare := newDriver(t, shortDir(t), newFakeSystemd(), &fakeGuest{})
	if err := bare.Start(t.Context(), spec); err == nil {
		t.Error("a host without firmware started a firmware VM")
	}
	// Nor is a firmware VM given a root disk, or no volume.
	for name, change := range map[string]func(*driver.VMSpec){
		"a root disk": func(s *driver.VMSpec) { s.Root = driver.Disk{MachineUID: "m2", Path: "/dev/zvol/r"} },
		"no volume":   func(s *driver.VMSpec) { s.Disks = nil },
	} {
		bad := spec
		bad.MachineUID = "m2"
		change(&bad)
		if err := h.Start(t.Context(), bad); err == nil {
			t.Errorf("%s: started", name)
		}
	}
}

// lockedBuffer is a log the test reads while its writer runs.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.b.Bytes())
}

// TestTheSerialConsoleIsFramedByLine: a console written a byte at a time is
// framed at line ends, and a prompt with no line end once it is quiet.
func TestTheSerialConsoleIsFramedByLine(t *testing.T) {
	var log lockedBuffer
	synctest.Test(t, func(t *testing.T) {
		r, w := io.Pipe()
		f := &serialFramer{log: &log, now: time.Now}
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.pump(r)
		}()
		for _, b := range []byte("Ubuntu 24.04 LTS\nweb login: ") {
			if _, err := w.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
		// Quiet for less than serialFlush, the prompt waits for more.
		time.Sleep(serialFlush - time.Nanosecond) //nolint:forbidigo // virtual time
		synctest.Wait()
		if bytes.Contains(log.Bytes(), []byte("web login")) {
			t.Error("the prompt was framed before the console was quiet")
		}
		time.Sleep(time.Nanosecond) //nolint:forbidigo // virtual time
		synctest.Wait()
		_, _ = w.Write([]byte(strings.Repeat("x", maxSerialRecord+1)))
		_ = w.Close()
		<-done
	})
	var got []string
	rd := console.NewReader(bytes.NewReader(log.Bytes()))
	for {
		rec, err := rd.Next()
		if err != nil {
			break
		}
		got = append(got, string(rec.Payload))
	}
	want := []string{"Ubuntu 24.04 LTS\n", "web login: ", strings.Repeat("x", maxSerialRecord+1)}
	if !slices.Equal(got, want) {
		t.Errorf("records %q, want %q", got, want)
	}
}
