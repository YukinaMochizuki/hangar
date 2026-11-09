package apiserver

import (
	"context"
	"fmt"
	"math"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/authz"
)

// Usage answers what the named object's running machine used over the last
// hour, as its node sampled it, under the object's own get permission
// (docs/decisions/machine-usage.md).
func (h machineCapabilities) Usage(ctx context.Context, req *connect.Request[computev1alpha1.UsageRequest]) (*connect.Response[computev1alpha1.UsageResponse], error) {
	s := h.s
	m := req.Msg
	kind, err := kindNamed(m.GetKind())
	if err != nil {
		return nil, err
	}
	b := s.backend(kind)
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := machineKinds[kind.Kind]; !ok {
		return nil, invalid("%s do not run as machines", kind.Plural)
	}
	key := resource.Key{Project: m.GetProject(), Name: m.GetName()}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	// Not s.reach: the numbers are what the object's status could say, so
	// whoever may read the object may read them, and no capability of its
	// machine is needed.
	_, obj, err := b.read(ctx, p, authz.Get, key)
	if err != nil {
		return nil, err
	}
	r := &reached{principal: p, kind: kind, key: key, obj: obj}
	machine, err := s.machineOf(ctx, r)
	if err != nil {
		return nil, err
	}
	if err := checkRunning(r, machine); err != nil {
		return nil, err
	}
	vcpus, err := quantity.CPU(machine.GetSpec().GetResources().GetCpu())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("%s's machine's cpu: %w", r, err))
	}
	memory, err := quantity.Bytes(machine.GetSpec().GetResources().GetMemory())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("%s's machine's memory: %w", r, err))
	}
	if memory > math.MaxInt64 {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("%s's machine's memory %d bytes is past an int64", r, memory))
	}
	call, err := s.callMachine(ctx, r, machine)
	if err != nil {
		return nil, err
	}
	defer call.release()
	used, err := call.nodes.MachineUsage(ctx, connect.NewRequest(&nodev1.MachineUsageRequest{MachineUid: machine.GetMetadata().GetUid()}))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, call.failed(err)
	}
	res := &computev1alpha1.UsageResponse{Vcpus: vcpus, MemoryLimitBytes: int64(memory), Latest: usagePoint(used.Msg.GetLatest())}
	switch used.Msg.GetMemorySource() {
	case nodev1.MemorySource_MEMORY_SOURCE_GUEST_USED:
		res.MemoryKind = computev1alpha1.MachineMemoryKind_MACHINE_MEMORY_KIND_GUEST_USED
	case nodev1.MemorySource_MEMORY_SOURCE_HOST_HELD:
		res.MemoryKind = computev1alpha1.MachineMemoryKind_MACHINE_MEMORY_KIND_HOST_HELD
	}
	for _, p := range used.Msg.GetPoints() {
		res.Points = append(res.Points, usagePoint(p))
	}
	return connect.NewResponse(res), nil
}

// usagePoint is a point as its node gives it, nil for none.
func usagePoint(p *nodev1.UsagePoint) *computev1alpha1.MachineUsagePoint {
	if p == nil {
		return nil
	}
	out := &computev1alpha1.MachineUsagePoint{Time: p.GetTime(), Cpu: p.GetCpu()}
	if p.MemoryBytes != nil {
		mem := int64(min(p.GetMemoryBytes(), math.MaxInt64))
		out.MemoryBytes = &mem
	}
	return out
}
