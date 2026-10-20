package apiserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"

	discoveryv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/discovery/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// TestDiscoveryServesEveryKind checks the served kinds and schemas against
// the registry and the descriptors, so the generated schemas cannot drift
// from the messages the API takes.
func TestDiscoveryServesEveryKind(t *testing.T) {
	s, _, _ := running(t)
	d := newDiscovery(s)
	// Anyone signed in may ask, though they were never invited.
	stranger := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "stranger@example.com"})

	list, err := d.ListAPIResources(stranger, connect.NewRequest(&discoveryv1alpha1.ListAPIResourcesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(list.Msg.GetResources()); got != len(registry.All) {
		t.Fatalf("%d kinds served, %d in the registry", got, len(registry.All))
	}
	for i, r := range registry.All {
		got := list.Msg.GetResources()[i]
		if got.GetKind() != r.Kind || got.GetApiVersion() != r.APIVersion() || got.GetPlural() != r.Plural || got.GetScope() != r.Scope {
			t.Errorf("served %v for %s", got, r.Kind)
		}
		if got.GetDescription() == "" || strings.Contains(got.GetDescription(), "\n") {
			t.Errorf("%s: description %q, want one sentence", r.Kind, got.GetDescription())
		}

		res, err := d.GetSchema(stranger, connect.NewRequest(&discoveryv1alpha1.GetSchemaRequest{ApiVersion: r.APIVersion(), Kind: r.Kind}))
		if err != nil {
			t.Fatalf("%s: %v", r.Kind, err)
		}
		var doc struct {
			Properties map[string]any            `json:"properties"`
			Defs       map[string]map[string]any `json:"$defs"`
		}
		js := res.Msg.GetJsonSchema()
		if err := json.Unmarshal([]byte(js), &doc); err != nil {
			t.Fatalf("%s: %v", r.Kind, err)
		}
		constOf := func(field string) any { p, _ := doc.Properties[field].(map[string]any); return p["const"] }
		if constOf("kind") != r.Kind || constOf("apiVersion") != r.APIVersion() {
			t.Errorf("%s: envelope %v", r.Kind, doc.Properties)
		}
		checkProperties(t, r.Kind, r.Message.Descriptor(), doc.Properties)
		for name, def := range doc.Defs {
			if md := messageNamed(r.Message.Descriptor(), protoreflect.FullName(name)); md != nil {
				props, _ := def["properties"].(map[string]any)
				checkProperties(t, name, md, props)
			}
		}
		// What the repo's own layout says means nothing to a reader of the
		// API; only docs are linked.
		if m := regexp.MustCompile(`\b(internal|cmd)/[a-z]`).FindString(js); m != "" {
			t.Errorf("%s's schema names a path of the repo, %q", r.Kind, m)
		}
		for _, link := range regexp.MustCompile(`https://github.com/YukinaMochizuki/hangar/blob/main/(docs/[^\s"]*?\.md)`).FindAllStringSubmatch(js, -1) {
			if _, err := os.Stat(filepath.Join("..", "..", link[1])); err != nil {
				t.Errorf("%s's schema links %s, which is not in the repo", r.Kind, link[1])
			}
		}
		if strings.Count(js, "docs/") != strings.Count(js, "/blob/main/docs/") {
			t.Errorf("%s's schema names a doc it does not link", r.Kind)
		}
	}

	_, err = d.GetSchema(stranger, connect.NewRequest(&discoveryv1alpha1.GetSchemaRequest{ApiVersion: "compute.hangarlab.cloud/v1", Kind: "Container"}))
	wantCodeOf(t, err, connect.CodeNotFound, "a kind under another apiVersion")
	if err != nil && !strings.Contains(err.Error(), "compute.hangarlab.cloud/v1alpha1 Container") {
		t.Errorf("the refusal does not list the kinds: %v", err)
	}
	_, err = d.ListAPIResources(context.Background(), connect.NewRequest(&discoveryv1alpha1.ListAPIResourcesRequest{}))
	wantCodeOf(t, err, connect.CodeUnauthenticated, "no caller")
}

// checkProperties fails unless props has exactly md's fields, by JSON name.
func checkProperties(t *testing.T, where string, md protoreflect.MessageDescriptor, props map[string]any) {
	t.Helper()
	if len(props) != md.Fields().Len() {
		t.Errorf("%s: %d properties for %d fields", where, len(props), md.Fields().Len())
	}
	for i := range md.Fields().Len() {
		if _, ok := props[md.Fields().Get(i).JSONName()]; !ok {
			t.Errorf("%s: no property %s", where, md.Fields().Get(i).JSONName())
		}
	}
}

// messageNamed finds the message called name among those md reaches.
func messageNamed(md protoreflect.MessageDescriptor, name protoreflect.FullName) protoreflect.MessageDescriptor {
	seen := map[protoreflect.FullName]bool{}
	var find func(protoreflect.MessageDescriptor) protoreflect.MessageDescriptor
	find = func(m protoreflect.MessageDescriptor) protoreflect.MessageDescriptor {
		if m.FullName() == name {
			return m
		}
		if seen[m.FullName()] {
			return nil
		}
		seen[m.FullName()] = true
		for i := range m.Fields().Len() {
			fd := m.Fields().Get(i)
			if fd.IsMap() {
				fd = fd.MapValue()
			}
			if fd.Message() != nil {
				if found := find(fd.Message()); found != nil {
					return found
				}
			}
		}
		return nil
	}
	return find(md)
}

// TestServedSchemaShapes pins what the schemas say of the forms protojson
// writes, by fields that have them.
func TestServedSchemaShapes(t *testing.T) {
	s, _, _ := running(t)
	d := newDiscovery(s)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "alice@example.com"})
	defs := func(r registry.Resource) map[string]any {
		t.Helper()
		res, err := d.GetSchema(ctx, connect.NewRequest(&discoveryv1alpha1.GetSchemaRequest{ApiVersion: r.APIVersion(), Kind: r.Kind}))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Defs map[string]any `json:"$defs"`
		}
		if err := json.Unmarshal([]byte(res.Msg.GetJsonSchema()), &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Defs
	}
	field := func(defs map[string]any, msg, name string) map[string]any {
		t.Helper()
		m, _ := defs[msg].(map[string]any)
		props, _ := m["properties"].(map[string]any)
		f, ok := props[name].(map[string]any)
		if !ok {
			t.Fatalf("%s has no property %s", msg, name)
		}
		return f
	}
	jsonOf := func(v any) string { js, _ := json.Marshal(v); return string(js) }

	ct := defs(registry.Container)
	if d, _ := field(ct, "hangar.compute.v1alpha1.ContainerSpec", "image")["description"].(string); !strings.HasPrefix(d, "OCI image reference.") {
		t.Errorf("spec.image's description is %q, not its comment", d)
	}
	if got := jsonOf(field(ct, "hangar.meta.v1.ObjectMeta", "resourceVersion")); got != `{"description":"Store revision of the last write to this object.","format":"int64","pattern":"^-?[0-9]+$","type":["integer","string"]}` {
		t.Errorf("a 64-bit integer: %s", got)
	}
	if got := jsonOf(field(ct, "hangar.meta.v1.ObjectMeta", "labels")["additionalProperties"]); got != `{"type":"string"}` {
		t.Errorf("a map's values: %s", got)
	}
	if got := jsonOf(field(defs(registry.Secret), "hangar.secrets.v1alpha1.SecretSpec", "data")["additionalProperties"]); got != `{"contentEncoding":"base64","type":"string"}` {
		t.Errorf("a map of bytes: %s", got)
	}
	rb := defs(registry.RoleBinding)
	if got := jsonOf(field(rb, "hangar.iam.v1alpha1.BindingScope", "platform")); got != `{"description":"Set at most one of platform, tenant, project.","maxProperties":0,"type":"object"}` {
		t.Errorf("an Empty: %s", got)
	}
	for name := range rb {
		if strings.HasPrefix(name, "google.protobuf.") {
			t.Errorf("protobuf's own %s is a definition, with protobuf's comments", name)
		}
	}

	_, err := d.GetSchema(context.Background(), connect.NewRequest(&discoveryv1alpha1.GetSchemaRequest{ApiVersion: registry.Container.APIVersion(), Kind: "Container"}))
	wantCodeOf(t, err, connect.CodeUnauthenticated, "a schema for no caller")
}

func TestLinkDocs(t *testing.T) {
	const base = "https://github.com/YukinaMochizuki/hangar/blob/0123abc/"
	for in, want := range map[string]string{
		`"See docs/decisions/vm-boot.md. And (docs/runbook/testbed.md)."`: `"See ` + base + `docs/decisions/vm-boot.md. And (` + base + `docs/runbook/testbed.md)."`,
		`docs/a.md at the start`:             base + `docs/a.md at the start`,
		"a link made: " + base + "docs/a.md": "a link made: " + base + "docs/a.md",
		"another path: sdk/go/docs/usage.md": "another path: sdk/go/docs/usage.md",
		"a relative one: ../docs/a.md":       "a relative one: ../docs/a.md",
		"not a doc: docs/decisions/":         "not a doc: docs/decisions/",
		"two: docs/a.md docs/b.md":           "two: " + base + "docs/a.md " + base + "docs/b.md",
	} {
		got := linkDocs(in, "0123abc")
		if got != want {
			t.Errorf("linkDocs(%q) = %q\nwant %q", in, got, want)
		}
		if again := linkDocs(got, "0123abc"); again != got {
			t.Errorf("linkDocs again on %q = %q", got, again)
		}
	}
}

func TestDocsRef(t *testing.T) {
	sha1, sha256 := strings.Repeat("a1", 20), strings.Repeat("b2", 32)
	for version, want := range map[string]string{
		sha1:            sha1,
		sha256:          sha256,
		sha1 + "-dirty": "main",
		"(devel)":       "main",
		"unknown":       "main",
		"v0.3.0":        "main",
	} {
		if got := docsRef(version); got != want {
			t.Errorf("docsRef(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestFirstSentence(t *testing.T) {
	for in, want := range map[string]string{
		"Disk is a block device. More follows.":           "Disk is a block device.",
		"Disk is a block device, e.g. a root disk. More.": "Disk is a block device, e.g. a root disk.",
		"One line only": "One line only",
		"First paragraph ends.\n\nSecond. Paragraph.": "First paragraph ends.",
		"A version 1.10 stays whole. Then.":           "A version 1.10 stays whole.",
	} {
		if got := firstSentence(in); got != want {
			t.Errorf("firstSentence(%q) = %q, want %q", in, got, want)
		}
	}
}
