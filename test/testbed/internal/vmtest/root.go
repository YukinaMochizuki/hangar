//go:build testbed

package vmtest

import (
	"os"
	"testing"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// NewOn is New, with the guest booting from root, such as a zvol, instead of
// a copy of the root image. The test owns root.
func NewOn(t testing.TB, cfg *guestv1.BootConfig, root string) *VM {
	t.Helper()
	// New copies nothing from /dev/null; no testbed test runs in parallel,
	// so no other New reads the root image meanwhile.
	image := in.Rootfs
	in.Rootfs = os.DevNull
	v := New(t, cfg)
	in.Rootfs = image
	// Cloud Hypervisor and the disk checks follow the link to root.
	if err := os.Remove(v.Path("root.img")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, v.Path("root.img")); err != nil {
		t.Fatal(err)
	}
	return v
}
