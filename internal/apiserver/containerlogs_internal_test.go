package apiserver

import (
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
)

// logsOf reads a container's logs as principal p, to the first error.
func logsOf(t *testing.T, s *Server, p auth.Principal, name string) error {
	t.Helper()
	srv := httptest.NewServer(s.Handler(auth.Chain{fixedAuth{p, true}}))
	t.Cleanup(srv.Close)
	stream, err := computev1alpha1connect.NewContainerLogsServiceClient(http.DefaultClient, srv.URL).
		ContainerLogs(t.Context(), connect.NewRequest(&computev1alpha1.ContainerLogsRequest{Project: "web", Name: name}))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
	}
	return stream.Err()
}

// TestARecreatedContainerDoesNotReadTheOldMachinesLog: a container made
// again under the name of one deleted, while the old one's machine is not
// gone yet, is not given that machine's log: the machine is another
// container's. Owned by the container, the same machine leads on to its
// node.
func TestARecreatedContainerDoesNotReadTheOldMachinesLog(t *testing.T) {
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
	deadline := time.Now().Add(5 * time.Second)
	for err := logsOf(t, s, alice, "api"); connect.CodeOf(err) != connect.CodeUnavailable; err = logsOf(t, s, alice, "api") {
		if time.Now().After(deadline) {
			t.Fatalf("the logs of the machine's own container: %v, want unavailable", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGettingAContainerIsNotReadingItsLogs: the node agent may get any
// container, but not read its logs.
func TestGettingAContainerIsNotReadingItsLogs(t *testing.T) {
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
