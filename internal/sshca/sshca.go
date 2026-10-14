// Package sshca is the platform's SSH certificate authority: it signs a
// caller's key for one VM, for minutes. Its private key is a file the
// apiserver alone reads, and never leaves it. See docs/decisions/vm-ssh.md.
package sshca

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Validity is how long a certificate lasts: long enough to log in with,
// which is all it is for; a session outlives it.
const Validity = 5 * time.Minute

// clockSkew backdates a certificate, for a guest whose clock is behind.
const clockSkew = time.Minute

// ErrNoCA is what signing says when the apiserver has no CA.
var ErrNoCA = errors.New("the platform has no SSH CA: its key file is not installed (docs/runbook/host-bootstrap.md)")

// CA signs certificates. A nil CA has no key, and signs nothing.
type CA struct {
	signer ssh.Signer
	now    func() time.Time
}

// Load reads the CA's private key, an unencrypted OpenSSH key, from path.
// A missing file is no CA, nil and fs.ErrNotExist; a file others may read
// or write is refused.
func Load(path string) (*CA, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %o: only its owner may read the SSH CA's key", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		// Never the key itself, nor what parsing it saw of it.
		return nil, fmt.Errorf("%s is not an unencrypted OpenSSH private key", path)
	}
	return &CA{signer: signer, now: time.Now}, nil
}

// IsMissing says whether Load failed for want of the file.
func IsMissing(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// New is a CA of signer, for tests.
func New(signer ssh.Signer, now func() time.Time) *CA {
	return &CA{signer: signer, now: now}
}

// PublicKey is the CA's public key as an authorized_keys line, which guests
// trust; "" for no CA.
func (c *CA) PublicKey() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(c.signer.PublicKey()))) + " hangar-ssh-ca"
}

// Sign certifies key, a user's public key in authorized_keys form, for
// principal alone, for Validity, naming keyID, the caller, in it.
func (c *CA) Sign(key, principal, keyID string) (*ssh.Certificate, error) {
	if c == nil {
		return nil, ErrNoCA
	}
	pub, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(key))
	if err != nil || len(strings.TrimSpace(string(rest))) > 0 {
		return nil, fmt.Errorf("not one public key in authorized_keys form")
	}
	if _, ok := pub.(*ssh.Certificate); ok {
		return nil, errors.New("a certificate, not a public key")
	}
	var serial [8]byte
	if _, err := rand.Read(serial[:]); err != nil {
		return nil, err
	}
	now := c.now()
	cert := &ssh.Certificate{
		// Random, so each certificate is told apart, as in a revocation
		// list or sshd's log.
		Serial:          binary.BigEndian.Uint64(serial[:]),
		Key:             pub,
		CertType:        ssh.UserCert,
		KeyId:           keyID,
		ValidPrincipals: []string{principal},
		ValidAfter:      uint64(now.Add(-clockSkew).Unix()),
		ValidBefore:     uint64(now.Add(Validity).Unix()),
		// What an OpenSSH user certificate gets by default, but agent
		// forwarding: a session on the VM need not reach back.
		Permissions: ssh.Permissions{Extensions: map[string]string{
			"permit-pty": "", "permit-port-forwarding": "", "permit-user-rc": "", "permit-X11-forwarding": "",
		}},
	}
	if err := cert.SignCert(rand.Reader, c.signer); err != nil {
		return nil, err
	}
	return cert, nil
}
