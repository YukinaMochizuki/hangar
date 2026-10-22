package fake

import (
	"cmp"
	"context"
	"errors"
	"net/netip"
	"slices"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// SetSharedIPv4 gives the host a shared IPv4 address, without which it
// refuses every forward; an invalid one takes it away.
func (h *Host) SetSharedIPv4(a netip.Addr) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sharedIPv4 = a
}

func (d network) EnsureForwards(_ context.Context, forwards []driver.Forward) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.netErr != nil {
		return d.h.netErr
	}
	if len(forwards) > 0 && !d.h.sharedIPv4.IsValid() {
		return errors.New("the host has no shared IPv4 address to forward ports of")
	}
	if err := driver.CheckForwards(forwards); err != nil {
		return err
	}
	var set []driver.Forward
	for _, f := range forwards {
		if _, ok := d.h.networks[f.NetworkUID]; !ok {
			continue
		}
		set = append(set, f)
	}
	slices.SortFunc(set, compareForwards)
	if !slices.Equal(set, d.h.forwards) {
		d.h.forwards = set
		d.h.forwardWrites++
	}
	return nil
}

func (d network) Forwards(context.Context) ([]driver.Forward, error) { return d.h.Forwards(), nil }

func compareForwards(a, b driver.Forward) int {
	return cmp.Or(cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Port, b.Port))
}

// Forwards returns the forwards in force, by protocol and port: a deleted
// network's go with its namespace.
func (h *Host) Forwards() []driver.Forward {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(h.forwards), func(f driver.Forward) bool {
		_, ok := h.networks[f.NetworkUID]
		return !ok
	})
}

// ForwardWrites counts the times the forwards in force changed.
func (h *Host) ForwardWrites() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.forwardWrites
}
