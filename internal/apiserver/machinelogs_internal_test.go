package apiserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// machineLogsOf reads the logs of the object of kind name as principal p, to
// the first error.
func machineLogsOf(t *testing.T, s *Server, p auth.Principal, kind, name string) error {
	t.Helper()
	srv := httptest.NewServer(s.Handler(auth.Chain{fixedAuth{p, true}}))
	t.Cleanup(srv.Close)
	stream, err := computev1alpha1connect.NewMachineCapabilityServiceClient(http.DefaultClient, srv.URL).
		Logs(t.Context(), connect.NewRequest(&computev1alpha1.LogsRequest{Project: "web", Kind: kind, Name: name}))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
	}
	return stream.Err()
}

// logsOf reads a container's logs as principal p, to the first error.
func logsOf(t *testing.T, s *Server, p auth.Principal, name string) error {
	t.Helper()
	return machineLogsOf(t, s, p, registry.Container.Kind, name)
}

// TestARecreatedContainerDoesNotReadTheOldMachinesLog: a container made
// again under the name of one deleted, while the old one's machine is not
// gone yet, is not given that machine's log: the machine is another
// container's. Owned by the container, the same machine leads on to its
// node.
func TestARecreatedContainerDoesNotReadTheOldMachinesLog(t *testing.T) {
	t.Parallel()
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"} })
	if _, err := s.backend(registry.Container).Apply(user, &computev1alpha1.Container{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: "api"},
		Spec:     &computev1alpha1.ContainerSpec{Image: "nginx"},
	}); err != nil {
		t.Fatal(err)
	}
	c, err := s.backend(registry.Container).Get(user, resource.Key{Project: "web", Name: "api"})
	if err != nil {
		t.Fatal(err)
	}
	uid := c.(*computev1alpha1.Container).GetMetadata().GetUid()
	owned := func(by string) *computev1alpha1.Machine {
		m := machine(machineName("api"), "n1")
		m.Metadata.OwnerReferences = []*metav1.OwnerReference{{ApiVersion: "compute.hangarlab.cloud/v1alpha1", Kind: "Container", Name: "api", Uid: by, Controller: true}}
		return m
	}
	if _, err := s.backend(registry.Machine).Apply(system, owned("the-deleted-containers-uid")); err != nil {
		t.Fatal(err)
	}
	alice := auth.Principal{Type: auth.User, ID: "alice@example.com"}
	wantCodeOf(t, logsOf(t, s, alice, "api"), connect.CodeFailedPrecondition, "the logs of a container recreated over an old machine")

	if _, err := s.backend(registry.Machine).Apply(system, owned(uid)); err != nil {
		t.Fatal(err)
	}
	// No node is connected: the call gets as far as the node.
	testutil.EventuallyWithin(t, 5*time.Second, "the logs of the machine's own container say unavailable", func() (bool, string) {
		err := logsOf(t, s, alice, "api")
		return connect.CodeOf(err) == connect.CodeUnavailable, fmt.Sprint(err)
	})
}

// TestGettingAContainerIsNotReadingItsLogs: the node agent may get any
// container, but not read its logs.
func TestGettingAContainerIsNotReadingItsLogs(t *testing.T) {
	t.Parallel()
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"} })
	if _, err := s.backend(registry.Container).Apply(user, &computev1alpha1.Container{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: "api"},
		Spec:     &computev1alpha1.ContainerSpec{Image: "nginx"},
	}); err != nil {
		t.Fatal(err)
	}
	node := auth.Principal{Type: auth.Node, ID: "n1"}
	if _, err := s.backend(registry.Container).Get(asNode(system, "n1"), resource.Key{Project: "web", Name: "api"}); err != nil {
		t.Fatalf("the node getting the container: %v", err)
	}
	wantCodeOf(t, logsOf(t, s, node, "api"), connect.CodePermissionDenied, "the node reading the container's logs")
}

// vmLogsOf reads a VM's serial console as principal p, to the first error.
func vmLogsOf(t *testing.T, s *Server, p auth.Principal, name string) error {
	t.Helper()
	return machineLogsOf(t, s, p, registry.VirtualMachine.Kind, name)
}

// TestGettingAVirtualMachineIsNotReadingItsLogs: the node agent may get any
// virtual machine, but not read its serial console; its owner may.
func TestGettingAVirtualMachineIsNotReadingItsLogs(t *testing.T) {
	t.Parallel()
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"} })
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vmFromImage("web", "noble")); err != nil {
		t.Fatal(err)
	}
	node := auth.Principal{Type: auth.Node, ID: "n1"}
	if _, err := s.backend(registry.VirtualMachine).Get(asNode(system, "n1"), resource.Key{Project: "web", Name: "web"}); err != nil {
		t.Fatalf("the node getting the virtual machine: %v", err)
	}
	wantCodeOf(t, vmLogsOf(t, s, node, "web"), connect.CodePermissionDenied, "the node reading the virtual machine's logs")
	// Its owner gets as far as its machine, which no controller made.
	alice := auth.Principal{Type: auth.User, ID: "alice@example.com"}
	wantCodeOf(t, vmLogsOf(t, s, alice, "web"), connect.CodeFailedPrecondition, "the owner reading the logs of a VM not on a node")
}
