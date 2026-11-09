package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestContainerUsage: a running container's usage, as its node samples it,
// reaches its owner: the memory its guest says it uses, the share of its
// vCPUs it keeps busy, and its machine's shape.
func TestContainerUsage(t *testing.T) {
	t.Parallel()
	w := newIdleWorld(t)
	w.usagePoll = 50 * time.Millisecond
	w.startAgent()
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	m := w.machine("api")
	w.host.UseGuestMemory(m.GetMetadata().GetUid(), 123<<20)
	if err := w.host.SpinCPU(m.GetMetadata().GetUid(), 0.5); err != nil {
		t.Fatal(err)
	}
	c := w.logsAs(apiservertest.Root)
	var res *computev1alpha1.UsageResponse
	// A sample's share is measured over real time between two reads, so
	// it is near half, not exactly.
	testutil.Eventually(t, "a sample of the guest's memory and of half its vCPUs busy", func() (bool, string) {
		r, err := c.Usage(context.Background(), connect.NewRequest(&computev1alpha1.UsageRequest{Project: project, Kind: registry.Container.Kind, Name: "api"}))
		if err != nil {
			return false, err.Error()
		}
		res = r.Msg
		l := res.GetLatest()
		return l.GetMemoryBytes() == 123<<20 && l.GetCpu() > 0.4 && l.GetCpu() < 0.6, fmt.Sprint(res)
	})
	vcpus, err := quantity.CPU(m.GetSpec().GetResources().GetCpu())
	if err != nil {
		t.Fatal(err)
	}
	memory, err := quantity.Bytes(m.GetSpec().GetResources().GetMemory())
	if err != nil {
		t.Fatal(err)
	}
	if res.GetVcpus() != vcpus || res.GetMemoryLimitBytes() != int64(memory) ||
		res.GetMemoryKind() != computev1alpha1.MachineMemoryKind_MACHINE_MEMORY_KIND_GUEST_USED {
		t.Errorf("the usage is %v, of a machine of %d vCPUs and %d bytes", res, vcpus, memory)
	}
}
