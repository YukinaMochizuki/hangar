package cli

import (
	"testing"

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
