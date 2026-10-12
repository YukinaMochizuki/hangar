// Command hangar-node is the node agent. It runs as root on each host,
// reaches the apiserver over its node socket, and reports the host as the
// Node it is named for. See docs/architecture.md.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/node"
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
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	agent, err := node.New(node.Config{
		Name:          *name,
		HTTP:          client.UnixSocket(*socket),
		BaseURL:       client.UnixSocketURL,
		Reserved:      &computev1alpha1.NodeResources{Cpu: *reservedCPU, Memory: *reservedMemory, Disk: *reservedDisk},
		MaxPulls:      *maxPulls,
		MaxUnpackWork: *maxUnpackWork,
		Log:           log,
	})
	if err == nil {
		log.Info("node agent starting", "node", *name, "socket", *socket, "version", node.Version())
		err = agent.Run(ctx)
	}
	if err != nil {
		log.Error("node agent stopped", "error", err)
		os.Exit(1)
	}
}
