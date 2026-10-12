// Command hangar-node is the node agent. It runs as root on each host,
// reaches the apiserver over its node socket, and reports the host as the
// Node it is named for. See docs/architecture.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/node"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
)

func main() {
	var (
		name           = flag.String("name", "", "the Node this agent reports as; the apiserver's --node-name")
		socket         = flag.String("socket", "/run/hangar/node.sock", "the apiserver's node socket")
		reservedMemory = flag.String("reserved-memory", "", "memory kept back for the host, including the ZFS ARC, as a quantity such as 4Gi")
		reservedCPU    = flag.String("reserved-cpu", "", "CPUs kept back for the host, as a whole number")
		maxPulls       = flag.Int("max-pulls", node.DefaultMaxPulls, "images unpacked at once; fetching them takes no turn")
		maxUnpackWork  = flag.Int64("max-unpack-work", ocilayer.DefaultMaxWork, "steps of work unpacking one image may take, about a million for 45,000 files; past it the image is too large")
		reservedDisk   = flag.String("reserved-disk", "", "storage kept back from root disks, such as for the image cache, as a quantity such as 50Gi")
		hostDrivers    = flag.String("drivers", "none", "the host drivers: none, to report the node only, or fake, a simulated host")
		networkDriver  = flag.String("network-driver", "fake", "with --drivers=fake, the network driver: fake, or host for this host's (needs --uplink)")
		uplink         = flag.String("uplink", "", "for --network-driver=host, the link the host reaches the internet through")
		publicIPv4     = flag.String("public-ipv4", "", "for --network-driver=host, the address networks' traffic leaves the uplink as; empty for the uplink's own")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d, closeDrivers, err := drivers(*hostDrivers, *networkDriver, *uplink, *publicIPv4)
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
		MaxUnpackWork: *maxUnpackWork,
		Drivers:       d,
		Log:           log,
	})
	if err == nil {
		log.Info("node agent starting", "node", *name, "socket", *socket, "version", node.Version())
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
func drivers(kind, network, uplink, publicIPv4 string) (*node.Drivers, func(), error) {
	none := func() {}
	switch kind {
	case "none":
		return nil, none, nil
	case "fake":
	default:
		return nil, none, fmt.Errorf("--drivers=%q is neither none nor fake", kind)
	}
	h := fake.New()
	d := &node.Drivers{Images: h.Images(), Storage: h.Storage(), Network: h.Network(), Hypervisor: h.Hypervisor(), Guest: h.Guest()}
	switch network {
	case "fake":
		return d, none, nil
	case "host":
	default:
		return nil, none, fmt.Errorf("--network-driver=%q is neither fake nor host", network)
	}
	var public netip.Addr
	if publicIPv4 != "" {
		var err error
		if public, err = netip.ParseAddr(publicIPv4); err != nil {
			return nil, none, fmt.Errorf("--public-ipv4: %w", err)
		}
	}
	n, err := hostnet.New(hostnet.Config{Uplink: uplink, PublicIPv4: public})
	if err != nil {
		return nil, none, fmt.Errorf("network driver: %w", err)
	}
	d.Network = n
	return d, func() { _ = n.Close() }, nil
}
