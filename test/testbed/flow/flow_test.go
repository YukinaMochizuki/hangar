//go:build testbed

package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
)

// scratch lists what the image and storage drivers hold in their scratch
// space: blobs being fetched and images being unpacked.
func scratch(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, d := range []string{filepath.Join(imageDir, "cache", "tmp"), filepath.Join(imageDir, "unpack")} {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			out = append(out, filepath.Join(d, e.Name()))
		}
	}
	return out
}

// webScript is what web runs on busybox: it fetches Cloudflare's trace page
// over plain HTTP by name, which goes through the public resolvers and both
// NATs and says the address it came from, then waits. It says it got
// SIGTERM, and exits 0, on SIGTERM.
const webScript = `trap 'echo got SIGTERM; exit 0' TERM
wget -q -T 10 -O- http://cloudflare.com/cdn-cgi/trace || echo fetch failed
echo ready
while :; do sleep 1 & wait $!; done`

// debianScript is what web runs on debian, which has no wget.
const debianScript = `trap 'echo got SIGTERM; exit 0' TERM
echo "debian $(cat /etc/debian_version)"
echo ready
while :; do sleep 1 & wait $!; done`

// crashScript fails on its first boot only: the mark it leaves on its root
// disk survives the restart.
const crashScript = `if [ ! -e /crashed ]; then touch /crashed; sync; echo crashing; exit 3; fi
echo second boot
echo ready
while :; do sleep 1 & wait $!; done`

const idleScript = `trap 'exit 0' TERM
echo ready
while :; do sleep 1 & wait $!; done`

// containerYAML is a container's manifest.
func containerYAML(name, image, restart, disk, script string) string {
	restartLine := ""
	if restart != "" {
		restartLine = "  restartPolicy: " + restart + "\n"
	}
	return fmt.Sprintf(`apiVersion: compute.hangar.dev/v1alpha1
kind: Container
metadata: {name: %s}
spec:
  image: %s
%s  command: ["/bin/sh", "-c", %q]
  resources: {cpu: 1, memory: 256Mi, disk: %s}
`, name, image, restartLine, script, disk)
}

const gib = int64(1) << 30

// TestFlow follows containers from apply to an empty host, each scenario
// building on the one before, as docs/runbook/testbed.md describes.
func TestFlow(t *testing.T) {
	c := startCluster(t)
	f := &flow{c: c, uids: map[string]string{}}
	listing(t, "before")
	steps := []struct {
		name string
		run  func(*testing.T)
	}{
		{"1-apply-and-ready", f.applyAndReady},
		{"2-online", f.online},
		{"3-stop-and-start", f.stopAndStart},
		{"4-restart-policy", f.restartPolicy},
		{"5-grow-the-disk", f.growTheDisk},
		{"6-change-the-image", f.changeTheImage},
		{"8-the-node-survives-a-restart", f.nodeSurvivesARestart},
		{"9-host-shutdown", f.hostShutdown},
		{"10-logs", f.logs},
		{"11-exec", f.exec},
		{"7-delete", f.delete},
	}
	for _, s := range steps {
		// Each scenario stands on the state the one before left.
		if !t.Run(s.name, func(t *testing.T) { f.c.t = t; s.run(t) }) {
			f.c.t = t
			t.Fatalf("%s failed; the scenarios after it depend on it", s.name)
		}
	}
	f.c.t = t
	listing(t, "after")
}

// flow is what the scenarios share: the cluster, and the machine uid of
// each container they made.
type flow struct {
	c    *cluster
	uids map[string]string
}

// listing logs what the host holds of the flow: units, root disks and bases,
// namespaces and taps, VM directories.
func listing(t *testing.T, when string) {
	t.Helper()
	units := mustRun(t, "systemctl", "list-units", "--all", "--plain", "--no-legend", "hangar-vm-*.service")
	zfs := mustRun(t, "zfs", "list", "-H", "-r", "-t", "all", "-o", "name,volsize,origin", dataset)
	var tps []string
	for _, tp := range taps(t) {
		tps = append(tps, fmt.Sprintf("%s in %s: machine %s, %s", tp.name, tp.netns, tp.machine, tp.ipv4))
	}
	dirs, _ := os.ReadDir(vmDir)
	var names []string
	for _, d := range dirs {
		names = append(names, d.Name())
	}
	t.Logf("===== host %s\n--- units:\n%s--- zfs:\n%s--- netns: %v\n--- taps:\n%s\n--- VM directories: %v",
		when, units, zfs, namespaces(t), strings.Join(tps, "\n"), names)
}

// Scenario 1: a container applied with the CLI becomes Ready, and the host
// holds its root disk, its network's namespace, its tap and its unit.
func (f *flow) applyAndReady(t *testing.T) {
	c := f.c
	c.applyYAML(containerYAML("web", busyboxRef, "", "1Gi", webScript))
	c.waitReady("web", 5*time.Minute)

	m := c.machine("web")
	uid := m.GetMetadata().GetUid()
	f.uids["web"] = uid
	st := m.GetStatus()
	if st.GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING || st.GetImageDigest() != busyboxDigest ||
		st.GetBootTime() == nil || st.GetObservedGeneration() != m.GetMetadata().GetGeneration() || m.GetSpec().GetNode() != nodeName {
		t.Errorf("machine %s: spec %v, status %v", uid, m.GetSpec(), st)
	}

	root, ok := roots(t)[uid]
	if !ok || root.origin != baseOf(busyboxDigest) || root.volsize != gib {
		t.Errorf("root disk of %s: %+v (present: %t), want a 1 GiB clone of %s", uid, root, ok, baseOf(busyboxDigest))
	}
	nw := c.network("default")
	ns := "hn-" + nw.GetMetadata().GetUid()
	if !slices.Contains(namespaces(t), ns) {
		t.Errorf("no namespace %s for network default; namespaces %v", ns, namespaces(t))
	}
	tps := tapsOf(t, uid)
	want := m.GetSpec().GetInterfaces()[0].GetIpv4()
	if len(tps) != 1 || tps[0].netns != ns || tps[0].ipv4 != want {
		t.Errorf("taps of %s: %+v, want one in %s with %s", uid, tps, ns, want)
	}
	if !unitActive(uid) || chPID(t, uid) == 0 {
		t.Errorf("unit %s active: %t, Cloud Hypervisor pid %d", unitOf(uid), unitActive(uid), chPID(t, uid))
	}
	for _, file := range []string{"spec.json", "boot.img", "ch/vsock", "console.log"} {
		if !exists(filepath.Join(vmDir, uid, file)) {
			t.Errorf("VM directory of %s has no %s", uid, file)
		}
	}
	t.Logf("web is machine %s: root %s, tap %s in %s, unit %s", uid, root.name, tps[0].name, ns, unitOf(uid))
}

var traceIP = regexp.MustCompile(`(?m)^ip=(\S+)$`)

// Scenario 2: the container reaches the internet by name, through both
// NATs, leaving as the host does, and its console log says so.
func (f *flow) online(t *testing.T) {
	uid := f.uids["web"]
	var text string
	eventually(t, 30*time.Second, "web's fetch in its console log", func() (bool, string) {
		text = consoleText(t, uid)
		return strings.Contains(text, "ready"), text
	})
	m := traceIP.FindStringSubmatch(text)
	if m == nil || strings.Contains(text, "fetch failed") {
		dumpVM(t, uid)
		t.Fatalf("web did not reach cloudflare.com: %q", text)
	}
	hostIP := hostEgress(t)
	if m[1] != hostIP {
		t.Errorf("web left as %s, the host leaves as %s", m[1], hostIP)
	}
	t.Logf("web fetched http://cloudflare.com/cdn-cgi/trace and left as %s, as the host does", m[1])
}

// hostEgress is the address the host's own traffic reaches Cloudflare
// from.
func hostEgress(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://cloudflare.com/cdn-cgi/trace", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	m := traceIP.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("the host's trace: %q", b)
	}
	return m[1]
}

// withRunState adds a run state to a container's manifest.
func withRunState(manifest, runState string) string {
	return strings.Replace(manifest, "\nspec:\n", "\nspec:\n  runState: "+runState+"\n", 1)
}

// Scenario 3: stopping the container sends its process SIGTERM and takes
// its VM down for good, keeping its disk and tap, and starting it again
// boots the same root disk.
func (f *flow) stopAndStart(t *testing.T) {
	c := f.c
	uid := f.uids["web"]
	mustExec(t, uid, "echo kept > /marker && sync")
	launcher := mainPID(t, uid)
	manifest := containerYAML("web", busyboxRef, "", "1Gi", webScript)

	c.applyYAML(withRunState(manifest, "CONTAINER_RUN_STATE_STOPPED"))
	start := time.Now()
	if _, err := c.tryCLI(nil, "wait", "container", "web", "--for=condition=Stopped", "--timeout=1m"); err != nil {
		c.dump("web")
		t.Fatalf("web never Stopped: %v", err)
	}
	t.Logf("container web Stopped in %s", time.Since(start).Round(time.Millisecond))

	ct, _ := get(c, "container", "web", &computev1alpha1.Container{})
	if r := conditions.Find(ct.GetStatus().GetConditions(), "Ready"); r.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || r.GetReason() != "Stopped" {
		t.Errorf("a stopped container's Ready: %v", r)
	}
	if p := c.machine("web").GetStatus().GetPhase(); p != computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED {
		t.Errorf("web's machine phase %v", p)
	}
	eventually(t, 30*time.Second, "web's VM down after SIGTERM, exit 0", func() (bool, string) {
		ex := exits(t, uid)
		down := !unitActive(uid) && chPID(t, uid) == 0
		return down && strings.Contains(consoleText(t, uid), "got SIGTERM") && len(ex) > 0 && ex[len(ex)-1].Code == 0,
			fmt.Sprintf("VM down: %t, exits %+v, console %q", down, ex, consoleText(t, uid))
	})
	if _, ok := roots(t)[uid]; !ok || len(tapsOf(t, uid)) != 1 {
		t.Errorf("a stopped web lost its disk (%t) or tap (%+v)", ok, tapsOf(t, uid))
	}
	// Down for good: under restart policy always, nothing brings it back.
	for range 10 {
		if unitActive(uid) || chPID(t, uid) != 0 {
			t.Fatalf("web's VM is running while stopped: %v", unitProps(t, uid, "ActiveState", "MainPID", "NRestarts"))
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("web stopped: SIGTERM, exit 0, unit %s, disk and tap kept", unitProps(t, uid, "LoadState")["LoadState"])

	c.applyYAML(withRunState(manifest, "CONTAINER_RUN_STATE_RUNNING"))
	c.waitReady("web", 2*time.Minute)
	if mainPID(t, uid) == launcher || mainPID(t, uid) == 0 {
		t.Errorf("web did not boot again: launcher %d, was %d", mainPID(t, uid), launcher)
	}
	if out := mustExec(t, uid, "cat /marker"); strings.TrimSpace(out) != "kept" {
		t.Errorf("/marker after the stop and start: %q", out)
	}
	if ct, _ := get(c, "container", "web", &computev1alpha1.Container{}); conditions.IsTrue(ct.GetStatus().GetConditions(), "Stopped") {
		t.Error("web says Stopped while running")
	}
	t.Log("web booted again on the same root disk: /marker kept")
}

// Scenario 4: a container exiting non-zero under on-failure is restarted by
// the host, and the API's boot time follows the restart.
func (f *flow) restartPolicy(t *testing.T) {
	c := f.c
	c.applyYAML(containerYAML("crashy", busyboxRef, "RESTART_POLICY_ON_FAILURE", "1Gi", crashScript))
	var uid string
	eventually(t, 3*time.Minute, "crashy's machine on the host", func() (bool, string) {
		m, ok := get(c, "machine", "ct-crashy", &computev1alpha1.Machine{})
		uid = m.GetMetadata().GetUid()
		return ok && uid != "" && unitActive(uid), fmt.Sprintf("machine %v", m.GetMetadata())
	})
	f.uids["crashy"] = uid
	eventually(t, 2*time.Minute, "crashy restarted once and up again", func() (bool, string) {
		return nRestarts(t, uid) == 1 && strings.Contains(consoleText(t, uid), "second boot"),
			fmt.Sprintf("NRestarts %d, console %q", nRestarts(t, uid), consoleText(t, uid))
	})
	c.waitReady("crashy", time.Minute)

	if ex := exits(t, uid); len(ex) != 1 || ex[0].Code != 3 {
		t.Errorf("exit records of crashy: %+v, want one with code 3", ex)
	}
	// The boot the API reports is the restart's, as systemd started it.
	started := unitProps(t, uid, "ExecMainStartTimestampMonotonic", "ExecMainStartTimestamp")
	startedAt, err := time.Parse("Mon 2006-01-02 15:04:05 MST", started["ExecMainStartTimestamp"])
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "crashy's boot time to be the restart's", func() (bool, string) {
		boot := c.machine("crashy").GetStatus().GetBootTime().AsTime()
		d := boot.Sub(startedAt)
		return d > -time.Second && d < 2*time.Second, fmt.Sprintf("boot time %v, the unit's main process started %v", boot, startedAt)
	})
	if p := c.machine("crashy").GetStatus().GetPhase(); p != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
		t.Errorf("crashy's phase %v after its restart", p)
	}
	t.Logf("crashy exited 3, systemd restarted it (NRestarts 1), and its boot time is the restart's, %s", started["ExecMainStartTimestamp"])
}

// Scenario 5: raising the disk restarts the VM on a larger zvol, and the
// guest's root has grown into it, keeping what was written on it.
func (f *flow) growTheDisk(t *testing.T) {
	c := f.c
	uid := f.uids["web"]
	mustExec(t, uid, "echo kept > /marker && sync")
	before := rootSizeKiB(t, uid)
	if before > gib/1024 {
		t.Fatalf("web's root is %d KiB before growing", before)
	}
	launcher := mainPID(t, uid)

	c.applyYAML(containerYAML("web", busyboxRef, "", "2Gi", webScript))
	c.waitReady("web", 2*time.Minute)

	if r := roots(t)[uid]; r.volsize != 2*gib {
		t.Errorf("web's root zvol is %d bytes, want 2 GiB", r.volsize)
	}
	if mainPID(t, uid) == launcher {
		t.Error("web's VM was not restarted to grow its disk")
	}
	after := rootSizeKiB(t, uid)
	if after < 2*gib/1024*9/10 {
		t.Errorf("web's root is %d KiB after growing to 2 GiB (it was %d KiB)", after, before)
	}
	if out := mustExec(t, uid, "cat /marker"); strings.TrimSpace(out) != "kept" {
		t.Errorf("/marker after growing: %q", out)
	}
	if !strings.Contains(consoleText(t, uid), "got SIGTERM") {
		t.Error("web's process was not sent SIGTERM before the restart")
	}
	t.Logf("web's root grew from %d KiB to %d KiB; /marker kept", before, after)
}

// rootSizeKiB is the size of the guest's root filesystem as df says.
func rootSizeKiB(t *testing.T, uid string) int64 {
	t.Helper()
	out := mustExec(t, uid, "df -k / | tail -n 1")
	fields := strings.Fields(out)
	if len(fields) < 2 {
		t.Fatalf("df: %q", out)
	}
	n, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("df: %q: %v", out, err)
	}
	return n
}

// Scenario 6: naming another image rebuilds the root from it, at the disk
// size the container has.
func (f *flow) changeTheImage(t *testing.T) {
	c := f.c
	uid := f.uids["web"]
	c.applyYAML(containerYAML("web", debianRef, "", "2Gi", debianScript))
	c.waitReady("web", 5*time.Minute)

	m := c.machine("web")
	if m.GetMetadata().GetUid() != uid || m.GetStatus().GetImageDigest() != debianDigest || m.GetStatus().GetImage() != debianRef {
		t.Errorf("machine after the image change: uid %s, status %v", m.GetMetadata().GetUid(), m.GetStatus())
	}
	if r := roots(t)[uid]; r.origin != baseOf(debianDigest) || r.volsize != 2*gib {
		t.Errorf("web's root after the image change: %+v, want a 2 GiB clone of %s", r, baseOf(debianDigest))
	}
	if out := mustExec(t, uid, "cat /etc/debian_version"); !strings.HasPrefix(strings.TrimSpace(out), "13") {
		t.Errorf("/etc/debian_version: %q", out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, code, err := execIn(ctx, uid, "/bin/sh", "-c", "test -e /marker"); err != nil || code == 0 {
		t.Errorf("/marker survived the image change: exit %d, %v, %s", code, err, out)
	}
	if size := rootSizeKiB(t, uid); size < 2*gib/1024*9/10 {
		t.Errorf("web's root on debian is %d KiB, want about 2 GiB", size)
	}
	if !strings.Contains(consoleText(t, uid), "debian 13") {
		t.Errorf("web's console: %q", consoleText(t, uid))
	}
	t.Logf("web's root was rebuilt from debian (%s), 2 GiB", baseOf(debianDigest))
}

// Scenario 8: the agent killed outright mid-run leaves the running VMs as
// they are, and once started again converges, with nothing doubled.
func (f *flow) nodeSurvivesARestart(t *testing.T) {
	c := f.c
	running := []string{"web", "crashy"}
	type proc struct {
		launcher, vmm int
		boot          time.Time
	}
	before := map[string]proc{}
	for _, name := range running {
		uid := f.uids[name]
		before[name] = proc{mainPID(t, uid), chPID(t, uid), c.machine(name).GetStatus().GetBootTime().AsTime()}
	}

	// Killed mid-way through pulling third's image, which no machine has
	// used yet.
	c.applyYAML(containerYAML("third", nginxRef, "", "1Gi", idleScript))
	eventually(t, time.Minute, "third's image being pulled", func() (bool, string) {
		m, ok := get(c, "machine", "ct-third", &computev1alpha1.Machine{})
		r := conditions.Find(m.GetStatus().GetConditions(), "Ready")
		return ok && r.GetReason() == "Pulling", fmt.Sprintf("status %v", m.GetStatus())
	})
	time.Sleep(time.Second)
	c.killNode()
	t.Logf("killed the node agent while it pulled %s; image scratch: %v", nginxRef, scratch(t))
	time.Sleep(3 * time.Second)
	check := func(when string) {
		t.Helper()
		for _, name := range running {
			uid := f.uids[name]
			now := proc{mainPID(t, uid), chPID(t, uid), before[name].boot}
			if !unitActive(uid) || now != before[name] {
				t.Errorf("%s, %s's VM: launcher %d then %d, Cloud Hypervisor %d then %d", when, name,
					before[name].launcher, now.launcher, before[name].vmm, now.vmm)
			}
		}
	}
	check("with the agent dead")

	c.startNode()
	t.Log("started the node agent again")
	c.waitReady("third", 3*time.Minute)
	f.uids["third"] = c.machine("third").GetMetadata().GetUid()
	for _, name := range running {
		c.waitReady(name, time.Minute)
		if boot := c.machine(name).GetStatus().GetBootTime().AsTime(); !boot.Equal(before[name].boot) {
			t.Errorf("%s's boot time moved from %v to %v", name, before[name].boot, boot)
		}
	}
	check("with the agent back")
	if installed {
		// What deploy/host does on an upgrade: the VMs are units of their
		// own, which the agent's unit leaves alone.
		c.restartNodeUnit()
		t.Logf("restarted %s with systemctl", nodeUnit)
		for _, name := range running {
			c.waitReady(name, time.Minute)
		}
		check("after systemctl restart " + nodeUnit)
	}

	// One of everything per machine, and nothing else.
	var want []string
	for _, uid := range f.uids {
		want = append(want, uid)
	}
	slices.Sort(want)
	units := hangarUnits(t)
	slices.Sort(units)
	if !slices.Equal(units, want) {
		t.Errorf("units for machines %v, want %v", units, want)
	}
	var disks []string
	for uid := range roots(t) {
		disks = append(disks, uid)
	}
	slices.Sort(disks)
	if !slices.Equal(disks, want) {
		t.Errorf("root disks for machines %v, want %v", disks, want)
	}
	for _, uid := range want {
		if n := len(tapsOf(t, uid)); n != 1 {
			t.Errorf("machine %s has %d taps", uid, n)
		}
	}
	if len(taps(t)) != len(want) || len(namespaces(t)) != 1 {
		t.Errorf("taps %+v, namespaces %v", taps(t), namespaces(t))
	}
	// The pull cut short left nothing half made: each base is whole, and
	// the scratch space is empty.
	vols := strings.Fields(mustRun(t, "zfs", "list", "-H", "-r", "-t", "all", "-o", "name", dataset+"/bases"))
	var wantVols []string
	for _, d := range []string{busyboxDigest, debianDigest, nginxDigest} {
		b := strings.TrimSuffix(baseOf(d), "@base")
		wantVols = append(wantVols, b, b+"@base")
	}
	wantVols = append(wantVols, dataset+"/bases")
	slices.Sort(vols)
	slices.Sort(wantVols)
	if installed {
		// The installed host's image cache may hold bases from before;
		// each must still be whole.
		for _, v := range vols {
			if v != dataset+"/bases" && !strings.Contains(v, "@") && !slices.Contains(vols, v+"@base") {
				t.Errorf("base %s has no @base snapshot", v)
			}
		}
		for _, v := range wantVols {
			if !slices.Contains(vols, v) {
				t.Errorf("bases %v lack %s", vols, v)
			}
		}
	} else if !slices.Equal(vols, wantVols) {
		t.Errorf("bases %v, want %v", vols, wantVols)
	}
	if s := scratch(t); len(s) > 0 {
		t.Errorf("image scratch left: %v", s)
	}
	listing(t, "after the agent's restart")
}

// termScript says it got SIGTERM, and exits 0, on SIGTERM.
const termScript = `trap 'echo got SIGTERM; exit 0' TERM
echo ready
while :; do sleep 1 & wait $!; done`

// chUser is the uid the machine's Cloud Hypervisor runs as, which must be
// the VM user its spec file holds, and none of root's.
func chUser(t *testing.T, uid string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vmDir, uid, "spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ln struct{ VMUser int }
	if err := json.Unmarshal(b, &ln); err != nil {
		t.Fatal(err)
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", chPID(t, uid)))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(strings.Repeat(strconv.Itoa(ln.VMUser)+" ", 4))
	for _, line := range strings.Split(string(status), "\n") {
		if v, ok := strings.CutPrefix(line, "Uid:"); ok {
			if got := strings.Join(strings.Fields(v), " "); ln.VMUser == 0 || got != want {
				t.Errorf("machine %s's Cloud Hypervisor runs as %s, its spec file says %d", uid, got, ln.VMUser)
			}
		}
	}
	return strconv.Itoa(ln.VMUser)
}

// Scenario 9: a host shutdown stops the agent and then each VM's unit,
// systemd alone: the containers get SIGTERM and their grace, and write to
// their logs, yet each VM reads as interrupted, never as a container that
// exited 0 (decision 16). Once the agent is back, always and on-failure
// boot again, as the VM users they had, and never is Exited with 253. A
// real reboot would also drop the units, which are transient; the stop
// already unloads them.
func (f *flow) hostShutdown(t *testing.T) {
	c := f.c
	policies := map[string]string{
		"down-always": "RESTART_POLICY_ALWAYS", "down-on-failure": "RESTART_POLICY_ON_FAILURE", "down-never": "RESTART_POLICY_NEVER",
	}
	names := slices.Sorted(maps.Keys(policies))
	for _, name := range names {
		c.applyYAML(containerYAML(name, busyboxRef, policies[name], "1Gi", termScript))
	}
	boots := map[string]time.Time{}
	users := map[string]string{}
	for _, name := range names {
		c.waitReady(name, 3*time.Minute)
		f.uids[name] = c.machine(name).GetMetadata().GetUid()
		boots[name] = c.machine(name).GetStatus().GetBootTime().AsTime()
		users[name] = chUser(t, f.uids[name])
		eventually(t, 30*time.Second, name+" ready", func() (bool, string) {
			text := consoleText(t, f.uids[name])
			return strings.Contains(text, "ready"), text
		})
	}

	// The agent goes first, then the VMs, all at once, as systemd orders a
	// shutdown.
	if installed {
		mustRun(t, "systemctl", "stop", nodeUnit)
	} else {
		c.killNode()
	}
	start := time.Now()
	args := []string{"stop"}
	for _, name := range names {
		args = append(args, unitOf(f.uids[name]))
	}
	mustRun(t, "systemctl", args...)
	took := time.Since(start)
	t.Logf("systemctl stopped the VMs' units in %s", took.Round(time.Millisecond))
	if took > 10*time.Second {
		t.Errorf("containers exiting at once on SIGTERM took %v to stop", took)
	}
	for _, name := range names {
		uid := f.uids[name]
		if load := unitProps(t, uid, "LoadState")["LoadState"]; load != "not-found" || chPID(t, uid) != 0 {
			t.Errorf("%s's unit is %s after the stop, Cloud Hypervisor pid %d", name, load, chPID(t, uid))
		}
		if text := consoleText(t, uid); !strings.Contains(text, "got SIGTERM") {
			t.Errorf("%s's console has no SIGTERM handler's output: %q", name, text)
		}
		if ex := exits(t, uid); len(ex) == 0 || ex[len(ex)-1].Code != 0 {
			t.Errorf("%s's exits on its console %+v, want the handler's 0 last", name, ex)
		}
		if exists(filepath.Join(vmDir, uid, "exit.json")) || exists(filepath.Join(vmDir, uid, "stopping")) {
			t.Errorf("%s: the shutdown was recorded as an exit or a stop", name)
		}
	}

	if installed {
		mustRun(t, "systemctl", "start", nodeUnit)
		c.cli("wait", "node", nodeName, "--for=condition=Ready", "--timeout=60s")
	} else {
		c.startNode()
	}
	for _, name := range []string{"down-always", "down-on-failure"} {
		// Its status says Ready from before the shutdown until the agent
		// has seen it again.
		eventually(t, 2*time.Minute, name+" booted again", func() (bool, string) {
			boot := c.machine(name).GetStatus().GetBootTime().AsTime()
			return boot.After(boots[name]) && unitActive(f.uids[name]), fmt.Sprintf("boot time %v, was %v", boot, boots[name])
		})
		c.waitReady(name, 2*time.Minute)
		// The host's reboot keeps a VM's id: its spec file holds it.
		if got := chUser(t, f.uids[name]); got != users[name] {
			t.Errorf("%s's Cloud Hypervisor runs as %s after the shutdown, as %s before", name, got, users[name])
		}
	}
	eventually(t, time.Minute, "down-never Exited with 253", func() (bool, string) {
		m := c.machine("down-never")
		r := conditions.Find(m.GetStatus().GetConditions(), "Ready")
		return m.GetStatus().GetPhase() == computev1alpha1.MachinePhase_MACHINE_PHASE_FAILED &&
				r.GetReason() == "Exited" && strings.Contains(r.GetMessage(), "code 253"),
			fmt.Sprintf("phase %v, Ready %v", m.GetStatus().GetPhase(), r)
	})
	if unitActive(f.uids["down-never"]) {
		t.Error("down-never was started again")
	}
	t.Log("after the shutdown: down-always and down-on-failure booted again, down-never Exited with 253")
}

// Scenario 7: deleting the containers takes their machines, and leaves
// the host without their root disks, taps, units, directories or logs.
// Image bases may stay: they are a cache.
func (f *flow) delete(t *testing.T) {
	c := f.c
	names := []string{"web", "crashy", "third", "down-always", "down-on-failure", "down-never"}
	c.cli(append([]string{"delete", "container"}, names...)...)
	for _, name := range names {
		c.cli("wait", "container", name, "--for=delete", "--timeout=2m")
		if _, ok := get(c, "machine", "ct-"+name, &computev1alpha1.Machine{}); ok {
			t.Errorf("container %s is gone but its machine is not", name)
		}
	}
	if out := strings.TrimSpace(c.cli("get", "machines", "-o", "name")); out != "" {
		t.Errorf("machines left: %s", out)
	}
	// Released means the host is clean, but cleaning may finish in the
	// background, so the host is given a bound to show it.
	eventually(t, time.Minute, "the host to hold nothing of the deleted machines", func() (bool, string) {
		var left []string
		rs := roots(t)
		for _, name := range names {
			uid := f.uids[name]
			if _, ok := rs[uid]; ok {
				left = append(left, name+"'s root disk")
			}
			if tps := tapsOf(t, uid); len(tps) > 0 {
				left = append(left, fmt.Sprintf("%s's taps %+v", name, tps))
			}
			if load := unitProps(t, uid, "LoadState")["LoadState"]; load != "not-found" {
				left = append(left, name+"'s unit, "+load)
			}
			if exists(filepath.Join(vmDir, uid)) {
				entries, _ := os.ReadDir(filepath.Join(vmDir, uid))
				left = append(left, fmt.Sprintf("%s's VM directory %v", name, entries))
			}
		}
		if units := hangarUnits(t); len(units) > 0 {
			left = append(left, fmt.Sprintf("units %v", units))
		}
		if tps := taps(t); len(tps) > 0 {
			left = append(left, fmt.Sprintf("taps %+v", tps))
		}
		return len(left) == 0, strings.Join(left, "; ")
	})
}
