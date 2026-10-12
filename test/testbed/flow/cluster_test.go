//go:build testbed

// Package flow is the full flow on a real host (L4 in
// docs/decisions/testing-strategy.md): the real hangar-apiserver, and the
// real hangar-node with --drivers=host talking to it over its node socket,
// both as processes, driven by the real hangar CLI. Containers are applied,
// run, reach the internet, restart, grow their disks, change their images,
// outlive a killed node and are deleted, and every step is checked from the
// API and from the host (docs/runbook/testbed.md).
package flow

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// What a run makes on the host, all under these, which a run sweeps first
// and last: the apiserver's and the CLI's files and the VMs' directories
// under workDir, datasets under dataset, image files under imageDir, and the
// units hangar-vm-*.
const (
	pool     = "hangar"
	dataset  = pool + "/testbed-flow"
	imageDir = "/var/lib/hangar/images/testbed-flow"
	workDir  = "/var/tmp/hangar-testbed-flow"
	// apiDir holds the apiserver's database and node socket, and its
	// binary: the apiserver runs as nobody, who cannot read a checkout in
	// a home directory.
	apiDir = workDir + "/api"
	vmDir  = workDir + "/vms"
	// nodeName is the Node the apiserver registers and the agent reports.
	nodeName = "testbed"
	tenant   = "flow"
	project  = "flow"
	admin    = "root@testbed.invalid"
)

// The images are pinned by digest, as the disk tests pin them: busybox 1.37,
// debian trixie and nginx 1.29.
const (
	nginxDigest   = "sha256:1881968aff6f7cdcc4b888c00a11f4ce241ad7ec957e0cb4a9e19e93a3ff87ea"
	nginxRef      = "docker.io/library/nginx@" + nginxDigest
	busyboxDigest = "sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
	busyboxRef    = "docker.io/library/busybox@" + busyboxDigest
	debianDigest  = "sha256:913f6706df59a68922d1dd08f78c2476560a8d367897200a6005b00e5f67c2d5"
	debianRef     = "docker.io/library/debian@" + debianDigest
)

func TestMain(m *testing.M) { vmtest.Main(m) }

// cluster is an apiserver and a node agent on this host, and a CLI
// configured for them.
type cluster struct {
	t      *testing.T
	uplink string
	api    *exec.Cmd
	node   *exec.Cmd
	// nodeDone is closed once the agent has exited.
	nodeDone chan struct{}
	nodeRuns int
}

// startCluster sweeps what an earlier run left, starts the apiserver and the
// agent, and makes the tenant and project the tests use.
func startCluster(t *testing.T) *cluster {
	t.Helper()
	in := vmtest.Artifacts()
	for _, p := range []string{in.Node, in.Apiserver, in.CLI} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%v (make e2e-host builds it)", err)
		}
	}
	if out, err := exec.Command("zpool", "list", "-H", "-o", "name", pool).CombinedOutput(); err != nil {
		t.Fatalf("no pool %s: run test/testbed/setup.sh first: %v: %s", pool, err, out)
	}
	sweep()
	vmtest.AtExit(sweep)
	mustRun(t, "zfs", "create", "-o", "mountpoint=none", dataset)
	for _, d := range []string{apiDir, vmDir, imageDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The forwarding the network driver needs; vmtest.Main turns it on too.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0); err != nil {
		t.Fatal(err)
	}
	c := &cluster{t: t, uplink: uplink(t)}
	c.startAPI()
	t.Cleanup(c.stop)
	c.startNode()
	c.cli("config", "set-context", "testbed", "--endpoint", "http://"+c.apiAddr(), "--auth", "dev", "--user", admin, "--project", project)
	c.applyYAML(fmt.Sprintf(`apiVersion: tenancy.hangar.dev/v1alpha1
kind: Tenant
metadata: {name: %s}
---
apiVersion: tenancy.hangar.dev/v1alpha1
kind: Project
metadata: {name: %s}
spec: {tenant: %s}
`, tenant, project, tenant))
	c.cli("wait", "node", nodeName, "--for=condition=Ready", "--timeout=60s")
	return c
}

var apiPort int

func (c *cluster) apiAddr() string { return fmt.Sprintf("127.0.0.1:%d", apiPort) }

// startAPI runs the apiserver as nobody, as an unprivileged user runs it on
// a host, with dev auth on loopback and the node socket accepting root.
func (c *cluster) startAPI() {
	t := c.t
	t.Helper()
	nobody := lookupUID(t, "nobody")
	bin := filepath.Join(apiDir, "hangar-apiserver")
	copyFile(t, vmtest.Artifacts().Apiserver, bin, 0o755)
	if err := os.Chown(apiDir, nobody, nobody); err != nil {
		t.Fatal(err)
	}
	apiPort = freePort(t)
	log := openLog(t, "apiserver.log")
	cmd := exec.Command(bin,
		"--db", filepath.Join(apiDir, "hangar.db"),
		"--listen", c.apiAddr(),
		"--auth-mode", "dev",
		"--socket", filepath.Join(apiDir, "node.sock"),
		"--node-uid", "0",
		"--node-name", nodeName,
		"--bootstrap-admin", admin,
	)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(nobody), Gid: uint32(nobody)},
		Pdeathsig:  syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c.api = cmd
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := net.Dial("tcp", c.apiAddr())
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the apiserver never listened: %v\n%s", err, tail(filepath.Join(workDir, "apiserver.log")))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startNode runs the agent, as root, on the host's real drivers, over the
// test's dataset and directories.
func (c *cluster) startNode() {
	t := c.t
	t.Helper()
	in := vmtest.Artifacts()
	ch, err := exec.LookPath("cloud-hypervisor")
	if err != nil {
		t.Fatal(err)
	}
	c.nodeRuns++
	log := openLog(t, fmt.Sprintf("node-%d.log", c.nodeRuns))
	cmd := exec.Command(in.Node,
		"--name", nodeName,
		"--socket", filepath.Join(apiDir, "node.sock"),
		"--drivers", "host",
		"--uplink", c.uplink,
		"--zfs-dataset", dataset,
		"--image-dir", imageDir,
		"--cloud-hypervisor", ch,
		"--guest-kernel", in.Kernel,
		"--guest-initramfs", in.Initramfs,
		"--guest-cmdline", in.Cmdline,
		"--vm-dir", vmDir,
	)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	c.node, c.nodeDone = cmd, done
}

// killNode kills the agent outright, as a crash or an OOM kill would.
func (c *cluster) killNode() {
	c.t.Helper()
	if err := c.node.Process.Signal(syscall.SIGKILL); err != nil {
		c.t.Fatal(err)
	}
	<-c.nodeDone
}

// stop stops the agent and the apiserver as their units would, and logs the
// tail of their logs if the test failed.
func (c *cluster) stop() {
	if c.node != nil {
		_ = c.node.Process.Signal(syscall.SIGTERM)
		select {
		case <-c.nodeDone:
		case <-time.After(30 * time.Second):
			_ = c.node.Process.Kill()
			<-c.nodeDone
		}
	}
	if c.api != nil {
		_ = c.api.Process.Signal(syscall.SIGTERM)
		_ = c.api.Wait()
	}
	if c.t.Failed() {
		c.t.Logf("--- apiserver.log:\n%s", tail(filepath.Join(workDir, "apiserver.log")))
		for i := 1; i <= c.nodeRuns; i++ {
			c.t.Logf("--- node-%d.log:\n%s", i, tail(filepath.Join(workDir, fmt.Sprintf("node-%d.log", i))))
		}
	}
}

// cli runs the hangar CLI and returns its stdout, failing the test if it
// fails.
func (c *cluster) cli(args ...string) string {
	c.t.Helper()
	out, err := c.tryCLI(nil, args...)
	if err != nil {
		c.t.Fatalf("hangar %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func (c *cluster) tryCLI(stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command(vmtest.Artifacts().CLI, args...)
	cmd.Env = append(os.Environ(), "HANGAR_CONFIG="+filepath.Join(workDir, "cli.yaml"))
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// applyYAML applies manifests with hangar apply -f -.
func (c *cluster) applyYAML(doc string) {
	c.t.Helper()
	if out, err := c.tryCLI(strings.NewReader(doc), "apply", "-f", "-"); err != nil {
		c.t.Fatalf("hangar apply: %v\n%s", err, doc)
	} else {
		c.t.Logf("hangar apply: %s", strings.TrimSpace(out))
	}
}

// waitReady waits with hangar wait for the container to be Ready at its
// current generation.
func (c *cluster) waitReady(name string, timeout time.Duration) {
	c.t.Helper()
	start := time.Now()
	if _, err := c.tryCLI(nil, "wait", "container", name, "--for=condition=Ready", "--timeout="+timeout.String()); err != nil {
		c.dump(name)
		c.t.Fatalf("container %s never Ready: %v", name, err)
	}
	c.t.Logf("container %s Ready in %s", name, time.Since(start).Round(time.Millisecond))
}

// get reads one object with hangar get -o json; ok is false if it is not
// there.
func get[T proto.Message](c *cluster, kind, name string, obj T) (T, bool) {
	c.t.Helper()
	out, err := c.tryCLI(nil, "get", kind, name, "-o", "json")
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "not_found") {
			return obj, false
		}
		c.t.Fatalf("hangar get %s %s: %v", kind, name, err)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(out), obj); err != nil {
		c.t.Fatalf("hangar get %s %s: %v: %s", kind, name, err, out)
	}
	return obj, true
}

func (c *cluster) machine(name string) *computev1alpha1.Machine {
	c.t.Helper()
	m, ok := get(c, "machine", "ct-"+name, &computev1alpha1.Machine{})
	if !ok {
		c.t.Fatalf("container %s has no machine", name)
	}
	return m
}

func (c *cluster) network(name string) *networkv1alpha1.Network {
	c.t.Helper()
	n, ok := get(c, "network", name, &networkv1alpha1.Network{})
	if !ok {
		c.t.Fatalf("no network %s", name)
	}
	return n
}

// eventually polls cond until it holds, describing what it waits for and,
// on timeout, what it last saw.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, last := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never happened in %s: %s; last: %s", timeout, what, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// dump logs what the API and the host say of a container.
func (c *cluster) dump(name string) {
	c.t.Helper()
	ct, _ := c.tryCLI(nil, "get", "container", name, "-o", "yaml")
	m, _ := c.tryCLI(nil, "get", "machine", "ct-"+name, "-o", "yaml")
	c.t.Logf("--- container %s:\n%s--- machine ct-%s:\n%s", name, ct, name, m)
	if mc, ok := get(c, "machine", "ct-"+name, &computev1alpha1.Machine{}); ok {
		dumpVM(c.t, mc.GetMetadata().GetUid())
	}
}

// dumpVM logs what the host saw of a machine's VM.
func dumpVM(t *testing.T, uid string) {
	t.Helper()
	out, _ := exec.Command("journalctl", "--no-pager", "-n", "30", "-u", unitOf(uid)).CombinedOutput()
	t.Logf("--- journal of %s:\n%s", unitOf(uid), out)
	for _, r := range consoleOf(t, uid) {
		t.Logf("console %d: %q", r.Stream, r.Payload)
	}
}

// sweep removes what a run makes: the agent and apiserver of a run killed
// outright, every hangar-vm-* unit, the network driver's namespaces and
// links, the test's dataset and its directories.
func sweep() {
	for _, pat := range []string{"--vm-dir " + vmDir, apiDir + "/hangar-apiserver"} {
		_ = exec.Command("pkill", "-KILL", "-f", "--", pat).Run()
	}
	units, _ := exec.Command("systemctl", "list-units", "--all", "--plain", "--no-legend", "hangar-vm-*.service").Output()
	for _, line := range strings.Split(string(units), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			_ = exec.Command("systemctl", "stop", f[0]).Run()
			_ = exec.Command("systemctl", "reset-failed", f[0]).Run()
		}
	}
	if d, err := hostnet.New(hostnet.Config{Uplink: "lo"}); err == nil {
		ctx := context.Background()
		ifaces, _ := d.Interfaces(ctx)
		for _, i := range ifaces {
			_ = d.DeleteInterfaces(ctx, i.MachineUID)
		}
		nets, _ := d.Networks(ctx)
		for _, n := range nets {
			_ = d.DeleteNetwork(ctx, n.UID)
		}
		_ = d.Close()
	}
	// udev opens a zvol to probe it on every change event, and a package
	// install triggers events for every device, so a destroy refused as busy
	// is tried again for a while, as the storage driver does.
	for deadline := time.Now().Add(15 * time.Second); exec.Command("zfs", "list", "-H", dataset).Run() == nil; {
		out, err := exec.Command("zfs", "destroy", "-r", dataset).CombinedOutput()
		if err == nil {
			break
		}
		if !strings.Contains(string(out), "dataset is busy") || time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "flow: destroying %s: %v: %s\n", dataset, err, out)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	for _, d := range []string{imageDir, workDir} {
		if err := os.RemoveAll(d); err != nil {
			fmt.Fprintf(os.Stderr, "flow: %v\n", err)
		}
	}
}

// Host: what the host holds of a machine, by its uid.

func unitOf(uid string) string { return "hangar-vm-" + uid + ".service" }

// unitProps reads properties of a machine's unit.
func unitProps(t *testing.T, uid string, props ...string) map[string]string {
	t.Helper()
	out := mustRun(t, "systemctl", "show", "-p", strings.Join(props, ","), unitOf(uid))
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

func unitActive(uid string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unitOf(uid)).Run() == nil
}

// mainPID is the launcher's pid, the unit's main process.
func mainPID(t *testing.T, uid string) int {
	t.Helper()
	pid, _ := strconv.Atoi(unitProps(t, uid, "MainPID")["MainPID"])
	return pid
}

// chPID is the pid of the machine's Cloud Hypervisor, the launcher's child.
func chPID(t *testing.T, uid string) int {
	t.Helper()
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(mainPID(t, uid)), "-x", "cloud-hyperviso").Output()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return pid
}

func nRestarts(t *testing.T, uid string) int {
	t.Helper()
	n, _ := strconv.Atoi(unitProps(t, uid, "NRestarts")["NRestarts"])
	return n
}

// hangarUnits lists the uids of the hangar-vm-* units loaded.
func hangarUnits(t *testing.T) []string {
	t.Helper()
	out := mustRun(t, "systemctl", "list-units", "--all", "--plain", "--no-legend", "hangar-vm-*.service")
	var uids []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			uids = append(uids, strings.TrimSuffix(strings.TrimPrefix(f[0], "hangar-vm-"), ".service"))
		}
	}
	return uids
}

// zvol is a root disk as zfs lists it.
type zvol struct {
	name, origin string
	volsize      int64
}

// roots lists the root disks under the test's dataset, by machine uid.
func roots(t *testing.T) map[string]zvol {
	t.Helper()
	out := mustRun(t, "zfs", "list", "-H", "-p", "-r", "-t", "volume", "-o", "name,volsize,origin", dataset+"/roots")
	m := map[string]zvol{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		m[filepath.Base(f[0])] = zvol{name: f[0], origin: f[2], volsize: size}
	}
	return m
}

func baseOf(digest string) string {
	return dataset + "/bases/" + strings.TrimPrefix(digest, "sha256:") + "@base"
}

// tap is a machine's tap as the host holds it.
type tap struct {
	name, netns string
	machine     string
	ipv4        string
}

// taps lists the taps in the network driver's namespaces.
func taps(t *testing.T) []tap {
	t.Helper()
	var out []tap
	for _, ns := range namespaces(t) {
		js := mustRun(t, "ip", "-n", ns, "-j", "link", "show")
		var links []struct {
			Name  string `json:"ifname"`
			Alias string `json:"ifalias"`
		}
		if err := json.Unmarshal([]byte(js), &links); err != nil {
			t.Fatalf("ip -n %s link: %v", ns, err)
		}
		for _, l := range links {
			var meta struct {
				Machine string `json:"machine"`
				IPv4    string `json:"ipv4"`
			}
			if !strings.HasPrefix(l.Name, hostnet.Prefix+"t") || json.Unmarshal([]byte(l.Alias), &meta) != nil {
				continue
			}
			out = append(out, tap{name: l.Name, netns: ns, machine: meta.Machine, ipv4: meta.IPv4})
		}
	}
	return out
}

func tapsOf(t *testing.T, uid string) []tap {
	t.Helper()
	var out []tap
	for _, tp := range taps(t) {
		if tp.machine == uid {
			out = append(out, tp)
		}
	}
	return out
}

// namespaces lists the network driver's namespaces.
func namespaces(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("/run/netns")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), hostnet.Prefix+"-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// consoleOf reads a machine's framed console log, the rotated file first.
func consoleOf(t *testing.T, uid string) []console.Record {
	t.Helper()
	var out []console.Record
	for _, f := range []string{chvm.ConsoleLog + ".1", chvm.ConsoleLog} {
		fh, err := os.Open(filepath.Join(vmDir, uid, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		r := console.NewReader(bufio.NewReader(fh))
		for {
			rec, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Errorf("%s: %v after %d records", f, err, len(out))
				break
			}
			out = append(out, rec)
		}
		_ = fh.Close()
	}
	return out
}

// consoleText is the container's stdout and stderr, as one string.
func consoleText(t *testing.T, uid string) string {
	t.Helper()
	var b strings.Builder
	for _, r := range consoleOf(t, uid) {
		if r.Stream == console.Stdout || r.Stream == console.Stderr {
			b.Write(r.Payload)
		}
	}
	return b.String()
}

// exits are the exit records in a machine's console log, one per boot that
// ended.
func exits(t *testing.T, uid string) []console.ExitStatus {
	t.Helper()
	var out []console.ExitStatus
	for _, r := range consoleOf(t, uid) {
		if r.Stream != console.Exit {
			continue
		}
		st, err := console.ExitOf(r)
		if err != nil {
			t.Errorf("an exit record: %v", err)
			continue
		}
		out = append(out, st)
	}
	return out
}

// guest reaches the guest agents of the VMs the agent runs.
func guest() chguest.Guest {
	return chguest.Guest{Socket: func(uid string) string { return filepath.Join(vmDir, uid, "vsock") }}
}

// execIn runs a process in the machine's guest and returns its stdout and
// stderr and its exit code.
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
		case *guestv1.ExecResponse_Stderr:
			out.Write(m.Stderr)
		case *guestv1.ExecResponse_Exit:
			_ = stream.CloseRequest()
			return out.String(), m.Exit.GetCode(), nil
		}
	}
}

// mustExec runs a shell command in the guest and fails unless it exits 0.
func mustExec(t *testing.T, uid, script string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, code, err := execIn(ctx, uid, "/bin/sh", "-c", script)
	if err != nil || code != 0 {
		t.Fatalf("in the guest of %s, %q: exit %d, %v: %s", uid, script, code, err, out)
	}
	return out
}

// Small helpers.

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

func lookupUID(t *testing.T, user string) int {
	t.Helper()
	uid, err := strconv.Atoi(strings.TrimSpace(mustRun(t, "id", "-u", user)))
	if err != nil {
		t.Fatal(err)
	}
	return uid
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func copyFile(t *testing.T, from, to string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, mode); err != nil {
		t.Fatal(err)
	}
}

func openLog(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(workDir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// tail is the last lines of a file.
func tail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > 60 {
		lines = lines[len(lines)-60:]
	}
	return strings.Join(lines, "\n")
}
