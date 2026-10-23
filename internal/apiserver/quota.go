package apiserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// DefaultPlan is the Plan of a tenant whose spec.plan is empty.
const DefaultPlan = "default"

// DefaultMaxOwnedTenants is how many tenants a person or service token may
// own when nothing says otherwise: the one signing up opens.
const DefaultMaxOwnedTenants = 1

// DefaultLimits are the limits the default Plan is made with, when the
// apiserver first starts on a store: a trial's. The plan is data from then
// on, which the administrator changes with apply
// (docs/runbook/host-bootstrap.md).
func DefaultLimits() *tenancyv1alpha1.Limits {
	return &tenancyv1alpha1.Limits{
		Cpu:             proto.String("8"),
		Memory:          proto.String("16Gi"),
		Disk:            proto.String("100Gi"),
		Machines:        proto.Int32(10),
		Projects:        proto.Int32(5),
		Networks:        proto.Int32(5),
		Addresses:       proto.Int32(5),
		PublicEndpoints: proto.Int32(10),
		ServiceTokens:   proto.Int32(10),
		FileShares:      proto.Int32(3),
		Domains:         proto.Int32(3),
		PortForwards:    proto.Int32(5),
		CustomImages:    proto.Int32(5),
	}
}

// dim is one thing a quota limits.
type dim int

const (
	// dimCPU is in millicores.
	dimCPU dim = iota
	// dimMemory and dimDisk are in bytes.
	dimMemory
	dimDisk
	dimMachines
	dimProjects
	dimNetworks
	dimAddresses
	dimPublicEndpoints
	dimServiceTokens
	dimFileShares
	// dimDomains counts a tenant's CustomDomains, which belong to no project.
	dimDomains
	dimPortForwards
	dimCustomImages
	numDims
)

// dimNames are the dimensions as Limits and Usage name them.
var dimNames = [numDims]string{"cpu", "memory", "disk", "machines", "projects", "networks", "addresses", "public_endpoints", "service_tokens", "file_shares", "domains", "port_forwards", "custom_images"}

// amounts are what something holds, by dimension.
type amounts [numDims]int64

func (a *amounts) add(b amounts) {
	for d := range numDims {
		a[d] += b[d]
	}
}

// limitSet is a Limits read as numbers; a dimension not set has no limit.
type limitSet struct {
	val [numDims]int64
	set [numDims]bool
}

// parseLimits reads l, refusing a quantity that is not one or a count
// below zero.
func parseLimits(l *tenancyv1alpha1.Limits) (limitSet, error) {
	var ls limitSet
	if l == nil {
		return ls, nil
	}
	if l.Cpu != nil {
		m, err := milliCPU(l.GetCpu())
		if err != nil {
			return ls, fmt.Errorf("cpu: %w", err)
		}
		ls.val[dimCPU], ls.set[dimCPU] = m, true
	}
	for _, q := range []struct {
		d dim
		v *string
	}{{dimMemory, l.Memory}, {dimDisk, l.Disk}} {
		if q.v == nil {
			continue
		}
		b, err := quantity.Bytes(*q.v)
		if err != nil {
			return ls, fmt.Errorf("%s: %w", dimNames[q.d], err)
		}
		if b > math.MaxInt64 {
			return ls, fmt.Errorf("%s: %q is too large", dimNames[q.d], *q.v)
		}
		ls.val[q.d], ls.set[q.d] = int64(b), true
	}
	for _, c := range []struct {
		d dim
		v *int32
	}{
		{dimMachines, l.Machines}, {dimProjects, l.Projects}, {dimNetworks, l.Networks}, {dimAddresses, l.Addresses},
		{dimPublicEndpoints, l.PublicEndpoints}, {dimServiceTokens, l.ServiceTokens}, {dimFileShares, l.FileShares},
		{dimDomains, l.Domains}, {dimPortForwards, l.PortForwards}, {dimCustomImages, l.CustomImages},
	} {
		if c.v == nil {
			continue
		}
		if *c.v < 0 {
			return ls, fmt.Errorf("%s: %d is below zero", dimNames[c.d], *c.v)
		}
		ls.val[c.d], ls.set[c.d] = int64(*c.v), true
	}
	return ls, nil
}

// milliCPU reads a cpu quantity, such as "2" or "500m", in millicores.
func milliCPU(s string) (int64, error) {
	v, err := quantity.Parse(s)
	if err != nil {
		return 0, err
	}
	v.Mul(v, big.NewRat(1000, 1))
	switch {
	case v.Sign() < 0:
		return 0, fmt.Errorf("%q is negative", s)
	case !v.IsInt():
		return 0, fmt.Errorf("%q is finer than a millicore", s)
	case !v.Num().IsInt64():
		return 0, fmt.Errorf("%q is too large", s)
	}
	return v.Num().Int64(), nil
}

// tighter is the smaller of a and b in every dimension either limits.
func tighter(a, b limitSet) limitSet {
	out := a
	for d := range numDims {
		if b.set[d] && (!out.set[d] || b.val[d] < out.val[d]) {
			out.val[d], out.set[d] = b.val[d], true
		}
	}
	return out
}

// formatAmount is v of dimension d as Limits and Usage write it.
func formatAmount(d dim, v int64) string {
	switch d {
	case dimCPU:
		if v%1000 == 0 {
			return strconv.FormatInt(v/1000, 10)
		}
		return strconv.FormatInt(v, 10) + "m"
	case dimMemory, dimDisk:
		switch {
		case v == 0:
		case v%int64(quantity.GiB) == 0:
			return strconv.FormatInt(v/int64(quantity.GiB), 10) + "Gi"
		case v%int64(quantity.MiB) == 0:
			return strconv.FormatInt(v/int64(quantity.MiB), 10) + "Mi"
		}
	}
	return strconv.FormatInt(v, 10)
}

// proto is ls as a Limits.
func (ls limitSet) proto() *tenancyv1alpha1.Limits {
	l := &tenancyv1alpha1.Limits{}
	str := func(d dim) *string {
		if !ls.set[d] {
			return nil
		}
		return proto.String(formatAmount(d, ls.val[d]))
	}
	count := func(d dim) *int32 {
		if !ls.set[d] {
			return nil
		}
		return proto.Int32(int32(ls.val[d]))
	}
	l.Cpu, l.Memory, l.Disk = str(dimCPU), str(dimMemory), str(dimDisk)
	l.Machines, l.Projects, l.Networks, l.Addresses = count(dimMachines), count(dimProjects), count(dimNetworks), count(dimAddresses)
	l.PublicEndpoints, l.ServiceTokens, l.FileShares = count(dimPublicEndpoints), count(dimServiceTokens), count(dimFileShares)
	l.Domains, l.PortForwards, l.CustomImages = count(dimDomains), count(dimPortForwards), count(dimCustomImages)
	return l
}

// usage is a as a Usage.
func (a amounts) usage() *tenancyv1alpha1.Usage {
	return &tenancyv1alpha1.Usage{
		Cpu:             formatAmount(dimCPU, a[dimCPU]),
		Memory:          formatAmount(dimMemory, a[dimMemory]),
		Disk:            formatAmount(dimDisk, a[dimDisk]),
		Machines:        int32(a[dimMachines]),
		Projects:        int32(a[dimProjects]),
		Networks:        int32(a[dimNetworks]),
		Addresses:       int32(a[dimAddresses]),
		PublicEndpoints: int32(a[dimPublicEndpoints]),
		ServiceTokens:   int32(a[dimServiceTokens]),
		FileShares:      int32(a[dimFileShares]),
		Domains:         int32(a[dimDomains]),
		PortForwards:    int32(a[dimPortForwards]),
		CustomImages:    int32(a[dimCustomImages]),
	}
}

// projectObjects are one project's objects of every kind in quotaCounters,
// by kind.
type projectObjects map[string][]proto.Message

// quotaCounters say what one object of each project-scoped kind counts
// toward its project's and tenant's quotas; in holds the project's objects
// of every kind here, for a kind whose count depends on another's. A kind
// counts exactly when it is here: admission checks its writes, which hold
// Server.quota, and the usage controller watches it.
var quotaCounters = map[string]func(obj proto.Message, in projectObjects) amounts{
	registry.ServiceToken.Kind:   countsOne(dimServiceTokens),
	registry.Container.Kind:      containerAmounts,
	registry.VirtualMachine.Kind: vmAmounts,
	registry.Disk.Kind:           diskAmounts,
	registry.Network.Kind:        countsOne(dimNetworks),
	registry.Address.Kind:        countsOne(dimAddresses),
	registry.PublicEndpoint.Kind: endpointAmounts,
	registry.FileShare.Kind:      fileShareAmounts,
	registry.PortForward.Kind:    countsOne(dimPortForwards),
	registry.CustomImage.Kind:    customImageAmounts,
}

func countsOne(d dim) func(proto.Message, projectObjects) amounts {
	return func(proto.Message, projectObjects) amounts {
		var a amounts
		a[d] = 1
		return a
	}
}

// containerAmounts is what a container's machine is asked for, with the
// defaults and in the canonical sizes the container controller gives it,
// plus a public endpoint for each public port, which the apiserver makes
// for it. A stopped container holds its disk, its endpoints and its place
// among machines, but no cpu or memory.
func containerAmounts(obj proto.Message, _ projectObjects) amounts {
	spec := obj.(*computev1alpha1.Container).GetSpec()
	var a amounts
	a[dimMachines] = 1
	for _, p := range spec.GetPorts() {
		if p.GetPublic() {
			a[dimPublicEndpoints]++
		}
	}
	// A stored container that no longer parses counts only as a machine and
	// its endpoints.
	sz, err := containerSizes(spec.GetResources())
	if err != nil {
		return a
	}
	a[dimDisk] = int64(wholeGi(sz.disk) * quantity.GiB)
	if spec.GetRunState() != computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED {
		a[dimCPU] = int64(sz.cpu) * 1000
		a[dimMemory] = int64(wholeMi(sz.memory) * quantity.MiB)
	}
	return a
}

// vmAmounts is what a VM's machine is asked for, as containerAmounts is a
// container's, plus the root disk it has made from an image: the size it
// asks for, or, once the Disk exists, the larger of that and the Disk's
// spec and status sizes, so neither shrinking the VM's size nor growing the
// Disk itself holds more than is counted. diskAmounts leaves that Disk out.
func vmAmounts(obj proto.Message, in projectObjects) amounts {
	vm := obj.(*computev1alpha1.VirtualMachine)
	var a amounts
	a[dimMachines] = 1
	if vm.GetSpec().GetRootDisk().GetFromImage() != nil {
		var gi uint64
		if b, err := diskBytes(imageDisk(vm).GetSpec()); err == nil {
			gi = wholeGi(b)
		}
		if d := imageRootOf(vm, in); d != nil {
			gi = max(gi, diskGi(d))
		}
		a[dimDisk] = int64(gi * quantity.GiB)
	}
	if vm.GetSpec().GetRunState() == computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED {
		return a
	}
	if sz, err := vmSizes(vm.GetSpec().GetResources()); err == nil {
		a[dimCPU] = int64(sz.cpu) * 1000
		a[dimMemory] = int64(wholeMi(sz.memory) * quantity.MiB)
	}
	return a
}

// imageRootOf returns the Disk in that vm has made from an image as its
// root disk, if any.
func imageRootOf(vm *computev1alpha1.VirtualMachine, in projectObjects) *storagev1alpha1.Disk {
	uid := vm.GetMetadata().GetUid()
	if uid == "" {
		return nil
	}
	for _, obj := range in[registry.Disk.Kind] {
		if d := obj.(*storagev1alpha1.Disk); d.GetMetadata().GetAnnotations()[RootDiskOf] == uid {
			return d
		}
	}
	return nil
}

// diskAmounts is a disk's size, which vmAmounts counts instead for a root
// disk a VM in has made from an image.
func diskAmounts(obj proto.Message, in projectObjects) amounts {
	d := obj.(*storagev1alpha1.Disk)
	var a amounts
	if uid := d.GetMetadata().GetAnnotations()[RootDiskOf]; uid != "" {
		for _, v := range in[registry.VirtualMachine.Kind] {
			vm := v.(*computev1alpha1.VirtualMachine)
			if vm.GetMetadata().GetUid() == uid && vm.GetSpec().GetRootDisk().GetFromImage() != nil {
				return a
			}
		}
	}
	a[dimDisk] = int64(diskGi(d) * quantity.GiB)
	return a
}

// endpointAmounts is one public endpoint, which containerAmounts counts
// instead for an endpoint a container in has made for a public port.
func endpointAmounts(obj proto.Message, in projectObjects) amounts {
	ep := obj.(*networkv1alpha1.PublicEndpoint)
	var a amounts
	for _, c := range in[registry.Container.Kind] {
		c := c.(*computev1alpha1.Container)
		if uid := c.GetMetadata().GetUid(); uid == "" || !ownedBy(ep.GetMetadata(), uid) {
			continue
		}
		if slices.ContainsFunc(c.GetSpec().GetPorts(), func(p *computev1alpha1.ContainerPort) bool {
			return p.GetPublic() && containerEndpointName(c.GetMetadata().GetName(), p.GetName()) == ep.GetMetadata().GetName()
		}) {
			return a
		}
	}
	a[dimPublicEndpoints] = 1
	return a
}

// diskGi is what a disk holds, in whole Gi: the larger of the size it asks
// for and the size it has been made with.
func diskGi(d *storagev1alpha1.Disk) uint64 {
	var gi uint64
	if b, err := diskBytes(d.GetSpec()); err == nil {
		gi = wholeGi(b)
	}
	if st := d.GetStatus().GetSize(); st != "" {
		if b, err := quantity.Bytes(st); err == nil {
			gi = max(gi, wholeGi(b))
		}
	}
	return gi
}

// wholeMi is a memory size in its canonical unit, whole Mi rounded up.
func wholeMi(bytes uint64) uint64 {
	mi := bytes / quantity.MiB
	if bytes%quantity.MiB != 0 {
		mi++
	}
	return mi
}

// countsTowardQuota says whether a write of the kind can raise a tenant's
// usage, and so holds Server.quota.
func countsTowardQuota(kind string) bool {
	if kind == registry.Tenant.Kind || kind == registry.Project.Kind || kind == registry.CustomDomain.Kind {
		return true
	}
	_, ok := quotaCounters[kind]
	return ok
}

// override is an object tally counts as if it were stored: the one an
// admission check is about to write.
type override struct {
	kind    string
	project string
	name    string
	obj     proto.Message
}

func (o *override) is(kind, project, name string) bool {
	return o != nil && o.kind == kind && o.project == project && o.name == name
}

// tally is what a tenant and each of its projects hold.
type tally struct {
	tenant   amounts
	projects map[string]amounts
	// objects are the tenant's projects, by name.
	objects map[string]*tenancyv1alpha1.Project
}

// tally counts what tenant holds, read straight from the store, with over,
// if any, in place of what is stored at its key. A project counts its
// default network before the controller has made it.
func (s *Server) tally(ctx context.Context, tenant string, over *override) (tally, error) {
	t := tally{projects: map[string]amounts{}, objects: map[string]*tenancyv1alpha1.Project{}}
	objs, err := s.listWith(ctx, registry.Project, "", over)
	if err != nil {
		return tally{}, err
	}
	for _, obj := range objs {
		p := obj.(*tenancyv1alpha1.Project)
		if p.GetSpec().GetTenant() != tenant {
			continue
		}
		t.objects[p.GetMetadata().GetName()] = p
	}
	for name, p := range t.objects {
		in := projectObjects{}
		for kind := range quotaCounters {
			if in[kind], err = s.listWith(ctx, s.backends[kind].r, name, over); err != nil {
				return tally{}, err
			}
		}
		var a amounts
		a[dimProjects] = 1
		for kind, count := range quotaCounters {
			for _, obj := range in[kind] {
				a.add(count(obj, in))
			}
		}
		hasDefault := slices.ContainsFunc(in[registry.Network.Kind], func(n proto.Message) bool {
			return object.Meta(n).GetName() == computeapi.DefaultNetwork
		})
		if !hasDefault && p.GetMetadata().GetDeleteTime() == nil {
			a[dimNetworks]++
		}
		t.projects[name] = a
		t.tenant.add(a)
	}
	domains, err := s.listWith(ctx, registry.CustomDomain, "", over)
	if err != nil {
		return tally{}, err
	}
	for _, obj := range domains {
		if obj.(*networkv1alpha1.CustomDomain).GetSpec().GetTenant() == tenant {
			t.tenant[dimDomains]++
		}
	}
	return t, nil
}

// listWith lists the objects of r in project, every project for "", with
// over in place of what is stored at its key.
func (s *Server) listWith(ctx context.Context, r registry.Resource, project string, over *override) ([]proto.Message, error) {
	b := s.backend(r)
	stored, _, err := s.store.List(ctx, store.ListOptions{Kind: r.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	var out []proto.Message
	replaced := false
	for _, o := range stored {
		if over.is(r.Kind, o.Project, o.Name) {
			replaced = true
			if over.obj != nil {
				out = append(out, over.obj)
			}
			continue
		}
		obj, err := b.decode(o)
		if err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	if !replaced && over != nil && over.kind == r.Kind && over.obj != nil && (project == "" || over.project == project) {
		out = append(out, over.obj)
	}
	return out, nil
}

// planOf returns the name of tenant's Plan and its limits.
func (s *Server) planOf(ctx context.Context, t *tenancyv1alpha1.Tenant) (string, limitSet, error) {
	name := cmp.Or(t.GetSpec().GetPlan(), DefaultPlan)
	obj, err := s.get(ctx, registry.Plan, resource.Key{Name: name})
	if err != nil {
		return name, limitSet{}, err
	}
	if obj == nil {
		return name, limitSet{}, failedPrecondition("tenant %q has plan %q, which does not exist; a platform administrator has to make it",
			t.GetMetadata().GetName(), name)
	}
	// Admission refuses a plan whose limits do not parse.
	ls, err := parseLimits(obj.(*tenancyv1alpha1.Plan).GetSpec().GetLimits())
	return name, ls, err
}

func (s *Server) tenant(ctx context.Context, name string) (*tenancyv1alpha1.Tenant, error) {
	obj, err := s.get(ctx, registry.Tenant, resource.Key{Name: name})
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, failedPrecondition("tenant %q does not exist", name)
	}
	return obj.(*tenancyv1alpha1.Tenant), nil
}

// checkQuota refuses a write by anyone but the apiserver that would take
// what a tenant or project holds past one of its limits; old is nil on
// create. Only what the write raises is checked, so a tenant over a limit,
// as after its plan was lowered, can still shrink and change anything else.
// The caller holds Server.quota from before the store is read here to the
// write, so concurrent writes cannot together overshoot.
func (s *Server) checkQuota(ctx context.Context, r registry.Resource, p auth.Principal, old, obj proto.Message) error {
	if p.Type == auth.System || !countsTowardQuota(r.Kind) {
		return nil
	}
	meta := object.Meta(obj)
	var tenantName string
	switch r.Kind {
	case registry.Tenant.Kind:
		if old != nil {
			return nil
		}
		return s.checkOwnedTenants(ctx, p)
	case registry.Project.Kind:
		tenantName = obj.(*tenancyv1alpha1.Project).GetSpec().GetTenant()
	case registry.CustomDomain.Kind:
		tenantName = obj.(*networkv1alpha1.CustomDomain).GetSpec().GetTenant()
	default:
		proj, err := s.get(ctx, registry.Project, resource.Key{Name: meta.GetProject()})
		if err != nil {
			return err
		}
		if proj == nil {
			return failedPrecondition("project %q does not exist", meta.GetProject())
		}
		tenantName = proj.(*tenancyv1alpha1.Project).GetSpec().GetTenant()
	}
	t, err := s.tenant(ctx, tenantName)
	if err != nil {
		return err
	}
	planName, planLimits, err := s.planOf(ctx, t)
	if err != nil {
		return err
	}
	before, err := s.tally(ctx, tenantName, nil)
	if err != nil {
		return err
	}
	after, err := s.tally(ctx, tenantName, &override{kind: r.Kind, project: meta.GetProject(), name: meta.GetName(), obj: asStored(old, obj)})
	if err != nil {
		return err
	}
	for d := range numDims {
		if after.tenant[d] > before.tenant[d] && planLimits.set[d] && after.tenant[d] > planLimits.val[d] {
			return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(
				"quota exceeded: tenant %q's %s limit is %s (plan %q) and it uses %s; this would bring it to %s. Free some, or ask a platform administrator for a bigger plan",
				tenantName, dimNames[d], formatAmount(d, planLimits.val[d]), planName, formatAmount(d, before.tenant[d]), formatAmount(d, after.tenant[d])))
		}
	}
	if r.Kind == registry.Project.Kind || r.Kind == registry.CustomDomain.Kind {
		return nil
	}
	proj := after.objects[meta.GetProject()]
	own, err := parseLimits(proj.GetSpec().GetLimits())
	if err != nil {
		return err
	}
	was, now := before.projects[meta.GetProject()], after.projects[meta.GetProject()]
	for d := range numDims {
		if now[d] > was[d] && own.set[d] && now[d] > own.val[d] {
			return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(
				"quota exceeded: project %q's %s limit is %s (its spec.limits) and it uses %s; this would bring it to %s. Free some, or ask an owner of tenant %q to raise the project's limit",
				meta.GetProject(), dimNames[d], formatAmount(d, own.val[d]), formatAmount(d, was[d]), formatAmount(d, now[d]), tenantName))
		}
	}
	return nil
}

// asStored is obj with what an update takes from old, the stored object:
// its status, and, as an update by anyone but the apiserver does, its uid,
// owner references and reserved labels and annotations, which tie a VM and
// its root disk together. A manifest applied again, which carries none of
// them, then counts as what is stored. checkQuota runs only for callers
// other than the apiserver.
//
// On create, old is nil, and obj counts as create stores it: with no uid,
// owner references or status of the caller's. A uid the caller borrowed,
// such as one a retained root disk names, would otherwise make that disk
// count as the new VM's and drop out of the total.
func asStored(old, obj proto.Message) proto.Message {
	obj = proto.Clone(obj)
	if old == nil {
		meta := object.Meta(obj)
		meta.Uid, meta.OwnerReferences = "", nil
		object.SetStatus(obj, nil)
		return obj
	}
	meta, was := object.Meta(obj), object.Meta(old)
	meta.Uid = was.GetUid()
	meta.OwnerReferences = was.GetOwnerReferences()
	meta.Labels = keepReserved(meta.GetLabels(), was.GetLabels())
	meta.Annotations = keepReserved(meta.GetAnnotations(), was.GetAnnotations())
	object.CopyStatus(obj, old)
	return obj
}

// OpenedBy is the Tenant annotation naming who opened it, as a principal
// ("user:alice@example.com", "serviceToken:..."). The apiserver sets it
// when the tenant is created; the key is reserved, so no one else can set,
// change or drop it, whatever happens to the tenant's bindings.
const OpenedBy = registry.Domain + "/opened-by"

// stampOpener records on a new tenant who opens it, if that is a person or
// a service token, the principals whose tenants are capped.
func stampOpener(p auth.Principal, obj proto.Message) {
	if p.Type != auth.User && p.Type != auth.ServiceToken {
		return
	}
	meta := object.Meta(obj)
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	meta.Annotations[OpenedBy] = p.String()
}

// checkOwnedTenants refuses a new tenant to someone who already owns as many
// as they may: those they opened, by OpenedBy, which are not being deleted.
// A person's User can set their own cap; otherwise, and for service tokens,
// the platform default holds. Whoever may apply tenants at the platform,
// root, is not capped.
func (s *Server) checkOwnedTenants(ctx context.Context, p auth.Principal) error {
	if p.Type != auth.User && p.Type != auth.ServiceToken {
		return nil
	}
	if s.authz.Allowed(p, authz.Permission(registry.Tenant, authz.Apply), authz.Target{}) {
		return nil
	}
	limit, source := int64(s.cfg.DefaultMaxOwnedTenants), "the platform default"
	if name, ok := s.authz.Username(p); ok {
		obj, err := s.get(ctx, registry.User, resource.Key{Name: name})
		if err != nil {
			return err
		}
		if u, ok := obj.(*iamv1alpha1.User); ok && u.GetSpec().MaxOwnedTenants != nil {
			limit, source = int64(u.GetSpec().GetMaxOwnedTenants()), "set on their User"
		}
	}
	owned, err := s.ownedTenants(ctx, p)
	if err != nil {
		return err
	}
	if owned >= limit {
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(
			"quota exceeded: %s owns %d tenants and may own %d (%s); delete one, or ask a platform administrator to raise the limit",
			p, owned, limit, source))
	}
	return nil
}

// ownedTenants counts the live tenants p opened.
func (s *Server) ownedTenants(ctx context.Context, p auth.Principal) (int64, error) {
	tenants, err := s.listWith(ctx, registry.Tenant, "", nil)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, obj := range tenants {
		if meta := object.Meta(obj); meta.GetDeleteTime() == nil && meta.GetAnnotations()[OpenedBy] == p.String() {
			n++
		}
	}
	return n, nil
}

// validatePlan checks a plan's limits.
func validatePlan(_ context.Context, _ *Server, _ auth.Principal, _, obj proto.Message) error {
	if _, err := parseLimits(obj.(*tenancyv1alpha1.Plan).GetSpec().GetLimits()); err != nil {
		return invalid("spec.limits.%v", err)
	}
	return nil
}

// refuseDeletingPlanInUse keeps the default plan, and a plan a tenant, a
// User or an Invitation names: a User's is the plan of the tenants they
// open, an Invitation's their User's to be.
func refuseDeletingPlanInUse(ctx context.Context, s *Server, obj proto.Message) error {
	name := object.Meta(obj).GetName()
	if name == DefaultPlan {
		return failedPrecondition("plan %q is every tenant's that names none; change it rather than delete it", DefaultPlan)
	}
	for _, use := range []struct {
		r    registry.Resource
		plan func(proto.Message) string
		fix  string
	}{
		{registry.Tenant, func(m proto.Message) string { return m.(*tenancyv1alpha1.Tenant).GetSpec().GetPlan() }, "give the tenant another plan first"},
		{registry.User, func(m proto.Message) string { return m.(*iamv1alpha1.User).GetSpec().GetPlan() }, "give the user another plan first"},
		{registry.Invitation, func(m proto.Message) string { return m.(*iamv1alpha1.Invitation).GetSpec().GetPlan() }, "change or delete the invitation first"},
	} {
		objs, err := s.listWith(ctx, use.r, "", nil)
		if err != nil {
			return err
		}
		for _, o := range objs {
			if use.plan(o) == name {
				return failedPrecondition("plan %q is %s %q's; %s", name, strings.ToLower(use.r.Kind), object.Meta(o).GetName(), use.fix)
			}
		}
	}
	return nil
}

// validateProjectLimits checks a project's spec.limits: they parse, leave
// projects to the plan, and are no higher than the tenant's. A change needs
// a grant at the tenant, so a role held on the project alone, however
// broad, cannot raise its own budget.
func (s *Server) validateProjectLimits(ctx context.Context, p auth.Principal, old, obj proto.Message) error {
	proj := obj.(*tenancyv1alpha1.Project)
	limits := proj.GetSpec().GetLimits()
	var was *tenancyv1alpha1.Limits
	if old != nil {
		was = old.(*tenancyv1alpha1.Project).GetSpec().GetLimits()
	}
	ls, err := parseLimits(limits)
	if err != nil {
		return invalid("spec.limits.%v", err)
	}
	// Compared by value: no limits and empty ones are the same, and so are
	// 4Gi and 4096Mi.
	if wasLs, err := parseLimits(was); err == nil && wasLs == ls {
		return nil
	}
	if ls.set[dimProjects] {
		return invalid("spec.limits.projects is not a project's to set; the tenant's plan limits projects")
	}
	if ls.set[dimDomains] {
		return invalid("spec.limits.domains is not a project's to set; the tenant's plan limits domains")
	}
	tenantName := proj.GetSpec().GetTenant()
	if !s.authz.Allowed(p, authz.Permission(registry.Project, authz.Apply), authz.Target{Tenant: tenantName}) {
		return denied("%s may not change spec.limits of project %q: that needs apply on projects at tenant %q", p, proj.GetMetadata().GetName(), tenantName)
	}
	t, err := s.tenant(ctx, tenantName)
	if err != nil {
		return err
	}
	planName, planLimits, err := s.planOf(ctx, t)
	if err != nil {
		return err
	}
	for d := range numDims {
		if ls.set[d] && planLimits.set[d] && ls.val[d] > planLimits.val[d] {
			return invalid("spec.limits.%s %s is above tenant %q's limit of %s (plan %q)",
				dimNames[d], formatAmount(d, ls.val[d]), tenantName, formatAmount(d, planLimits.val[d]), planName)
		}
	}
	return nil
}

// ensureDefaultPlan makes the default plan if there is none.
func (s *Server) ensureDefaultPlan(ctx context.Context) error {
	limits := s.cfg.DefaultPlanLimits
	if limits == nil {
		limits = DefaultLimits()
	}
	_, err := s.backend(registry.Plan).Create(s.system(ctx), &tenancyv1alpha1.Plan{
		Metadata: &metav1.ObjectMeta{Name: DefaultPlan},
		Spec:     &tenancyv1alpha1.PlanSpec{DisplayName: "Default", Limits: limits},
	})
	if err != nil && connect.CodeOf(err) != connect.CodeAlreadyExists {
		return fmt.Errorf("making the default plan: %w", err)
	}
	return nil
}

// usageController keeps each tenant's and project's status.plan, limits and
// usage up to date. They are for people to read; admission counts from the
// store itself.
func (s *Server) usageController() *controller.Controller {
	watches := []controller.Watch{
		{Kind: registry.Tenant.Kind, Map: controller.Self},
		{Kind: registry.Project.Kind, Map: s.tenantOfProject},
		{Kind: registry.Plan.Kind, Map: s.tenantsOfPlan, SkipResync: true},
		{Kind: registry.CustomDomain.Kind, Map: tenantOfDomain, SkipResync: true},
	}
	for kind := range quotaCounters {
		watches = append(watches, controller.Watch{Kind: kind, Map: s.tenantOfObject, SkipResync: true})
	}
	return &controller.Controller{
		Name: "usage", Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
		Reconciler: controller.ReconcilerFunc(s.reconcileUsage),
		Watches:    watches,
	}
}

func (s *Server) tenantOfProject(_ context.Context, o store.Object) []controller.Key {
	var p tenancyv1alpha1.Project
	if proto.Unmarshal(o.Data, &p) != nil || p.GetSpec().GetTenant() == "" {
		return nil
	}
	return []controller.Key{{Name: p.GetSpec().GetTenant()}}
}

func tenantOfDomain(_ context.Context, o store.Object) []controller.Key {
	var d networkv1alpha1.CustomDomain
	if proto.Unmarshal(o.Data, &d) != nil || d.GetSpec().GetTenant() == "" {
		return nil
	}
	return []controller.Key{{Name: d.GetSpec().GetTenant()}}
}

func (s *Server) tenantOfObject(ctx context.Context, o store.Object) []controller.Key {
	obj, err := s.get(ctx, registry.Project, resource.Key{Name: o.Project})
	if err != nil {
		s.log.Error("mapping to a tenant failed", "project", o.Project, "error", err)
		return nil
	}
	if obj == nil {
		return nil
	}
	return []controller.Key{{Name: obj.(*tenancyv1alpha1.Project).GetSpec().GetTenant()}}
}

func (s *Server) tenantsOfPlan(ctx context.Context, o store.Object) []controller.Key {
	tenants, err := s.listWith(ctx, registry.Tenant, "", nil)
	if err != nil {
		s.log.Error("listing tenants failed", "error", err)
		return nil
	}
	var keys []controller.Key
	for _, t := range tenants {
		if cmp.Or(t.(*tenancyv1alpha1.Tenant).GetSpec().GetPlan(), DefaultPlan) == o.Name {
			keys = append(keys, controller.Key{Name: object.Meta(t).GetName()})
		}
	}
	return keys
}

// reconcileUsage writes what a tenant and each of its projects hold, and
// the limits that hold for them, into their status.
func (s *Server) reconcileUsage(ctx context.Context, k controller.Key) error {
	obj, err := s.get(ctx, registry.Tenant, resource.Key{Name: k.Name})
	if err != nil || obj == nil {
		return err
	}
	t := obj.(*tenancyv1alpha1.Tenant)
	planName, planLimits, err := s.planOf(ctx, t)
	var noPlan *connect.Error
	if errors.As(err, &noPlan) {
		// The usage is still worth showing; the plan's absence is in the
		// admission errors.
		err = nil
	}
	if err != nil {
		return err
	}
	counted, err := s.tally(ctx, k.Name, nil)
	if err != nil {
		return err
	}
	status := proto.Clone(t.GetStatus()).(*tenancyv1alpha1.TenantStatus)
	if status == nil {
		status = &tenancyv1alpha1.TenantStatus{}
	}
	status.Plan, status.Limits, status.Usage = planName, planLimits.proto(), counted.tenant.usage()
	if !proto.Equal(status, t.GetStatus()) {
		rev := t.GetMetadata().GetResourceVersion()
		if _, err := s.backend(registry.Tenant).UpdateStatus(s.system(ctx), resource.Key{Name: k.Name}, status, &rev); err != nil {
			return err
		}
	}
	for name, p := range counted.objects {
		own, _ := parseLimits(p.GetSpec().GetLimits())
		ps := proto.Clone(p.GetStatus()).(*tenancyv1alpha1.ProjectStatus)
		if ps == nil {
			ps = &tenancyv1alpha1.ProjectStatus{}
		}
		ps.Limits, ps.Usage = tighter(planLimits, own).proto(), counted.projects[name].usage()
		if proto.Equal(ps, p.GetStatus()) {
			continue
		}
		rev := p.GetMetadata().GetResourceVersion()
		if _, err := s.backend(registry.Project).UpdateStatus(s.system(ctx), resource.Key{Name: name}, ps, &rev); err != nil {
			return err
		}
	}
	return nil
}
