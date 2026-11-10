package fake

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/fileshare"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// ListDir lists the directory ShareRoot gave the machine's share server, as
// the real server lists its share (fileshare.Dirs), and an empty share
// without one.
func (d guest) ListDir(ctx context.Context, machineUID string, req driver.ShareListDir) (driver.ShareDirPage, error) {
	d.h.mu.Lock()
	v, ok := d.h.vms[machineUID]
	running := ok && v.State == driver.VMRunning
	server := ok && v.spec.FileShareServer != nil
	silent, old, root := d.h.silent[machineUID], d.h.oldListGuests[machineUID], d.h.shareRoots[machineUID]
	d.h.mu.Unlock()
	switch {
	case !running:
		return driver.ShareDirPage{}, errUnknownVM
	case silent:
		return driver.ShareDirPage{}, fmt.Errorf("the guest agent of machine %s does not answer", machineUID)
	case !server || old:
		return driver.ShareDirPage{}, fmt.Errorf("%w: listing a share", driver.ErrGuestCannot)
	case root == "" && len(req.Path) > 0:
		return driver.ShareDirPage{}, connect.NewError(connect.CodeNotFound, errors.New("no such directory"))
	case root == "":
		return driver.ShareDirPage{}, nil
	}
	path := make([]string, len(req.Path))
	for i, p := range req.Path {
		path[i] = string(p)
	}
	entries, more, err := fileshare.NewDirs(root).List(ctx, path, string(req.After), int(req.Limit))
	if err != nil {
		return driver.ShareDirPage{}, err
	}
	page := driver.ShareDirPage{More: more}
	for _, e := range entries {
		t := driver.ShareFileOther
		switch e.Type {
		case guestv1.FileType_FILE_TYPE_REGULAR:
			t = driver.ShareFileRegular
		case guestv1.FileType_FILE_TYPE_DIRECTORY:
			t = driver.ShareFileDirectory
		case guestv1.FileType_FILE_TYPE_SYMLINK:
			t = driver.ShareFileSymlink
		}
		page.Entries = append(page.Entries, driver.ShareDirEntry{Name: []byte(e.Name), Type: t, Size: e.Size, ModTime: e.ModTime})
	}
	return page, nil
}

// ShareRoot has the machine's share server list dir, a directory of the
// test's.
func (h *Host) ShareRoot(machineUID, dir string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shareRoots[machineUID] = dir
}

// OldListGuest makes the machine's share server one that cannot list, as
// one booted before it could, or one that can again.
func (h *Host) OldListGuest(machineUID string, old bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.oldListGuests[machineUID] = old
}
