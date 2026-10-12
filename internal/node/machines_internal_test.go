package node

import (
	"testing"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// TestAnotherNodesMachineIsNeverBuilt: whatever queues a machine placed on
// another node, the reconcile builds nothing for it. No watch mapping
// queues one today, so the reconcile is called directly.
func TestAnotherNodesMachineIsNeverBuilt(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	host := fake.New()
	host.AddImage("nginx", "sha256:1", driver.ImageConfig{})
	a, err := New(Config{
		Name: apiservertest.Node, HTTP: client.UnixSocket(env.Socket), BaseURL: client.UnixSocketURL,
		Drivers: &Drivers{Images: host.Images(), Storage: host.Storage(), Network: host.Network(), Hypervisor: host.Hypervisor(), Guest: host.Guest()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.AsSystem().Apply(t.Context(), registry.Machine, &computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "elsewhere"},
		Spec: &computev1alpha1.MachineSpec{
			Node:          "n2",
			Resources:     &computev1alpha1.Resources{Cpu: "1", Memory: "512Mi", Disk: "10Gi"},
			RunState:      computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
			Boot:          &computev1alpha1.MachineSpec_Container{Container: &computev1alpha1.ContainerBoot{Image: "nginx"}},
			RestartPolicy: computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcileMachine(t.Context(), controller.Key{Project: apiservertest.Project, Name: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if s := host.Snapshot(); !s.Empty() {
		t.Errorf("built another node's machine: %+v", s)
	}
}
