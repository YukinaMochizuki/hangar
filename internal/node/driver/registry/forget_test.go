package registry_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
)

// TestForgetWaitsForAPullOfItsDigest: a digest forgotten while it is being
// pulled stays forgotten; the pull does not bring it back.
func TestForgetWaitsForAPullOfItsDigest(t *testing.T) {
	r := registrytest.New(t)
	d := newImages(t, t.TempDir(), ocilayer.Limits{})
	ref, digest := r.Push(t, "app:1", image(t, "amd64", nil, gzLayer(t, "f", "x")))
	arrived, release := r.Block("app")
	pulled := make(chan error, 1)
	go func() {
		_, err := d.Config(t.Context(), ref, digest, anon)
		pulled <- err
	}()
	<-arrived // the pull is under way
	forgot := make(chan error, 1)
	go func() { forgot <- d.Forget(t.Context(), digest) }()
	select {
	case err := <-forgot:
		t.Fatalf("Forget returned during the pull of its digest: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-pulled; err != nil {
		t.Fatal(err)
	}
	if err := <-forgot; err != nil {
		t.Fatal(err)
	}
	r.Withdraw("app")
	if _, err := d.Config(t.Context(), ref, digest, anon); err == nil {
		t.Error("a digest forgotten during its pull is still cached")
	}
}

// TestThePlainestVariantIsTaken: of amd64 images for several CPU levels,
// the one any amd64 host runs is pulled.
func TestThePlainestVariantIsTaken(t *testing.T) {
	r := registrytest.New(t)
	d := newImages(t, t.TempDir(), ocilayer.Limits{})
	for what, tt := range map[string]struct {
		variants []string
		want     string
	}{
		"none after v3":   {[]string{"v3", "", "v2"}, ""},
		"v1 after v4":     {[]string{"v4", "v1"}, "v1"},
		"v2 of v2 and v3": {[]string{"v3", "v2"}, "v2"},
	} {
		t.Run(what, func(t *testing.T) {
			var adds []mutate.IndexAddendum
			for _, v := range tt.variants {
				img := image(t, "amd64", []string{"variant " + v}, gzLayer(t, "v", v))
				adds = append(adds, mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{
					Platform: &v1.Platform{OS: "linux", Architecture: "amd64", Variant: v},
				}})
			}
			idx := mutate.AppendManifests(empty.Index, adds...)
			n, _ := name.ParseReference(r.Host + "/variants:" + strings.ReplaceAll(what, " ", "-"))
			if err := remote.WriteIndex(n, idx); err != nil {
				t.Fatal(err)
			}
			digest, _ := idx.Digest()
			cfg, err := d.Config(t.Context(), n.String(), digest.String(), anon)
			if err != nil || len(cfg.Cmd) != 1 || cfg.Cmd[0] != "variant "+tt.want {
				t.Errorf("config %+v %v, want the variant %q", cfg, err, tt.want)
			}
		})
	}
}
