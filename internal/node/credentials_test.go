package node

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// credentialsFor answers every machine with a login for one registry.
type credentialsFor struct {
	registry string
	asked    []*nodev1.ResolveImagePullCredentialsRequest
}

func (c *credentialsFor) ResolveImagePullCredentials(_ context.Context, req *connect.Request[nodev1.ResolveImagePullCredentialsRequest]) (*connect.Response[nodev1.ResolveImagePullCredentialsResponse], error) {
	c.asked = append(c.asked, req.Msg)
	return connect.NewResponse(&nodev1.ResolveImagePullCredentialsResponse{Registry: c.registry, Username: "bot", Password: "pass"}), nil
}

// TestALoginIsForItsRegistryAlone: the agent gives the Images driver the
// login the apiserver answers only for the registry that login is for, by
// its normalized host; any other registry the driver asks about, as a
// token server or a redirect may make it, gets none.
func TestALoginIsForItsRegistryAlone(t *testing.T) {
	m := &computev1alpha1.Machine{Metadata: &metav1.ObjectMeta{Project: "web", Name: "ct-app"}}
	for _, c := range []struct {
		answers, asked string
		gets           bool
	}{
		{"ghcr.io", "ghcr.io", true},
		{"ghcr.io", "GHCR.io", true},
		{"index.docker.io", "docker.io", true},
		{"index.docker.io", "registry-1.docker.io", true},
		{"ghcr.io", "evil.example", false},
		{"ghcr.io", "ghcr.io.evil.example", false},
		{"ghcr.io", "", false},
		{"", "", false},
		{"", "ghcr.io", false},
	} {
		stub := &credentialsFor{registry: c.answers}
		a := &Agent{pullCreds: stub}
		got, err := a.pullCredentials(m)(t.Context(), c.asked)
		if err != nil {
			t.Fatal(err)
		}
		if gets := got != (driver.Login{}); gets != c.gets {
			t.Errorf("a login for %q asked for %q: %+v, want one: %t", c.answers, c.asked, got, c.gets)
		}
		if len(stub.asked) > 0 && (stub.asked[0].GetProject() != "web" || stub.asked[0].GetMachine() != "ct-app") {
			t.Errorf("asked for %v", stub.asked[0])
		}
	}
}
