//go:build testbed

package flow

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestAGuestAskingDHCPGetsItsAddress: a guest asking its network's DHCP from
// no address, by broadcast, as a cloud image does at boot, is offered the
// address the apiserver assigned its interface.
func TestAGuestAskingDHCPGetsItsAddress(t *testing.T) {
	c := startCluster(t)
	c.applyYAML(idlerYAML("asker"))
	c.waitReady("asker", 3*time.Minute)
	want, _, _ := strings.Cut(c.machine("asker").GetSpec().GetInterfaces()[0].GetIpv4(), "/")
	var last string
	eventually(t, 30*time.Second, "asker leasing "+want, func() (bool, string) {
		// -s /bin/true: report the lease, leave the interface as it is.
		out, errOut, code := execCLI(t, nil, "ct", "asker", "--", "udhcpc", "-i", "eth0", "-n", "-q", "-f", "-s", "/bin/true", "-t", "3", "-T", "1")
		last = fmt.Sprintf("exit %d: %s%s", code, out, errOut)
		return code == 0 && strings.Contains(out+errOut, "lease of "+want+" obtained"), last
	})
}
