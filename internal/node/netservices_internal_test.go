package node

import (
	"testing"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/metadata"
)

// TestOnlyItsNodeServesAGuestsData: a node serves a machine's cloud-init
// data, and its instance id, only if the machine is on it. The apiserver
// keeps it from other nodes; a machine of another node that holds it
// anyway, so read whole, is served as a machine without it.
func TestOnlyItsNodeServesAGuestsData(t *testing.T) {
	a := &Agent{cfg: Config{Name: "n1"}}
	m := func(node string) *computev1alpha1.Machine {
		return &computev1alpha1.Machine{
			Metadata: &metav1.ObjectMeta{Uid: "machine-uid"},
			Spec: &computev1alpha1.MachineSpec{
				Node: node,
				Boot: &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{
					RootDisk:  "root",
					CloudInit: &computev1alpha1.MachineCloudInit{InstanceId: "vm-uid", UserData: "secret", VendorData: "keys"},
				}},
			},
		}
	}
	for _, tc := range []struct {
		node string
		want metadata.Instance
	}{
		{"n1", metadata.Instance{UID: "vm-uid", Hostname: "web", UserData: "secret", VendorData: "keys"}},
		{"n2", metadata.Instance{UID: "machine-uid", Hostname: "web"}},
	} {
		if got := a.instance(m(tc.node), "web"); got != tc.want {
			t.Errorf("a machine on %s is served as %+v, want %+v", tc.node, got, tc.want)
		}
	}
}
