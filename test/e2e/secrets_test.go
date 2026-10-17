package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

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
	"github.com/YukinaMochizuki/hangar/internal/testutil"
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
	w.applySecretFor(proj, name, "registry.example", l)
}

// applySecretFor applies a registry-auth secret with a login for host.
func (w *world) applySecretFor(proj, name, host string, l driver.Login) {
	w.t.Helper()
	cfg := fmt.Sprintf(`{"auths":{%q:{"username":%q,"password":%q}}}`, host, l.Username, l.Password)
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
//
// A refused pull is tried again with a backoff, and says Pulling while it
// is, so on a loaded host the reason can take longer than testutil.Wait to
// be seen.
func (w *world) waitReasonIn(proj, name, reason string) {
	w.t.Helper()
	testutil.EventuallyWithin(w.t, time.Minute, fmt.Sprintf("container %s/%s %s", proj, name, reason), func() (bool, string) {
		return w.reasonIs(proj, name, reason)
	})
}

// reasonIs says whether the container says Ready with the reason, at its
// current generation, and what it says.
func (w *world) reasonIs(proj, name, reason string) (bool, string) {
	w.t.Helper()
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
	testutil.Eventually(w.t, "the change observed", func() (bool, string) {
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
	// The rival's own registry says it has acme's digest, by digest and by a
	// tag, but cannot serve it: the review's repository-confusion attack.
	evilByDigest := w.host.ClaimDigest("evil.example/rival/app@"+privateDigest, privateDigest)
	w.host.ClaimDigest("evil.example/rival/app:1", privateDigest)
	w.applySecretFor(otherProject, "evil-login", "evil.example", driver.Login{Username: "rival", Password: "rival"})
	unauthorized, notFound := computeapi.ReasonImagePullUnauthorized, computeapi.ReasonImagePullFailed
	refused := map[string]struct {
		apply func()
		want  string
	}{
		"by-tag":          {func() { w.applyIn(otherProject, "by-tag", privateImage) }, unauthorized},
		"by-digest":       {func() { w.applyIn(otherProject, "by-digest", privateByDig) }, unauthorized},
		"by-tag-guessed":  {func() { w.applyIn(otherProject, "by-tag-guessed", privateImage, "guess") }, unauthorized},
		"by-digest-guess": {func() { w.applyIn(otherProject, "by-digest-guess", privateByDig, "guess") }, unauthorized},
		// acme's secret's name, which in its own project names nothing.
		"acmes-name":      {func() { w.applyIn(otherProject, "acmes-name", privateImage, "pull") }, apiserver.ReasonImagePullSecretNotFound},
		"evil-digest":     {func() { w.applyIn(otherProject, "evil-digest", evilByDigest) }, notFound},
		"evil-tag":        {func() { w.applyIn(otherProject, "evil-tag", "evil.example/rival/app:1") }, notFound},
		"evil-digest-own": {func() { w.applyIn(otherProject, "evil-digest-own", evilByDigest, "evil-login") }, notFound},
		"evil-tag-own":    {func() { w.applyIn(otherProject, "evil-tag-own", "evil.example/rival/app:1", "evil-login") }, notFound},
	}
	for _, c := range refused {
		c.apply()
	}
	check := func(when string) {
		t.Helper()
		for name, c := range refused {
			w.waitReasonIn(otherProject, name, c.want)
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

// TestAnUpgradedNodeFetchesEachImageAgain: a node upgraded from a release
// whose cache recorded no sources trusts none of what it holds. A machine
// running keeps running, its registry down or not; starting it again needs
// its image fetched once from its own registry, which a registry that is
// down blocks until it is back (docs/runbook/host-bootstrap.md).
func TestAnUpgradedNodeFetchesEachImageAgain(t *testing.T) {
	w := newWorld(t)
	c := container("api", "nginx")
	w.apply(c)
	w.waitReason("api", "Running")
	before, _, _ := w.vmOfIn(project, "api")

	// The upgrade: the agent restarts on a cache with no sources, and the
	// registry is down meanwhile.
	w.stopAgent()
	w.host.ForgetSources()
	w.host.FailPull("nginx", fmt.Errorf("%w: the registry is down", driver.ErrRegistryUnreachable))
	w.startAgent()
	// The new agent checks the machine, and its image, within this; its
	// pull fails meanwhile.
	testutil.Unchanged(w.t, "api's VM, its registry down", time.Second, w.vmVersion(before.MachineUID))
	w.waitReason("api", "Running")
	if vm, _, running := w.vmOfIn(project, "api"); !running || !vm.Started.Equal(before.Started) {
		t.Fatal("a running machine was stopped or restarted for its image's registry being down")
	}

	// Starting it again needs the image fetched from its registry.
	c.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	w.apply(c)
	w.waitStopped("api", metav1.ConditionStatus_CONDITION_STATUS_TRUE)
	c.Spec.RunState = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_RUNNING
	w.apply(c)
	w.waitReasonIn(project, "api", computeapi.ReasonImagePullFailed)
	if _, _, running := w.vmOfIn(project, "api"); running {
		t.Error("the machine started from a cache with no sources, its registry down")
	}

	// The registry back, the one fetch lets it start.
	w.host.FailPull("nginx", nil)
	w.waitReasonIn(project, "api", "Running")
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
