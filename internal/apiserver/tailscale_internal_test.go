package apiserver

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
)

// tailscaleCanary is an auth key no answer, log line or error may hold.
const tailscaleCanary = "tskey-auth-kCANARY7Hq2-SECRETpart"

func tailscaleSecret(name, key string) *secretsv1alpha1.Secret {
	return &secretsv1alpha1.Secret{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec: &secretsv1alpha1.SecretSpec{
			Type: secretsv1alpha1.SecretType_SECRET_TYPE_TAILSCALE_AUTH,
			Data: map[string][]byte{secrets.TailscaleAuthKey: []byte(key)},
		},
	}
}

// TestTailscaleIsValidated: a container's spec.tailscale names a Secret,
// and a hostname, if any, that is a DNS label; a machine's always has its
// hostname, which its controller sets.
func TestTailscaleIsValidated(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	for _, c := range []struct {
		what string
		ts   *computev1alpha1.ContainerTailscale
		ok   bool
	}{
		{"none", nil, true},
		{"a secret alone", &computev1alpha1.ContainerTailscale{AuthSecret: "ts"}, true},
		{"a hostname and routes", &computev1alpha1.ContainerTailscale{AuthSecret: "ts", Hostname: "box-1", AcceptRoutes: true}, true},
		{"no secret", &computev1alpha1.ContainerTailscale{Hostname: "box"}, false},
		{"a secret that is no name", &computev1alpha1.ContainerTailscale{AuthSecret: "Ts_key"}, false},
		{"a hostname with a dot", &computev1alpha1.ContainerTailscale{AuthSecret: "ts", Hostname: "box.example"}, false},
		{"a hostname with an underscore", &computev1alpha1.ContainerTailscale{AuthSecret: "ts", Hostname: "my_box"}, false},
		{"a hostname ending in a dash", &computev1alpha1.ContainerTailscale{AuthSecret: "ts", Hostname: "box-"}, false},
		{"a hostname too long", &computev1alpha1.ContainerTailscale{AuthSecret: "ts", Hostname: strings.Repeat("a", 64)}, false},
	} {
		c0 := ctr("ts-" + strings.ReplaceAll(c.what, " ", "-"))
		c0.Metadata.Name = "ts-check"
		c0.Spec.Tailscale = c.ts
		_, err := s.backend(registry.Container).Apply(user, c0)
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.what, err)
		}
		if !c.ok && connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want invalid", c.what, err)
		}
	}
	m := machine("ts-machine", "")
	m.GetSpec().GetContainer().Tailscale = &computev1alpha1.ContainerTailscale{AuthSecret: "ts"}
	if _, err := s.backend(registry.Machine).Apply(system, m); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a machine whose tailnet has no hostname: %v, want invalid", err)
	}
}

// TestTailscaleIsMirrored: a container's machine joins its tailnet as the
// container's name unless it names a hostname, and a change of it is a
// change of the machine's spec, which its node restarts it for.
func TestTailscaleIsMirrored(t *testing.T) {
	t.Parallel()
	spec, err := machineSpec("box", &computev1alpha1.ContainerSpec{Image: "nginx", Tailscale: &computev1alpha1.ContainerTailscale{AuthSecret: "ts", AcceptRoutes: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.GetContainer().GetTailscale(); got.GetHostname() != "box" || got.GetAuthSecret() != "ts" || !got.GetAcceptRoutes() {
		t.Errorf("mirrored %v, want the container's name as its hostname", got)
	}
	spec, _ = machineSpec("box", &computev1alpha1.ContainerSpec{Image: "nginx", Tailscale: &computev1alpha1.ContainerTailscale{AuthSecret: "ts", Hostname: "dev"}}, nil)
	if got := spec.GetContainer().GetTailscale().GetHostname(); got != "dev" {
		t.Errorf("a hostname given: mirrored %q", got)
	}
	if spec, _ := machineSpec("box", &computev1alpha1.ContainerSpec{Image: "nginx"}, nil); spec.GetContainer().GetTailscale() != nil {
		t.Errorf("no tailnet mirrored as %v", spec.GetContainer().GetTailscale())
	}
}

// TestTailscaleSecretsAreChecked: a tailscale-auth secret holds one key,
// authkey, and its errors never quote it.
func TestTailscaleSecretsAreChecked(t *testing.T) {
	t.Parallel()
	s, _, user := runningWith(t, withSecrets(keyring(t, secrets.NewKEK("k1")), nil))
	b := s.backend(registry.Secret)
	if _, err := b.Apply(user, tailscaleSecret("ok", tailscaleCanary+"\n")); err != nil {
		t.Errorf("a key with a trailing newline: %v", err)
	}
	for what, sec := range map[string]*secretsv1alpha1.Secret{
		"empty":         tailscaleSecret("empty", ""),
		"a space":       tailscaleSecret("space", tailscaleCanary+" "+tailscaleCanary),
		"another key":   {Metadata: &metav1.ObjectMeta{Project: "web", Name: "other"}, Spec: &secretsv1alpha1.SecretSpec{Type: secretsv1alpha1.SecretType_SECRET_TYPE_TAILSCALE_AUTH, Data: map[string][]byte{"key": []byte(tailscaleCanary)}}},
		"a control key": tailscaleSecret("ctl", tailscaleCanary+"\x00"+tailscaleCanary),
	} {
		_, err := b.Apply(user, sec)
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want invalid", what, err)
		}
		if err != nil && strings.Contains(err.Error(), "CANARY") {
			t.Errorf("%s: the error quotes the key: %v", what, err)
		}
	}
}

// TestResolveTailscaleAuthKey: a node gets the auth key of a machine placed
// on it that joins a tailnet, from the Secret it names; a Secret that gives
// none is NOT_FOUND, naming it; no one else gets a key, and no log line
// holds one.
func TestResolveTailscaleAuthKey(t *testing.T) {
	t.Parallel()
	log := &lockedBuffer{}
	s, system, user := runningWith(t, func(c *Config) {
		withSecrets(keyring(t, secrets.NewKEK("k1")), log)(c)
		c.Nodes = []string{"n1", "n2"}
	})
	sb := s.backend(registry.Secret)
	if _, err := sb.Apply(user, tailscaleSecret("ts", tailscaleCanary)); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Apply(user, registrySecret("reg", dockerConfig("ghcr.io", "me", secretCanary))); err != nil {
		t.Fatal(err)
	}
	joining := func(name, node, secret string) *computev1alpha1.Machine {
		m := machine(name, node)
		if secret != "" {
			m.GetSpec().GetContainer().Tailscale = &computev1alpha1.ContainerTailscale{AuthSecret: secret, Hostname: name}
		}
		return m
	}
	mb := s.backend(registry.Machine)
	for _, m := range []*computev1alpha1.Machine{
		joining("on-n1", "n1", "ts"),
		joining("missing-on-n1", "n1", "nothing"),
		joining("registry-on-n1", "n1", "reg"),
		joining("plain-on-n1", "n1", ""),
		joining("on-n2", "n2", "ts"),
		joining("unplaced", "", "ts"),
		joining("on-n3", "n3", "ts"),
	} {
		if _, err := mb.Apply(system, m); err != nil {
			t.Fatalf("machine %s: %v", m.GetMetadata().GetName(), err)
		}
	}
	h := tailscaleAuthKeys{s}
	ask := func(ctx context.Context, machine string) (string, error) {
		res, err := h.ResolveTailscaleAuthKey(ctx, connect.NewRequest(&nodev1.ResolveTailscaleAuthKeyRequest{Project: "web", Machine: machine}))
		if err != nil {
			return "", err
		}
		return res.Msg.GetAuthKey(), nil
	}
	n1, n2 := asNode(user, "n1"), asNode(user, "n2")
	if got, err := ask(n1, "on-n1"); err != nil || got != tailscaleCanary {
		t.Errorf("its node: %q, %v; want the key", got, err)
	}
	if got, err := ask(n2, "on-n2"); err != nil || got != tailscaleCanary {
		t.Errorf("the other node, for its own: %q, %v; want the key", got, err)
	}
	for _, c := range []struct {
		machine, says string
	}{
		{"missing-on-n1", `"nothing" in project "web" does not exist`},
		{"registry-on-n1", `"reg" in project "web" is not of type tailscale-auth`},
	} {
		got, err := ask(n1, c.machine)
		if connect.CodeOf(err) != connect.CodeNotFound || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %q, %v; want NOT_FOUND saying %s", c.machine, got, err, c.says)
		}
	}
	for _, c := range []struct {
		what    string
		ctx     context.Context
		machine string
	}{
		{"another node's machine", n2, "on-n1"},
		{"a machine that joins no tailnet", n1, "plain-on-n1"},
		{"a machine on no node", n1, "unplaced"},
		{"a machine that does not exist", n1, "nowhere"},
		{"a node this apiserver does not run", asNode(user, "n3"), "on-n3"},
		{"the project's owner", user, "on-n1"},
		{"the system", system, "on-n1"},
		{"a service token named as the node", auth.WithPrincipal(user, auth.Principal{Type: auth.ServiceToken, ID: "n1"}), "on-n1"},
	} {
		if got, err := ask(c.ctx, c.machine); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s: %q %v, want denied", c.what, got, err)
		}
	}
	if !strings.Contains(log.String(), "a tailscale auth key given") {
		t.Errorf("the log does not say a key was given:\n%s", log.String())
	}
	if strings.Contains(log.String(), "CANARY") || strings.Contains(log.String(), secretCanary) {
		t.Errorf("the log holds a key:\n%s", log.String())
	}
}

// TestTailnetJoinedIsTheMachines: a container that joins a tailnet says
// TailnetJoined as its machine's node does, Starting while that says
// nothing; one that joins none has no such condition.
func TestTailnetJoinedIsTheMachines(t *testing.T) {
	t.Parallel()
	s, _, _ := running(t)
	c := ctr("box")
	c.Metadata.Uid = "c-uid"
	c.Spec.Tailscale = &computev1alpha1.ContainerTailscale{AuthSecret: "ts"}
	m := machine("box-machine", "n1")
	m.Metadata.OwnerReferences = []*metav1.OwnerReference{{Kind: "Container", Name: "box", Uid: "c-uid", Controller: true}}
	m.GetSpec().GetContainer().Tailscale = &computev1alpha1.ContainerTailscale{AuthSecret: "ts", Hostname: "box"}

	got := conditions.Find(s.withTailnetJoined(nil, c, m), computeapi.TailnetJoined)
	if got.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || got.GetReason() != computeapi.ReasonTailnetStarting {
		t.Errorf("before the node says: %v, want False Starting", got)
	}
	m.Status = &computev1alpha1.MachineStatus{Conditions: []*metav1.Condition{{
		Type: computeapi.TailnetJoined, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: computeapi.ReasonTailnetKeyRejected, Message: "refused",
	}}}
	got = conditions.Find(s.withTailnetJoined(nil, c, m), computeapi.TailnetJoined)
	if got.GetReason() != computeapi.ReasonTailnetKeyRejected || got.GetMessage() != "refused" {
		t.Errorf("as the node says: %v, want KeyRejected", got)
	}
	c.Spec.Tailscale = nil
	if got := s.withTailnetJoined([]*metav1.Condition{got}, c, m); len(got) != 0 {
		t.Errorf("a container that joins no tailnet: %v", got)
	}
}
