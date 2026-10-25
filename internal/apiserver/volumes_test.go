package apiserver

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

func vol(source, target string) *computev1alpha1.ContainerVolume {
	return &computev1alpha1.ContainerVolume{Type: "volume", Source: source, Target: target}
}

func TestContainerVolumeAdmission(t *testing.T) {
	t.Parallel()
	s, _, user := running(t)
	b := s.backend(registry.Container)
	nine := make([]*computev1alpha1.ContainerVolume, 9)
	for i := range nine {
		nine[i] = vol("d"+string(rune('a'+i)), "/v/"+string(rune('a'+i)))
	}
	for name, tt := range map[string]struct {
		volumes []*computev1alpha1.ContainerVolume
		ok      bool
	}{
		"none":                {nil, true},
		"one":                 {[]*computev1alpha1.ContainerVolume{vol("pgdata", "/var/lib/postgresql/data")}, true},
		"type left out":       {[]*computev1alpha1.ContainerVolume{{Source: "d", Target: "/d"}}, true},
		"source kind Disk":    {[]*computev1alpha1.ContainerVolume{{Source: "d", SourceKind: "Disk", Target: "/d"}}, true},
		"nested":              {[]*computev1alpha1.ContainerVolume{vol("a", "/data"), vol("b", "/data/b")}, true},
		"read-only, subpath":  {[]*computev1alpha1.ContainerVolume{{Source: "d", Target: "/d", ReadOnly: true, Volume: &computev1alpha1.ContainerVolumeOptions{Nocopy: true, Subpath: "a/b"}}}, true},
		"eight":               {nine[:8], true},
		"nine":                {nine, false},
		"bind":                {[]*computev1alpha1.ContainerVolume{{Type: "bind", Source: "/srv", Target: "/d"}}, false},
		"tmpfs":               {[]*computev1alpha1.ContainerVolume{{Type: "tmpfs", Target: "/d"}}, false},
		"an unknown type":     {[]*computev1alpha1.ContainerVolume{{Type: "npipe", Source: "d", Target: "/d"}}, false},
		"a FileShare":         {[]*computev1alpha1.ContainerVolume{{Source: "d", SourceKind: "FileShare", Target: "/d"}}, true},
		"a share's directory": {[]*computev1alpha1.ContainerVolume{{Source: "d", SourceKind: "FileShare", Target: "/d", Volume: &computev1alpha1.ContainerVolumeOptions{Subpath: "a/b"}}}, true},
		"a share twice":       {[]*computev1alpha1.ContainerVolume{{Source: "s", SourceKind: "FileShare", Target: "/a"}, {Source: "s", SourceKind: "FileShare", Target: "/b"}}, false},
		"a disk and a share":  {[]*computev1alpha1.ContainerVolume{vol("d", "/a"), {Source: "d", SourceKind: "FileShare", Target: "/b"}}, true},
		"an unknown kind":     {[]*computev1alpha1.ContainerVolume{{Source: "d", SourceKind: "Secret", Target: "/d"}}, false},
		"no source":           {[]*computev1alpha1.ContainerVolume{vol("", "/d")}, false},
		"a source not a name": {[]*computev1alpha1.ContainerVolume{vol("Data_1", "/d")}, false},
		"a disk twice":        {[]*computev1alpha1.ContainerVolume{vol("d", "/a"), vol("d", "/b")}, false},
		"a target twice":      {[]*computev1alpha1.ContainerVolume{vol("a", "/d"), vol("b", "/d")}, false},
		"no target":           {[]*computev1alpha1.ContainerVolume{vol("d", "")}, false},
		"a relative target":   {[]*computev1alpha1.ContainerVolume{vol("d", "data")}, false},
		"an unclean target":   {[]*computev1alpha1.ContainerVolume{vol("d", "/a/../b")}, false},
		"a trailing slash":    {[]*computev1alpha1.ContainerVolume{vol("d", "/data/")}, false},
		"the root":            {[]*computev1alpha1.ContainerVolume{vol("d", "/")}, false},
		"/proc":               {[]*computev1alpha1.ContainerVolume{vol("d", "/proc")}, false},
		"under /dev":          {[]*computev1alpha1.ContainerVolume{vol("d", "/dev/shm")}, false},
		"under /run":          {[]*computev1alpha1.ContainerVolume{vol("d", "/run/hangar")}, false},
		"beside /run":         {[]*computev1alpha1.ContainerVolume{vol("d", "/runner")}, true},
		"a long target":       {[]*computev1alpha1.ContainerVolume{vol("d", "/"+strings.Repeat("a", 1024))}, false},
		"an absolute subpath": {[]*computev1alpha1.ContainerVolume{{Source: "d", Target: "/d", Volume: &computev1alpha1.ContainerVolumeOptions{Subpath: "/a"}}}, false},
		"a subpath out":       {[]*computev1alpha1.ContainerVolume{{Source: "d", Target: "/d", Volume: &computev1alpha1.ContainerVolumeOptions{Subpath: "../a"}}}, false},
		"a subpath of .":      {[]*computev1alpha1.ContainerVolume{{Source: "d", Target: "/d", Volume: &computev1alpha1.ContainerVolumeOptions{Subpath: "."}}}, false},
	} {
		c := ctr("api")
		c.Spec.Volumes = tt.volumes
		_, err := b.Apply(user, c)
		if got := err == nil; got != tt.ok {
			t.Errorf("%s: accepted %t, want %t (%v)", name, got, tt.ok, err)
		}
	}
}

func TestOnlyListedDisksAreMounted(t *testing.T) {
	t.Parallel()
	spec := func(disks []string, volumes ...*computev1alpha1.ContainerBootVolume) *computev1alpha1.MachineSpec {
		m := withDisks(machine("ct-a", ""), disks...).GetSpec()
		m.Boot = &computev1alpha1.MachineSpec_Container{Container: &computev1alpha1.ContainerBoot{Image: "nginx", Volumes: volumes}}
		return m
	}
	mount := func(disk, target string) *computev1alpha1.ContainerBootVolume {
		return &computev1alpha1.ContainerBootVolume{Disk: disk, Target: target}
	}
	shared := func(spec *computev1alpha1.MachineSpec, shares ...string) *computev1alpha1.MachineSpec {
		for _, s := range shares {
			spec.FileShares = append(spec.FileShares, &computev1alpha1.MachineFileShare{FileShare: s})
		}
		return spec
	}
	share := func(name, target string) *computev1alpha1.ContainerBootVolume {
		return &computev1alpha1.ContainerBootVolume{FileShare: name, Target: target, Nocopy: true}
	}
	for name, tt := range map[string]struct {
		spec *computev1alpha1.MachineSpec
		ok   bool
	}{
		"a disk it lists":            {spec([]string{"a", "b"}, mount("b", "/b")), true},
		"a disk not mounted":         {spec([]string{"a"}), true},
		"a disk it lists not":        {spec([]string{"a"}, mount("b", "/b")), false},
		"a disk mounted twice":       {spec([]string{"a"}, mount("a", "/a"), mount("a", "/b")), false},
		"two at one target":          {spec([]string{"a", "b"}, mount("a", "/d"), mount("b", "/d")), false},
		"a target under /sys":        {spec([]string{"a"}, mount("a", "/sys/x")), false},
		"a share it declares":        {shared(spec([]string{"a"}, mount("a", "/a"), share("s", "/s")), "s"), true},
		"a share it does not":        {spec(nil, share("s", "/s")), false},
		"a share listed as a disk":   {spec([]string{"s"}, share("s", "/s")), false},
		"a share mounted twice":      {shared(spec(nil, share("s", "/a"), share("s", "/b")), "s"), false},
		"a disk and a share at once": {shared(spec([]string{"a"}, &computev1alpha1.ContainerBootVolume{Disk: "a", FileShare: "s", Target: "/a"}), "s"), false},
	} {
		err := validateBootVolumes(tt.spec)
		if got := err == nil; got != tt.ok {
			t.Errorf("%s: accepted %t, want %t (%v)", name, got, tt.ok, err)
		}
	}
}

func TestADiskAttachmentFollowsTheMachineSpec(t *testing.T) {
	t.Parallel()
	owned := func(m *computev1alpha1.Machine, kind, name string) *computev1alpha1.Machine {
		m.Metadata.OwnerReferences = []*metav1.OwnerReference{
			{Kind: "Project", Name: "web"},
			{Kind: kind, Name: name, Controller: true},
		}
		return m
	}
	ct := owned(withDisks(machine("ct-api", ""), "logs", "pgdata"), "Container", "api")
	ct.Spec.Boot = &computev1alpha1.MachineSpec_Container{Container: &computev1alpha1.ContainerBoot{
		Image: "postgres", Volumes: []*computev1alpha1.ContainerBootVolume{{Disk: "pgdata", Target: "/var/lib/postgresql/data", ReadOnly: true}},
	}}
	vm := owned(withDisks(machine("vm-web", ""), "data"), "VirtualMachine", "web")
	vm.Spec.Boot = &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{RootDisk: "web-root"}}
	const (
		boot   = storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_BOOT
		data   = storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_DATA
		volume = storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_VOLUME
	)
	for _, tt := range []struct {
		m    *computev1alpha1.Machine
		disk string
		want *storagev1alpha1.DiskAttachment
	}{
		{vm, "web-root", &storagev1alpha1.DiskAttachment{Kind: "VirtualMachine", Name: "web", Machine: "vm-web", As: boot, Device: "vda"}},
		{vm, "data", &storagev1alpha1.DiskAttachment{Kind: "VirtualMachine", Name: "web", Machine: "vm-web", As: data, Device: "vdb"}},
		{ct, "logs", &storagev1alpha1.DiskAttachment{Kind: "Container", Name: "api", Machine: "ct-api", As: data, Device: "vdc"}},
		{ct, "pgdata", &storagev1alpha1.DiskAttachment{
			Kind: "Container", Name: "api", Machine: "ct-api", As: volume, Device: "vdd", Path: "/var/lib/postgresql/data", ReadOnly: true,
		}},
		{ct, "other", nil},
	} {
		if got := diskAttachment(tt.m, tt.disk); !proto.Equal(got, tt.want) {
			t.Errorf("disk %s of %s: %v, want %v", tt.disk, tt.m.GetMetadata().GetName(), got, tt.want)
		}
	}
}

// TestAPlacedContainerKeepsItsNode: a volume added to a placed container
// is made on the container's node; one living on another node is refused,
// saying why, since a machine never moves.
func TestAPlacedContainerKeepsItsNode(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1", "n2")
	nodeSized(t, s, system, "n1", 8192, 100)
	nodeSized(t, s, system, "n2", 4096, 100)
	for _, d := range []string{"a", "b", "far"} {
		if _, err := s.backend(registry.Disk).Apply(user, disk(d, "5Gi")); err != nil {
			t.Fatal(err)
		}
	}
	bindDisk(t, s, system, "far", "n2", "5Gi")
	c := ctr("db")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{vol("a", "/a")}
	if _, err := s.backend(registry.Container).Apply(user, c); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, machineName("db"), "placed on n1", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	c.Spec.Volumes = append(c.Spec.Volumes, vol("b", "/b"))
	if _, err := s.backend(registry.Container).Apply(user, c); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "b", "made on n1 as the container's second volume", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetNode() == "n1" && d.GetStatus().GetSize() == "5Gi" &&
			d.GetStatus().GetAttachment().GetDevice() == "vdd" && conditions.IsTrue(d.GetStatus().GetConditions(), DiskAllocated)
	})

	c.Spec.Volumes = append(c.Spec.Volumes, vol("far", "/far"))
	if _, err := s.backend(registry.Container).Apply(user, c); err != nil {
		t.Fatal(err)
	}
	got := waitContainer(t, s, system, "db", "refused a disk of another node", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "MachineRefused"
	})
	if msg := containerReady(got).GetMessage(); !strings.Contains(msg, `disk "far" lives on node n2`) {
		t.Errorf("Ready says %q", msg)
	}
	if d := getDisk(t, s, system, "far"); d.GetStatus().GetMachine() != "" || d.GetStatus().GetAttachment() != nil {
		t.Errorf("the refused disk says it is attached: %v", d.GetStatus())
	}
}

// TestAContainerWaitsForItsDisk: a volume naming a disk that does not exist
// yet, or one another machine has, holds the container back and says so;
// once the disk is there and free, the container runs with it.
func TestAContainerWaitsForItsDisk(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	c := ctr("db")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{vol("later", "/data")}
	if _, err := s.backend(registry.Container).Apply(user, c); err != nil {
		t.Fatal(err)
	}
	got := waitContainer(t, s, system, "db", "held back for its disk", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "MachineHeldBack"
	})
	if msg := containerReady(got).GetMessage(); !strings.Contains(msg, `disk "later" does not exist`) {
		t.Errorf("Ready says %q", msg)
	}
	if _, err := s.backend(registry.Disk).Apply(user, disk("later", "5Gi")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, machineName("db"), "placed once its disk exists", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	// Another container naming the same disk waits for it to be let go.
	other := ctr("copy")
	other.Spec.Volumes = []*computev1alpha1.ContainerVolume{vol("later", "/data")}
	if _, err := s.backend(registry.Container).Apply(user, other); err != nil {
		t.Fatal(err)
	}
	got = waitContainer(t, s, system, "copy", "held back while db has the disk", func(c *computev1alpha1.Container) bool {
		return containerReady(c).GetReason() == "MachineHeldBack"
	})
	if msg := containerReady(got).GetMessage(); !strings.Contains(msg, `disk "later" is attached to machine `+machineName("db")) {
		t.Errorf("Ready says %q", msg)
	}
	c.Spec.Volumes = nil
	if _, err := s.backend(registry.Container).Apply(user, c); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, machineName("copy"), "placed once db lets the disk go", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
}

// TestAContainerMountsItsVolumes: a container's volumes are its machine's
// disks and its boot's mounts, and each disk says it is a volume of the
// container, at its target, until the container lets it go.
func TestAContainerMountsItsVolumes(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	for _, d := range []string{"pgdata", "logs"} {
		if _, err := s.backend(registry.Disk).Apply(user, disk(d, "5Gi")); err != nil {
			t.Fatal(err)
		}
	}
	c := ctr("db")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{
		vol("pgdata", "/var/lib/postgresql/data"),
		{Source: "logs", Target: "/var/log", ReadOnly: true, Volume: &computev1alpha1.ContainerVolumeOptions{Nocopy: true, Subpath: "pg"}},
	}
	if _, err := s.backend(registry.Container).Apply(user, c); err != nil {
		t.Fatal(err)
	}
	m := waitMachine(t, s, system, machineName("db"), "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	wantMounts := []*computev1alpha1.ContainerBootVolume{
		{Disk: "pgdata", Target: "/var/lib/postgresql/data"},
		{Disk: "logs", Target: "/var/log", ReadOnly: true, Nocopy: true, Subpath: "pg"},
	}
	spec := m.GetSpec()
	if len(spec.GetDisks()) != 2 || spec.GetDisks()[0].GetDisk() != "pgdata" || spec.GetDisks()[1].GetDisk() != "logs" ||
		len(spec.GetContainer().GetVolumes()) != 2 ||
		!proto.Equal(spec.GetContainer().GetVolumes()[0], wantMounts[0]) || !proto.Equal(spec.GetContainer().GetVolumes()[1], wantMounts[1]) {
		t.Errorf("machine spec %v", spec)
	}
	d := waitDisk(t, s, system, "pgdata", "a volume of db", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetAttachment().GetAs() == storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_VOLUME
	})
	want := &storagev1alpha1.DiskAttachment{
		Kind: "Container", Name: "db", Machine: machineName("db"),
		As: storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_VOLUME, Device: "vdc", Path: "/var/lib/postgresql/data",
	}
	if !proto.Equal(d.GetStatus().GetAttachment(), want) || d.GetStatus().GetMachine() != machineName("db") || d.GetStatus().GetNode() != "n1" {
		t.Errorf("pgdata's status %v, want attachment %v", d.GetStatus(), want)
	}
	waitDisk(t, s, system, "logs", "a read-only volume at vdd", func(d *storagev1alpha1.Disk) bool {
		a := d.GetStatus().GetAttachment()
		return a.GetDevice() == "vdd" && a.GetReadOnly() && a.GetPath() == "/var/log"
	})

	// Moved, the volume says so; let go, the disk is attached to nothing.
	moved := proto.Clone(c).(*computev1alpha1.Container)
	moved.Spec.Volumes = []*computev1alpha1.ContainerVolume{vol("pgdata", "/data")}
	if _, err := s.backend(registry.Container).Apply(user, moved); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "pgdata", "moved to /data", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetAttachment().GetPath() == "/data"
	})
	waitDisk(t, s, system, "logs", "attached to nothing", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetAttachment() == nil && d.GetStatus().GetMachine() == ""
	})
	// Its machine being deleted still has the disk, until its node lets
	// the machine go.
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "db"}, nil); err != nil {
		t.Fatal(err)
	}
	m = waitMachine(t, s, system, machineName("db"), "marked for deletion", func(m *computev1alpha1.Machine) bool {
		return m.GetMetadata().GetDeleteTime() != nil
	})
	if a := getDisk(t, s, system, "pgdata").GetStatus().GetAttachment(); a.GetName() != "db" {
		t.Errorf("pgdata of a machine being deleted: attachment %v", a)
	}
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitDisk(t, s, system, "pgdata", "attached to nothing once the machine is gone", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetAttachment() == nil
	})
}
