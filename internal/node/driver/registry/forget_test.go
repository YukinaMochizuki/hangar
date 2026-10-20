package registry_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/ocilayer"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
)

// TestForgetRefusesAPullOfItsDigest: a digest being pulled is not
// forgotten, nor waited for: Forget says it is busy at once, and once the
// pull is done forgets it.
func TestForgetRefusesAPullOfItsDigest(t *testing.T) {
	r := registrytest.New(t)
	d := newImages(t, t.TempDir(), ocilayer.Limits{})
	ref, digest := r.Push(t, "app:1", image(t, "amd64", nil, gzLayer(t, "f", "x")))
	arrived, release := r.Block("app")
	defer release()
	pulled := make(chan error, 1)
	go func() {
		_, err := d.Config(t.Context(), ref, digest, anon)
		pulled <- err
	}()
	<-arrived // the pull is under way
	if err := d.Forget(t.Context(), digest); !errors.Is(err, driver.ErrBusy) {
		t.Errorf("forgetting a digest being pulled: %v, want busy", err)
	}
	release()
	if err := <-pulled; err != nil {
		t.Fatal(err)
	}
	if err := d.Forget(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	r.Withdraw("app")
	if _, err := d.Config(t.Context(), ref, digest, anon); err == nil {
		t.Error("a digest forgotten during its pull is still cached")
	}
}

// TestTheCacheSaysWhatIsBeingPulled: a cached digest being fetched again,
// from another repository, is listed busy until the fetch is done.
func TestTheCacheSaysWhatIsBeingPulled(t *testing.T) {
	r := registrytest.New(t)
	d := newImages(t, t.TempDir(), ocilayer.Limits{})
	ref, digest := r.Push(t, "app:1", image(t, "amd64", nil, gzLayer(t, "f", "x")))
	if _, err := d.Config(t.Context(), ref, digest, anon); err != nil {
		t.Fatal(err)
	}
	if c, _ := cachedOf(t, d, digest); c.Busy {
		t.Error("a digest no pull holds is listed busy")
	}
	mirror := r.Mirror(t, "mirror:1", digest)
	arrived, release := r.Block("mirror")
	defer release()
	pulled := make(chan error, 1)
	go func() {
		_, err := d.Config(t.Context(), mirror, digest, anon)
		pulled <- err
	}()
	<-arrived
	if c, _ := cachedOf(t, d, digest); !c.Busy {
		t.Error("a digest being fetched again is not listed busy")
	}
	release()
	if err := <-pulled; err != nil {
		t.Fatal(err)
	}
	if c, _ := cachedOf(t, d, digest); c.Busy {
		t.Error("a digest fetched is listed busy")
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
