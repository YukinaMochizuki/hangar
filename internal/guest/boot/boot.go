// Package boot is what hangar-init does as PID 1 before the container's
// process can run: the filesystems, the root disk, the network, and the few
// files a container expects the platform to provide. See
// docs/decisions/guest-agent.md.
package boot

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/bootdisk"
)

// The guest's devices, in the order the hypervisor driver attaches them.
const (
	RootDevice = "/dev/vda"
	BootDevice = "/dev/vdb"
	// Console is the virtio console the host keeps in a file.
	Console = "/dev/hvc0"
)

const newRoot = "/newroot"

type mount struct {
	source, target, fstype, data string
	flags                        uintptr
}

func (m mount) do() error {
	if err := os.MkdirAll(m.target, 0o755); err != nil {
		return err
	}
	if err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
		return fmt.Errorf("mounting %s on %s: %w", m.fstype, m.target, err)
	}
	return nil
}

// Early mounts what the initramfs needs to reach the disks.
func Early() error {
	for _, m := range []mount{
		{"proc", "/proc", "proc", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
		{"sysfs", "/sys", "sysfs", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
		{"devtmpfs", "/dev", "devtmpfs", "mode=0755", unix.MS_NOSUID},
	} {
		if err := m.do(); err != nil {
			return err
		}
	}
	return nil
}

// ReadConfig reads the boot disk.
func ReadConfig() (*guestv1.BootConfig, error) {
	f, err := os.Open(BootDevice)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return bootdisk.Read(f)
}

// SwitchRoot mounts the root disk and makes it the root, keeping /dev,
// /proc and /sys, and drops the initramfs, whose memory is the guest's.
// hangar-init runs on from the memory it was loaded into.
func SwitchRoot() error {
	if err := (mount{RootDevice, newRoot, "ext4", "", 0}).do(); err != nil {
		return err
	}
	for _, dir := range []string{"/dev", "/proc", "/sys"} {
		target := newRoot + dir
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(dir, target, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("moving %s: %w", dir, err)
		}
	}
	// The initramfs holds only hangar-init's binary and empty directories.
	_ = os.Remove("/init")
	if err := unix.Chdir(newRoot); err != nil {
		return err
	}
	if err := unix.Mount(newRoot, "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("moving the root disk to /: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	return unix.Chdir("/")
}

// System mounts what the container's processes expect, and the cgroups
// Docker in the guest needs: hangar-init in /agent, all it starts in
// /workload, which it returns. See docs/decisions/guest-agent.md.
func System() (*os.File, error) {
	for _, m := range []mount{
		{"devpts", "/dev/pts", "devpts", "mode=0620,ptmxmode=0666,gid=5", unix.MS_NOSUID | unix.MS_NOEXEC},
		{"tmpfs", "/dev/shm", "tmpfs", "mode=1777", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
		{"tmpfs", "/run", "tmpfs", "mode=0755", unix.MS_NOSUID | unix.MS_NODEV},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", "nsdelegate", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
	} {
		if err := m.do(); err != nil {
			return nil, err
		}
	}
	for target, link := range map[string]string{
		"/dev/fd": "/proc/self/fd", "/dev/stdin": "/proc/self/fd/0",
		"/dev/stdout": "/proc/self/fd/1", "/dev/stderr": "/proc/self/fd/2",
	} {
		_ = os.Symlink(link, target)
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte(subtreeControl(string(controllers))), 0); err != nil {
		return nil, fmt.Errorf("enabling cgroup controllers: %w", err)
	}
	for _, g := range []string{"agent", "workload"} {
		if err := os.Mkdir("/sys/fs/cgroup/"+g, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	if err := os.WriteFile("/sys/fs/cgroup/agent/cgroup.procs", []byte("0"), 0); err != nil {
		return nil, fmt.Errorf("moving hangar-init to /agent: %w", err)
	}
	return os.Open("/sys/fs/cgroup/workload")
}

// subtreeControl enables every available controller for the children.
func subtreeControl(controllers string) string {
	var on []string
	for _, c := range strings.Fields(controllers) {
		on = append(on, "+"+c)
	}
	return strings.Join(on, " ")
}

// Network brings up lo, and each interface, found by its MAC, with its
// address and its default route; only one may have one.
func Network(ifaces []*guestv1.Interface) error {
	gateways := 0
	for _, i := range ifaces {
		if i.GetGateway() != "" {
			gateways++
		}
	}
	if gateways > 1 {
		return fmt.Errorf("%d interfaces have a default route; one may", gateways)
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		return err
	}
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	for _, want := range ifaces {
		link, err := linkByMAC(links, want.GetMac())
		if err != nil {
			return err
		}
		addr, err := netlink.ParseAddr(want.GetAddress())
		if err != nil {
			return fmt.Errorf("interface %s: address %q: %w", want.GetMac(), want.GetAddress(), err)
		}
		if err := netlink.AddrReplace(link, addr); err != nil {
			return err
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return err
		}
		if want.GetGateway() == "" {
			continue
		}
		gw := net.ParseIP(want.GetGateway())
		if gw == nil {
			return fmt.Errorf("interface %s: gateway %q", want.GetMac(), want.GetGateway())
		}
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}); err != nil {
			return fmt.Errorf("interface %s: default route: %w", want.GetMac(), err)
		}
	}
	return nil
}

func linkByMAC(links []netlink.Link, mac string) (netlink.Link, error) {
	want, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("MAC %q: %w", mac, err)
	}
	for _, l := range links {
		if l.Attrs().HardwareAddr.String() == want.String() {
			return l, nil
		}
	}
	return nil, fmt.Errorf("no interface has MAC %s", mac)
}

// Identity gives the container its hostname, /etc/hostname, /etc/hosts and
// /etc/resolv.conf: the platform's files, in /run/hangar, mounted over the
// image's as Docker does, so the image's are never written to.
func Identity(root string, cfg *guestv1.BootConfig) error {
	if h := cfg.GetHostname(); h != "" {
		if err := unix.Sethostname([]byte(h)); err != nil {
			return err
		}
	}
	files := map[string]string{
		"hostname":    cfg.GetHostname() + "\n",
		"hosts":       hosts(cfg),
		"resolv.conf": resolvConf(cfg.GetNameservers(), cfg.GetSearch()),
	}
	dir := filepath.Join(root, "run/hangar")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, content := range files {
		source := filepath.Join(dir, name)
		if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
			return err
		}
		a, err := placement(root, "/etc/"+name)
		if err != nil {
			return err
		}
		if a.write != "" {
			if err := os.MkdirAll(filepath.Dir(a.write), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(a.write, []byte(content), 0o644); err != nil {
				return err
			}
			continue
		}
		if a.create {
			if err := os.WriteFile(a.bind, nil, 0o644); err != nil {
				return err
			}
		}
		if err := unix.Mount(source, a.bind, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("mounting %s over %s: %w", name, a.bind, err)
		}
	}
	return nil
}

// action is how one of the platform's files reaches the path the image
// reads it from.
type action struct {
	// bind is the file to mount over; create makes it first, empty.
	bind   string
	create bool
	// write is a path under /run, the platform's own tmpfs, written to
	// directly: where an image's symlink points, such as systemd's
	// /run/systemd/resolve/stub-resolv.conf.
	write string
}

// placement decides how the platform's file reaches path in root.
func placement(root, path string) (action, error) {
	target, err := resolveIn(root, path)
	if err != nil {
		return action{}, err
	}
	if target == "/run" || strings.HasPrefix(target, "/run/") {
		return action{write: filepath.Join(root, target)}, nil
	}
	full := filepath.Join(root, target)
	_, err = os.Stat(full)
	switch {
	case err == nil:
		return action{bind: full}, nil
	case !errors.Is(err, os.ErrNotExist):
		return action{}, err
	case target != filepath.Clean(path):
		return action{}, fmt.Errorf("%s points at %s, which is not there", path, target)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return action{}, err
	}
	return action{bind: full, create: true}, nil
}

// resolveIn follows the symlinks in path as root's processes would see
// them, root being their /, and returns where it leads, which need not
// exist.
func resolveIn(root, path string) (string, error) {
	rest := strings.Split(path, "/")
	cur := "/"
	for links := 0; len(rest) > 0; {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(filepath.Join(root, next))
		if errors.Is(err, os.ErrNotExist) {
			return filepath.Join(append([]string{next}, rest...)...), nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > 40 {
			return "", fmt.Errorf("%s: %w", path, unix.ELOOP)
		}
		link, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(link) {
			cur = "/"
		}
		rest = append(strings.Split(link, "/"), rest...)
	}
	return cur, nil
}

// Unmount unmounts what it can but the root, innermost first, for the root
// to be remounted read-only before power-off. What is busy stays mounted.
func Unmount() {
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return
	}
	for _, target := range unmountOrder(string(info)) {
		_ = unix.Unmount(target, 0)
	}
}

// unmountOrder lists the mount points in mountinfo but /, the last mounted
// first, so a mount comes before what it was mounted on.
func unmountOrder(mountinfo string) []string {
	var order []string
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[4] == "/" {
			continue
		}
		order = append([]string{unescape(fields[4])}, order...)
	}
	return order
}

// unescape undoes mountinfo's octal escapes, such as \040 for a space.
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func hosts(cfg *guestv1.BootConfig) string {
	var b strings.Builder
	b.WriteString("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	if h := cfg.GetHostname(); h != "" {
		for _, i := range cfg.GetInterfaces() {
			if p, err := netip.ParsePrefix(i.GetAddress()); err == nil {
				fmt.Fprintf(&b, "%s\t%s\n", p.Addr(), h)
			}
		}
	}
	return b.String()
}

func resolvConf(nameservers, search []string) string {
	var b strings.Builder
	for _, ns := range nameservers {
		fmt.Fprintf(&b, "nameserver %s\n", ns)
	}
	if len(search) > 0 {
		fmt.Fprintf(&b, "search %s\n", strings.Join(search, " "))
	}
	return b.String()
}
