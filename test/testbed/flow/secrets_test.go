//go:build testbed

package flow

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"google.golang.org/protobuf/encoding/protojson"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
)

// rivalProject is another tenant's, which the private image is not.
const rivalProject = "flow-rival"

// TestAPrivateRegistry: a container pulls its image from a private registry
// with the login of the secret it names, with the real Images driver, and
// runs it; a container of another tenant naming the same image by its
// digest, which is now on the host, is refused, before and after the agent
// restarts, and so is one naming it in a repository of its own that claims
// the digest but cannot serve its layers. The registry is
// go-containerregistry's, served on the host's loopback and asking for a
// login on its "acme" repositories.
func TestAPrivateRegistry(t *testing.T) {
	if installed {
		t.Skip("the installed services run with their own key-encryption key; this test makes its own")
	}
	c := startClusterWith(t, func(c *cluster) {
		kek := filepath.Join(apiDir, "secrets-kek")
		if err := os.WriteFile(kek, []byte(secrets.NewKEK("flow").String()), 0o600); err != nil {
			t.Fatal(err)
		}
		nobody := lookupUID(t, "nobody")
		if err := os.Chown(kek, nobody, nobody); err != nil {
			t.Fatal(err)
		}
		c.apiArgs = append(c.apiArgs, "--secrets-kek", kek)
	})

	// busybox, from Docker Hub by its digest, published privately.
	reg := registrytest.New(t)
	img, err := remote.Image(name.MustParseReference(busyboxRef), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		t.Fatalf("fetching busybox: %v", err)
	}
	ref, digest := reg.Push(t, "acme/busybox:1", img)
	reg.Private("acme")
	byDigest := strings.TrimSuffix(ref, ":1") + "@" + digest

	cfg := filepath.Join(t.TempDir(), "config.json")
	auth := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	if err := os.WriteFile(cfg, []byte(`{"auths":{"`+reg.Host+`":{"auth":"`+auth+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := c.cli("create", "secret", "registry", "acme-pull", "--from-docker-config", cfg)
	t.Logf("hangar create secret: %s", strings.TrimSpace(out))
	if listed := c.cli("get", "secrets", "-o", "yaml"); strings.Contains(listed, "user:pass") || strings.Contains(listed, auth) {
		t.Fatalf("get secrets shows the login:\n%s", listed)
	}

	c.applyYAML(privateContainerYAML("private", project, ref, "acme-pull"))
	c.waitReady("private", 5*time.Minute)
	m := c.machine("private")
	if m.GetStatus().GetImageDigest() != digest || m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
		t.Errorf("the machine is %s with %s, want running %s", m.GetStatus().GetPhase(), m.GetStatus().GetImageDigest(), digest)
	}
	t.Logf("private: machine %s %s, image %s; VM units %v", m.GetMetadata().GetUid(), m.GetStatus().GetPhase(), m.GetStatus().GetImageDigest(), hangarUnits(t))

	c.applyYAML(fmt.Sprintf(`apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Tenant
metadata: {name: %[1]s}
---
apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: %[1]s}
spec: {tenant: %[1]s}
`, rivalProject))
	c.applyYAML(privateContainerYAML("rival", rivalProject, byDigest, ""))
	// The rival's own repository says it has the digest, and serves its
	// manifest and config, but none of its layers: the repository-confusion
	// attack.
	confused := reg.Claim(t, "rival", digest)
	c.applyYAML(privateContainerYAML("confused", rivalProject, confused, ""))
	refused := func(when string) {
		t.Helper()
		for name, reason := range map[string]string{"rival": "ImagePullUnauthorized", "confused": "ImagePullFailed"} {
			eventually(t, 2*time.Minute, "container "+name+" refused "+when, func() (bool, string) {
				out, err := c.tryCLI(nil, "get", "container", name, "-p", rivalProject, "-o", "json")
				if err != nil {
					return false, err.Error()
				}
				r := conditionIn(out, "Ready")
				if r != nil && r.GetReason() == reason {
					t.Logf("%s %s: %s: %s", name, when, r.GetReason(), r.GetMessage())
					return true, ""
				}
				return false, out
			})
		}
		if units := hangarUnits(t); len(units) != 1 {
			t.Errorf("%s: VM units %v, want the private container's alone", when, units)
		}
	}
	refused("before a restart")
	c.killNode()
	c.startNode()
	c.cli("wait", "node", nodeName, "--for=condition=Ready", "--timeout=60s")
	refused("after a restart")
	c.waitReady("private", time.Minute)

	for _, del := range [][]string{{"container", "private"}, {"container", "rival", "-p", rivalProject}, {"container", "confused", "-p", rivalProject}} {
		c.cli(append([]string{"delete"}, del...)...)
		c.cli(append([]string{"wait"}, append(del, "--for=delete", "--timeout=2m")...)...)
	}
}

func privateContainerYAML(name, proj, image, pullSecret string) string {
	pull := ""
	if pullSecret != "" {
		pull = "  imagePullSecrets: [" + pullSecret + "]\n"
	}
	return fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata: {name: %s, project: %s}
spec:
  image: %s
%s  command: ["/bin/sh", "-c", %q]
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi}
`, name, proj, image, pull, idleScript)
}

// conditionIn reads a condition from a container's JSON.
func conditionIn(js, typ string) *metav1.Condition {
	var ct computev1alpha1.Container
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(js), &ct); err != nil {
		return nil
	}
	return conditions.Find(ct.GetStatus().GetConditions(), typ)
}
