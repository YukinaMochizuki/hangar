package fileshare

import (
	"errors"
	"fmt"
	"net/netip"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// Export flags of include/uapi/linux/nfsd/export.h. Leaving out
// NFSEXP_INSECURE_PORT (0x2) and NFSEXP_ASYNC (0x10) makes the export secure
// and sync (docs/decisions/file-shares.md).
const (
	expReadOnly       = 0x0001
	expRootSquash     = 0x0004
	expAllSquash      = 0x0008
	expNoSubtreeCheck = 0x0400
	expFSID           = 0x2000
)

// export is a FileShareExport checked: each client by its address.
type export struct {
	readOnly         map[netip.Addr]bool
	squash           guestv1.FileShareSquash
	anonUID, anonGID uint32
}

// noID is (uid_t)-1, which the kernel refuses as an export's anonymous id:
// it would refuse the whole answer, and hold every client up.
const noID = 1<<32 - 1

// parseExport checks e, refusing the whole of it for any part nfsd would
// not take.
func parseExport(e *guestv1.FileShareExport) (*export, error) {
	if e == nil {
		return nil, errors.New("no export")
	}
	switch e.GetSquash() {
	case guestv1.FileShareSquash_FILE_SHARE_SQUASH_NONE, guestv1.FileShareSquash_FILE_SHARE_SQUASH_ROOT, guestv1.FileShareSquash_FILE_SHARE_SQUASH_ALL:
	default:
		return nil, fmt.Errorf("squash %v: the export must say which users are squashed", e.GetSquash())
	}
	if e.GetAnonUid() == noID || e.GetAnonGid() == noID {
		return nil, fmt.Errorf("anonymous ids %d:%d: %d is no id", e.GetAnonUid(), e.GetAnonGid(), uint32(noID))
	}
	out := &export{readOnly: map[netip.Addr]bool{}, squash: e.GetSquash(), anonUID: e.GetAnonUid(), anonGID: e.GetAnonGid()}
	for _, c := range e.GetClients() {
		a, err := netip.ParseAddr(c.GetAddress())
		if err != nil || a.Zone() != "" {
			return nil, fmt.Errorf("client %q is not an IP address", c.GetAddress())
		}
		a = a.Unmap()
		if !hostAddress(a) {
			return nil, fmt.Errorf("client %s is not a host's address", a)
		}
		if _, dup := out.readOnly[a]; dup {
			return nil, fmt.Errorf("client %s is listed twice", a)
		}
		out.readOnly[a] = c.GetReadOnly()
	}
	return out, nil
}

// hostAddress reports whether a can be one machine's: not unspecified,
// loopback, multicast or broadcast.
func hostAddress(a netip.Addr) bool {
	return !a.IsUnspecified() && !a.IsLoopback() && !a.IsMulticast() && a != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

// flags are the export's flags for a client, read-only or not.
func (e *export) flags(readOnly bool) int {
	f := expNoSubtreeCheck | expFSID
	if readOnly {
		f |= expReadOnly
	}
	switch e.squash {
	case guestv1.FileShareSquash_FILE_SHARE_SQUASH_ROOT:
		f |= expRootSquash
	case guestv1.FileShareSquash_FILE_SHARE_SQUASH_ALL:
		f |= expRootSquash | expAllSquash
	}
	return f
}

// domain names the client at a in nfsd's caches: one auth domain per
// client, so each has its own export entry.
func domain(a netip.Addr) string { return "c-" + a.String() }

// clientOf is the client a domain names, if the export lists it.
func (e *export) clientOf(dom string) (readOnly, ok bool) {
	if e == nil || len(dom) < 3 || dom[:2] != "c-" {
		return false, false
	}
	a, err := netip.ParseAddr(dom[2:])
	if err != nil {
		return false, false
	}
	readOnly, ok = e.readOnly[a]
	return readOnly, ok
}
