package sshca_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/YukinaMochizuki/hangar/internal/sshca"
)

func newKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(block)
}

// TestACertificateIsForTheVMForMinutes: a key is certified for the one
// principal, as a user, for Validity from a minute back, naming the caller,
// without agent forwarding; what checks it against the CA takes it.
func TestACertificateIsForTheVMForMinutes(t *testing.T) {
	caSigner, _ := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	ca := sshca.New(caSigner, func() time.Time { return now })
	user, _ := newKey(t)
	cert, err := ca.Sign(string(ssh.MarshalAuthorizedKey(user.PublicKey())), "vm-uid-1", "user:alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if cert.CertType != ssh.UserCert || len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != "vm-uid-1" ||
		cert.KeyId != "user:alice@example.com" {
		t.Errorf("certificate %+v", cert)
	}
	if got := time.Unix(int64(cert.ValidBefore), 0).Sub(now); got != 5*time.Minute || sshca.Validity != 5*time.Minute {
		t.Errorf("valid for %v after signing", got)
	}
	if got := now.Sub(time.Unix(int64(cert.ValidAfter), 0)); got != time.Minute {
		t.Errorf("valid from %v before signing, want a minute, for a guest's clock behind", got)
	}
	again, err := ca.Sign(string(ssh.MarshalAuthorizedKey(user.PublicKey())), "vm-uid-1", "user:alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if cert.Serial == again.Serial || cert.Serial == 0 {
		t.Errorf("serials %d and %d: each certificate wants its own", cert.Serial, again.Serial)
	}
	if _, ok := cert.Extensions["permit-agent-forwarding"]; ok {
		t.Error("the certificate permits agent forwarding")
	}
	checker := ssh.CertChecker{
		IsUserAuthority: func(k ssh.PublicKey) bool { return string(k.Marshal()) == string(caSigner.PublicKey().Marshal()) },
		Clock:           func() time.Time { return now },
	}
	if err := checker.CheckCert("vm-uid-1", cert); err != nil {
		t.Errorf("a guest trusting the CA refuses it: %v", err)
	}
	if err := checker.CheckCert("vm-uid-2", cert); err == nil {
		t.Error("the certificate passes for another VM")
	}
	checker.Clock = func() time.Time { return now.Add(6 * time.Minute) }
	if err := checker.CheckCert("vm-uid-1", cert); err == nil {
		t.Error("the certificate passes after its minutes")
	}
	if !strings.HasPrefix(ca.PublicKey(), "ssh-ed25519 ") || !strings.HasSuffix(ca.PublicKey(), " hangar-ssh-ca") {
		t.Errorf("public key %q", ca.PublicKey())
	}

	for name, key := range map[string]string{
		"nothing":       "",
		"garbage":       "not a key",
		"two keys":      string(ssh.MarshalAuthorizedKey(user.PublicKey())) + string(ssh.MarshalAuthorizedKey(user.PublicKey())),
		"a certificate": string(ssh.MarshalAuthorizedKey(cert)),
	} {
		if _, err := ca.Sign(key, "vm-uid-1", "user:alice@example.com"); err == nil {
			t.Errorf("signed %s", name)
		}
	}
	var none *sshca.CA
	if _, err := none.Sign(string(ssh.MarshalAuthorizedKey(user.PublicKey())), "vm-uid-1", "x"); !errors.Is(err, sshca.ErrNoCA) {
		t.Errorf("no CA signs: %v", err)
	}
	if none.PublicKey() != "" {
		t.Error("no CA has a public key")
	}
}

// TestLoad: the key file is loaded only when its owner alone reads it; a
// missing one is told apart, and a bad one's error holds nothing of it.
func TestLoad(t *testing.T) {
	dir := t.TempDir()
	_, pemKey := newKey(t)
	path := filepath.Join(dir, "ssh-ca")
	if err := os.WriteFile(path, pemKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if ca, err := sshca.Load(path); err != nil || ca.PublicKey() == "" {
		t.Fatalf("loading the key: %v", err)
	}
	if _, err := sshca.Load(filepath.Join(dir, "none")); !sshca.IsMissing(err) {
		t.Errorf("a missing file: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := sshca.Load(path); err == nil || sshca.IsMissing(err) {
		t.Errorf("a key its group reads: %v", err)
	}
	bad := filepath.Join(dir, "bad")
	secret := "-----BEGIN OPENSSH PRIVATE KEY-----\nc2VjcmV0LXNlY3JldA==\n-----END OPENSSH PRIVATE KEY-----\n"
	if err := os.WriteFile(bad, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sshca.Load(bad); err == nil || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Errorf("a key that is none: %v", err)
	}
}
