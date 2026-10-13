//go:build testbed

package flow

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestEachGuestReadsItsOwnMetadata: the metadata service, at 169.254.169.254
// in every network, tells each guest its own instance id and hostname.
func TestEachGuestReadsItsOwnMetadata(t *testing.T) {
	c := startCluster(t)
	names := []string{"md-one", "md-two"}
	for _, n := range names {
		c.applyYAML(idlerYAML(n))
	}
	for _, n := range names {
		c.waitReady(n, 3*time.Minute)
	}
	for _, n := range names {
		uid := c.machine(n).GetMetadata().GetUid()
		want := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", uid, n)
		var last string
		eventually(t, 30*time.Second, n+" reading its meta-data", func() (bool, string) {
			out, errOut, code := execCLI(t, nil, "ct", n, "--", "wget", "-q", "-T", "3", "-O-", "http://169.254.169.254/nocloud/meta-data")
			last = fmt.Sprintf("exit %d: %q %s", code, out, errOut)
			return code == 0 && out == want, last
		})
	}
	out, _, code := execCLI(t, nil, "ct", names[0], "--", "wget", "-q", "-T", "3", "-O-", "http://169.254.169.254/latest/meta-data/")
	if code == 0 || strings.Contains(out, "instance-id") {
		t.Errorf("a path the service does not serve answered (exit %d): %q", code, out)
	}
}
