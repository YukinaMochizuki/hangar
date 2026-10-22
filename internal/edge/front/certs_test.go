package front_test

import (
	"context"
	"crypto/tls"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/edge/front"
	"github.com/YukinaMochizuki/hangar/internal/edge/front/acmetest"
)

// TestCertificatesNeedAnEmail: without an ACME email, certificates are
// refused before anything asks a CA.
func TestCertificatesNeedAnEmail(t *testing.T) {
	_, err := front.Certificates(context.Background(), front.CertConfig{
		Storage: t.TempDir(), DNS: &acmetest.Server{}, Names: []string{apiHost},
	})
	if err == nil || !strings.Contains(err.Error(), "email") {
		t.Fatalf("no email: %v", err)
	}
}

// TestDelegatedCertificates: a tenant's domain that delegates its DNS-01
// challenges by a CNAME into the endpoints' zone gets certificates for
// itself and its wildcard, answered there, and loses them when it is
// undelegated.
func TestDelegatedCertificates(t *testing.T) {
	c := newCA(t)
	d := startDoor(t, c, defaultOptions())
	challenge := "k3x9.acme." + userDomain
	c.dns.SetCNAME("_acme-challenge."+tenantDomain, challenge)

	if err := d.certs.Delegate(tenantDomain, challenge); err != nil {
		t.Fatal(err)
	}
	d.waitCertificates(t, tenantDomain, "app."+tenantDomain)
	for _, tt := range []struct{ name, want string }{
		{tenantDomain, tenantDomain},
		{"app." + tenantDomain, "*." + tenantDomain},
	} {
		conn, err := d.handshake(tt.name)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		leaf := conn.ConnectionState().PeerCertificates[0]
		_ = conn.Close()
		if !slices.Equal(leaf.DNSNames, []string{tt.want}) {
			t.Errorf("%s: certificate for %v; want %s alone", tt.name, leaf.DNSNames, tt.want)
		}
	}
	// The challenges were answered at the delegated name, never in the
	// tenant's zone, and nothing of them is left.
	if got := c.dns.TXT(challenge); len(got) > 0 {
		t.Errorf("%s still has %q", challenge, got)
	}
	// Renewals answer there too; the front door's own names' at their own.
	for names, want := range map[string]string{
		tenantDomain:        challenge,
		"*." + tenantDomain: challenge,
		apiHost:             "",
		"*." + userDomain:   "",
	} {
		if got := front.RenewalChallenge(d.certs, names); got != want {
			t.Errorf("a certificate for %s renews at %q; want %q", names, got, want)
		}
	}

	d.certs.Undelegate(tenantDomain)
	if got := front.RenewalChallenge(d.certs, tenantDomain); got != "" {
		t.Errorf("after the domain was undelegated, its renewal answers at %q", got)
	}
	for _, name := range []string{tenantDomain, "app." + tenantDomain} {
		if conn, err := d.handshake(name); err == nil {
			_ = conn.Close()
			t.Errorf("%s: a handshake after the domain was undelegated", name)
		}
	}
}

// TestCertificatesOverDNS01: the front door gets its certificates from an
// ACME CA through DNS-01 alone, the control plane's names and the
// endpoints' wildcard, hands out none for any other name, keeps them only
// for itself, and a restart serves them from storage without the CA.
func TestCertificatesOverDNS01(t *testing.T) {
	c := newCA(t)
	storage := filepath.Join(t.TempDir(), "certs")
	o := defaultOptions()
	o.storage = storage
	d := startDoor(t, c, o)

	for _, tt := range []struct{ name, want string }{
		{apiHost, apiHost},
		{consoleHost, consoleHost},
		{webHost, "*." + userDomain},
		{"other-demo." + userDomain, "*." + userDomain},
	} {
		conn, err := d.handshake(tt.name)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		leaf := conn.ConnectionState().PeerCertificates[0]
		_ = conn.Close()
		if !slices.Equal(leaf.DNSNames, []string{tt.want}) {
			t.Errorf("%s: certificate for %v; want %s alone", tt.name, leaf.DNSNames, tt.want)
		}
	}
	for _, name := range []string{"", "other." + cpDomain, "a.b." + userDomain, "example.com"} {
		cfg := d.tlsConfig(name)
		cfg.InsecureSkipVerify = true // whatever it would hand out, it must not
		conn, err := tls.Dial("tcp", d.https, cfg)
		if err == nil {
			t.Errorf("server name %q: a handshake with %v", name, conn.ConnectionState().PeerCertificates[0].DNSNames)
			_ = conn.Close()
		}
	}
	// Nothing of the challenges is left in the zones.
	for _, n := range []string{"_acme-challenge." + apiHost, "_acme-challenge." + userDomain} {
		if got := c.dns.TXT(n); len(got) > 0 {
			t.Errorf("%s still has %q", n, got)
		}
	}
	// The storage is the edge's alone: directories 0700, files 0600.
	var files int
	err := filepath.WalkDir(storage, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o600)
		if e.IsDir() {
			want = 0o700
		} else {
			files++
		}
		if info.Mode().Perm() != want && !strings.HasSuffix(p, ".lock") {
			t.Errorf("%s is %v; want %v", p, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil || files == 0 {
		t.Fatalf("storage: %d files, %v", files, err)
	}

	// Another front door on the same storage serves at once what the
	// first got, and asks the CA for nothing.
	before := c.dns.Appended()
	again := startDoor(t, c, o)
	conn, err := again.handshake(apiHost)
	if err != nil {
		t.Fatalf("after a restart: %v", err)
	}
	_ = conn.Close()
	if got := c.dns.Appended(); got != before {
		t.Errorf("after a restart, %d more challenges; want none", got-before)
	}
}
