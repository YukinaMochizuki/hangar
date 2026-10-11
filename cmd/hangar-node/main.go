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
)

func main() {
	var (
		name           = flag.String("name", "", "the Node this agent reports as; the apiserver's --node-name")
		socket         = flag.String("socket", "/run/hangar/node.sock", "the apiserver's node socket")
		reservedMemory = flag.Int64("reserved-memory-mib", 0, "memory kept back for the host, including the ZFS ARC")
		reservedVCPUs  = flag.Int("reserved-vcpus", 0, "vCPUs kept back for the host")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	agent, err := node.New(node.Config{
		Name:     *name,
		HTTP:     client.UnixSocket(*socket),
		BaseURL:  client.UnixSocketURL,
		Reserved: &computev1alpha1.NodeResources{Vcpus: int32(*reservedVCPUs), MemoryMib: *reservedMemory},
		Log:      log,
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
