package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/cli"
)

func TestAPIResources(t *testing.T) {
	w := newWorld(t)
	// Someone who signed in and was never invited still reads the schema.
	w.as("stranger@example.com")
	out := w.must("", "api-resources")
	for _, want := range []string{
		"NAME", "SHORTNAMES", "APIVERSION",
		"containers        ct           compute.hangarlab.cloud/v1alpha1   Project   Container",
		"tenants                        tenancy.hangarlab.cloud/v1alpha1   Global    Tenant",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("api-resources lacks %q:\n%s", want, out)
		}
	}
	var doc struct {
		Items []struct {
			APIVersion  string   `json:"apiVersion"`
			Kind        string   `json:"kind"`
			ShortNames  []string `json:"shortNames"`
			Scope       string   `json:"scope"`
			Description string   `json:"description"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(w.must("", "api-resources", "-o", "json")), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Items) == 0 || doc.Items[0].Kind != "Container" || doc.Items[0].Scope != "SCOPE_PROJECT" ||
		!strings.HasPrefix(doc.Items[0].Description, "Container runs an OCI image") {
		t.Errorf("api-resources -o json: %+v", doc.Items)
	}
	if out := w.must("", "api-resources", "-o", "name"); !strings.HasPrefix(out, "containers\nmachines\n") {
		t.Errorf("api-resources -o name:\n%s", out)
	}
}

func TestExplain(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")

	out := w.must("", "explain", "ct")
	for _, want := range []string{
		"KIND:     Container\nVERSION:  compute.hangarlab.cloud/v1alpha1\n",
		"DESCRIPTION:\n    Container runs an OCI image",
		// Docs the comments name are links a reader outside the repo can
		// follow.
		"https://github.com/YukinaMochizuki/hangar/blob/main/docs/decisions/machine-model.md",
		"  apiVersion\t<string> -required- = compute.hangarlab.cloud/v1alpha1\n",
		"  spec\t<ContainerSpec>\n    ContainerSpec is the desired state of a container.\n",
		"  status\t<ContainerStatus> -read-only-\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("explain ct lacks %q:\n%s", want, out)
		}
	}

	out = w.must("", "explain", "containers.spec.runState")
	for _, want := range []string{"FIELD:    spec.runState <enum>", "VALUES:\n    CONTAINER_RUN_STATE_RUNNING\n    CONTAINER_RUN_STATE_STOPPED\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain of an enum lacks %q:\n%s", want, out)
		}
	}
	// A field's comment is what describes it.
	if out := w.must("", "explain", "container.spec"); !strings.Contains(out, "  image\t<string>\n    OCI image reference.") {
		t.Errorf("explain container.spec lacks image's description:\n%s", out)
	}
	// A list is explained by its items, and the field is named as the
	// schema names it, whatever the case it was asked by.
	out = w.must("", "explain", "container.SPEC.Ports.port")
	if !strings.Contains(out, "FIELD:    spec.ports.port <integer>") {
		t.Errorf("explain through a list:\n%s", out)
	}
	if out := w.must("", "explain", "secret.spec.data"); !strings.Contains(out, "FIELD:    spec.data <map[string]base64>") {
		t.Errorf("explain of bytes:\n%s", out)
	}
	if out := w.must("", "explain", "container.spec.image_pull_secrets"); !strings.Contains(out, "<[]string>") {
		t.Errorf("explain by proto name:\n%s", out)
	}

	out = w.must("", "explain", "container.spec", "--recursive")
	for _, want := range []string{"  ports\t<[]ContainerPort>\n    name\t<string>\n", "  resources\t<Resources>\n    cpu\t<string>\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain --recursive lacks %q:\n%s", want, out)
		}
	}

	var schema map[string]any
	if err := json.Unmarshal([]byte(w.must("", "explain", "container.spec.resources", "-o", "json")), &schema); err != nil {
		t.Fatal(err)
	}
	defs, _ := schema["$defs"].(map[string]any)
	if schema["$ref"] != "#/$defs/hangar.compute.v1alpha1.Resources" || len(defs) != 1 || defs["hangar.compute.v1alpha1.Resources"] == nil {
		t.Errorf("explain -o json of a field, which refers to Resources alone: %v", schema)
	}
	if out := w.must("", "explain", "container.spec.ports", "-o", "json"); !strings.Contains(out, `"<container>-<name>`) {
		t.Errorf("explain -o json escapes what it prints:\n%s", out)
	}

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"explain", "containr"}, "did you mean containers?"},
		{[]string{"explain", "container.spec.resources.cpus"}, `spec.resources has no field "cpus"; did you mean cpu?; it has cpu, disk, memory`},
		{[]string{"explain", "container.spec.image.x"}, "spec.image has no fields"},
	} {
		r := w.run("", tt.args...)
		if r.code != cli.ExitUsage || !strings.Contains(r.err, tt.want) {
			t.Errorf("hangar %s: exit %d, %q; want usage and %q", strings.Join(tt.args, " "), r.code, r.err, tt.want)
		}
	}
}
