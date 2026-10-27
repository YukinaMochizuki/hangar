package boot

import (
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

// TestDiskMounts: the filesystems grown and trimmed are the ext4 ones on
// the guest's virtio disks, each once with every place it is mounted, a
// space in a mount point unescaped; a tmpfs, an NFS share, an overlay, and
// an ext4 on a loop device a container made, are not.
func TestDiskMounts(t *testing.T) {
	const mountinfo = `22 1 253:0 / / rw,relatime - ext4 /dev/vda rw
23 22 0:5 / /dev rw,nosuid - devtmpfs devtmpfs rw,mode=755
30 22 0:25 / /run rw,nosuid,nodev - tmpfs tmpfs rw,mode=755
31 22 253:32 / /var/lib/app rw,relatime shared:1 - ext4 /dev/vdc rw
32 22 253:32 /logs /srv/my\040logs ro,relatime - ext4 /dev/vdc rw
33 22 0:40 / /shared rw,relatime - nfs4 10.0.0.9:/ rw,vers=4.2
34 22 0:41 / /var/lib/docker/overlay2/x/merged rw - overlay overlay rw
35 22 7:0 / /mnt/loop rw - ext4 /dev/loop0 rw
36 22 253:48 / /data rw - ext4 /dev/vdd rw
`
	want := []diskMount{
		{dev: unix.Mkdev(253, 0), source: "/dev/vda", dirs: []string{"/"}},
		{dev: unix.Mkdev(253, 32), source: "/dev/vdc", dirs: []string{"/var/lib/app", "/srv/my logs"}},
		{dev: unix.Mkdev(253, 48), source: "/dev/vdd", dirs: []string{"/data"}},
	}
	if got := diskMounts(mountinfo); !reflect.DeepEqual(got, want) {
		t.Errorf("diskMounts:\n%+v\nwant\n%+v", got, want)
	}
	// A FileShare's server mounts its share alone.
	server := "40 1 0:2 / / rw - rootfs rootfs rw\n41 40 253:32 / /share rw,nosuid,nodev - ext4 /dev/vdc rw\n"
	if got := diskMounts(server); len(got) != 1 || got[0].source != "/dev/vdc" || got[0].dirs[0] != "/share" {
		t.Errorf("a server's mounts: %+v", got)
	}
	for _, bad := range []string{"", "garbage", "1 2 x:y / / rw - ext4 /dev/vda rw", "1 2 253:0 / / rw ext4 /dev/vda rw"} {
		if got := diskMounts(bad); len(got) != 0 {
			t.Errorf("diskMounts(%q) = %+v", bad, got)
		}
	}
}
