package node

import (
	"bytes"
	"net/http"
	"slices"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1/storagev1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// afterListingCustomImages runs then once, after the first list of
// CustomImages has been answered, before its answer is read.
type afterListingCustomImages struct {
	next connect.HTTPClient
	once sync.Once
	then func()
}

func (a *afterListingCustomImages) Do(r *http.Request) (*http.Response, error) {
	resp, err := a.next.Do(r)
	if r.URL.Path == storagev1alpha1connect.CustomImageServiceListCustomImagesProcedure {
		a.once.Do(a.then)
	}
	return resp, err
}

// TestASweepLeavesAnUploadTakenAfterItsList: an upload the node takes once
// the sweep has listed the custom images, as of an image made then, is
// neither stopped nor removed by that sweep, though its list does not
// show the image.
func TestASweepLeavesAnUploadTakenAfterItsList(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	host := fake.New()
	hook := &afterListingCustomImages{next: client.UnixSocket(env.Socket)}
	a, err := New(Config{
		Name: apiservertest.Node, HTTP: hook, BaseURL: client.UnixSocketURL, UploadDir: t.TempDir(),
		Drivers: &Drivers{Images: host.Images(), Storage: host.Storage(), Network: host.Network(), Hypervisor: host.Hypervisor(), Guest: host.Guest(), Converter: host.Converter()},
	})
	if err != nil {
		t.Fatal(err)
	}
	const uid = "made-after-the-list"
	chunk := bytes.Repeat([]byte("w"), 500)
	taken := false
	hook.then = func() {
		taken = true
		a.stager.expect(uid, 1000)
		if _, err := a.stager.write(t.Context(), uid, 0, 500, hexSum(chunk), bytes.NewReader(chunk)); err != nil {
			t.Errorf("the chunk taken after the list: %v", err)
		}
	}
	if err := a.sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !taken {
		t.Fatal("the sweep listed no custom images")
	}
	if n, err := a.stager.received(uid); err != nil || n != 500 {
		t.Errorf("the upload taken after the sweep's list: received %d %v, want 500 still taken", n, err)
	}
	if staged, err := a.stager.staged(); err != nil || !slices.Contains(staged, uid) {
		t.Errorf("staged %v %v, want the upload taken after the sweep's list kept", staged, err)
	}
}
