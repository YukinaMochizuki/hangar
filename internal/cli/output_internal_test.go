package cli

import (
	"testing"

	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
)

func TestUsedAs(t *testing.T) {
	for _, tt := range []struct {
		a    *storagev1alpha1.DiskAttachment
		want string
	}{
		{nil, "-"},
		{&storagev1alpha1.DiskAttachment{Kind: "Container", Name: "db", Machine: "ct-db", As: storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_VOLUME}, "volume of container/db"},
		{&storagev1alpha1.DiskAttachment{Kind: "VirtualMachine", Name: "web", Machine: "vm-web", As: storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_BOOT}, "boot of virtualmachine/web"},
		{&storagev1alpha1.DiskAttachment{Machine: "m", As: storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_DATA}, "data of machine/m"},
	} {
		if got := usedAs(tt.a); got != tt.want {
			t.Errorf("usedAs(%v) = %q, want %q", tt.a, got, tt.want)
		}
	}
}

func TestForwardColumns(t *testing.T) {
	for _, tt := range []struct {
		pf   *networkv1alpha1.PortForward
		want string
	}{
		{&networkv1alpha1.PortForward{}, "-\t-\tUNSPECIFIED\t-"},
		{&networkv1alpha1.PortForward{
			Spec: &networkv1alpha1.PortForwardSpec{
				Target:   &networkv1alpha1.PortForwardTarget{Object: &networkv1alpha1.PortForwardTarget_Container{Container: "web"}},
				Protocol: networkv1alpha1.Protocol_PROTOCOL_TCP, TargetPort: 8080,
			},
			Status: &networkv1alpha1.PortForwardStatus{Port: 21234},
		}, "-\t21234\tTCP\tcontainer/web:eth0:8080"},
		{&networkv1alpha1.PortForward{
			Spec: &networkv1alpha1.PortForwardSpec{
				Target:   &networkv1alpha1.PortForwardTarget{Object: &networkv1alpha1.PortForwardTarget_VirtualMachine{VirtualMachine: "db"}, Interface: "eth1"},
				Protocol: networkv1alpha1.Protocol_PROTOCOL_UDP, TargetPort: 53,
			},
			Status: &networkv1alpha1.PortForwardStatus{Ipv4: "198.51.100.3", Port: 10053},
		}, "198.51.100.3\t10053\tUDP\tvirtualmachine/db:eth1:53"},
	} {
		if got := forwardColumns(tt.pf); got != tt.want {
			t.Errorf("forwardColumns(%v) = %q, want %q", tt.pf, got, tt.want)
		}
	}
}
