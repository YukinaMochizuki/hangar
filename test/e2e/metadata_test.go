package e2e

import (
	"fmt"
	"net/http"
	"net/netip"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestEachNetworkHasItsMetadataService: a network on the host has its
// metadata service listening, and it tells nothing to an address that is
// no machine's. Which machine is which is the L4 flow tests': here every
// request comes from the loopback.
func TestEachNetworkHasItsMetadataService(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	var status string
	w.eventually("the metadata service refusing a stranger", func() (bool, string) {
		at, ok := w.host.ServiceAddr(w.networkUID("default"), "tcp", netip.AddrPortFrom(driver.MetadataAddr, 80))
		if !ok {
			return false, "nothing listens"
		}
		res, err := http.Get("http://" + at.String() + "/nocloud/meta-data")
		if err != nil {
			return false, err.Error()
		}
		_ = res.Body.Close()
		status = res.Status
		return res.StatusCode == http.StatusNotFound, fmt.Sprint(status)
	})
	w.delete("api")
	w.waitGone("api")
	w.waitHostEmpty()
}
