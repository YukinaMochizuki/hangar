//go:build testbed

package disks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry/registrytest"
)

// Images from Docker Hub, by tag and by the digest the tag named when it
// was pinned; a tag that moved since is pulled as it is now, and said so.
var hubImages = []struct{ repo, tag, digest string }{
	{"docker.io/library/nginx", "1.29", "sha256:1881968aff6f7cdcc4b888c00a11f4ce241ad7ec957e0cb4a9e19e93a3ff87ea"},
	{"docker.io/library/debian", "trixie", "sha256:913f6706df59a68922d1dd08f78c2476560a8d367897200a6005b00e5f67c2d5"},
}

// deadRegistry reaches nothing, as a registry that is down.
var deadRegistry = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:1")
}}

// TestDockerHubImages: real images, by tag and digest, unpack as crane export
// flattens them, make bases and roots that grow and delete in every allowed
// order, and need the registry no more once pulled.
func TestDockerHubImages(t *testing.T) {
	h := newHost(t)
	images, s := h.images(t), h.storage(t)
	ctx := context.Background()
	h.listing(t, "before")

	type pulled struct{ ref, digest string }
	var all []pulled
	for _, img := range hubImages {
		tagged := img.repo + ":" + img.tag
		start := time.Now()
		digest, err := images.Resolve(ctx, tagged, anon)
		if err != nil {
			t.Fatalf("resolving %s: %v", tagged, err)
		}
		t.Logf("%s resolved to %s in %v", tagged, digest, time.Since(start))
		if digest != img.digest {
			t.Logf("%s moved from the pinned %s", tagged, img.digest)
		}
		pinned := img.repo + "@" + img.digest
		if got, err := images.Resolve(ctx, pinned, anon); err != nil || got != img.digest {
			t.Errorf("resolving %s: %s %v", pinned, got, err)
		}
		all = append(all, pulled{tagged, digest})
	}

	// By digest, into a cache of its own, so it is pulled again.
	byDigest, err := registry.New(registry.Config{Cache: h.cache + "-by-digest"})
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range hubImages {
		pinned := img.repo + "@" + img.digest
		dir, err := os.MkdirTemp(filepath.Dir(h.cache), "pinned-")
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := byDigest.Unpack(ctx, pinned, img.digest, dir, anon); err != nil {
			t.Fatalf("unpacking %s: %v", pinned, err)
		}
		t.Logf("%s: pulled and unpacked in %v", pinned, time.Since(start))
		matchesHub(t, pinned, dir)
		_ = os.RemoveAll(dir)
	}

	for _, p := range all {
		start := time.Now()
		cfg, err := images.Config(ctx, p.ref, p.digest, anon)
		if err != nil {
			t.Fatalf("config of %s: %v", p.ref, err)
		}
		t.Logf("%s: pulled in %v; entrypoint %q cmd %q", p.ref, time.Since(start), cfg.Entrypoint, cfg.Cmd)

		// Unpacked as the reference flattening has it.
		dir, err := os.MkdirTemp(filepath.Dir(h.cache), "ref-")
		if err != nil {
			t.Fatal(err)
		}
		start = time.Now()
		if err := images.Unpack(ctx, p.ref, p.digest, dir, anon); err != nil {
			t.Fatalf("unpacking %s: %v", p.ref, err)
		}
		t.Logf("%s: unpacked in %v", p.ref, time.Since(start))
		matchesHub(t, p.ref[:strings.LastIndex(p.ref, ":")]+"@"+p.digest, dir)
		_ = os.RemoveAll(dir)

		start = time.Now()
		if err := s.EnsureBase(ctx, p.digest, 0, func(dir string) error { return images.Unpack(ctx, p.ref, p.digest, dir, anon) }); err != nil {
			t.Fatalf("the base of %s: %v", p.ref, err)
		}
		t.Logf("%s: base made in %v (unpack, mkfs.ext4 -d, snapshot)", p.ref, time.Since(start))
		for i := range 3 {
			start = time.Now()
			root, err := s.EnsureRoot(ctx, fmt.Sprintf("%s-%d", shortName(p.ref), i), p.digest, 10<<30)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s cloned in %v, %d bytes", root.Path, time.Since(start), root.Size)
		}
	}
	h.listing(t, "with bases and roots")

	// Grown, and listed so.
	first := shortName(all[0].ref) + "-0"
	grown, err := s.EnsureRoot(ctx, first, all[0].digest, 20<<30)
	if err != nil || grown.Size != 20<<30 {
		t.Fatalf("grown: %+v %v", grown, err)
	}
	if out := mustRun(t, "zfs", "get", "-Hp", "-o", "value", "volsize", h.dataset+"/roots/"+first); strings.TrimSpace(out) != fmt.Sprint(20<<30) {
		t.Errorf("volsize after growing: %s", out)
	}

	// Once pulled, a digest needs no registry until it is forgotten.
	dead, err := registry.New(registry.Config{Cache: h.cache, Transport: deadRegistry})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if _, err := dead.Config(ctx, p.ref, p.digest, anon); err != nil {
			t.Errorf("config of %s with the registry dead: %v", p.ref, err)
		}
		dir, _ := os.MkdirTemp(filepath.Dir(h.cache), "dead-")
		if err := dead.Unpack(ctx, p.ref, p.digest, dir, anon); err != nil {
			t.Errorf("unpacking %s with the registry dead: %v", p.ref, err)
		}
		_ = os.RemoveAll(dir)
	}
	if _, err := dead.Resolve(ctx, all[0].ref, anon); !errors.Is(err, driver.ErrRegistryUnreachable) {
		t.Errorf("resolving with the registry dead: %v", err)
	}

	// Deleted in every order the contract allows: a base stays while any
	// root of it does, a root while it is open.
	for _, p := range all {
		base := p.digest
		prefix := shortName(p.ref)
		if err := s.DeleteBase(ctx, base); err == nil {
			t.Errorf("deleted the base of %s with three roots", p.ref)
		}
		mustDelete := func(uid string) {
			if err := s.DeleteRoot(ctx, uid); err != nil {
				t.Errorf("deleting %s: %v", uid, err)
			}
		}
		mustDelete(prefix + "-1")
		if err := s.DeleteBase(ctx, base); err == nil {
			t.Errorf("deleted the base of %s with two roots", p.ref)
		}
		open, err := os.OpenFile("/dev/zvol/"+h.dataset+"/roots/"+prefix+"-2", os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteRoot(ctx, prefix+"-2"); err == nil {
			t.Errorf("deleted %s-2 while open", prefix)
		}
		_ = open.Close()
		mustDelete(prefix + "-2")
		mustDelete(prefix + "-0")
		mustDelete(prefix + "-0")
		if err := s.DeleteBase(ctx, base); err != nil {
			t.Errorf("deleting the base of %s with no roots: %v", p.ref, err)
		}
		if err := images.Forget(ctx, base); err != nil {
			t.Error(err)
		}
		if _, err := dead.Config(ctx, p.ref, p.digest, anon); err == nil {
			t.Errorf("config of forgotten %s with the registry dead", p.ref)
		}
	}
	h.listing(t, "after")
	if blobs, _ := os.ReadDir(filepath.Join(h.cache, "blobs", "sha256")); len(blobs) != 0 {
		t.Errorf("blobs left in the cache: %d", len(blobs))
	}
}

// matchesHub compares dir with the reference flattening of the image pinned
// names, fetched from its registry for the host's platform.
func matchesHub(t *testing.T, pinned, dir string) {
	t.Helper()
	n, err := name.ParseReference(pinned)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := remote.Image(n, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d entries match the reference flattening", pinned, registrytest.MatchesExtract(t, ref, dir))
}

// shortName names a machine after an image reference: nginx-1.29, or
// debian-913f6706 when by digest.
func shortName(ref string) string {
	last := ref[strings.LastIndex(ref, "/")+1:]
	if repo, d, ok := strings.Cut(last, "@sha256:"); ok {
		return repo + "-" + d[:8]
	}
	return strings.ReplaceAll(strings.ReplaceAll(last, ":", "-"), ".", "_")
}
