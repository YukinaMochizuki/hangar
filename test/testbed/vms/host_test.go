//go:build testbed

// Package vms holds the real hypervisor driver (internal/node/driver/chvm)
// and its launcher, hangar-node vmm, to the drivers' contract on a real
// host, with real systemd and Cloud Hypervisor, and every other real
// driver about them: the conformance suite, graceful and forced stops, VMs
// outliving the agent, root disks refused by BLKZNAME, the console log
// under a flood and forged records, and leaving nothing but what was there
// (docs/runbook/testbed.md).
package vms

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/vishvananda/netlink"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/zfsstore"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// What the tests make on the host, all under these, which a run sweeps
// first and last: datasets under testParent, image files under imageRoot,
// the VMs' directories in vmDir and their units, hangar-vm-*.
const (
	pool       = "hangar"
	testParent = pool + "/testbed-vms"
	imageRoot  = "/var/lib/hangar/images/testbed-vms"
	vmDir      = "/var/tmp/hangar-testbed-vms"
	// decoyDir and the decoy units are not the driver's, and must outlive
	// every test.
	decoyDir = "/var/tmp/hangar-testbed-vms-decoy"
)

// decoyUnits run beside the driver's units, with names close to theirs.
var decoyUnits = []string{"hangar-vmdecoy.service", "hangar-decoy-vm-m1.service", "decoy-hangar-vm-m1.service"}

// busybox is pinned as the disk tests pin it.
const busyboxRef = "docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// testGrace is the drivers' grace for a stop, shorter than a host's to keep
// runs short; TestStopKillsWhatIgnoresSIGTERM uses the host's.
const testGrace = 3 * time.Second

func TestMain(m *testing.M) {
	// An agent of TestTheVMOutlivesTheAgent's: not a test run, and it
	// must not wait for the testbed's lock its parent holds.
	if job := os.Getenv(agentEnv); job != "" {
		os.Exit(agent(job))
	}
	vmtest.Main(m)
}

var sweepOnce sync.Once

// prepare sweeps what an earlier run left, once per run, under the
// testbed's lock, which vmtest.Main holds, and makes the decoys.
func prepare(t *testing.T) {
	t.Helper()
	sweepOnce.Do(func() {
		if _, err := os.Stat(vmtest.Artifacts().Node); err != nil {
			t.Fatalf("hangar-node: %v (make e2e-host builds it)", err)
		}
		if out, err := exec.Command("zpool", "list", "-H", "-o", "name", pool).CombinedOutput(); err != nil {
			t.Fatalf("no pool %s: run test/testbed/setup.sh first: %v: %s", pool, err, out)
		}
		sweep()
		vmtest.AtExit(sweep)
		mustRun(t, "zfs", "create", "-o", "mountpoint=none", testParent)
		if err := os.MkdirAll(decoyDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(decoyDir, chvm.ConsoleLog), []byte("decoy"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, u := range decoyUnits {
			mustRun(t, "systemd-run", "--unit", u, "--collect", "sleep", "infinity")
		}
	})
}

// sweep removes what a run makes, the driver's units and namespaces
// included, and the decoys.
func sweep() {
	units, _ := exec.Command("systemctl", "list-units", "--all", "--plain", "--no-legend", "hangar-vm-*.service").Output()
	for _, line := range strings.Split(string(units), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			_ = exec.Command("systemctl", "stop", f[0]).Run()
			_ = exec.Command("systemctl", "reset-failed", f[0]).Run()
		}
	}
	for _, u := range decoyUnits {
		_ = exec.Command("systemctl", "stop", u).Run()
	}
	if d, err := hostnet.New(hostnet.Config{Uplink: "lo"}); err == nil {
		ctx := context.Background()
		nets, _ := d.Networks(ctx)
		for _, n := range nets {
			_ = d.DeleteNetwork(ctx, n.UID)
		}
		_ = d.Close()
	}
	if exec.Command("zfs", "list", "-H", testParent).Run() == nil {
		if out, err := exec.Command("zfs", "destroy", "-r", testParent).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "vms: destroying %s: %v: %s\n", testParent, err, out)
		}
	}
	for _, d := range []string{imageRoot, vmDir, decoyDir, upgradeDir} {
		if err := os.RemoveAll(d); err != nil {
			fmt.Fprintf(os.Stderr, "vms: %v\n", err)
		}
	}
}

func mustRun(t testing.TB, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// uplink is the link of the testbed's default route.
func uplink(t *testing.T) string {
	t.Helper()
	routes, err := netlink.RouteGet(net.ParseIP("1.1.1.1"))
	if err != nil || len(routes) == 0 {
		t.Fatalf("no route out: %v", err)
	}
	l, err := netlink.LinkByIndex(routes[0].LinkIndex)
	if err != nil {
		t.Fatal(err)
	}
	return l.Attrs().Name
}

var hostCount atomic.Int32

// host is every real driver, on a dataset and directories of one test's
// own, with a registry serving what the test publishes.
type host struct {
	dataset, cache, scratch string
	reg                     *registrytest.Registry
	images                  *registry.Images
	storage                 *zfsstore.Store
	net                     *hostnet.Driver
	hv                      *chvm.Hypervisor
	published               int
}

// guestScript is what the guests run unless a test says otherwise: it
// exits with the code in /dev/shm/code on SIGUSR1, and says it got SIGTERM,
// and exits 0, on SIGTERM. Once its traps are set it leaves its pid in
// /dev/shm, which each boot has afresh, and says it is ready.
const guestScript = `trap 'exit $(cat /dev/shm/code)' USR1
trap 'echo got SIGTERM; exit 0' TERM
echo $$ > /dev/shm/main.pid
echo ready
while :; do sleep 1 & wait $!; done`

func newHost(t *testing.T) *host {
	t.Helper()
	return newHostOn(t, hostnet.Config{Uplink: uplink(t)})
}

// newHostOn is newHost with the network driver configured as netCfg says.
func newHostOn(t *testing.T, netCfg hostnet.Config) *host {
	t.Helper()
	prepare(t)
	n := hostCount.Add(1)
	h := &host{
		dataset: fmt.Sprintf("%s/h%d", testParent, n),
		cache:   filepath.Join(imageRoot, fmt.Sprint(n), "cache"),
		scratch: filepath.Join(imageRoot, fmt.Sprint(n), "scratch"),
		reg:     registrytest.New(t),
	}
	mustRun(t, "zfs", "create", "-o", "mountpoint=none", h.dataset)
	h.images, h.storage = h.reopen(t)
	nd, err := hostnet.New(netCfg)
	if err != nil {
		t.Fatal(err)
	}
	h.net = nd
	h.hv = newHypervisor(t, testGrace)
	t.Cleanup(func() {
		ctx := context.Background()
		// A driver of the cleanup's own: a test may have closed h.hv.
		hv := openHypervisor(t, testGrace)
		defer func() { _ = hv.Close() }()
		h.removeAll(t, ctx, hv)
		ifaces, _ := h.net.Interfaces(ctx)
		for _, i := range ifaces {
			_ = h.net.DeleteInterfaces(ctx, i.MachineUID)
		}
		nets, _ := h.net.Networks(ctx)
		for _, nw := range nets {
			_ = h.net.DeleteNetwork(ctx, nw.UID)
		}
		_ = h.net.Close()
		_, _ = exec.Command("zfs", "destroy", "-r", h.dataset).CombinedOutput()
		_ = os.RemoveAll(filepath.Join(imageRoot, fmt.Sprint(n)))
	})
	return h
}

// removeAll stops every VM and deletes every log, as the agent's sweep
// does once no machine is left.
func (h *host) removeAll(t *testing.T, ctx context.Context, hv *chvm.Hypervisor) {
	t.Helper()
	vms, err := hv.VMs(ctx)
	if err != nil {
		t.Errorf("listing VMs: %v", err)
	}
	for _, vm := range vms {
		if err := hv.Stop(ctx, vm.MachineUID); err != nil {
			t.Errorf("stopping %s: %v", vm.MachineUID, err)
		}
	}
	logs, err := hv.Logs(ctx)
	if err != nil {
		t.Errorf("listing logs: %v", err)
	}
	for _, uid := range logs {
		if err := hv.DeleteLogs(ctx, uid); err != nil {
			t.Errorf("deleting %s's logs: %v", uid, err)
		}
	}
}

func (h *host) reopen(t *testing.T) (*registry.Images, *zfsstore.Store) {
	t.Helper()
	images, err := registry.New(registry.Config{Cache: h.cache})
	if err != nil {
		t.Fatal(err)
	}
	storage, err := zfsstore.New(context.Background(), zfsstore.Config{Dataset: h.dataset, Scratch: h.scratch})
	if err != nil {
		t.Fatal(err)
	}
	return images, storage
}

// newHypervisor is a new instance of the driver, as the agent makes at its
// start, with the stop grace given.
func newHypervisor(t *testing.T, grace time.Duration) *chvm.Hypervisor {
	t.Helper()
	hv := openHypervisor(t, grace)
	t.Cleanup(func() { _ = hv.Close() })
	return hv
}

// openHypervisor is newHypervisor, for the caller to close.
func openHypervisor(t *testing.T, grace time.Duration) *chvm.Hypervisor {
	t.Helper()
	in := vmtest.Artifacts()
	ch, err := exec.LookPath("cloud-hypervisor")
	if err != nil {
		t.Fatal(err)
	}
	// It boots firmware VMs too, as a host's does, their guests given the
	// same grace once their power button is pressed: the conformance
	// suite's has nothing to boot, and its firmware never answers.
	hv, err := chvm.New(context.Background(), chvm.Config{
		Dir: vmDir, CloudHypervisor: ch, Kernel: in.Kernel, Initramfs: in.Initramfs, Cmdline: in.Cmdline,
		Launcher: []string{in.Node, "vmm"}, StopGrace: grace,
		Firmware: diskimgtest.Firmware(t), FirmwareStopGrace: grace,
	})
	if err != nil {
		t.Fatal(err)
	}
	return hv
}

var (
	busyboxOnce   sync.Once
	busyboxLayers []v1.Layer
	busyboxErr    error
)

// image is busybox, from Docker Hub once per run, with a layer of its own
// naming it, so each published image has a digest of its own.
func image(t *testing.T, label string) v1.Image {
	t.Helper()
	busyboxOnce.Do(func() {
		img, err := remote.Image(name.MustParseReference(busyboxRef), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
		if err != nil {
			busyboxErr = err
			return
		}
		layers, err := img.Layers()
		if err != nil {
			busyboxErr = err
			return
		}
		for _, l := range layers {
			rc, err := l.Compressed()
			if err != nil {
				busyboxErr = err
				return
			}
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				busyboxErr = err
				return
			}
			kept, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
			if err != nil {
				busyboxErr = err
				return
			}
			busyboxLayers = append(busyboxLayers, kept)
		}
	})
	if busyboxErr != nil {
		t.Fatalf("busybox from Docker Hub: %v", busyboxErr)
	}
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/hangar-image", Mode: 0o644, Size: int64(len(label))})
	_, _ = w.Write([]byte(label))
	_ = w.Close()
	mine, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: "amd64", Config: v1.Config{Cmd: []string{"sh"}}})
	if err != nil {
		t.Fatal(err)
	}
	if img, err = mutate.AppendLayers(img, append(append([]v1.Layer{}, busyboxLayers...), mine)...); err != nil {
		t.Fatal(err)
	}
	return img
}

func (h *host) publish(t *testing.T, ref string) (string, string) {
	t.Helper()
	h.published++
	return h.reg.Push(t, ref, image(t, fmt.Sprintf("%s %d", ref, h.published)))
}

// machine makes what a machine's VM needs, a root disk and an interface on
// a network of its own, and returns its spec, running script.
func (h *host) machine(t *testing.T, uid string, restart driver.RestartPolicy, script string) driver.VMSpec {
	t.Helper()
	ctx := context.Background()
	ref, digest := h.publish(t, "app:"+uid)
	if err := h.storage.EnsureBase(ctx, digest, 0, func(dir string) error {
		return h.images.Unpack(ctx, ref, digest, dir, driver.Anonymous)
	}); err != nil {
		t.Fatal(err)
	}
	root, err := h.storage.EnsureRoot(ctx, uid, digest, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	net := testNet()
	if err := h.net.EnsureNetwork(ctx, net); err != nil {
		t.Fatal(err)
	}
	n := hostCount.Add(1) // an address of its own
	nic, err := h.net.EnsureInterface(ctx, driver.Interface{
		MachineUID: uid, Name: "eth0", NetworkUID: net.UID, MAC: fmt.Sprintf("02:00:00:00:%02x:01", n%256),
		Address: mustPrefix(fmt.Sprintf("10.0.0.%d/24", 2+n%250)), Gateway: mustAddr("10.0.0.1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return driver.VMSpec{
		MachineUID: uid, Machine: "testbed/" + uid, VCPUs: 1, MemoryMiB: 256,
		Root: root, Interfaces: []driver.Interface{nic}, Nameservers: []string{"1.1.1.1"},
		Run:     driver.RunSpec{Command: []string{"/bin/sh", "-c", script}},
		Restart: restart,
	}
}

// guest reaches the guest agents of the VMs the driver starts, and of one
// a launcher without VM users started (TestAnUpgradeKeepsVMsRunning).
func guest() chguest.Guest {
	return chguest.Guest{Socket: func(uid string) string {
		if fi, err := os.Lstat(filepath.Join(vmDir, uid, "vsock")); err == nil && fi.Mode().Type() == os.ModeSocket {
			return filepath.Join(vmDir, uid, "vsock")
		}
		return filepath.Join(vmDir, uid, "ch", "vsock")
	}}
}

// ready waits for the machine's guest agent to answer.
func ready(t *testing.T, uid string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := guest().Ready(ctx, uid)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			dump(t, uid)
			t.Fatalf("machine %s's guest never answered: %v", uid, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// execIn runs a process in the machine's guest and returns its stdout and
// exit code.
func execIn(ctx context.Context, uid string, args ...string) (string, int32, error) {
	c, done := guest().Client(uid)
	defer done()
	stream := c.Exec(ctx)
	defer func() { _ = stream.CloseResponse() }()
	start := &guestv1.ExecStart{Process: &guestv1.Process{Args: args}}
	if err := stream.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Start{Start: start}}); err != nil {
		return "", 0, err
	}
	var out bytes.Buffer
	for {
		msg, err := stream.Receive()
		if err != nil {
			return out.String(), 0, err
		}
		switch m := msg.GetMessage().(type) {
		case *guestv1.ExecResponse_Stdout:
			out.Write(m.Stdout)
		case *guestv1.ExecResponse_Exit:
			_ = stream.CloseRequest()
			return out.String(), m.Exit.GetCode(), nil
		}
	}
}

// exit ends the guest's main process, running guestScript, with code. The
// guest may power off before the exec's answer comes, which is no error.
func exit(t *testing.T, uid string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The process may not have set its traps yet when the agent answers.
	_, _, err := execIn(ctx, uid, "sh", "-c", fmt.Sprintf(
		"while [ ! -s /dev/shm/main.pid ]; do sleep 0.05; done; echo %d > /dev/shm/code && kill -USR1 $(cat /dev/shm/main.pid)", code))
	if err != nil && !errors.Is(err, io.EOF) {
		t.Logf("exec to end the process: %v (the guest may have powered off first)", err)
	}
}

// dump logs what the host saw of a machine's VM.
func dump(t *testing.T, uid string) {
	t.Helper()
	out, _ := exec.Command("journalctl", "--no-pager", "-n", "50", "-u", "hangar-vm-"+uid+".service").CombinedOutput()
	t.Logf("--- journal of hangar-vm-%s:\n%s", uid, out)
	for _, f := range []string{chvm.SerialLog, "exit.json"} {
		b, _ := os.ReadFile(filepath.Join(vmDir, uid, f))
		t.Logf("--- %s:\n%s", f, b)
	}
	for _, r := range consoleOf(t, uid) {
		t.Logf("console %d: %q", r.Stream, r.Payload)
	}
}

// listing logs what is on the host of the VMs: units, directories,
// Cloud Hypervisor processes, and returns them.
func listing(t *testing.T, when string) (units, dirs, procs string) {
	t.Helper()
	u, _ := exec.Command("systemctl", "list-units", "--all", "--plain", "--no-legend", "hangar-*", "decoy-*").CombinedOutput()
	d, _ := exec.Command("find", vmDir, decoyDir, "-maxdepth", "2").CombinedOutput()
	p, _ := exec.Command("pgrep", "-a", "-f", "^([^ ]*/)?cloud-hypervisor( |$)").CombinedOutput()
	t.Logf("===== %s\n--- units:\n%s--- files:\n%s--- cloud-hypervisor:\n%s", when, u, d, p)
	return string(u), string(d), string(p)
}
