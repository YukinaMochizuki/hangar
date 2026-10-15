package driver_test

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestRepository: references naming one repository, however they spell it,
// are one source to the cache; others are not.
func TestRepository(t *testing.T) {
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	same := []string{
		"x", "x:1", "x@" + digest, "library/x", "docker.io/library/x", "index.docker.io/library/x:2",
		"docker.io/x", "index.docker.io/library/x@" + digest, "registry-1.docker.io/library/x",
	}
	want := "index.docker.io/library/x"
	for _, ref := range same {
		if got, err := driver.Repository(ref); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", ref, got, err, want)
		}
	}
	others := map[string]string{
		"ghcr.io/acme/x:1":                  "ghcr.io/acme/x",
		"evil.example/acme/x@" + digest:     "evil.example/acme/x",
		"docker.io/acme/x":                  "index.docker.io/acme/x",
		"127.0.0.1:5000/acme/x@sha256:1":    "127.0.0.1:5000/acme/x",
		"registry.example/library/x:latest": "registry.example/library/x",
	}
	for ref, want := range others {
		if got, err := driver.Repository(ref); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", ref, got, err, want)
		}
	}
	if _, err := driver.Repository("Not A Reference"); err == nil {
		t.Error("a malformed reference has a repository")
	}
}
