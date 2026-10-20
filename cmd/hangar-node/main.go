// Command hangar-node is the node agent. It runs as root on each host,
// reaches the apiserver over its node socket, and reports the host as the
// Node it is named for. See docs/architecture.md.
//
// hangar-node vmm MACHINE_UID is the launcher of one VM, the main process
// of the VM's systemd unit (internal/node/driver/chvm); the agent starts
// those units, never this command itself. hangar-node vmm-exec is what the
// launcher runs Cloud Hypervisor through, as the VM's user. hangar-node
// check-placement takes the placement flags alone and fails, saying why,
// unless the node would report each as given.
//
// hangar-node natlog --port PORT --at TIME finds the machine an abuse
// report about the host's IPv4 address names, in the agent's NAT log.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"os"
	"os/signal"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/diskimage"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/zfsstore"
)

// defaultCmdline is the guest kernel's command line,
// HANGAR_KERNEL_CMDLINE in guest/kernel/kernel.env.
const defaultCmdline = "console=ttyS0 root=/dev/vda rw quiet spectre_bhi=on"

// hostFlags are what the host drivers are built with.
type hostFlags struct {
	network, uplink, publicIPv4     string
	publicIPv6                      string
	ipv6Prefix                      string
	dataset, imageDir               string
	maxUnpackWork                   int64
	cloudHypervisor, kernel, initrd string
	cmdline, vmDir                  string
	// firmware is what VirtualMachines boot through.
	firmware string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "vmm" {
		os.Exit(chvm.LauncherMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "vmm-exec" {
		os.Exit(chvm.ExecMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "check-placement" {
		os.Exit(checkPlacementMain(os.Args[2:], os.Stderr))
	}
	if code, ok := natlogCommand(os.Args[1:]); ok {
		os.Exit(code)
	}
	var hf hostFlags
	var (
		name           = flag.String("name", "", "the Node this agent reports as; the apiserver's --node-name")
		socket         = flag.String("socket", "/run/hangar/node.sock", "the apiserver's node socket")
		reservedMemory = flag.String("reserved-memory", "", "memory kept back for the host, including the ZFS ARC, as a quantity such as 4Gi")
		reservedCPU    = flag.String("reserved-cpu", "", "CPUs kept back for the host, as a whole number")
		maxPulls       = flag.Int("max-pulls", node.DefaultMaxPulls, "images unpacked at once; fetching them takes no turn")
		reservedDisk   = flag.String("reserved-disk", "", "storage kept back from root disks, such as for the image cache, as a quantity such as 50Gi")
		hostDrivers    = flag.String("drivers", "none", "the host drivers: none, to report the node only; fake, a simulated host; or host, this host's")
		dnsUpstreams   = flag.String("dns-upstreams", "1.1.1.1,8.8.8.8", "the resolvers each network's resolver forwards names outside hangar.internal to, comma-separated addresses")
		edgeSocket     = flag.String("edge-dial-socket", "", "the unix socket hangar-edge asks for connections to public endpoints on; empty disables it")
		edgeUser       = flag.String("edge-user", "", "the user hangar-edge runs as: the only peer the edge dial socket takes, and its group the socket's; required with --edge-dial-socket")
		natLog         = flag.String("nat-log-dir", defaultNATLog, "with this host's network driver, where each new IPv4 connection out of the networks is logged for 30 days, root's alone; empty logs none")
		cacheHigh      = flag.String("image-cache-high", fmt.Sprintf("%dGi", node.DefaultImageCacheHigh>>30), "what the container images no machine uses may keep, their cached blobs and their bases, past which the least recently used go; count it in --reserved-disk")
		cacheLow       = flag.String("image-cache-low", "", "what is left of the images no machine uses once some go past --image-cache-high; empty for four fifths of it")
	)
	placement := bindPlacement(flag.CommandLine)
	hf.bind(flag.CommandLine)
	flag.Usage = func() { usage(flag.CommandLine) }
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	prefix, err := hf.prefix()
	if err != nil {
		log.Error("node agent stopped", "error", err)
		os.Exit(1)
	}
	d, closeDrivers, err := drivers(ctx, *hostDrivers, hf)
	if err != nil {
		log.Error("node agent stopped", "error", err)
		os.Exit(1)
	}
	defer closeDrivers()
	upstreams, err := parseUpstreams(*dnsUpstreams)
	if err != nil {
		log.Error("node agent stopped", "error", err)
		os.Exit(1)
	}
	high, low, err := parseImageCache(*cacheHigh, *cacheLow)
	if err != nil {
		log.Error("node agent stopped", "error", err)
		os.Exit(1)
	}
	edgeDial, edgeUIDs, err := listenEdgeDial(*edgeSocket, *edgeUser)
	if err != nil {
		log.Error("node agent stopped", "error", err)
		os.Exit(1)
	}
	agent, err := node.New(node.Config{
		Name:    *name,
		HTTP:    client.UnixSocket(*socket),
		BaseURL: client.UnixSocketURL,
		Tunnel: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", *socket)
		},
		Reserved:       &computev1alpha1.NodeResources{Cpu: *reservedCPU, Memory: *reservedMemory, Disk: *reservedDisk},
		Placement:      placement,
		MaxPulls:       *maxPulls,
		MaxUnpackWork:  hf.maxUnpackWork,
		Drivers:        d,
		DNSUpstreams:   upstreams,
		IPv6Prefix:     prefix,
		EdgeDial:       edgeDial,
		EdgeUIDs:       edgeUIDs,
		NATLog:         *natLog,
		ImageCacheHigh: high,
		ImageCacheLow:  low,
		Log:            log,
	})
	if err == nil {
		log.Info("node agent starting", "node", *name, "socket", *socket, "drivers", *hostDrivers, "version", node.Version())
		err = agent.Run(ctx)
	}
	if err != nil {
		log.Error("node agent stopped", "error", err)
		closeDrivers()
		os.Exit(1)
	}
}

// listenEdgeDial makes the edge dial socket at path, root's and user's
// group's, 0660, and returns it with the uid it takes peers of: user's.
// Nothing for an empty path.
func listenEdgeDial(path, user string) (net.Listener, []uint32, error) {
	if path == "" {
		return nil, nil, nil
	}
	if user == "" {
		return nil, nil, errors.New("--edge-dial-socket needs --edge-user: the socket takes hangar-edge alone")
	}
	u, err := osuser.Lookup(user)
	if err != nil {
		return nil, nil, fmt.Errorf("--edge-user: %w", err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("--edge-user %s: uid %q: %w", user, u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, nil, fmt.Errorf("--edge-user %s: gid %q: %w", user, u.Gid, err)
	}
	if uid == 0 {
		return nil, nil, errors.New("--edge-user is root; hangar-edge runs as its own unprivileged user")
	}
	// A socket left by an agent that did not stop cleanly is replaced.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, nil, fmt.Errorf("--edge-dial-socket: %w", err)
	}
	// Until both hold, the socket is root's alone, as umask leaves it.
	err = os.Chown(path, 0, gid)
	if err == nil {
		err = os.Chmod(path, 0o660)
	}
	if err != nil {
		_ = ln.Close()
		return nil, nil, fmt.Errorf("--edge-dial-socket: %w", err)
	}
	return ln, []uint32{uint32(uid)}, nil
}

// drivers returns the host drivers the flags select, and what releases
// them.
func drivers(ctx context.Context, kind string, hf hostFlags) (*node.Drivers, func(), error) {
	none := func() {}
	switch kind {
	case "none":
		return nil, none, nil
	case "fake":
		return fakeDrivers(hf)
	case "host":
		return hostDrivers(ctx, hf)
	}
	return nil, none, fmt.Errorf("--drivers=%q is none, fake or host", kind)
}

func fakeDrivers(hf hostFlags) (*node.Drivers, func(), error) {
	none := func() {}
	h := fake.New()
	prefix, err := hf.prefix()
	if err != nil {
		return nil, none, err
	}
	h.SetIPv6Prefix(prefix)
	d := &node.Drivers{Images: h.Images(), DiskImages: h.DiskImages(), Storage: h.Storage(), Network: h.Network(), Hypervisor: h.Hypervisor(), Guest: h.Guest()}
	switch hf.network {
	case "fake":
		return d, none, nil
	case "host":
	default:
		return nil, none, fmt.Errorf("--network-driver=%q is neither fake nor host", hf.network)
	}
	n, err := network(hf)
	if err != nil {
		return nil, none, err
	}
	d.Network = n
	return d, func() { _ = n.Close() }, nil
}

func network(hf hostFlags) (*hostnet.Driver, error) {
	var public netip.Addr
	if hf.publicIPv4 != "" {
		var err error
		if public, err = netip.ParseAddr(hf.publicIPv4); err != nil {
			return nil, fmt.Errorf("--public-ipv4: %w", err)
		}
	}
	prefix, err := hf.prefix()
	if err != nil {
		return nil, err
	}
	var public6 netip.Addr
	if hf.publicIPv6 != "" {
		if public6, err = netip.ParseAddr(hf.publicIPv6); err != nil {
			return nil, fmt.Errorf("--public-ipv6: %w", err)
		}
	}
	n, err := hostnet.New(hostnet.Config{Uplink: hf.uplink, PublicIPv4: public, IPv6Prefix: prefix, PublicIPv6: public6})
	if err != nil {
		return nil, fmt.Errorf("network driver: %w", err)
	}
	return n, nil
}

// bind defines the flags the host drivers are built with on fs.
func (hf *hostFlags) bind(fs *flag.FlagSet) {
	fs.Int64Var(&hf.maxUnpackWork, "max-unpack-work", ocilayer.DefaultMaxWork, "steps of work unpacking one image may take, about a million for 45,000 files; past it the image is too large")
	fs.StringVar(&hf.network, "network-driver", "fake", "with --drivers=fake, the network driver: fake, or host for this host's (needs --uplink)")
	fs.StringVar(&hf.uplink, "uplink", "", "with this host's network driver, the link the host reaches the internet through")
	fs.StringVar(&hf.publicIPv4, "public-ipv4", "", "with this host's network driver, the address networks' traffic leaves the uplink as; empty for the uplink's own")
	fs.StringVar(&hf.publicIPv6, "public-ipv6", "", "with this host's network driver, the host's own public IPv6 address, where machines reach the front door (:80 and :443) as they do at --public-ipv4; empty, they reach nothing of the host's over IPv6")
	fs.StringVar(&hf.ipv6Prefix, "public-ipv6-prefix", "", "the host's public IPv6 /64, such as 2001:db8:0:1::/64, on the uplink's link, from which each machine interface gets an address; empty for no IPv6 in the networks")
	fs.StringVar(&hf.dataset, "zfs-dataset", "hangar", "for --drivers=host, the dataset root disks and image bases go under")
	fs.StringVar(&hf.imageDir, "image-dir", "/var/lib/hangar/images", "for --drivers=host, where images are cached, unpacked and downloaded: the hangar/images dataset's mount point")
	fs.StringVar(&hf.cloudHypervisor, "cloud-hypervisor", "/usr/local/bin/cloud-hypervisor", "for --drivers=host, Cloud Hypervisor")
	fs.StringVar(&hf.kernel, "guest-kernel", "/usr/local/lib/hangar/vmlinux", "for --drivers=host, the guests' kernel")
	fs.StringVar(&hf.initrd, "guest-initramfs", "/usr/local/lib/hangar/initramfs.cpio", "for --drivers=host, the guests' initramfs, with hangar-init")
	fs.StringVar(&hf.cmdline, "guest-cmdline", defaultCmdline, "for --drivers=host, the guests' kernel command line")
	fs.StringVar(&hf.vmDir, "vm-dir", chvm.DefaultDir, "for --drivers=host, the VMs' directories: what they run, their sockets and their console logs")
	fs.StringVar(&hf.firmware, "vm-firmware", "/usr/local/lib/hangar/CLOUDHV.fd", "for --drivers=host, the firmware virtual machines boot through; without the file, they are refused")
}

// prefix reads --public-ipv6-prefix; invalid when it is empty.
func (hf hostFlags) prefix() (netip.Prefix, error) {
	if hf.ipv6Prefix == "" {
		return netip.Prefix{}, nil
	}
	p, err := computeapi.ParseIPv6Prefix(hf.ipv6Prefix)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("--public-ipv6-prefix: %w", err)
	}
	return p, nil
}

// parseImageCache reads --image-cache-high and --image-cache-low, the
// latter zero when empty.
func parseImageCache(high, low string) (int64, int64, error) {
	h, err := quantity.Bytes(high)
	if err != nil || h == 0 || h > math.MaxInt64 {
		return 0, 0, fmt.Errorf("--image-cache-high: %q is not a size above zero", high)
	}
	if low == "" {
		return int64(h), 0, nil
	}
	l, err := quantity.Bytes(low)
	if err != nil || l == 0 || l > h {
		return 0, 0, fmt.Errorf("--image-cache-low: %q is not a size above zero and up to --image-cache-high", low)
	}
	return int64(h), int64(l), nil
}

// parseUpstreams reads --dns-upstreams: addresses, each on port 53.
func parseUpstreams(s string) ([]netip.AddrPort, error) {
	var out []netip.AddrPort
	for _, a := range strings.Split(s, ",") {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		addr, err := netip.ParseAddr(a)
		if err != nil {
			return nil, fmt.Errorf("--dns-upstreams: %w", err)
		}
		out = append(out, netip.AddrPortFrom(addr, 53))
	}
	return out, nil
}

// registryConfig is the image driver's configuration from the flags.
func registryConfig(hf hostFlags) registry.Config {
	return registry.Config{
		Cache:  filepath.Join(hf.imageDir, "cache"),
		Limits: ocilayer.Limits{MaxWork: hf.maxUnpackWork},
	}
}

// hostDrivers are this host's: images from registries into a cache, disks
// on ZFS, networks in namespaces, VMs under Cloud Hypervisor as systemd
// units.
func hostDrivers(ctx context.Context, hf hostFlags) (*node.Drivers, func(), error) {
	none := func() {}
	images, err := registry.New(registryConfig(hf))
	if err != nil {
		return nil, none, fmt.Errorf("image driver: %w", err)
	}
	storage, err := zfsstore.New(ctx, zfsstore.Config{Dataset: hf.dataset, Scratch: filepath.Join(hf.imageDir, "unpack")})
	if err != nil {
		return nil, none, fmt.Errorf("storage driver: %w", err)
	}
	diskImages, err := diskimage.New(diskimage.Config{Dir: filepath.Join(hf.imageDir, "disk-images")})
	if err != nil {
		return nil, none, fmt.Errorf("disk image driver: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, none, err
	}
	firmware := hf.firmware
	if _, err := os.Stat(firmware); errors.Is(err, os.ErrNotExist) {
		slog.Warn("no firmware: virtual machines are refused on this host", "vm_firmware", firmware)
		firmware = ""
	}
	vms, err := chvm.New(ctx, chvm.Config{
		Firmware: firmware,
		Dir:      hf.vmDir, CloudHypervisor: hf.cloudHypervisor, Kernel: hf.kernel, Initramfs: hf.initrd, Cmdline: hf.cmdline,
		Launcher: []string{exe, "vmm"},
	})
	if err != nil {
		return nil, none, fmt.Errorf("hypervisor driver: %w", err)
	}
	n, err := network(hf)
	if err != nil {
		_ = vms.Close()
		return nil, none, err
	}
	d := &node.Drivers{Images: images, DiskImages: diskImages, Storage: storage, Network: n, Hypervisor: vms, Guest: vms.Guest()}
	return d, func() { _ = errors.Join(n.Close(), vms.Close()) }, nil
}
