// Command hangar-node is the node agent. It runs as root on each host,
// reaches the apiserver over its node socket, and reports the host as the
// Node it is named for. See docs/architecture.md.
//
// hangar-node vmm MACHINE_UID is the launcher of one VM, the main process
// of the VM's systemd unit (internal/node/driver/chvm); the agent starts
// those units, never this command itself.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/node"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
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
	dataset, imageDir               string
	maxUnpackWork                   int64
	cloudHypervisor, kernel, initrd string
	cmdline, vmDir, nameservers     string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "vmm" {
		os.Exit(chvm.LauncherMain(os.Args[2:]))
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
	)
	hf.bind(flag.CommandLine)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d, closeDrivers, err := drivers(ctx, *hostDrivers, hf)
	if err != nil {
		log.Error("node agent stopped", "error", err)
		os.Exit(1)
	}
	defer closeDrivers()
	agent, err := node.New(node.Config{
		Name:          *name,
		HTTP:          client.UnixSocket(*socket),
		BaseURL:       client.UnixSocketURL,
		Reserved:      &computev1alpha1.NodeResources{Cpu: *reservedCPU, Memory: *reservedMemory, Disk: *reservedDisk},
		MaxPulls:      *maxPulls,
		MaxUnpackWork: hf.maxUnpackWork,
		Drivers:       d,
		Log:           log,
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
	d := &node.Drivers{Images: h.Images(), Storage: h.Storage(), Network: h.Network(), Hypervisor: h.Hypervisor(), Guest: h.Guest()}
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
	n, err := hostnet.New(hostnet.Config{Uplink: hf.uplink, PublicIPv4: public})
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
	fs.StringVar(&hf.dataset, "zfs-dataset", "hangar", "for --drivers=host, the dataset root disks and image bases go under")
	fs.StringVar(&hf.imageDir, "image-dir", "/var/lib/hangar/images", "for --drivers=host, where images are cached and unpacked: the hangar/images dataset's mount point")
	fs.StringVar(&hf.cloudHypervisor, "cloud-hypervisor", "/usr/local/bin/cloud-hypervisor", "for --drivers=host, Cloud Hypervisor")
	fs.StringVar(&hf.kernel, "guest-kernel", "/usr/local/lib/hangar/vmlinux", "for --drivers=host, the guests' kernel")
	fs.StringVar(&hf.initrd, "guest-initramfs", "/usr/local/lib/hangar/initramfs.cpio", "for --drivers=host, the guests' initramfs, with hangar-init")
	fs.StringVar(&hf.cmdline, "guest-cmdline", defaultCmdline, "for --drivers=host, the guests' kernel command line")
	fs.StringVar(&hf.vmDir, "vm-dir", chvm.DefaultDir, "for --drivers=host, the VMs' directories: what they run, their sockets and their console logs")
	fs.StringVar(&hf.nameservers, "nameservers", "1.1.1.1,8.8.8.8", "for --drivers=host, the guests' resolvers, comma-separated")
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
	exe, err := os.Executable()
	if err != nil {
		return nil, none, err
	}
	var nameservers []string
	for _, s := range strings.Split(hf.nameservers, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if _, err := netip.ParseAddr(s); err != nil {
			return nil, none, fmt.Errorf("--nameservers: %w", err)
		}
		nameservers = append(nameservers, s)
	}
	vms, err := chvm.New(ctx, chvm.Config{
		Dir: hf.vmDir, CloudHypervisor: hf.cloudHypervisor, Kernel: hf.kernel, Initramfs: hf.initrd, Cmdline: hf.cmdline,
		Launcher: []string{exe, "vmm"}, Nameservers: nameservers,
	})
	if err != nil {
		return nil, none, fmt.Errorf("hypervisor driver: %w", err)
	}
	n, err := network(hf)
	if err != nil {
		_ = vms.Close()
		return nil, none, err
	}
	d := &node.Drivers{Images: images, Storage: storage, Network: n, Hypervisor: vms, Guest: vms.Guest()}
	return d, func() { _ = errors.Join(n.Close(), vms.Close()) }, nil
}
