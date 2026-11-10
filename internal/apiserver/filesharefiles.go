package apiserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"strings"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// Bounds of a listing, as the share's server keeps them
// (internal/guest/fileshare), checked here so a bad request never reaches
// a node.
const (
	maxFilesPageSize = 1000
	maxFilesPath     = 256
	maxFileName      = 255
	// maxFilesPathBytes is the longest path, its names joined by "/", the
	// kernel takes (PATH_MAX less its NUL).
	maxFilesPathBytes = 4095
)

// fileShareFiles is FileShareFilesService: what a share holds, as its server
// lists it (docs/decisions/file-shares.md).
type fileShareFiles struct{ s *Server }

func (h fileShareFiles) ListFiles(ctx context.Context, req *connect.Request[storagev1alpha1.ListFilesRequest]) (*connect.Response[storagev1alpha1.ListFilesResponse], error) {
	s := h.s
	m := req.Msg
	b := s.backend(registry.FileShare)
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	key := resource.Key{Project: m.GetProject(), Name: m.GetName()}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	if err := checkFilesPath(m.GetPath()); err != nil {
		return nil, err
	}
	if m.GetPageSize() < 0 || m.GetPageSize() > maxFilesPageSize {
		return nil, invalid("page_size %d is not 0 to %d", m.GetPageSize(), maxFilesPageSize)
	}
	after, err := base64.RawURLEncoding.DecodeString(m.GetPageToken())
	if err != nil || len(after) > maxFileName {
		return nil, invalid("page_token is not one a listing gave")
	}
	_, obj, err := b.read(ctx, p, authz.Browse, key)
	if err != nil {
		return nil, err
	}
	fs := obj.(*storagev1alpha1.FileShare)
	what := fmt.Sprintf("file share %q", key.Name)
	if fs.GetMetadata().GetDeleteTime() != nil {
		return nil, failedPrecondition("%s is being deleted", what)
	}
	server, err := s.shareServer(ctx, key, fs)
	if err != nil {
		return nil, err
	}
	project, err := s.get(ctx, registry.Project, resource.Key{Name: key.Project})
	if err != nil {
		return nil, apiError(err)
	}
	if project == nil {
		return nil, failedPrecondition("project %q is gone", key.Project)
	}
	call, err := s.callNode(ctx, p, project.(*tenancyv1alpha1.Project).GetSpec().GetTenant(), server.GetSpec().GetNode(), what+"'s server")
	if err != nil {
		return nil, err
	}
	defer call.release()
	res, err := call.nodes.ListFileShareDir(ctx, connect.NewRequest(&nodev1.ListFileShareDirRequest{
		MachineUid: server.GetMetadata().GetUid(), Path: m.GetPath(), After: after, Limit: uint32(m.GetPageSize()),
	}))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%s's server started before it could list files; it lists them once it starts again", what))
		}
		return nil, call.failed(err)
	}
	out := &storagev1alpha1.ListFilesResponse{}
	for _, e := range res.Msg.GetEntries() {
		out.Entries = append(out.Entries, &storagev1alpha1.FileShareEntry{
			Name: e.GetName(), Type: entryTypeOf(e.GetType()), SizeBytes: int64(min(e.GetSize(), math.MaxInt64)), ModifyTime: e.GetModifyTime(),
		})
	}
	if res.Msg.GetMore() && len(out.Entries) > 0 {
		out.NextPageToken = base64.RawURLEncoding.EncodeToString(out.Entries[len(out.Entries)-1].GetName())
	}
	return connect.NewResponse(out), nil
}

// shareServer is the running machine that serves fs, Unavailable while
// there is none.
func (s *Server) shareServer(ctx context.Context, key resource.Key, fs *storagev1alpha1.FileShare) (*computev1alpha1.Machine, error) {
	what := fmt.Sprintf("file share %q", key.Name)
	mobj, err := s.get(ctx, registry.Machine, resource.Key{Project: key.Project, Name: shareServerName(key.Name)})
	if err != nil {
		return nil, apiError(err)
	}
	m, _ := mobj.(*computev1alpha1.Machine)
	switch {
	case m == nil || !ownedBy(m.GetMetadata(), fs.GetMetadata().GetUid()):
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s has no server yet", what))
	case m.GetSpec().GetNode() == "":
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s's server is not placed on a node yet", what))
	case m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING:
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s's server is not running: its machine is %s", what,
			strings.ToLower(strings.TrimPrefix(m.GetStatus().GetPhase().String(), "MACHINE_PHASE_"))))
	}
	return m, nil
}

// checkFilesPath refuses a path the share's server would.
func checkFilesPath(path [][]byte) error {
	if len(path) > maxFilesPath {
		return invalid("path has %d names, past %d", len(path), maxFilesPath)
	}
	for _, name := range path {
		switch n := string(name); {
		case n == "", n == ".", n == "..":
			return invalid("path: %q is not a name of an entry", n)
		case strings.ContainsAny(n, "/\x00"):
			return invalid("path: %q holds a slash or NUL", n)
		case len(n) > maxFileName:
			return invalid("path: a name is %d bytes, past %d", len(n), maxFileName)
		}
	}
	if n := len(bytes.Join(path, []byte("/"))); n > maxFilesPathBytes {
		return invalid("path is %d bytes, past %d", n, maxFilesPathBytes)
	}
	return nil
}

func entryTypeOf(t nodev1.FileShareFileType) storagev1alpha1.FileShareEntryType {
	switch t {
	case nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_REGULAR:
		return storagev1alpha1.FileShareEntryType_FILE_SHARE_ENTRY_TYPE_REGULAR
	case nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_DIRECTORY:
		return storagev1alpha1.FileShareEntryType_FILE_SHARE_ENTRY_TYPE_DIRECTORY
	case nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_SYMLINK:
		return storagev1alpha1.FileShareEntryType_FILE_SHARE_ENTRY_TYPE_SYMLINK
	}
	return storagev1alpha1.FileShareEntryType_FILE_SHARE_ENTRY_TYPE_OTHER
}
