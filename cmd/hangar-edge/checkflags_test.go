package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckFlagsRunsCheckFlagsAlone: check-flags exits 1 with checkFlags'
// reason for flags the edge would refuse, 0 for flags it would start with,
// and 2 for flags it cannot parse or words that are not flags. It refuses
// every check of the flags alone the edge makes as it starts (nothing to
// serve, a --listen off loopback, a console that is no https origin), and
// starts nothing, so an address already taken and a state directory that
// is not there are left as they were.
func TestCheckFlagsRunsCheckFlagsAlone(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	storage := filepath.Join(t.TempDir(), "not-made")
	door := []string{"--acme-email", "ops@example.com", "--cert-storage", storage,
		"--listen", taken.Addr().String(), "--https-listen", taken.Addr().String(), "--http-listen", taken.Addr().String()}
	for _, tt := range []struct {
		args []string
		code int
		says string
	}{
		{append([]string{"--cp-domain", "hangarlab.cloud", "--domain", "hangarlab.dev"}, door...), 0, ""},
		{append([]string{"--cp-domain", "cp.example.com", "--domain", "user.example.com"}, door...), 1, "must not share a registrable domain"},
		{append([]string{"--cp-domain", "example.com", "--domain", "example.com"}, door...), 1, "must not share a registrable domain"},
		{[]string{"--cp-domain", "hangarlab.cloud", "--domain", "hangarlab.dev"}, 1, "needs --acme-email"},
		{[]string{"--domain", "hangarlab.dev", "--listen", taken.Addr().String(), "--console-url", "https://console.example.com"}, 0, ""},
		{[]string{"--domain", "hangarlab.dev", "--console-url", "https://console.example.com"}, 1, "nothing to serve"},
		{[]string{"--domain", "hangarlab.dev", "--listen", "0.0.0.0:8090", "--console-url", "https://console.example.com"}, 1, "is not a loopback address"},
		{[]string{"--domain", "hangarlab.dev", "--listen", taken.Addr().String()}, 1, "private endpoints need --console-url"},
		{append([]string{"--cp-domain", "hangarlab.cloud", "--domain", "hangarlab.dev", "--console-url", "http://console.hangarlab.cloud"}, door...), 1, "is not an https origin"},
		{[]string{"--no-such-flag"}, 2, "flag provided but not defined"},
		{[]string{"--domain", "hangarlab.dev", "extra"}, 2, "takes flags alone"},
	} {
		var stderr strings.Builder
		code := checkFlagsMain(tt.args, &stderr)
		if code != tt.code || (tt.says == "") != (stderr.Len() == 0) || !strings.Contains(stderr.String(), tt.says) {
			t.Errorf("check-flags %q: exit %d, %q; want exit %d, %q", tt.args, code, stderr.String(), tt.code, tt.says)
		}
	}
	if _, err := os.Stat(storage); !os.IsNotExist(err) {
		t.Errorf("check-flags made the state directory: %v", err)
	}
}
