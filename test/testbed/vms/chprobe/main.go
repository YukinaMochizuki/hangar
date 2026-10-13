//go:build testbed

// Command chprobe stands in for Cloud Hypervisor in a VM's unit, for
// test/testbed/vms TestCHIsConfined: the launcher runs it exactly as it
// runs Cloud Hypervisor, so it runs as Cloud Hypervisor would, and tries
// from there what a guest that took Cloud Hypervisor over might. It says it
// is up with ready in the VM's chDir, waits for targets.json there, the
// other VM's paths and pid, writes what each try did to probe.json, and
// waits to be killed. It must be static: the unit has no dynamic loader.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Targets is what the test points the probe at.
type Targets struct {
	// OtherDir, OtherSocket, OtherZvol and OtherPID are another VM's
	// directory, vsock socket, root disk and Cloud Hypervisor.
	OtherDir, OtherSocket, OtherZvol string
	OtherPID                         int
	// OwnZvol is this VM's root disk's link.
	OwnZvol string
	// HostPaths are paths of the host the probe must not reach.
	HostPaths []string
	// PlantLink is where a link the probe leaves at its socket's path
	// points, for the test to see the driver refuse it.
	PlantLink string
}

// Result is what one try did.
type Result struct {
	// OK says the try came out as confinement wants it to.
	OK     bool
	Detail string
}

func main() {
	dir := ""
	for i, a := range os.Args {
		if i > 0 && os.Args[i-1] == "--vsock" {
			_, sock, _ := strings.Cut(a, "socket=")
			dir = filepath.Dir(filepath.Dir(sock))
		}
	}
	if dir == "" {
		fmt.Fprintln(os.Stderr, "chprobe: no --vsock")
		os.Exit(2)
	}
	ch := filepath.Join(dir, "ch")
	if err := os.WriteFile(filepath.Join(ch, "ready"), nil, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "chprobe:", err)
		os.Exit(2)
	}
	var tg Targets
	for {
		b, err := os.ReadFile(filepath.Join(ch, "targets.json"))
		if err == nil && json.Unmarshal(b, &tg) == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	r := probe(dir, tg)
	sock := filepath.Join(ch, "vsock")
	_ = os.Remove(sock)
	r["plant a link at its socket's path"] = allowed(os.Symlink(tg.PlantLink, sock))
	b, _ := json.MarshalIndent(r, "", "  ")
	_ = os.WriteFile(filepath.Join(ch, "probe.json.part"), b, 0o600)
	_ = os.Rename(filepath.Join(ch, "probe.json.part"), filepath.Join(ch, "probe.json"))
	for {
		time.Sleep(time.Hour)
	}
}

func errOf(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}

// refused is a try that must fail with one of errnos, or any if none.
func refused(err error, errnos ...unix.Errno) Result {
	if err == nil {
		return Result{false, "succeeded"}
	}
	if len(errnos) == 0 {
		return Result{true, err.Error()}
	}
	for _, e := range errnos {
		if errors.Is(err, e) {
			return Result{true, err.Error()}
		}
	}
	return Result{false, err.Error()}
}

func allowed(err error) Result { return Result{err == nil, errOf(err)} }

func open(path string, flags int) error {
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC, 0)
	if err == nil {
		_ = unix.Close(fd)
	}
	return err
}

func probe(dir string, tg Targets) map[string]Result {
	r := map[string]Result{}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		r["status"] = Result{false, err.Error()}
	}
	for _, line := range strings.Split(string(status), "\n") {
		key, value, _ := strings.Cut(line, ":")
		value = strings.Join(strings.Fields(value), " ")
		switch key {
		case "Uid", "Gid", "Groups":
			r["status "+key] = Result{true, value}
		case "CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb":
			r["status "+key] = Result{value == "0000000000000000", value}
		case "NoNewPrivs":
			r["status "+key] = Result{value == "1", value}
		case "Seccomp":
			r["status "+key] = Result{value == "2", value} // a filter: systemd's
		}
	}

	// What of the host it sees: the root holds only what the unit binds.
	var names []string
	entries, err := os.ReadDir("/")
	for _, e := range entries {
		names = append(names, e.Name())
	}
	r["ls /"] = Result{err == nil, strings.Join(names, " ")}
	for _, p := range tg.HostPaths {
		_, err := os.Stat(p)
		r["stat "+p] = refused(err, unix.ENOENT, unix.EACCES)
	}
	parent := filepath.Dir(dir)
	names = nil
	entries, err = os.ReadDir(parent)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	r["ls "+parent] = Result{err == nil && len(names) == 1 && names[0] == filepath.Base(dir), strings.Join(names, " ") + " " + errOf(err)}
	_, err = os.Stat(tg.OtherDir)
	r["stat the other VM's directory"] = refused(err, unix.ENOENT)
	if c, err := net.Dial("unix", tg.OtherSocket); err == nil {
		_ = c.Close()
		r["connect to the other VM's socket"] = Result{false, "connected"}
	} else {
		r["connect to the other VM's socket"] = Result{true, err.Error()}
	}
	// Unmounted by the launcher once it had the taps; its mount point is
	// left.
	names = nil
	entries, err = os.ReadDir("/run/netns")
	for _, e := range entries {
		names = append(names, e.Name())
	}
	r["ls /run/netns"] = Result{len(names) == 0, strings.Join(names, " ") + " " + errOf(err)}

	// Its own VM's directory: what it needs, and nothing more.
	r["open its root disk's node"] = allowed(open(filepath.Join(dir, "disk"), unix.O_RDWR))
	r["read its boot disk"] = allowed(open(filepath.Join(dir, "boot.img"), unix.O_RDONLY))
	r["read its spec file"] = refused(open(filepath.Join(dir, "spec.json"), unix.O_RDONLY), unix.EACCES)
	r["read its console log"] = refused(open(filepath.Join(dir, "console.log"), unix.O_RDONLY), unix.EACCES)
	r["write its spec file"] = refused(open(filepath.Join(dir, "spec.json"), unix.O_WRONLY), unix.EACCES)
	r["create a file in its directory"] = refused(open(filepath.Join(dir, "x"), unix.O_WRONLY|unix.O_CREAT), unix.EACCES)
	r["remove its spec file"] = refused(unix.Unlink(filepath.Join(dir, "spec.json")), unix.EACCES)
	r["create a file in ch"] = allowed(open(filepath.Join(dir, "ch", "x"), unix.O_WRONLY|unix.O_CREAT))
	for _, p := range []string{"/x", "/run/x", "/tmp/x", "/home/x", "/root/x", "/run/user/x", "/dev/x", "/dev/shm/x", "/dev/mqueue/x", "/sys/x", "/usr/x"} {
		r["create "+p] = refused(open(p, unix.O_WRONLY|unix.O_CREAT))
	}
	r["write /proc/sys/kernel/hostname"] = refused(open("/proc/sys/kernel/hostname", unix.O_WRONLY))
	r["write /sys/kernel/mm/ksm/run"] = refused(open("/sys/kernel/mm/ksm/run", unix.O_WRONLY))

	// Disks and devices.
	r["open the other VM's zvol"] = refused(open(tg.OtherZvol, unix.O_RDWR), unix.EACCES, unix.EPERM)
	r["read the other VM's zvol"] = refused(open(tg.OtherZvol, unix.O_RDONLY), unix.EACCES, unix.EPERM)
	r["open its own zvol's link"] = refused(open(tg.OwnZvol, unix.O_RDWR), unix.EACCES, unix.EPERM)
	var st unix.Stat_t
	if err := unix.Stat(tg.OtherZvol, &st); err != nil {
		r["mknod the other VM's zvol"] = Result{false, "stat: " + err.Error()}
	} else {
		r["mknod the other VM's zvol"] = refused(unix.Mknod(filepath.Join(dir, "ch", "zvol"), unix.S_IFBLK|0o600, int(st.Rdev)), unix.EPERM)
	}
	r["open /dev/kvm"] = allowed(open("/dev/kvm", unix.O_RDWR))
	// Its group, kvm, could open this; the unit's device policy cannot.
	r["open /dev/vhost-net"] = refused(open("/dev/vhost-net", unix.O_RDWR), unix.EPERM)
	r["open /dev/vda"] = refused(open("/dev/vda", unix.O_RDONLY), unix.EACCES, unix.EPERM, unix.ENOENT)
	r["open /dev/mem"] = refused(open("/dev/mem", unix.O_RDONLY), unix.EACCES, unix.EPERM)
	r["open /dev/ptmx"] = refused(open("/dev/ptmx", unix.O_RDWR), unix.EACCES, unix.EPERM)
	r["open /dev/pts/ptmx"] = refused(open("/dev/pts/ptmx", unix.O_RDWR), unix.EACCES, unix.EPERM)
	r["open /dev/fuse"] = refused(open("/dev/fuse", unix.O_RDWR), unix.EACCES, unix.EPERM)
	r["open /dev/kmsg"] = refused(open("/dev/kmsg", unix.O_RDONLY), unix.EACCES, unix.EPERM)
	// The taps' device: the unit opens it, in its own empty network
	// namespace, where Cloud Hypervisor can make no tap.
	if fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0); err != nil {
		r["make a tap"] = Result{true, "open: " + err.Error()}
	} else {
		ifr, _ := unix.NewIfreq("probe0")
		ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
		r["make a tap"] = refused(unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr), unix.EPERM)
		_ = unix.Close(fd)
	}

	// Processes.
	r["signal the other VM's Cloud Hypervisor"] = refused(unix.Kill(tg.OtherPID, 0), unix.EPERM)
	r["signal its launcher"] = refused(unix.Kill(os.Getppid(), 0), unix.EPERM)
	r["signal pid 1"] = refused(unix.Kill(1, 0), unix.EPERM)
	r["ptrace the other VM's Cloud Hypervisor"] = refused(unix.PtraceAttach(tg.OtherPID), unix.EPERM)
	_, err = os.Stat(filepath.Join("/proc", strconv.Itoa(tg.OtherPID)))
	r["see the other VM's Cloud Hypervisor in /proc"] = refused(err, unix.ENOENT)
	var others []string
	entries, _ = os.ReadDir("/proc")
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		var pst unix.Stat_t
		if unix.Stat(filepath.Join("/proc", e.Name()), &pst) == nil && int(pst.Uid) != os.Getuid() {
			others = append(others, e.Name())
		}
	}
	r["processes of others in /proc"] = Result{len(others) == 0, strings.Join(others, " ")}

	// Privileges and the kernel.
	r["setuid 0"] = refused(unix.Setuid(0), unix.EPERM)
	r["unshare a user namespace"] = refused(unix.Unshare(unix.CLONE_NEWUSER), unix.EPERM)
	r["unshare a mount namespace"] = refused(unix.Unshare(unix.CLONE_NEWNS), unix.EPERM)
	_, err = unix.KeyctlInt(unix.KEYCTL_GET_KEYRING_ID, unix.KEY_SPEC_USER_KEYRING, 0, 0, 0)
	r["keyctl"] = refused(err, unix.EPERM)
	r["mount"] = refused(unix.Mount("none", "/run", "tmpfs", 0, ""), unix.EPERM)
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err == nil {
		_ = unix.Close(fd)
	}
	r["socket AF_INET"] = refused(err, unix.EAFNOSUPPORT, unix.EPERM)
	ifaces, err := net.Interfaces()
	names = nil
	for _, i := range ifaces {
		names = append(names, i.Name)
	}
	r["network interfaces"] = Result{err == nil && len(names) == 1 && names[0] == "lo", strings.Join(names, " ") + " " + errOf(err)}
	return r
}
