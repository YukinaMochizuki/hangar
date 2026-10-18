package apiserver_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// machineSize is a made-up size; the platform's real ones are its
// administrator's to define.
func machineSize(name, display, cpu, memory, disk string, order int32) *computev1alpha1.MachineSize {
	return &computev1alpha1.MachineSize{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec:     &computev1alpha1.MachineSizeSpec{DisplayName: display, Cpu: cpu, Memory: memory, Disk: disk, Order: order},
	}
}

func applySize(ms *computev1alpha1.MachineSize) *connect.Request[computev1alpha1.ApplyMachineSizeRequest] {
	return connect.NewRequest(&computev1alpha1.ApplyMachineSizeRequest{MachineSize: ms})
}

func listSizes(t *testing.T, c clients) []string {
	t.Helper()
	res := must[computev1alpha1.ListMachineSizesResponse](t)(c.sizes.ListMachineSizes(t.Context(), connect.NewRequest(&computev1alpha1.ListMachineSizesRequest{})))
	var names []string
	for _, ms := range res.GetItems() {
		names = append(names, ms.GetMetadata().GetName())
	}
	return names
}

// TestNoMachineSizeByDefault: the platform ships no size; its administrator
// defines every one.
func TestNoMachineSizeByDefault(t *testing.T) {
	e := newEnv(t)
	if got := listSizes(t, e.as(root)); len(got) != 0 {
		t.Errorf("a fresh store lists machine sizes %v", got)
	}
}

// TestOnlyPlatformRootWritesMachineSizes: everyone allowed to use Hangar
// reads the sizes, people and service tokens alike; only platform.root
// writes them, whatever role anyone else holds.
func TestOnlyPlatformRootWritesMachineSizes(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.setupTenant(alice, "acme", "web")
	e.addMember("carol@example.com")
	a := e.as(alice)
	must[iamv1alpha1.ApplyRoleBindingResponse](t)(a.bindings.ApplyRoleBinding(ctx, binding("web-bob", bob, "project.editor", projectScope("web"))))
	tok := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{
		Project: "web", Name: "ci", Role: "project.owner",
	})))

	_, err := a.sizes.ApplyMachineSize(ctx, applySize(machineSize("m", "M", "2", "4Gi", "20Gi", 20)))
	wantCode(t, err, connect.CodePermissionDenied, "a tenant owner applying a machine size")
	must[computev1alpha1.ApplyMachineSizeResponse](t)(e.as(root).sizes.ApplyMachineSize(ctx, applySize(machineSize("m", "M", "2", "4Gi", "20Gi", 20))))

	readers := map[string]clients{
		"a tenant owner":                     a,
		"a project editor":                   e.as(bob),
		"a platform member with no grant":    e.as("carol@example.com"),
		"a service token of a project owner": e.withToken(tok.GetSecret()),
	}
	for who, c := range readers {
		if _, err := c.sizes.GetMachineSize(ctx, connect.NewRequest(&computev1alpha1.GetMachineSizeRequest{Name: "m"})); err != nil {
			t.Errorf("%s getting a machine size: %v", who, err)
		}
		if got := listSizes(t, c); len(got) != 1 || got[0] != "m" {
			t.Errorf("%s lists machine sizes %v", who, got)
		}
		// A watch that is let in opens with a bookmark; a refusal ends it.
		stream, err := c.sizes.WatchMachineSizes(ctx, connect.NewRequest(&computev1alpha1.WatchMachineSizesRequest{}))
		if err != nil {
			t.Fatalf("%s watching machine sizes: %v", who, err)
		}
		if !stream.Receive() {
			t.Errorf("%s watching machine sizes: %v", who, stream.Err())
		}
		_ = stream.Close()
	}
	for who, c := range readers {
		changed := machineSize("m", "M", "64", "4Gi", "20Gi", 20)
		_, err := c.sizes.ApplyMachineSize(ctx, applySize(changed))
		wantCode(t, err, connect.CodePermissionDenied, who+" changing a machine size")
		_, err = c.sizes.DeleteMachineSize(ctx, connect.NewRequest(&computev1alpha1.DeleteMachineSizeRequest{Name: "m"}))
		wantCode(t, err, connect.CodePermissionDenied, who+" deleting a machine size")
	}
	// Nothing reports on a size: not root, not a node.
	status := connect.NewRequest(&computev1alpha1.UpdateMachineSizeStatusRequest{Name: "m", Status: &computev1alpha1.MachineSizeStatus{ObservedGeneration: 1}})
	_, err = e.as(root).sizes.UpdateMachineSizeStatus(ctx, status)
	wantCode(t, err, connect.CodePermissionDenied, "root writing a machine size's status")
	_, err = e.asNode().sizes.UpdateMachineSizeStatus(ctx, status)
	wantCode(t, err, connect.CodePermissionDenied, "a node writing a machine size's status")
	_, err = e.asNode().sizes.ApplyMachineSize(ctx, applySize(machineSize("m", "M", "64", "4Gi", "20Gi", 20)))
	wantCode(t, err, connect.CodePermissionDenied, "a node applying a machine size")

	_, err = e.as("nobody@example.com").sizes.GetMachineSize(ctx, connect.NewRequest(&computev1alpha1.GetMachineSizeRequest{Name: "m"}))
	wantCode(t, err, connect.CodePermissionDenied, "someone not a platform member reading a machine size")

	must[computev1alpha1.DeleteMachineSizeResponse](t)(e.as(root).sizes.DeleteMachineSize(ctx, connect.NewRequest(&computev1alpha1.DeleteMachineSizeRequest{Name: "m"})))
}

// TestMachineSizeAdmission: a size is checked as a container's resources
// are, though nothing in it has a default.
func TestMachineSizeAdmission(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	r := e.as(root)
	for name, ms := range map[string]*computev1alpha1.MachineSize{
		"no cpu":                  machineSize("x", "X", "", "4Gi", "20Gi", 1),
		"zero cpu":                machineSize("x", "X", "0", "4Gi", "20Gi", 1),
		"a fraction of a cpu":     machineSize("x", "X", "500m", "4Gi", "20Gi", 1),
		"cpu past the maximum":    machineSize("x", "X", "8193", "4Gi", "20Gi", 1),
		"no memory":               machineSize("x", "X", "2", "", "20Gi", 1),
		"memory below 128Mi":      machineSize("x", "X", "2", "127Mi", "20Gi", 1),
		"memory unreadable":       machineSize("x", "X", "2", "lots", "20Gi", 1),
		"no disk":                 machineSize("x", "X", "2", "4Gi", "", 1),
		"disk below 1Gi":          machineSize("x", "X", "2", "4Gi", "512Mi", 1),
		"a disk of 0Gi":           machineSize("x", "X", "2", "4Gi", "0Gi", 1),
		"memory past the maximum": machineSize("x", "X", "2", "5Ti", "20Gi", 1),
		"disk not in whole Gi":    machineSize("x", "X", "2", "4Gi", "1536Mi", 1),
		"disk past the maximum":   machineSize("x", "X", "2", "4Gi", "65Ti", 1),
		"no display name":         machineSize("x", "", "2", "4Gi", "20Gi", 1),
		"a blank display name":    machineSize("x", "  ", "2", "4Gi", "20Gi", 1),
		"a negative order":        machineSize("x", "X", "2", "4Gi", "20Gi", -1),
		"a display name too long": machineSize("x", strings.Repeat("x", 64), "2", "4Gi", "20Gi", 1),
	} {
		_, err := r.sizes.ApplyMachineSize(ctx, applySize(ms))
		wantCode(t, err, connect.CodeInvalidArgument, name)
	}
	// A refusal names the size's own field, not a machine's.
	for field, ms := range map[string]*computev1alpha1.MachineSize{
		"spec.cpu 8193 ":   machineSize("x", "X", "8193", "4Gi", "20Gi", 1),
		"spec.memory 5Ti ": machineSize("x", "X", "2", "5Ti", "20Gi", 1),
		"spec.disk 65Ti ":  machineSize("x", "X", "2", "4Gi", "65Ti", 1),
		"spec.disk 0Gi ":   machineSize("x", "X", "2", "4Gi", "0Gi", 1),
	} {
		if _, err := r.sizes.ApplyMachineSize(ctx, applySize(ms)); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("the refusal of %q does not name it: %v", field, err)
		}
	}
	for name, ms := range map[string]*computev1alpha1.MachineSize{
		"the smallest":          machineSize("tiny", "Tiny", "1", "128Mi", "1Gi", 0),
		"cpu in millicores":     machineSize("two", "Two", "2000m", "2Gi", "10Gi", 5),
		"disk in a larger unit": machineSize("big", "Big", "16", "64Gi", "1Ti", 90),
	} {
		if _, err := r.sizes.ApplyMachineSize(ctx, applySize(ms)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestMachineSizeRefusesUnknownFields: a field the apiserver's schema does
// not have is never stored. The binary codec keeps it, and the size is
// refused; the JSON codec drops it, and the size is stored without it.
func TestMachineSizeRefusesUnknownFields(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	r := e.as(root)
	ms := machineSize("m", "M", "2", "4Gi", "20Gi", 20)
	ms.Spec.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
	_, err := r.sizes.ApplyMachineSize(ctx, applySize(ms))
	wantCode(t, err, connect.CodeInvalidArgument, "a machine size with an unknown spec field")
	if err == nil || !strings.Contains(err.Error(), "machine_size.spec") {
		t.Errorf("the refusal does not name where the field is: %v", err)
	}
	if got := listSizes(t, r); len(got) != 0 {
		t.Errorf("a refused size was stored: %v", got)
	}

	body := `{"machineSize": {"metadata": {"name": "j"}, "spec": {"displayName": "J", "cpu": "1", "memory": "1Gi", "disk": "5Gi", "colour": "red"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.url+computev1alpha1connect.MachineSizeServiceApplyMachineSizeProcedure, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.DevUserHeader, root)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a JSON apply with an unknown field: %d %s", res.StatusCode, out)
	}
	got := must[computev1alpha1.GetMachineSizeResponse](t)(r.sizes.GetMachineSize(ctx, connect.NewRequest(&computev1alpha1.GetMachineSizeRequest{Name: "j"})))
	if unknownIn(got.GetMachineSize().ProtoReflect()) {
		t.Errorf("the size stored from JSON holds the unknown field: %v", got.GetMachineSize())
	}
	if got.GetMachineSize().GetSpec().GetDisk() != "5Gi" {
		t.Errorf("the size stored from JSON lost its known fields: %v", got.GetMachineSize())
	}
}

// unknownIn reports whether m, or a message in it, holds unknown fields.
func unknownIn(m protoreflect.Message) bool {
	if len(m.GetUnknown()) > 0 {
		return true
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Message() != nil && !fd.IsList() && !fd.IsMap() {
			found = unknownIn(v.Message())
		}
		return !found
	})
	return found
}
