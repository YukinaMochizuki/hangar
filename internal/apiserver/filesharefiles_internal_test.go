package apiserver

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1/storagev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// listingNode lists the share served by machine uid as a node does: three
// names in all, two to a page, or err.
type listingNode struct {
	nodev1connect.UnimplementedNodeServiceHandler
	uid string
	err error
	got chan *nodev1.ListFileShareDirRequest
}

var listedAt = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func (n listingNode) ListFileShareDir(_ context.Context, req *connect.Request[nodev1.ListFileShareDirRequest]) (*connect.Response[nodev1.ListFileShareDirResponse], error) {
	if n.got != nil {
		n.got <- req.Msg
	}
	if req.Msg.GetMachineUid() != n.uid {
		return nil, connect.NewError(connect.CodeUnavailable, nil)
	}
	if n.err != nil {
		return nil, n.err
	}
	all := []*nodev1.FileShareDirEntry{
		{Name: []byte("a"), Type: nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_DIRECTORY, ModifyTime: timestamppb.New(listedAt)},
		{Name: []byte("b\xff"), Type: nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_REGULAR, Size: 42, ModifyTime: timestamppb.New(listedAt)},
		{Name: []byte("c"), Type: nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_SYMLINK, ModifyTime: timestamppb.New(listedAt)},
	}
	var rest []*nodev1.FileShareDirEntry
	for _, e := range all {
		if string(e.GetName()) > string(req.Msg.GetAfter()) {
			rest = append(rest, e)
		}
	}
	if len(rest) > 2 {
		return connect.NewResponse(&nodev1.ListFileShareDirResponse{Entries: rest[:2], More: true}), nil
	}
	return connect.NewResponse(&nodev1.ListFileShareDirResponse{Entries: rest}), nil
}

// newFilesWorld is an execWorld with a share "data" whose server runs on
// n1, and dave, a viewer of project web.
func newFilesWorld(t *testing.T) (*execWorld, *computev1alpha1.Machine) {
	t.Helper()
	w := newExecWorld(t, nil)
	system := auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	user := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "alice@example.com"})
	addMember(t, w.s, "dave@example.com")
	project := &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}}
	if _, err := w.s.backend(registry.RoleBinding).Apply(system, binding("viewer-dave", "dave@example.com", "project.viewer", project)); err != nil {
		t.Fatal(err)
	}
	applied, err := w.s.backend(registry.FileShare).Apply(user, &storagev1alpha1.FileShare{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: "data"},
		Spec:     &storagev1alpha1.FileShareSpec{Size: "1Gi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fs := applied.Object.(*storagev1alpha1.FileShare)
	m := machine(shareServerName("data"), "n1")
	m.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: "storage.hangarlab.cloud/v1alpha1", Kind: "FileShare", Name: "data",
		Uid: fs.GetMetadata().GetUid(), Controller: true,
	}}
	mapplied, err := w.s.backend(registry.Machine).Apply(system, m)
	if err != nil {
		t.Fatal(err)
	}
	return w, mapplied.Object.(*computev1alpha1.Machine)
}

func (w *execWorld) listFiles(t *testing.T, user string, msg *storagev1alpha1.ListFilesRequest) (*storagev1alpha1.ListFilesResponse, error) {
	t.Helper()
	if msg.Project == "" {
		msg.Project = "web"
	}
	req := connect.NewRequest(msg)
	req.Header().Set(auth.DevUserHeader, user)
	res, err := storagev1alpha1connect.NewFileShareFilesServiceClient(http.DefaultClient, w.url).ListFiles(t.Context(), req)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// TestListFilesPagesThroughWhatTheServerLists: the share's owner lists its
// root a page at a time, the names as the server gives them, bytes and all,
// and asks the node with the server's uid, the path and the page's start.
func TestListFilesPagesThroughWhatTheServerLists(t *testing.T) {
	t.Parallel()
	w, server := newFilesWorld(t)
	system := auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	reportMachine(t, w.s, system, server, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, 1)
	got := make(chan *nodev1.ListFileShareDirRequest, 4)
	w.connectNodeServing(t, listingNode{uid: server.GetMetadata().GetUid(), got: got})

	alice := "alice@example.com"
	first, err := w.listFiles(t, alice, &storagev1alpha1.ListFilesRequest{Name: "data", Path: [][]byte{[]byte("x")}, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	asked := <-got
	if string(asked.GetPath()[0]) != "x" || asked.GetLimit() != 2 || len(asked.GetAfter()) != 0 {
		t.Errorf("the node was asked %v", asked)
	}
	if len(first.GetEntries()) != 2 || first.GetNextPageToken() == "" {
		t.Fatalf("the first page is %v", first)
	}
	if e := first.GetEntries()[1]; string(e.GetName()) != "b\xff" || e.GetSizeBytes() != 42 ||
		e.GetType() != storagev1alpha1.FileShareEntryType_FILE_SHARE_ENTRY_TYPE_REGULAR || !e.GetModifyTime().AsTime().Equal(listedAt) {
		t.Errorf("the second entry is %v, as the node gave it", e)
	}
	second, err := w.listFiles(t, alice, &storagev1alpha1.ListFilesRequest{Name: "data", Path: [][]byte{[]byte("x")}, PageToken: first.GetNextPageToken()})
	if err != nil {
		t.Fatal(err)
	}
	if asked := <-got; string(asked.GetAfter()) != "b\xff" {
		t.Errorf("the second page was asked after %q", asked.GetAfter())
	}
	if len(second.GetEntries()) != 1 || string(second.GetEntries()[0].GetName()) != "c" || second.GetNextPageToken() != "" {
		t.Errorf("the last page is %v", second)
	}
}

// TestListFilesNeedsBrowse: a viewer, who may get the share, may not list
// what it holds, and is told nothing of a share that does not exist.
func TestListFilesNeedsBrowse(t *testing.T) {
	t.Parallel()
	w, server := newFilesWorld(t)
	system := auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	reportMachine(t, w.s, system, server, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, 1)
	w.connectNodeServing(t, listingNode{uid: server.GetMetadata().GetUid()})
	for _, name := range []string{"data", "absent"} {
		_, err := w.listFiles(t, "dave@example.com", &storagev1alpha1.ListFilesRequest{Name: name})
		wantCodeOf(t, err, connect.CodePermissionDenied, "a viewer listing share "+name)
	}
	_, err := w.listFiles(t, "alice@example.com", &storagev1alpha1.ListFilesRequest{Name: "absent"})
	wantCodeOf(t, err, connect.CodeNotFound, "the owner listing an absent share")
}

// TestListFilesRefusesBadRequests before any node is asked.
func TestListFilesRefusesBadRequests(t *testing.T) {
	t.Parallel()
	w, _ := newFilesWorld(t)
	alice := "alice@example.com"
	for what, msg := range map[string]*storagev1alpha1.ListFilesRequest{
		"a path of ..":    {Name: "data", Path: [][]byte{[]byte("..")}},
		"a path of /":     {Name: "data", Path: [][]byte{[]byte("a/b")}},
		"an empty name":   {Name: "data", Path: [][]byte{{}}},
		"a NUL":           {Name: "data", Path: [][]byte{[]byte("a\x00")}},
		"a page too big":  {Name: "data", PageSize: 1001},
		"a negative page": {Name: "data", PageSize: -1},
		"a bad token":     {Name: "data", PageToken: "!!"},
		"a path too long": {Name: "data", Path: slices.Repeat([][]byte{bytes.Repeat([]byte("a"), 255)}, 20)},
	} {
		_, err := w.listFiles(t, alice, msg)
		wantCodeOf(t, err, connect.CodeInvalidArgument, what)
	}
}

// TestListFilesOfAServerNotUp: a server not running, or whose node is not
// connected, is Unavailable, and one that cannot list Unimplemented; the
// server's own refusals keep their codes.
func TestListFilesOfAServerNotUp(t *testing.T) {
	t.Parallel()
	w, server := newFilesWorld(t)
	system := auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	alice := "alice@example.com"
	_, err := w.listFiles(t, alice, &storagev1alpha1.ListFilesRequest{Name: "data"})
	wantCodeOf(t, err, connect.CodeUnavailable, "a share whose server is not running")

	reportMachine(t, w.s, system, server, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, 1)
	_, err = w.listFiles(t, alice, &storagev1alpha1.ListFilesRequest{Name: "data"})
	wantCodeOf(t, err, connect.CodeUnavailable, "a share whose node is not connected")

	node := &switchingNode{}
	w.connectNodeServing(t, node)
	for code, what := range map[connect.Code]string{
		connect.CodeUnimplemented:      "a server that cannot list",
		connect.CodeFailedPrecondition: "a path that is no directory",
		connect.CodeNotFound:           "a path that does not exist",
		connect.CodeResourceExhausted:  "a directory too large",
	} {
		node.set(listingNode{uid: server.GetMetadata().GetUid(), err: connect.NewError(code, nil)})
		_, err := w.listFiles(t, alice, &storagev1alpha1.ListFilesRequest{Name: "data", Path: [][]byte{[]byte("p")}})
		wantCodeOf(t, err, code, what)
	}
}

// switchingNode serves whichever listingNode it was last set to.
type switchingNode struct {
	nodev1connect.UnimplementedNodeServiceHandler
	n chan listingNode
}

func (s *switchingNode) set(n listingNode) {
	if s.n == nil {
		s.n = make(chan listingNode, 1)
	}
	select {
	case <-s.n:
	default:
	}
	s.n <- n
}

func (s *switchingNode) ListFileShareDir(ctx context.Context, req *connect.Request[nodev1.ListFileShareDirRequest]) (*connect.Response[nodev1.ListFileShareDirResponse], error) {
	n := <-s.n
	defer func() { s.n <- n }()
	return n.ListFileShareDir(ctx, req)
}
