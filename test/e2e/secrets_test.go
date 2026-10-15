package e2e

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
)

const (
	privateImage  = "registry.example/acme/app:1"
	privateDigest = "sha256:" + "5e" + "cafe0000000000000000000000000000000000000000000000000000000000"
	privateByDig  = "registry.example/acme/app@" + privateDigest
	// otherProject is another tenant's.
	otherProject = "rival-web"
)

var acmeLogin = driver.Login{Username: "acme-bot", Password: "acme-pass"}

// newSecretsWorld is a world whose apiserver holds a key-encryption key,
// with acme's private image on its registry and a project of another
// tenant.
func newSecretsWorld(t *testing.T) *world {
	t.Helper()
	keys, err := secrets.NewKeyring(secrets.NewKEK("test"))
	if err != nil {
		t.Fatal(err)
	}
	w := newIdleWorldWith(t, func(c *apiserver.Config) { c.Secrets = keys })
	w.host.AddImage(privateImage, privateDigest, nginx)
	w.host.MakePrivate(privateImage, acmeLogin)
	ctx := context.Background()
	if _, err := w.root.Apply(ctx, registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "rival"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.root.Apply(ctx, registry.Project, &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: otherProject}, Spec: &tenancyv1alpha1.ProjectSpec{Tenant: "rival"},
	}); err != nil {
		t.Fatal(err)
	}
	w.startAgent()
	return w
}

func (w *world) applySecret(proj, name string, l driver.Login) {
	w.t.Helper()
	cfg := fmt.Sprintf(`{"auths":{"registry.example":{"username":%q,"password":%q}}}`, l.Username, l.Password)
	if _, err := w.root.Apply(context.Background(), registry.Secret, &secretsv1alpha1.Secret{
		Metadata: &metav1.ObjectMeta{Project: proj, Name: name},
		Spec: &secretsv1alpha1.SecretSpec{
			Type: secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH,
			Data: map[string][]byte{secrets.DockerConfigKey: []byte(cfg)},
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) applyIn(proj, name, image string, pullSecrets ...string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.Container, &computev1alpha1.Container{
		Metadata: &metav1.ObjectMeta{Project: proj, Name: name},
		Spec:     &computev1alpha1.ContainerSpec{Image: image, ImagePullSecrets: pullSecrets},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// waitReasonIn is waitReason for a container in any project.
func (w *world) waitReasonIn(proj, name, reason string) {
	w.t.Helper()
	w.eventually(fmt.Sprintf("container %s/%s %s", proj, name, reason), func() (bool, string) {
		obj, err := w.root.Get(context.Background(), registry.Container, client.Key{Project: proj, Name: name})
		if connect.CodeOf(err) == connect.CodeNotFound {
			return false, "not found"
		}
		if err != nil {
			w.t.Fatal(err)
		}
		c := obj.(*computev1alpha1.Container)
		r := ready(c)
		return r.GetReason() == reason && r.GetObservedGeneration() == c.GetMetadata().GetGeneration(), fmt.Sprintf("%v", r)
	})
}

// TestAPrivateImageRunsWithItsSecret: a container pulls a private image
// with the login of the secret it names, and says so when it names none, or
// one that does not exist.
func TestAPrivateImageRunsWithItsSecret(t *testing.T) {
	w := newSecretsWorld(t)
	w.applyIn(project, "anon", privateImage)
	w.waitReasonIn(project, "anon", computeapi.ReasonImagePullUnauthorized)

	w.applyIn(project, "missing", privateImage, "nope")
	w.waitReasonIn(project, "missing", apiserver.ReasonImagePullSecretNotFound)

	w.applySecret(project, "pull", acmeLogin)
	w.applyIn(project, "app", privateImage, "pull")
	w.waitReason("app", "Running")
	if m := w.machine("app"); m.GetStatus().GetImageDigest() != privateDigest {
		t.Errorf("digest %s, want %s", m.GetStatus().GetImageDigest(), privateDigest)
	}
	// Naming another secret too, changed after it runs, restarts nothing.
	vm, _, _ := w.vmOfIn(project, "app")
	w.applyIn(project, "app", privateImage, "pull", "nope")
	w.eventually("the change observed", func() (bool, string) {
		m := w.machine("app")
		return m.GetStatus().GetObservedGeneration() == m.GetMetadata().GetGeneration(), ""
	})
	w.waitReason("app", "Running")
	if again, _, running := w.vmOfIn(project, "app"); !running || !again.Started.Equal(vm.Started) {
		t.Error("naming another pull secret restarted the VM")
	}
}

// TestAnotherTenantCannotRunAPrivateImageFromTheCache: what acme pulled with
// its login is on the host, yet another tenant's container naming it, by its
// tag or by its digest, without a login or with a wrong one, is refused;
// after the agent restarts too, while acme's keeps running.
func TestAnotherTenantCannotRunAPrivateImageFromTheCache(t *testing.T) {
	w := newSecretsWorld(t)
	w.applySecret(project, "pull", acmeLogin)
	w.applyIn(project, "app", privateImage, "pull")
	w.waitReason("app", "Running")
	if !w.host.Cached(privateDigest) {
		t.Fatal("the private image is not on the host, so the rest proves nothing")
	}

	w.applySecret(otherProject, "guess", driver.Login{Username: "acme-bot", Password: "guess"})
	refused := map[string]func(){
		"by-tag":          func() { w.applyIn(otherProject, "by-tag", privateImage) },
		"by-digest":       func() { w.applyIn(otherProject, "by-digest", privateByDig) },
		"by-tag-guessed":  func() { w.applyIn(otherProject, "by-tag-guessed", privateImage, "guess") },
		"by-digest-guess": func() { w.applyIn(otherProject, "by-digest-guess", privateByDig, "guess") },
		// acme's secret's name, which in its own project names nothing.
		"acmes-name": func() { w.applyIn(otherProject, "acmes-name", privateImage, "pull") },
	}
	for _, apply := range refused {
		apply()
	}
	check := func(when string) {
		t.Helper()
		for name := range refused {
			want := computeapi.ReasonImagePullUnauthorized
			if name == "acmes-name" {
				want = apiserver.ReasonImagePullSecretNotFound
			}
			w.waitReasonIn(otherProject, name, want)
			_, _, running := w.vmOfIn(otherProject, name)
			if running {
				t.Errorf("%s: %s runs acme's image", when, name)
			}
		}
	}
	check("before a restart")

	w.stopAgent()
	w.startAgent()
	check("after a restart")
	w.waitReason("app", "Running")
}

// vmOfIn says whether the container's machine has a VM running.
func (w *world) vmOfIn(proj, name string) (driver.VM, driver.VMSpec, bool) {
	obj, err := w.root.Get(context.Background(), registry.Machine, client.Key{Project: proj, Name: "ct-" + name})
	if err != nil {
		return driver.VM{}, driver.VMSpec{}, false
	}
	uid := obj.(*computev1alpha1.Machine).GetMetadata().GetUid()
	v, ok := w.host.Snapshot().VMs[uid]
	return v.VM, v.Spec, ok && v.State == driver.VMRunning
}
