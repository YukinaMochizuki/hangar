package boot

import (
	"errors"
	"fmt"
	"net/netip"
	"os"

	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// nfsClientID is where the kernel takes the NFSv4 client's identity from,
// for the guest's network namespace.
var nfsClientID = "/sys/fs/nfs/net/nfs_client/identifier"

// ShareOptions are the NFS options of every FileShare's mount: hard, so a
// restarting server pauses its clients rather than failing them, with a
// 5 s timeout (docs/decisions/file-shares.md).
const ShareOptions = "vers=4.2,hard,timeo=50"

// setNFSClientID gives the guest its own NFSv4 client identity, which must
// come before the first mount. Without one, every guest is the same client
// to a server, and one guest's mount takes away another's opens and locks.
func setNFSClientID(id string) error {
	if id == "" {
		return errors.New("a FileShare needs the machine's uid as its NFS client identity, and the boot disk has none")
	}
	if err := os.WriteFile(nfsClientID, []byte(id+"\n"), 0); err != nil {
		return fmt.Errorf("setting the NFS client identity: %w", err)
	}
	return nil
}

// shareMount is how v, a FileShare, is mounted at dir: its server's root
// over NFSv4.2, nosuid and nodev, so that what one machine's root makes
// setuid never runs as such in another, and read-only as declared.
func shareMount(v *guestv1.Volume, dir string) (mount, error) {
	if v.GetDevice() != "" {
		return mount{}, errors.New("it names both a device and a FileShare")
	}
	addr, err := netip.ParseAddr(v.GetFileShare().GetAddress())
	if err != nil || addr.Zone() != "" {
		return mount{}, fmt.Errorf("its server's address %q is not an IP address", v.GetFileShare().GetAddress())
	}
	addr = addr.Unmap()
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV)
	if v.GetReadOnly() {
		flags |= unix.MS_RDONLY
	}
	// The kernel takes the server by address; mount.nfs4 would resolve the
	// source's host into addr for it.
	source := addr.String() + ":/"
	if addr.Is6() {
		source = "[" + addr.String() + "]:/"
	}
	return mount{source, dir, "nfs4", ShareOptions + ",addr=" + addr.String(), flags}, nil
}
