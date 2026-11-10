//go:build testbed

package guest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// listDir lists a directory of srv's share as the node will, through its
// agent.
func listDir(t *testing.T, srv *vmtest.VM, path []string, after string, limit uint32) (*guestv1.ListDirResponse, error) {
	t.Helper()
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: protocols, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return chguest.Dial(ctx, srv.Path("vsock"), agent.Port)
	}}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req := &guestv1.ListDirRequest{After: []byte(after), Limit: limit}
	for _, p := range path {
		req.Path = append(req.Path, []byte(p))
	}
	c := guestv1connect.NewFileShareServerServiceClient(&http.Client{Transport: tr}, "http://guest", connect.WithReadMaxBytes(agent.MaxMessage))
	res, err := c.ListDir(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// TestAServerListsItsShare: a client writes a share, links out of it
// included, and the server lists it through its agent, beneath the share's
// root alone, names as the bytes written, a page at a time.
func TestAServerListsItsShare(t *testing.T) {
	srv := newShareServer(t)
	a := vmtest.New(t, &guestv1.BootConfig{})
	bootShareServer(t, srv, export(squashNone, client(a, false)))
	const files = 1200
	shareClient(t, a, srv, idle, &guestv1.Volume{Name: "files", Target: "/data"})
	mustSh(t, a, "", "cd /data && mkdir many dir && echo hi > dir/f && "+
		"ln -s /proc proc && ln -s / root && ln -s dir in && "+
		"printf x > \"$(printf 'caf\\351\\377')\" && "+
		fmt.Sprintf("cd many && i=0; while [ $i -lt %d ]; do : > $(printf 'f%%05d' $i); i=$((i+1)); done", files))

	t.Run("the root, as written", func(t *testing.T) {
		res, err := listDir(t, srv, nil, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		types := map[string]guestv1.FileType{}
		for _, e := range res.GetEntries() {
			names = append(names, string(e.GetName()))
			types[string(e.GetName())] = e.GetType()
		}
		if want := []string{"caf\xe9\xff", "dir", "in", "many", "proc", "root"}; !slices.Equal(names, want) || res.GetMore() {
			t.Fatalf("the root lists %q (more %v), want %q", names, res.GetMore(), want)
		}
		for _, l := range []string{"proc", "root", "in"} {
			if types[l] != guestv1.FileType_FILE_TYPE_SYMLINK {
				t.Errorf("%s is listed as %v, not a link", l, types[l])
			}
		}
		if e := res.GetEntries()[0]; e.GetType() != guestv1.FileType_FILE_TYPE_REGULAR || e.GetSize() != 1 {
			t.Errorf("the file not UTF-8 is %v", e)
		}
	})

	t.Run("no link is followed", func(t *testing.T) {
		for _, path := range [][]string{{"proc"}, {"proc", "self"}, {"root"}, {"root", "proc"}, {"in"}} {
			if _, err := listDir(t, srv, path, "", 0); connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Errorf("%q: %v, want FailedPrecondition", path, err)
			}
		}
		for _, path := range [][]string{{".."}, {"dir/.."}, {""}} {
			if _, err := listDir(t, srv, path, "", 0); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("%q: %v, want InvalidArgument", path, err)
			}
		}
		if _, err := listDir(t, srv, []string{"absent"}, "", 0); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("an absent directory: %v", err)
		}
		res, err := listDir(t, srv, []string{"dir"}, "", 0)
		if err != nil || len(res.GetEntries()) != 1 || string(res.GetEntries()[0].GetName()) != "f" || res.GetEntries()[0].GetSize() != 3 {
			t.Errorf("dir lists %v, %v", res, err)
		}
	})

	t.Run("a large directory, a page at a time", func(t *testing.T) {
		var seen []string
		after := ""
		for pages := 0; ; pages++ {
			if pages > files/500+1 {
				t.Fatalf("still more after %d pages", pages)
			}
			res, err := listDir(t, srv, []string{"many"}, after, 500)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range res.GetEntries() {
				seen = append(seen, string(e.GetName()))
			}
			if !res.GetMore() {
				break
			}
			after = seen[len(seen)-1]
		}
		if len(seen) != files || !slices.IsSorted(seen) || len(slices.Compact(slices.Clone(seen))) != files {
			t.Errorf("listed %d names, sorted %v, want %d once each", len(seen), slices.IsSorted(seen), files)
		}
	})

	t.Run("the share still serves", func(t *testing.T) {
		if got := mustSh(t, a, "", "cat /data/dir/f"); got != "hi\n" {
			t.Errorf("a read %q", got)
		}
	})
}
