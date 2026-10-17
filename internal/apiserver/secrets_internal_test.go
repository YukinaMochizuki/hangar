package apiserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// secretCanary is a password no answer, event, log line or error may hold.
const secretCanary = "canary-7Hq2-pass"

func dockerConfig(host, user, pass string) []byte {
	return []byte(`{"auths":{"` + host + `":{"username":"` + user + `","password":"` + pass + `"}}}`)
}

func registrySecret(name string, cfg []byte) *secretsv1alpha1.Secret {
	return &secretsv1alpha1.Secret{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec: &secretsv1alpha1.SecretSpec{
			Type: secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH,
			Data: map[string][]byte{secrets.DockerConfigKey: cfg},
		},
	}
}

// lockedBuffer collects the apiserver's log.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func keyring(t *testing.T, current secrets.KEK, previous ...secrets.KEK) *secrets.Keyring {
	t.Helper()
	k, err := secrets.NewKeyring(current, previous...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// withSecrets configures a keyring and logs into log.
func withSecrets(k *secrets.Keyring, log *lockedBuffer) func(*Config) {
	return func(c *Config) {
		c.Secrets = k
		if log != nil {
			c.Logger = slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug}))
		}
	}
}

// noValues fails if an answered secret holds its values, their sealed
// form, or the canary anywhere.
func noValues(t *testing.T, what string, msgs ...proto.Message) {
	t.Helper()
	for _, m := range msgs {
		if sec, ok := m.(*secretsv1alpha1.Secret); ok && (sec.GetSpec().GetData() != nil || sec.GetSpec().GetSealed() != nil) {
			t.Errorf("%s: answered the data or its sealed form: %v", what, sec.GetSpec())
		}
		b, err := protojson.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(secretCanary)) {
			t.Errorf("%s: answered the value: %s", what, b)
		}
	}
}

// TestSecretValuesAreNeverAnswered: no answer, event, log line or error
// holds a secret's value, nor its sealed form; status says what can be
// said without them.
func TestSecretValuesAreNeverAnswered(t *testing.T) {
	log := &lockedBuffer{}
	s, system, user := runningWith(t, withSecrets(keyring(t, secrets.NewKEK("k1")), log))
	b := s.backend(registry.Secret)
	key := resource.Key{Project: "web", Name: "ghcr"}

	since, err := s.store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []resource.Event
	watchCtx, stopWatch := context.WithCancel(user)
	watched := make(chan error, 1)
	go func() {
		watched <- b.Watch(watchCtx, resource.WatchOptions{Project: "web", SinceRevision: since}, func(ev resource.Event) error {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
			return nil
		})
	}()

	res, err := b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary)))
	if err != nil {
		t.Fatal(err)
	}
	noValues(t, "apply", res.Object)
	if err := s.reconcileSecret(system, controller.Key{Project: "web", Name: "ghcr"}); err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(user, key)
	if err != nil {
		t.Fatal(err)
	}
	noValues(t, "get", got)
	st := got.(*secretsv1alpha1.Secret).GetStatus()
	if !slices.Equal(st.GetKeys(), []string{secrets.DockerConfigKey}) || !slices.Equal(st.GetRegistries(), []string{"ghcr.io"}) ||
		!strings.HasPrefix(st.GetContentHmac(), "hmac-sha256:") || !conditions.IsTrue(st.GetConditions(), SecretReady) {
		t.Errorf("status %v", st)
	}
	for _, project := range []string{"web", ""} {
		list, err := b.List(user, resource.ListOptions{Project: project})
		if err != nil || len(list.Items) != 1 {
			t.Fatalf("list in %q: %v %v", project, list.Items, err)
		}
		noValues(t, "list", list.Items...)
	}

	// The same values again are no change; others are.
	rv := got.(*secretsv1alpha1.Secret).GetMetadata().GetResourceVersion()
	res, err = b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary)))
	if err != nil {
		t.Fatal(err)
	}
	noValues(t, "apply again", res.Object)
	if m := secretMeta(res.Object); m.GetGeneration() != 1 || m.GetResourceVersion() != rv {
		t.Errorf("the same values again: generation %d, version %d, want 1 and %d", m.GetGeneration(), m.GetResourceVersion(), rv)
	}
	res, err = b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary+"-2")))
	if err != nil {
		t.Fatal(err)
	}
	noValues(t, "apply new values", res.Object)
	if secretMeta(res.Object).GetGeneration() != 2 {
		t.Errorf("new values: generation %d, want 2", secretMeta(res.Object).GetGeneration())
	}
	if err := s.reconcileSecret(system, controller.Key{Project: "web", Name: "ghcr"}); err != nil {
		t.Fatal(err)
	}
	again, _ := b.Get(user, key)
	if again.(*secretsv1alpha1.Secret).GetStatus().GetContentHmac() == st.GetContentHmac() {
		t.Error("the content HMAC did not change with the values")
	}

	// Errors name what is wrong, never the value.
	bad := registrySecret("bad", []byte(`{"auths":{"ghcr.io":{"password":"`+secretCanary+`"`))
	if _, err := b.Apply(user, bad); connect.CodeOf(err) != connect.CodeInvalidArgument || strings.Contains(err.Error(), secretCanary) {
		t.Errorf("a bad config: %v", err)
	}
	bad = registrySecret("bad", dockerConfig("ghcr.io", "", secretCanary))
	if _, err := b.Apply(user, bad); connect.CodeOf(err) != connect.CodeInvalidArgument || strings.Contains(err.Error(), secretCanary) {
		t.Errorf("a login without a username: %v", err)
	}

	deleted, err := b.Delete(user, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	noValues(t, "delete", deleted)
	testutil.Eventually(t, "the watch saw the delete", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(events, func(ev resource.Event) bool { return ev.Type == metav1.EventType_EVENT_TYPE_DELETED })
	})
	stopWatch()
	<-watched
	mu.Lock()
	for _, ev := range events {
		if ev.Object != nil {
			noValues(t, "watch "+ev.Type.String(), ev.Object)
		}
	}
	mu.Unlock()
	if strings.Contains(log.String(), secretCanary) {
		t.Errorf("the log holds the value:\n%s", log.String())
	}
}

func secretMeta(m proto.Message) *metav1.ObjectMeta {
	return m.(*secretsv1alpha1.Secret).GetMetadata()
}

// TestSecretDataAtRestIsCiphertext: the database's files, its history
// included, hold no value of a secret, nor its Docker config at all.
func TestSecretDataAtRestIsCiphertext(t *testing.T) {
	dir := t.TempDir()
	s, _, user := runningAt(t, filepath.Join(dir, "hangar.db"), withSecrets(keyring(t, secrets.NewKEK("k1")), nil))
	b := s.backend(registry.Secret)
	for i := range 3 {
		// Each write leaves an event too.
		if _, err := b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary+string(rune('a'+i))))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Delete(user, resource.Key{Project: "web", Name: "ghcr"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary))); err != nil {
		t.Fatal(err)
	}
	stored, err := s.store.Get(t.Context(), s.backend(registry.Secret).storeKey(resource.Key{Project: "web", Name: "ghcr"}))
	if err != nil {
		t.Fatal(err)
	}
	var sec secretsv1alpha1.Secret
	if err := proto.Unmarshal(stored.Data, &sec); err != nil {
		t.Fatal(err)
	}
	if sec.GetSpec().GetData() != nil || len(sec.GetSpec().GetSealed().GetCiphertext()) == 0 {
		t.Fatalf("stored spec: data %v, sealed %v", sec.GetSpec().GetData(), sec.GetSpec().GetSealed())
	}
	files, err := filepath.Glob(filepath.Join(dir, "hangar.db*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("database files: %v %v", files, err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{secretCanary, `"auths"`, base64.StdEncoding.EncodeToString([]byte(secretCanary))} {
			if bytes.Contains(raw, []byte(needle)) {
				t.Errorf("%s holds %q in the clear", filepath.Base(f), needle)
			}
		}
	}
}

// TestSecretAppliesAreChecked: an apply carries the data, every time, never
// the sealed form, and holds to its type.
func TestSecretAppliesAreChecked(t *testing.T) {
	s, _, user := runningWith(t, withSecrets(keyring(t, secrets.NewKEK("k1")), nil))
	b := s.backend(registry.Secret)
	if _, err := b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", "p"))); err != nil {
		t.Fatal(err)
	}
	read, err := b.Get(user, resource.Key{Project: "web", Name: "ghcr"})
	if err != nil {
		t.Fatal(err)
	}
	// What get answers, applied back, has no data.
	if _, err := b.Apply(user, read); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "write-only") {
		t.Errorf("applying what get answered: %v, want refused as write-only", err)
	}
	sealed := registrySecret("ghcr", dockerConfig("ghcr.io", "me", "p"))
	sealed.Spec.Sealed = &secretsv1alpha1.SealedData{KekId: "k1"}
	wrongType := registrySecret("other", dockerConfig("ghcr.io", "me", "p"))
	wrongType.Spec.Type = secretsv1alpha1.SecretType_SECRET_TYPE_UNSPECIFIED
	wrongKey := registrySecret("other", nil)
	wrongKey.Spec.Data = map[string][]byte{"config.json": dockerConfig("ghcr.io", "me", "p")}
	large := registrySecret("other", dockerConfig("ghcr.io", "me", strings.Repeat("p", secrets.MaxDataSize)))
	for what, sec := range map[string]*secretsv1alpha1.Secret{
		"the sealed form": sealed, "no type": wrongType, "another key": wrongKey, "too much data": large,
	} {
		if _, err := b.Apply(user, sec); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want invalid", what, err)
		}
	}
}

// TestSecretsNeedAKEK: without a key-encryption key, writing a secret fails
// and says why; with none stored, the apiserver starts all the same.
func TestSecretsNeedAKEK(t *testing.T) {
	s, _, user := runningWith(t, func(*Config) {})
	if err := s.PrepareSecrets(t.Context()); err != nil {
		t.Errorf("an empty store without a KEK: %v", err)
	}
	_, err := s.backend(registry.Secret).Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", "p")))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "key-encryption key") {
		t.Errorf("writing a secret without a KEK: %v", err)
	}
}

// TestTheKEKIsCheckedAtStart: an apiserver over stored secrets refuses to
// start without the KEK they were sealed under, or with a wrong one, and
// seals them again under a new one given the old.
func TestTheKEKIsCheckedAtStart(t *testing.T) {
	k1 := secrets.NewKEK("k1")
	s, system, user := runningWith(t, withSecrets(keyring(t, k1), nil))
	if _, err := s.backend(registry.Secret).Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary))); err != nil {
		t.Fatal(err)
	}
	restart := func(k *secrets.Keyring) *Server {
		return New(Config{Store: s.store, Secrets: k, Clock: s.clock, DisableControllers: true})
	}
	for what, k := range map[string]*secrets.Keyring{
		"no KEK":                     nil,
		"another KEK of the same id": keyring(t, secrets.NewKEK("k1")),
		"a new KEK without the old":  keyring(t, secrets.NewKEK("k2")),
		"a new KEK with a wrong old": keyring(t, secrets.NewKEK("k2"), secrets.NewKEK("k1")),
	} {
		if err := restart(k).PrepareSecrets(t.Context()); err == nil {
			t.Errorf("started with %s", what)
		}
	}
	if err := restart(keyring(t, k1)).PrepareSecrets(t.Context()); err != nil {
		t.Errorf("started again with the same KEK: %v", err)
	}
	k2 := secrets.NewKEK("k2")
	if err := restart(keyring(t, k2, k1)).PrepareSecrets(t.Context()); err != nil {
		t.Fatalf("rotating to a new KEK: %v", err)
	}
	after := restart(keyring(t, k2))
	if err := after.PrepareSecrets(t.Context()); err != nil {
		t.Fatalf("the new KEK alone, once rotated: %v", err)
	}
	obj, err := after.get(system, registry.Secret, resource.Key{Project: "web", Name: "ghcr"})
	if err != nil {
		t.Fatal(err)
	}
	sec := obj.(*secretsv1alpha1.Secret)
	if sec.GetSpec().GetSealed().GetKekId() != "k2" || sec.GetMetadata().GetGeneration() != 1 {
		t.Errorf("after rotating: KEK %s, generation %d", sec.GetSpec().GetSealed().GetKekId(), sec.GetMetadata().GetGeneration())
	}
	data, err := keyring(t, k2).Open(secretObject(sec), sec.GetSpec().GetSealed())
	if err != nil || !bytes.Contains(data[secrets.DockerConfigKey], []byte(secretCanary)) {
		t.Errorf("the rotated secret does not open to its data: %v", err)
	}
}

// TestNodesCannotReadSecrets: a node agent, which reads every other kind,
// gets nothing of a secret, not even its metadata.
func TestNodesCannotReadSecrets(t *testing.T) {
	s, _, user := runningWith(t, withSecrets(keyring(t, secrets.NewKEK("k1")), nil))
	b := s.backend(registry.Secret)
	if _, err := b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", "p"))); err != nil {
		t.Fatal(err)
	}
	node := asNode(user, "n1")
	_, err := b.Get(node, resource.Key{Project: "web", Name: "ghcr"})
	wantCodeOf(t, err, connect.CodePermissionDenied, "a node getting a secret")
	_, err = b.List(node, resource.ListOptions{Project: "web"})
	wantCodeOf(t, err, connect.CodePermissionDenied, "a node listing a project's secrets")
	if res, err := b.List(node, resource.ListOptions{}); err != nil || len(res.Items) != 0 {
		t.Errorf("a node listing every project's secrets: %d, %v", len(res.Items), err)
	}
	// A watch let through would run until its context ends.
	watchCtx, cancel := context.WithTimeout(node, 5*time.Second)
	defer cancel()
	err = b.Watch(watchCtx, resource.WatchOptions{Project: "web"}, func(resource.Event) error { return nil })
	wantCodeOf(t, err, connect.CodePermissionDenied, "a node watching a project's secrets")
	if _, err := b.Apply(node, registrySecret("mine", dockerConfig("ghcr.io", "me", "p"))); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a node writing a secret: %v", err)
	}
}

func pullMachine(name, node, image string, secretNames ...string) *computev1alpha1.Machine {
	m := machine(name, node)
	m.GetSpec().GetContainer().Image = image
	m.GetSpec().GetContainer().ImagePullSecrets = secretNames
	return m
}

// TestImagePullCredentials: a node gets the login for a machine's image
// only for a machine placed on it, only from the secrets that machine
// names, the first that holds one for the image's registry, and only for
// that registry. No one else gets one.
func TestImagePullCredentials(t *testing.T) {
	log := &lockedBuffer{}
	s, system, user := runningWith(t, func(c *Config) {
		withSecrets(keyring(t, secrets.NewKEK("k1")), log)(c)
		c.Nodes = []string{"n1", "n2"}
	})
	sb := s.backend(registry.Secret)
	for name, cfg := range map[string][]byte{
		"hub":   dockerConfig("https://index.docker.io/v1/", "hub-user", "hub-"+secretCanary),
		"ghcr":  dockerConfig("ghcr.io", "ghcr-user", "ghcr-"+secretCanary),
		"ghcr2": dockerConfig("ghcr.io", "ghcr2-user", "ghcr2-pass"),
		"quay":  dockerConfig("quay.io", "quay-user", "quay-pass"),
	} {
		if _, err := sb.Apply(user, registrySecret(name, cfg)); err != nil {
			t.Fatal(err)
		}
	}
	mb := s.backend(registry.Machine)
	for _, m := range []*computev1alpha1.Machine{
		pullMachine("on-n1", "n1", "ghcr.io/acme/app:1", "missing", "hub", "ghcr", "ghcr2"),
		pullMachine("on-n2", "n2", "ghcr.io/acme/app:1", "ghcr2"),
		pullMachine("hub-on-n1", "n1", "acme/app:1", "ghcr", "hub"),
		pullMachine("quay-on-n1", "n1", "quay.io/acme/app:1", "ghcr"),
		pullMachine("none-on-n1", "n1", "ghcr.io/acme/app:1"),
		pullMachine("unplaced", "", "ghcr.io/acme/app:1", "ghcr"),
		pullMachine("on-n3", "n3", "ghcr.io/acme/app:1", "ghcr"),
	} {
		if _, err := mb.Apply(system, m); err != nil {
			t.Fatalf("machine %s: %v", m.GetMetadata().GetName(), err)
		}
	}
	h := imagePullCredentials{s}
	ask := func(ctx context.Context, machine string) (*nodev1.ResolveImagePullCredentialsResponse, error) {
		res, err := h.ResolveImagePullCredentials(ctx, connect.NewRequest(&nodev1.ResolveImagePullCredentialsRequest{Project: "web", Machine: machine}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	n1, n2 := asNode(user, "n1"), asNode(user, "n2")
	for _, c := range []struct {
		ctx           context.Context
		machine       string
		registry, who string
	}{
		{n1, "on-n1", "ghcr.io", "ghcr-user"},
		{n2, "on-n2", "ghcr.io", "ghcr2-user"},
		{n1, "hub-on-n1", "index.docker.io", "hub-user"},
		{n1, "quay-on-n1", "", ""},
		{n1, "none-on-n1", "", ""},
	} {
		got, err := ask(c.ctx, c.machine)
		if err != nil || got.GetRegistry() != c.registry || got.GetUsername() != c.who {
			t.Errorf("%s: %v %v, want %s's login for %q", c.machine, got, err, c.who, c.registry)
		}
	}
	for _, c := range []struct {
		what    string
		ctx     context.Context
		machine string
	}{
		{"another node's machine", n2, "on-n1"},
		{"another node's machine, the other way", n1, "on-n2"},
		{"a machine on no node", n1, "unplaced"},
		{"a machine that does not exist", n1, "nowhere"},
		{"a node this apiserver does not run", asNode(user, "n3"), "on-n1"},
		{"a node this apiserver does not run, for a machine on it", asNode(user, "n3"), "on-n3"},
		{"the project's owner", user, "on-n1"},
		{"the system", system, "on-n1"},
		{"a service token", auth.WithPrincipal(user, auth.Principal{Type: auth.ServiceToken, ID: "t"}), "on-n1"},
		// Only a node principal is a node, whatever its name.
		{"a service token named as the node", auth.WithPrincipal(user, auth.Principal{Type: auth.ServiceToken, ID: "n1"}), "on-n1"},
		{"a person named as the node", auth.WithPrincipal(user, auth.Principal{Type: auth.User, ID: "n1"}), "on-n1"},
	} {
		if got, err := ask(c.ctx, c.machine); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s: %v %v, want denied", c.what, got, err)
		}
	}
	if strings.Contains(log.String(), secretCanary) {
		t.Errorf("the log holds a password:\n%s", log.String())
	}
}

// TestPullSecretsAreMirrored: a container's image pull secrets reach its
// machine, and its names are checked.
func TestPullSecretsAreMirrored(t *testing.T) {
	want := []string{"a", "b"}
	spec, err := machineSpec(&computev1alpha1.ContainerSpec{Image: "nginx", ImagePullSecrets: want})
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.GetContainer().GetImagePullSecrets(); !slices.Equal(got, want) {
		t.Errorf("mirrored %v, want %v", got, want)
	}
	for _, names := range [][]string{{"Bad"}, {"a", "a"}, slices.Repeat([]string{"x"}, MaxImagePullSecrets+1)} {
		if err := validateImagePullSecrets(names); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%v: %v, want invalid", names, err)
		}
	}
}
