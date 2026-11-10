package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// listDirTimeout bounds a listing: the server reads a whole directory, up
// to 100,000 entries, on one vCPU, and may wait for another listing first.
const listDirTimeout = 20 * time.Second

func (s nodeService) ListFileShareDir(ctx context.Context, req *connect.Request[nodev1.ListFileShareDirRequest]) (*connect.Response[nodev1.ListFileShareDirResponse], error) {
	if s.a.cfg.Drivers == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this node runs no machines"))
	}
	uid := req.Msg.GetMachineUid()
	if uid == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("machine_uid is required"))
	}
	vm, err := s.a.vmOf(ctx, uid)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if vm == nil || vm.State != driver.VMRunning || !vm.FileShareServer {
		// Unavailable, not FailedPrecondition, which the server's own
		// refusals of a path take.
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("machine %s is no file share's server running on this node", uid))
	}
	ctx, cancel := context.WithTimeout(ctx, listDirTimeout)
	defer cancel()
	page, err := s.a.cfg.Drivers.Guest.ListDir(ctx, uid, driver.ShareListDir{Path: req.Msg.GetPath(), After: req.Msg.GetAfter(), Limit: req.Msg.GetLimit()})
	switch {
	case errors.Is(err, driver.ErrGuestCannot):
		return nil, connect.NewError(connect.CodeUnimplemented, err)
	case err != nil && connect.CodeOf(err) != connect.CodeUnknown:
		return nil, connect.NewError(connect.CodeOf(err), err)
	case err != nil:
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("the share's server: %w", err))
	}
	res := &nodev1.ListFileShareDirResponse{More: page.More}
	for _, e := range page.Entries {
		res.Entries = append(res.Entries, &nodev1.FileShareDirEntry{Name: e.Name, Type: fileTypeOf(e.Type), Size: e.Size, ModifyTime: timestamppb.New(e.ModTime)})
	}
	return connect.NewResponse(res), nil
}

func fileTypeOf(t driver.ShareFileType) nodev1.FileShareFileType {
	switch t {
	case driver.ShareFileRegular:
		return nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_REGULAR
	case driver.ShareFileDirectory:
		return nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_DIRECTORY
	case driver.ShareFileSymlink:
		return nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_SYMLINK
	}
	return nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_OTHER
}
