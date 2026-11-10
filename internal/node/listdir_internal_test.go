package node

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// TestListFileShareDirAsksTheServer: the node lists a share through its
// server's guest, with the server's codes, and only for a share's server
// running on it; a server that cannot list is Unimplemented.
func TestListFileShareDirAsksTheServer(t *testing.T) {
	ctx := t.Context()
	host := fake.New()
	vol, err := host.Storage().EnsureShareVolume(ctx, "s1", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Hypervisor().Start(ctx, driver.VMSpec{MachineUID: "srv", Machine: "p/fs-s1", VCPUs: 1, MemoryMiB: 256,
		FileShareServer: &driver.ShareServer{Volume: vol, Export: driver.ShareExport{Squash: driver.SquashNone, AnonUID: 65534, AnonGID: 65534}}}); err != nil {
		t.Fatal(err)
	}
	fill := func(dir string) error { return os.WriteFile(filepath.Join(dir, "image"), []byte("app"), 0o600) }
	if err := host.Storage().EnsureBase(ctx, "sha256:a", 0, fill); err != nil {
		t.Fatal(err)
	}
	root, err := host.Storage().EnsureRoot(ctx, "ct", "sha256:a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Hypervisor().Start(ctx, driver.VMSpec{MachineUID: "ct", Machine: "p/ct", VCPUs: 1, MemoryMiB: 512, Root: root}); err != nil {
		t.Fatal(err)
	}
	share := t.TempDir()
	if err := os.WriteFile(filepath.Join(share, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(share, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	host.ShareRoot("srv", share)
	a := &Agent{cfg: Config{
		Name: "n1", Clock: clockwork.NewFakeClockAt(time.Now()), Log: slog.New(slog.DiscardHandler),
		Drivers: &Drivers{Storage: host.Storage(), Hypervisor: host.Hypervisor(), Guest: host.Guest()},
	}, guests: newGuests(), usages: newUsages()}
	list := func(uid string, path ...string) (*nodev1.ListFileShareDirResponse, error) {
		req := &nodev1.ListFileShareDirRequest{MachineUid: uid}
		for _, p := range path {
			req.Path = append(req.Path, []byte(p))
		}
		res, err := nodeService{a: a, agent: ctx}.ListFileShareDir(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}

	res, err := list("srv")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetEntries()) != 2 || string(res.GetEntries()[0].GetName()) != "a.txt" || res.GetEntries()[0].GetSize() != 5 ||
		res.GetEntries()[0].GetType() != nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_REGULAR ||
		res.GetEntries()[1].GetType() != nodev1.FileShareFileType_FILE_SHARE_FILE_TYPE_DIRECTORY || res.GetMore() {
		t.Errorf("the share lists %v", res)
	}
	for what, c := range map[string]struct {
		uid  string
		path []string
		code connect.Code
	}{
		"a path that is no directory": {"srv", []string{"a.txt"}, connect.CodeFailedPrecondition},
		"a path that does not exist":  {"srv", []string{"nope"}, connect.CodeNotFound},
		"a path of ..":                {"srv", []string{".."}, connect.CodeInvalidArgument},
		"a machine that is no server": {"ct", nil, connect.CodeUnavailable},
		"a machine not on this node":  {"absent", nil, connect.CodeUnavailable},
		"no machine":                  {"", nil, connect.CodeInvalidArgument},
	} {
		if _, err := list(c.uid, c.path...); connect.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %v", what, err, c.code)
		}
	}
	host.OldListGuest("srv", true)
	if _, err := list("srv"); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("a server that cannot list: %v", err)
	}
}
