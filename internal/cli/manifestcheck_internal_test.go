package cli

import (
	"strings"
	"testing"
)

const containerHead = "apiVersion: compute.hangarlab.cloud/v1alpha1\nkind: Container\nmetadata: {name: web}\n"

// TestManifestErrorsSayWhatIsRight checks that a mistake in a manifest is
// reported at its line and field, with what the field would take.
func TestManifestErrorsSayWhatIsRight(t *testing.T) {
	for _, tt := range []struct {
		name string
		doc  string
		want []string
	}{
		{
			"unknown field",
			containerHead + "spec:\n  image: x\n  resources:\n    cpus: 2\n",
			[]string{"document 1, line 7: spec.resources.cpus: unknown field", "did you mean cpu?", "spec.resources has cpu, memory, disk", "\nrun hangar explain container.spec.resources for what it takes"},
		},
		{
			"unknown top-level field",
			containerHead + "specs: {image: x}\n",
			[]string{"line 4: specs: unknown field; did you mean spec?", "the object has ", "run hangar explain container for"},
		},
		{
			"enum value by its last word",
			containerHead + "spec: {image: x, runState: running}\n",
			[]string{"run hangar explain container.spec.runState for", "line 4: spec.runState: \"running\" is not a ContainerRunState", "want one of CONTAINER_RUN_STATE_RUNNING, CONTAINER_RUN_STATE_STOPPED", "did you mean CONTAINER_RUN_STATE_RUNNING?"},
		},
		{
			"field in a list item",
			containerHead + "spec:\n  image: x\n  ports:\n  - name: http\n    prot: 80\n",
			[]string{"line 8: spec.ports[0].prot: unknown field; did you mean port?", "run hangar explain container.spec.ports for"},
		},
		{
			"word for a number",
			containerHead + "spec:\n  image: x\n  ports: [{name: http, port: eighty}]\n",
			[]string{"line 6: spec.ports[0].port: \"eighty\" is not a whole number"},
		},
		{
			"object for a list",
			containerHead + "spec:\n  image: x\n  ports: {name: http}\n",
			[]string{"line 6: spec.ports: want a list"},
		},
		{
			"value in a map",
			"apiVersion: compute.hangarlab.cloud/v1alpha1\nkind: Container\nmetadata:\n  name: web\n  labels:\n    app: [a]\n",
			[]string{"line 6: metadata.labels[app]: want a single value"},
		},
		{
			"line in a later document",
			containerHead + "spec: {image: x}\n---\n" + containerHead + "spec: {image: x, colour: red}\n",
			[]string{"document 2, line 9: spec.colour: unknown field"},
		},
		{
			"wrong apiVersion",
			"apiVersion: compute.hangarlab.cloud/v1\nkind: Container\n",
			[]string{"kind Container is in apiVersion compute.hangarlab.cloud/v1alpha1, not \"compute.hangarlab.cloud/v1\""},
		},
		{
			"kind in the wrong case",
			"apiVersion: compute.hangarlab.cloud/v1alpha1\nkind: container\n",
			[]string{"unknown kind \"container\"; did you mean Container?", "compute.hangarlab.cloud/v1alpha1: Container, Machine"},
		},
		{
			"no kind",
			"apiVersion: compute.hangarlab.cloud/v1alpha1\nmetadata: {name: x}\n",
			[]string{"apiVersion and kind are required", "tenancy.hangarlab.cloud/v1alpha1: Plan, Project, Tenant"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeManifests("f.yaml", strings.NewReader(tt.doc))
			if err == nil {
				t.Fatal("decoded")
			}
			if code := exitCode(err); code != ExitUsage {
				t.Errorf("exit %d, want %d", code, ExitUsage)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q\nlacks %q", err, w)
				}
			}
		})
	}
}

// TestManifestCheckPassesWhatDecodes keeps the check from refusing what
// protojson takes.
func TestManifestCheckPassesWhatDecodes(t *testing.T) {
	doc := "apiVersion: compute.hangarlab.cloud/v1alpha1\nkind: Container\n" +
		"metadata:\n  name: web\n  labels: {app: web}\n" +
		"spec:\n  image: x\n  run_state: CONTAINER_RUN_STATE_STOPPED\n" +
		"  ports: [{name: http, port: \"80\"}]\n  resources: &r {cpu: 2}\n  env: ~\n"
	if _, err := decodeManifests("f.yaml", strings.NewReader(doc)); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownKindSuggests(t *testing.T) {
	for name, want := range map[string]string{
		"containr": "did you mean containers?",
		"vms":      "did you mean virtualmachines?",
		"widgets":  "kinds: addresses (addr), containers (ct)",
	} {
		if err := unknownKind(name); !strings.Contains(err.Error(), want) {
			t.Errorf("unknownKind(%q) = %q, lacks %q", name, err, want)
		}
	}
	if err := unknownKind("widgets"); strings.Contains(err.Error(), "did you mean") {
		t.Errorf("unknownKind(widgets) = %q, suggests a kind", err)
	}
}
