package front_test

import (
	"crypto/x509"
	"io"
	"log"
	"net/http/httptest"
	"testing"

	"github.com/letsencrypt/pebble/v2/ca"
	"github.com/letsencrypt/pebble/v2/db"
	"github.com/letsencrypt/pebble/v2/va"
	"github.com/letsencrypt/pebble/v2/wfe"
)

// pebble is Let's Encrypt's test CA, in this process: an ACME directory
// that validates DNS-01 challenges against resolver, and the roots its
// directory and the certificates it issues chain to.
type pebble struct {
	// Directory is the ACME directory URL.
	Directory string
	// DirectoryRoots are what the directory's own TLS certificate chains
	// to.
	DirectoryRoots *x509.CertPool
	// Roots are what the certificates it issues chain to.
	Roots *x509.CertPool
}

// startPebble runs pebble until the test ends. It reads its settings from
// the environment, so a test that starts one runs alone.
func startPebble(t *testing.T, resolver string) *pebble {
	t.Helper()
	t.Setenv("PEBBLE_VA_NOSLEEP", "1")
	t.Setenv("PEBBLE_WFE_NONCEREJECT", "0")
	t.Setenv("PEBBLE_AUTHZREUSE", "0")
	logger := log.New(io.Discard, "", 0)
	store := db.NewMemoryStore()
	authority := ca.New(logger, store, "", "ecdsa", 0, 1, map[string]ca.Profile{"default": {Description: "test"}})
	validator := va.New(logger, 0, 0, false, resolver, store)
	front := wfe.New(logger, store, validator, authority, []string{"pebble.letsencrypt.org"}, false, false, 1, 1)
	srv := httptest.NewTLSServer(front.Handler())
	t.Cleanup(srv.Close)

	p := &pebble{Directory: srv.URL + wfe.DirectoryPath, DirectoryRoots: x509.NewCertPool(), Roots: x509.NewCertPool()}
	p.DirectoryRoots.AddCert(srv.Certificate())
	if !p.Roots.AppendCertsFromPEM(authority.GetRootCert(0).PEM()) {
		t.Fatal("pebble's root does not parse")
	}
	return p
}
