package apiserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	billingv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1"
	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// DefaultMeterInterval is how often the meter looks for a minute it has not
// read yet. It is shorter than a minute, so each minute is read within that
// of its start, once.
const DefaultMeterInterval = 20 * time.Second

// meterRead is the minute the meter last read each tenant in.
type meterRead struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// first reports whether minute is one the meter has not read tenant in yet,
// and takes it if so.
func (r *meterRead) first(tenant string, minute time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = map[string]time.Time{}
	}
	if r.last[tenant].Equal(minute) {
		return false
	}
	r.last[tenant] = minute
	return true
}

// meteredDims are the quota dimensions billed, by the item the meter
// records and a PriceList prices; the rest, such as machines and projects,
// are not billed. cpu is in millicores, memory in MiB and disk in GiB.
var meteredDims = []struct {
	d    dim
	item string
}{
	{dimCPU, "cpu"}, {dimMemory, "memory"}, {dimDisk, "disk"}, {dimAddresses, "addresses"},
	{dimPublicEndpoints, "public_endpoints"}, {dimPortForwards, "port_forwards"},
	{dimFileShares, "file_shares"}, {dimNetworkEndpoints, "network_endpoints"},
}

// runMeter records, every MeterInterval, what each tenant with a
// BillingAccount holds in the minute it is (docs/decisions/billing.md).
func (s *Server) runMeter(ctx context.Context) error {
	ticker := s.clock.NewTicker(s.cfg.MeterInterval)
	defer ticker.Stop()
	for {
		if err := s.meterOnce(ctx, s.clock.Now()); err != nil && ctx.Err() == nil {
			s.log.Warn("metering billed tenants", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.Chan():
		}
	}
}

// meterOnce records what each billed tenant holds now as what it held in
// the minute now is in, unless the minute was read already. A tenant whose
// account has not reached its anchor, or is being deleted, is not metered:
// its last period is being billed.
func (s *Server) meterOnce(ctx context.Context, now time.Time) error {
	minute := now.Truncate(time.Minute)
	accounts, err := s.listWith(ctx, registry.BillingAccount, "", nil)
	if err != nil {
		return err
	}
	var errs []error
	for _, obj := range accounts {
		a := obj.(*billingv1alpha1.BillingAccount)
		if a.GetMetadata().GetDeleteTime() != nil || now.Before(a.GetSpec().GetAnchorTime().AsTime()) {
			continue
		}
		tenant := a.GetMetadata().GetName()
		if !s.meterRead.first(tenant, minute) {
			continue
		}
		readings, err := s.meterReadings(ctx, tenant)
		if err == nil {
			err = s.cfg.Meter.RecordMeter(ctx, tenant, minute, readings)
		}
		if err != nil {
			// Read again at the next tick.
			s.meterRead.first(tenant, time.Time{})
			errs = append(errs, fmt.Errorf("tenant %s: %w", tenant, err))
		}
	}
	return errors.Join(errs...)
}

// meterReadings is what tenant holds now, object by object. Each object
// counts as the quota counts it, but for a Container's or a
// VirtualMachine's cpu and memory, which are its Machine's, and only while
// its node reports it running.
func (s *Server) meterReadings(ctx context.Context, tenant string) ([]store.MeterReading, error) {
	projects, err := s.listWith(ctx, registry.Project, "", nil)
	if err != nil {
		return nil, err
	}
	var out []store.MeterReading
	for _, po := range projects {
		p := po.(*tenancyv1alpha1.Project)
		if p.GetSpec().GetTenant() != tenant {
			continue
		}
		project := p.GetMetadata().GetName()
		in := projectObjects{}
		for kind := range quotaCounters {
			if in[kind], err = s.listWith(ctx, s.backends[kind].r, project, nil); err != nil {
				return nil, err
			}
		}
		running, err := s.runningShapes(ctx, project)
		if err != nil {
			return nil, err
		}
		for kind, count := range quotaCounters {
			for _, obj := range in[kind] {
				a := count(obj, in)
				meta := object.Meta(obj)
				if _, ok := machineKinds[kind]; ok {
					a[dimCPU], a[dimMemory] = 0, 0
					if shape, ok := running[meta.GetUid()]; ok {
						a[dimCPU], a[dimMemory] = shape[dimCPU], shape[dimMemory]
					}
				}
				out = append(out, readingsOf(kind, project, meta.GetName(), meta.GetUid(), a)...)
			}
		}
	}
	domains, err := s.listWith(ctx, registry.CustomDomain, "", nil)
	if err != nil {
		return nil, err
	}
	for _, obj := range domains {
		d := obj.(*networkv1alpha1.CustomDomain)
		if d.GetSpec().GetTenant() == tenant {
			out = append(out, store.MeterReading{UID: d.GetMetadata().GetUid(), Item: "domains", Quantity: 1,
				Kind: registry.CustomDomain.Kind, Name: d.GetMetadata().GetName()})
		}
	}
	return out, nil
}

// runningShapes are the cpu and memory of each machine running in project
// for a Container or a VirtualMachine, by the uid of the object it runs
// for: its Machine's own, which is what runs, whatever its object's spec
// asks for since. A file share's server is the platform's, and not here.
func (s *Server) runningShapes(ctx context.Context, project string) (map[string]amounts, error) {
	objs, err := s.listWith(ctx, registry.Machine, project, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]amounts{}
	for _, obj := range objs {
		m := obj.(*computev1alpha1.Machine)
		if m.GetStatus().GetPhase() != computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING {
			continue
		}
		for _, ref := range m.GetMetadata().GetOwnerReferences() {
			if _, ok := machineKinds[ref.GetKind()]; !ok || !ref.GetController() {
				continue
			}
			var a amounts
			if cpu, err := quantity.CPU(m.GetSpec().GetResources().GetCpu()); err == nil {
				a[dimCPU] = int64(cpu) * 1000
			}
			if mem, err := quantity.Bytes(m.GetSpec().GetResources().GetMemory()); err == nil {
				a[dimMemory] = int64(wholeMi(mem) * quantity.MiB)
			}
			out[ref.GetUid()] = a
		}
	}
	return out, nil
}

// readingsOf is an object's amounts as the meter records them, in its
// items' units, leaving out what it does not hold.
func readingsOf(kind, project, name, uid string, a amounts) []store.MeterReading {
	var out []store.MeterReading
	for _, m := range meteredDims {
		q := a[m.d]
		switch m.d {
		case dimMemory:
			q /= int64(quantity.MiB)
		case dimDisk:
			q /= int64(quantity.GiB)
		}
		if q == 0 {
			continue
		}
		out = append(out, store.MeterReading{UID: uid, Item: m.item, Quantity: q, Kind: kind, Project: project, Name: name})
	}
	return out
}

// meterHeld is what tenant held from from to to, by item, summed over its
// objects and minutes.
func (s *Server) meterHeld(ctx context.Context, tenant string, from, to time.Time) (map[string]int64, error) {
	totals, err := s.cfg.Meter.SumMeter(ctx, tenant, from, to)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, t := range totals {
		out[t.Item] += t.Quantity
	}
	return out, nil
}
