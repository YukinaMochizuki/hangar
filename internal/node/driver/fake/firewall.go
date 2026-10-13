package fake

import (
	"cmp"
	"context"
	"fmt"
	"maps"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

func (d network) EnsureFirewall(_ context.Context, networkUID string, policies []driver.InterfacePolicy) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.netErr != nil {
		return d.h.netErr
	}
	if _, ok := d.h.networks[networkUID]; !ok {
		return fmt.Errorf("network %s has no namespace on the host", networkUID)
	}
	set := map[string]secgroup.Policy{}
	size := 0
	for _, p := range policies {
		if _, ok := d.h.interfaces[p.MachineUID][p.Name]; ok {
			set[p.MachineUID+"/"+p.Name] = p.Policy
			size += p.Policy.Size()
		}
	}
	if limit := cmp.Or(d.h.firewallLimit, driver.MaxFirewallRules); size > limit {
		return fmt.Errorf("network %s: security groups make %d rules, more than %d", networkUID, size, limit)
	}
	d.h.firewalls[networkUID] = set
	return nil
}

// Firewall returns the policies a network's interfaces hold, by machine uid
// and interface name, such as "uid/eth0".
func (h *Host) Firewall(networkUID string) map[string]secgroup.Policy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.firewalls[networkUID])
}

// LimitFirewall bounds each firewall at n rules in place of
// driver.MaxFirewallRules, so a test can pass the bound with a few; 0
// clears it.
func (h *Host) LimitFirewall(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.firewallLimit = n
}
