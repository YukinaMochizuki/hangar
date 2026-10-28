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

// SwitchRoot mounts the root disk, grows its filesystem to the disk (or says
// why not with logf), mounts the volumes in it, FileShares as the NFS client
// clientID, makes it the root keeping /dev, /proc and /sys, and drops the
// initramfs; hangar-init runs on from memory. It returns the volumes on
// disks mounted read-write.
func SwitchRoot(volumes []*guestv1.Volume, clientID string, logf func(format string, args ...any)) (Mounted, error) {
	if err := (mount{RootDevice, newRoot, "ext4", "", 0}).do(); err != nil {
		return nil, err
	}
	if from, to, err := GrowRoot(newRoot, RootDevice); err != nil {
		logf("hangar-init: the root filesystem stays as it is: %v", err)
	} else if to > from {
		logf("hangar-init: grew the root filesystem from %d to %d blocks", from, to)
	}
	// Before the switch: mke2fs is in the initramfs.
	mounted, err := Volumes(newRoot, volumes, clientID, logf)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{"/dev", "/proc", "/sys"} {
		target := newRoot + dir
		if err := os.MkdirAll(target, 0o755); err != nil {
			return nil, err
		}
		if err := unix.Mount(dir, target, "", unix.MS_MOVE, ""); err != nil {
			return nil, fmt.Errorf("moving %s: %w", dir, err)
		}
	}
	// The initramfs holds only its two binaries and empty directories.
	_ = os.Remove("/init")
	_ = os.Remove(Mke2fs)
	if err := unix.Chdir(newRoot); err != nil {
		return nil, err
	}
	if err := unix.Mount(newRoot, "/", "", unix.MS_MOVE, ""); err != nil {
		return nil, fmt.Errorf("moving the root disk to /: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return nil, err
	}
	return mounted, unix.Chdir("/")
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
// addresses and its default routes; only one may have an IPv4 default
// route, and one an IPv6 one.
func Network(ifaces []*guestv1.Interface) error {
	gateways, gateways6 := 0, 0
	for _, i := range ifaces {
		if i.GetGateway() != "" {
			gateways++
		}
		if i.GetIpv6Gateway() != "" {
			gateways6++
		}
	}
	if gateways > 1 {
		return fmt.Errorf("%d interfaces have a default route; one may", gateways)
	}
	if gateways6 > 1 {
		return fmt.Errorf("%d interfaces have an IPv6 default route; one may", gateways6)
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
		if v6 := want.GetIpv6(); v6 != "" {
			a, err := netlink.ParseAddr(v6)
			if err != nil || a.IP.To4() != nil {
				return fmt.Errorf("interface %s: IPv6 address %q", want.GetMac(), v6)
			}
			// The address is the guest's alone, which the host's binding
			// enforces, so it is usable at once.
			a.Flags |= unix.IFA_F_NODAD
			if err := netlink.AddrReplace(link, a); err != nil {
				return err
			}
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return err
		}
		if want.GetGateway() != "" {
			gw := net.ParseIP(want.GetGateway())
			if gw == nil {
				return fmt.Errorf("interface %s: gateway %q", want.GetMac(), want.GetGateway())
			}
			if err := netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}); err != nil {
				return fmt.Errorf("interface %s: default route: %w", want.GetMac(), err)
			}
		}
		if want.GetIpv6Gateway() != "" {
			gw := net.ParseIP(want.GetIpv6Gateway())
			if gw == nil || gw.To4() != nil {
				return fmt.Errorf("interface %s: IPv6 gateway %q", want.GetMac(), want.GetIpv6Gateway())
			}
			if err := netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}, Gw: gw}); err != nil {
				return fmt.Errorf("interface %s: IPv6 default route: %w", want.GetMac(), err)
			}
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
	rootfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(rootfd) }()
	for name, content := range files {
		source := filepath.Join(dir, name)
		if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
			return err
		}
		if err := provide(root, rootfd, "/etc/"+name, source, content); err != nil {
			return fmt.Errorf("/etc/%s: %w", name, err)
		}
	}
	return nil
}

// provide puts the platform's file source, holding content, where path
// leads in root: written there under /run, mounted over it elsewhere.
func provide(root string, rootfd int, path, source, content string) error {
	p, err := placement(root, rootfd, path)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(p.fd) }()
	if p.write {
		if err := unix.Ftruncate(p.fd, 0); err != nil {
			return err
		}
		if n, err := unix.Write(p.fd, []byte(content)); err != nil || n != len(content) {
			return fmt.Errorf("writing %s: %d bytes of %d: %w", p.where, n, len(content), err)
		}
		return nil
	}
	tree, err := unix.OpenTree(unix.AT_FDCWD, source, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(tree) }()
	if err := unix.MoveMount(tree, "", p.fd, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return fmt.Errorf("mounting it over %s: %w", p.where, err)
	}
	return nil
}

// placed is where one of the platform's files goes, held by its fd.
type placed struct {
	fd    int
	where string
	// write: under /run, the platform's own tmpfs, it is written, not
	// mounted over.
	write bool
}

func underRun(p string) bool { return p == "/run" || strings.HasPrefix(p, "/run/") }

// placement finds where the platform's file for path goes in root, every
// name resolved in root by the kernel and held by its fd, as a share over
// /etc may be changed meanwhile: an existing file, to mount over; a missing
// one, made empty, to mount over; one under /run, or that a symlink to
// nowhere puts there, such as systemd's stub-resolv.conf, to write.
func placement(root string, rootfd int, path string) (placed, error) {
	fd, err := openat2(rootfd, path, &unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: inRoot})
	if err == nil {
		return existing(root, rootfd, fd)
	}
	if !errors.Is(err, unix.ENOENT) {
		return placed{}, err
	}
	dir, name := filepath.Split(path)
	parent, err := makeDirIn(root, rootfd, dir, platformOwned)
	if err != nil {
		return placed{}, err
	}
	defer func() { _ = unix.Close(parent) }()
	under, err := whereIn(root, parent)
	if err != nil {
		return placed{}, err
	}
	nfd, err := unix.Openat(parent, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err == nil {
		where := filepath.Join(under, name)
		if platformOwned(where) && !underRun(where) {
			_ = unix.Close(nfd)
			return placed{}, fmt.Errorf("it leads to %s, where the platform mounts its own", where)
		}
		return placed{fd: nfd, where: where, write: underRun(where)}, nil
	}
	if !errors.Is(err, unix.EEXIST) {
		return placed{}, err
	}
	// A symlink to nowhere, followed only into /run.
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(parent, name, buf)
	if err != nil {
		return placed{}, err
	}
	lead := string(buf[:n])
	if !filepath.IsAbs(lead) {
		lead = filepath.Join(under, lead)
	}
	lead = filepath.Clean(lead)
	nowhere := fmt.Errorf("%s points at %s, which is not there", path, lead)
	ldir, lname := filepath.Split(lead)
	lfd, err := makeDirIn(root, rootfd, ldir, func(w string) bool { return !underRun(w) })
	if err != nil {
		return placed{}, nowhere
	}
	defer func() { _ = unix.Close(lfd) }()
	if w, err := whereIn(root, lfd); err != nil || !underRun(w) {
		return placed{}, nowhere
	}
	wfd, err := unix.Openat(lfd, lname, unix.O_CREAT|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return placed{}, err
	}
	where, err := whereIn(root, wfd)
	if err != nil {
		_ = unix.Close(wfd)
		return placed{}, err
	}
	return placed{fd: wfd, where: where, write: true}, nil
}

// existing places the platform's file over fd, an existing file in root:
// written if it is under /run, else mounted over.
func existing(root string, rootfd, fd int) (placed, error) {
	where, err := whereIn(root, fd)
	if err != nil {
		_ = unix.Close(fd)
		return placed{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return placed{}, fmt.Errorf("%s is not a file", where)
	}
	switch {
	case underRun(where):
		_ = unix.Close(fd)
		// /run is the platform's, so nobody else changes it meanwhile.
		wfd, err := openat2(rootfd, where, &unix.OpenHow{Flags: unix.O_WRONLY | unix.O_CLOEXEC, Resolve: inRoot | unix.RESOLVE_NO_SYMLINKS})
		if err != nil {
			return placed{}, err
		}
		return placed{fd: wfd, where: where, write: true}, nil
	case platformOwned(where):
		_ = unix.Close(fd)
		return placed{}, fmt.Errorf("it leads to %s, where the platform mounts its own", where)
	}
	return placed{fd: fd, where: where}, nil
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
			for _, a := range []string{i.GetAddress(), i.GetIpv6()} {
				if p, err := netip.ParsePrefix(a); err == nil {
					fmt.Fprintf(&b, "%s\t%s\n", p.Addr(), h)
				}
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
