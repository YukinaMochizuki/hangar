package node

import (
	"archive/tar"
	"bytes"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
)

// TestAStalledRegistryHoldsUpNoOtherImage: images on a registry that takes
// requests and never answers, as many as there are slots to unpack in, hold
// up no image from another: it is ready in seconds, long before the stalled
// ones time out.
func TestAStalledRegistryHoldsUpNoOtherImage(t *testing.T) {
	r := registrytest.New(t)
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	// The fake storage wants a file named image in a base.
	if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "image", Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, static.NewLayer(buf.Bytes(), types.OCIUncompressedLayer))
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{}
	for _, repo := range []string{"stalled1", "stalled2", "good"} {
		refs[repo], _ = r.Push(t, repo+":1", img)
	}
	var arrived []<-chan struct{}
	for _, repo := range []string{"stalled1", "stalled2"} {
		a, release := r.Block(repo)
		t.Cleanup(release)
		arrived = append(arrived, a)
	}
	// Stalls are given up after a minute: far longer than the good image
	// may take.
	images, err := registry.New(registry.Config{Cache: t.TempDir(), Timeouts: registry.Timeouts{ResponseHeader: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	host := fake.New()
	p := newPuller(images, host.Storage(), clockwork.NewRealClock(), 30*time.Minute, 2)
	done := make(chan struct{}, 3)
	p.start(t.Context(), func() { done <- struct{}{} })
	t.Cleanup(p.wait)

	_, _, _, _ = p.get("m1", refs["stalled1"], "")
	_, _, _, _ = p.get("m2", refs["stalled2"], "")
	for _, a := range arrived {
		<-a
	}
	start := time.Now()
	_, _, _, _ = p.get("m3", refs["good"], "")
	for {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("the good image not ready after %v, behind two stalled ones", time.Since(start))
		}
		if _, _, ok, err := p.get("m3", refs["good"], ""); ok || err != nil {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	t.Logf("the good image ready in %v", time.Since(start))
}
