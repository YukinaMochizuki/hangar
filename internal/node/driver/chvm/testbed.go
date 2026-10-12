//go:build testbed

package chvm

// Testbed builds only: what the real-host tests need to reach inside the
// driver. hangar-node is never built with the testbed tag.

// BreakSystemd closes the driver's connection to systemd, as a D-Bus
// restart does; the driver connects again on its own.
func (h *Hypervisor) BreakSystemd() {
	if s, ok := h.sd.(*dbusSystemd); ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.conn != nil {
			s.conn.Close()
		}
	}
}

// SystemdConnects counts the connections to systemd the driver has made.
func (h *Hypervisor) SystemdConnects() int {
	if s, ok := h.sd.(*dbusSystemd); ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.connects
	}
	return 0
}
