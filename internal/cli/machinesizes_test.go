package cli_test

import (
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/cli"
)

// A made-up size; the platform's real ones are its administrator's to define.
const sizeManifest = `apiVersion: compute.hangarlab.cloud/v1alpha1
kind: MachineSize
metadata:
  name: m
spec:
  displayName: M
  cpu: 2
  memory: 4Gi
  disk: 20Gi
  order: 20
`

// TestMachineSizes: platform.root applies a size from a manifest, and any
// platform member gets it by its plural or its short name.
func TestMachineSizes(t *testing.T) {
	w := newWorld(t)
	w.addMembers("carol@example.com")

	w.as("carol@example.com")
	if out := w.must("", "get", "machinesizes", "-o", "name"); out != "" {
		t.Errorf("a fresh platform lists machine sizes: %q", out)
	}
	if r := w.run(sizeManifest, "apply", "-f", "-"); r.code != cli.ExitError || !strings.Contains(r.err, "permission_denied") {
		t.Errorf("a member applying a machine size: exit %d (%s)", r.code, r.err)
	}

	w.as("root@example.com")
	if out := w.must(sizeManifest, "apply", "-f", "-"); out != "machinesize/m created\n" {
		t.Errorf("apply printed %q", out)
	}

	w.as("carol@example.com")
	table := w.must("", "get", "machinesizes")
	if !strings.HasPrefix(table, "NAME") || !strings.Contains(table, "m ") {
		t.Errorf("get machinesizes:\n%s", table)
	}
	if out := w.must("", "get", "ms", "m", "-o", "yaml"); !strings.Contains(out, "kind: MachineSize") || !strings.Contains(out, "disk: 20Gi") {
		t.Errorf("get ms m -o yaml:\n%s", out)
	}
}
