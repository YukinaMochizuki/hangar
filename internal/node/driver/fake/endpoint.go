package fake

import (
	"context"
	"slices"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

func (d network) EnsureEndpoints(_ context.Context, endpoints []driver.Endpoint) error {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if d.h.netErr != nil {
		return d.h.netErr
	}
	if err := driver.CheckEndpoints(endpoints); err != nil {
		return err
	}
	var set []driver.Endpoint
	for _, e := range endpoints {
		_, in := d.h.networks[e.NetworkUID]
		_, svc := d.h.networks[e.ServiceNetworkUID]
		if in && svc {
			set = append(set, e)
		}
	}
	driver.SortEndpoints(set)
	if !slices.Equal(set, d.h.endpoints) {
		d.h.endpoints = set
		d.h.endpointWrites++
	}
	return nil
}

func (d network) Endpoints(context.Context) ([]driver.Endpoint, error) { return d.h.Endpoints(), nil }

// Endpoints returns the network endpoints in force: a deleted network's go
// with its namespace, those to it included.
func (h *Host) Endpoints() []driver.Endpoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(h.endpoints), func(e driver.Endpoint) bool {
		_, in := h.networks[e.NetworkUID]
		_, svc := h.networks[e.ServiceNetworkUID]
		return !in || !svc
	})
}

// EndpointWrites counts the times the endpoints in force changed.
func (h *Host) EndpointWrites() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.endpointWrites
}
