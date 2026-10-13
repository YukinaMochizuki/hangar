//go:build testbed

package diskimgtest

import (
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// An Ubuntu cloud image, as Canonical publishes it, with the sha256 its
// SHA256SUMS gives.
const (
	UbuntuName   = "ubuntu-24.04-server-cloudimg-amd64-20260926.img"
	ubuntuURL    = "https://cloud-images.ubuntu.com/releases/noble/release-20260926/ubuntu-24.04-server-cloudimg-amd64.img"
	UbuntuSHA256 = "6a81c37564db9b1ee84e141922625e1d7c5b389b99bb3c572e0243607d5bb4d2"
)

// Ubuntu is the Ubuntu cloud image, cached on the testbed.
func Ubuntu(t *testing.T) string {
	t.Helper()
	return Cached(t, UbuntuName, ubuntuURL, UbuntuSHA256)
}

// Firmware is the firmware VMs boot through: HANGAR_TESTBED_FIRMWARE, or
// the one deploy/host/versions.env pins, cached on the testbed.
func Firmware(t *testing.T) string {
	t.Helper()
	if fw := os.Getenv("HANGAR_TESTBED_FIRMWARE"); fw != "" {
		return fw
	}
	_, self, _, _ := runtime.Caller(0)
	env := filepath.Join(filepath.Dir(self), "..", "..", "..", "..", "deploy", "host", "versions.env")
	out, err := exec.Command("bash", "-c", `. "$1" && echo "$HANGAR_FW_VERSION $HANGAR_FW_URL $HANGAR_FW_SHA256"`, "-", env).Output()
	if err != nil {
		t.Fatalf("reading %s: %v", env, err)
	}
	f := strings.Fields(string(out))
	if len(f) != 3 {
		t.Fatalf("%s pins no firmware: %q", env, out)
	}
	return Cached(t, "CLOUDHV-"+f[0]+".fd", f[1], f[2])
}

// CertFile writes the server's certificate, PEM, to a file and returns its
// path: a process given it as SSL_CERT_FILE trusts the server, and still
// the host's own authorities, from /etc/ssl/certs.
func (s *Server) CertFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "diskimgtest.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
