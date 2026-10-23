package apiserver

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

// uploadImage is a custom image uploaded from a file of size bytes.
func uploadImage(name string, size int64, sha string) *storagev1alpha1.CustomImage {
	return &storagev1alpha1.CustomImage{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec: &storagev1alpha1.CustomImageSpec{
			Source: &storagev1alpha1.CustomImageSource{Source: &storagev1alpha1.CustomImageSource_Upload{
				Upload: &storagev1alpha1.CustomImageUploadSource{Size: size, Sha256: sha},
			}},
			Os: "nixos", DefaultUser: "root",
		},
	}
}

// withNode1 runs the server with node n1 registered, not yet reporting.
func withNode1(t *testing.T, configure func(*Config)) (*Server, context.Context, context.Context) {
	t.Helper()
	s, system, user := runningWith(t, func(c *Config) {
		c.Nodes = []string{"n1"}
		configure(c)
	})
	waitNode(t, s, "n1")
	return s, system, user
}

// uploadingNode reports node ready, with room, and taking uploads.
func uploadingNode(t *testing.T, s *Server, system context.Context, node string) {
	t.Helper()
	status := &computev1alpha1.NodeStatus{
		Capacity:           &computev1alpha1.NodeResources{Cpu: "8", Memory: "8192Mi"},
		CustomImageUploads: true,
		Conditions: []*metav1.Condition{{
			Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting",
		}},
	}
	if _, err := s.backend(registry.Node).UpdateStatus(asNode(system, node), resource.Key{Name: node}, status, nil); err != nil {
		t.Fatal(err)
	}
}

func uploadFailed(reason string) *metav1.Condition {
	return &metav1.Condition{Type: CustomImageReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason}
}

func TestCustomImageUploadAdmission(t *testing.T) {
	s, system, user := withNode1(t, func(c *Config) { c.MaxUploadSize = 4 << 30 })
	b := s.backend(registry.CustomImage)
	sha := strings.Repeat("ab", 32)

	_, err := b.Apply(user, uploadImage("img", 1<<20, sha))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "an upload with no node ready to take it")
	nodeWithRoom(t, s, system, "n1", 8192)
	_, err = b.Apply(user, uploadImage("img", 1<<20, sha))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no node takes uploads") {
		t.Errorf("an upload with no node taking uploads: %v, want failed_precondition saying so", err)
	}

	uploadingNode(t, s, system, "n1")
	for name, ci := range map[string]*storagev1alpha1.CustomImage{
		"no size":                  uploadImage("bad", 0, sha),
		"a negative size":          uploadImage("bad", -1, sha),
		"a size past the bound":    uploadImage("bad", 4<<30+1, sha),
		"a sha256 in upper case":   uploadImage("bad", 1<<20, strings.ToUpper(sha)),
		"a sha256 short of digits": uploadImage("bad", 1<<20, sha[:63]),
	} {
		_, err := b.Apply(user, ci)
		wantCodeOf(t, err, connect.CodeInvalidArgument, name)
	}
	both := uploadImage("bad", 1<<20, "")
	both.Spec.Source = &storagev1alpha1.CustomImageSource{}
	_, err = b.Apply(user, both)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a source that is neither")

	res, err := b.Apply(user, uploadImage("img", 4<<30, sha))
	if err != nil {
		t.Fatalf("an upload of the bound's size: %v", err)
	}
	if st := res.Object.(*storagev1alpha1.CustomImage).GetStatus(); st.GetNode() != "n1" || st.GetDiskSize() != "" {
		t.Errorf("status %v, want node n1 and no disk_size until it is converted", st)
	}
	if _, err := b.Apply(user, uploadImage("nosum", 1<<20, "")); err != nil {
		t.Errorf("an upload with no sha256: %v", err)
	}
	changed := uploadImage("img", 4<<30, "")
	_, err = b.Apply(user, changed)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "changing an upload's sha256")
	changed = uploadImage("img", 4<<30, sha)
	changed.Spec.Version = "25.05"
	if _, err := b.Apply(user, changed); err != nil {
		t.Errorf("changing an upload's version: %v", err)
	}

	// An upload holds no disk.
	if held, err := s.heldSources(system, "web"); err != nil || len(held) != 0 {
		t.Errorf("disks held by uploads: %v %v", held, err)
	}
}

// TestAnUploadsNodeSetsItsDiskSizeOnce: the node reports what the apiserver
// cannot know of an upload, its disk size among it, and never moves the
// image to another node.
func TestAnUploadsNodeSetsItsDiskSizeOnce(t *testing.T) {
	s, system, user := withNode1(t, func(*Config) {})
	uploadingNode(t, s, system, "n1")
	if _, err := s.backend(registry.CustomImage).Apply(user, uploadImage("img", 3<<30, "")); err != nil {
		t.Fatal(err)
	}
	key := resource.Key{Project: "web", Name: "img"}
	report := func(st *storagev1alpha1.CustomImageStatus) *storagev1alpha1.CustomImageStatus {
		t.Helper()
		res, err := s.backend(registry.CustomImage).UpdateStatus(asNode(system, "n1"), key, st, nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.(*storagev1alpha1.CustomImage).GetStatus()
	}
	expires := timestamppb.New(s.clock.Now().Add(24 * time.Hour))
	got := report(&storagev1alpha1.CustomImageStatus{
		Node: "n2", ReceivedBytes: 1 << 30, Sha256: strings.Repeat("0", 64), UploadExpires: expires, DiskSize: "10Gi", UsedSize: "2Gi",
	})
	if got.GetNode() != "n1" || got.GetReceivedBytes() != 1<<30 || got.GetSha256() == "" || !proto.Equal(got.GetUploadExpires(), expires) || got.GetDiskSize() != "10Gi" {
		t.Errorf("stored %v, want the node's report on node n1", got)
	}
	if got := report(&storagev1alpha1.CustomImageStatus{DiskSize: "20Gi"}); got.GetDiskSize() != "10Gi" {
		t.Errorf("disk_size %s once set, want it kept at 10Gi", got.GetDiskSize())
	}
}

func TestCustomImageUploadQuota(t *testing.T) {
	s, system, user := withNode1(t, func(*Config) {})
	uploadingNode(t, s, system, "n1")
	images := s.backend(registry.CustomImage)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("10Gi"), CustomImages: proto.Int32(2)})
	used := func(what string, disk, count int64) {
		t.Helper()
		got, err := s.tally(system, "acme", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.tenant[dimDisk] != disk*int64(quantity.GiB) || got.tenant[dimCustomImages] != count {
			t.Errorf("%s: %s of disk and %d images, want %dGi and %d", what,
				formatAmount(dimDisk, got.tenant[dimDisk]), got.tenant[dimCustomImages], disk, count)
		}
	}
	if _, err := images.Apply(user, uploadImage("a", 6<<30+1, "")); err != nil {
		t.Fatal(err)
	}
	used("an upload, reserving its size in whole Gi", 7, 1)
	_, err := images.Apply(user, uploadImage("b", 4<<30, ""))
	wantExhausted(t, err, "disk limit", "a second upload past the disk limit")

	settle := func(name string, st *storagev1alpha1.CustomImageStatus) {
		t.Helper()
		ci := getCustomImage(t, s, system, name)
		st.Node = ci.GetStatus().GetNode()
		if _, err := images.UpdateStatus(asNode(system, "n1"), resource.Key{Project: "web", Name: name}, st, nil); err != nil {
			t.Fatal(err)
		}
	}
	settle("a", &storagev1alpha1.CustomImageStatus{DiskSize: "20Gi", UsedSize: "1Gi", Conditions: []*metav1.Condition{imageReady()}})
	used("the upload Ready, settled at what it holds, not its disk", 1, 1)
	if _, err := images.Apply(user, uploadImage("b", 4<<30, "")); err != nil {
		t.Fatalf("a second upload once the first settled: %v", err)
	}
	used("two images", 5, 2)
	_, err = images.Apply(user, uploadImage("c", 1, ""))
	wantExhausted(t, err, "custom_images limit", "a third image past the count limit")
	for _, reason := range []string{
		computeapi.ReasonUploadExpired, computeapi.ReasonDigestMismatch, computeapi.ReasonInvalidImage,
		computeapi.ReasonImageTooLarge, computeapi.ReasonConvertFailed,
	} {
		settle("b", &storagev1alpha1.CustomImageStatus{Conditions: []*metav1.Condition{uploadFailed(reason)}})
		used("an upload failed as "+reason+", released", 1, 1)
	}
	settle("b", &storagev1alpha1.CustomImageStatus{Conditions: []*metav1.Condition{uploadFailed(computeapi.ReasonConverting)}})
	used("an upload converting", 5, 2)
}

// TestAnUploadsVirtualSizeIsKeptWithinTheDiskQuota: the virtual size the
// node reports counts toward the disk quota, if more than the file's size,
// once the apiserver keeps it; one past the tenant's or the project's
// limit fails the image as QuotaExceeded instead, for good, and releases
// it. Ready, the image settles at what it holds.
func TestAnUploadsVirtualSizeIsKeptWithinTheDiskQuota(t *testing.T) {
	s, system, user := withNode1(t, func(*Config) {})
	uploadingNode(t, s, system, "n1")
	images := s.backend(registry.CustomImage)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("10Gi")})
	used := func(what string, disk, count int64) {
		t.Helper()
		got, err := s.tally(system, "acme", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.tenant[dimDisk] != disk*int64(quantity.GiB) || got.tenant[dimCustomImages] != count {
			t.Errorf("%s: %s of disk and %d images, want %dGi and %d", what,
				formatAmount(dimDisk, got.tenant[dimDisk]), got.tenant[dimCustomImages], disk, count)
		}
	}
	inspected := func(virtual int64) *storagev1alpha1.CustomImageStatus {
		return &storagev1alpha1.CustomImageStatus{VirtualSize: virtual, Conditions: []*metav1.Condition{
			{Type: computeapi.CustomImageUploaded, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: computeapi.ReasonUploaded},
			uploadFailed(computeapi.ReasonConverting),
		}}
	}
	report := func(name string, st *storagev1alpha1.CustomImageStatus) *storagev1alpha1.CustomImageStatus {
		t.Helper()
		res, err := images.UpdateStatus(asNode(system, "n1"), resource.Key{Project: "web", Name: name}, st, nil)
		if err != nil {
			t.Fatal(err)
		}
		return res.(*storagev1alpha1.CustomImage).GetStatus()
	}
	ready := func(st *storagev1alpha1.CustomImageStatus) string {
		return conditions.Find(st.GetConditions(), CustomImageReady).GetReason()
	}
	for _, name := range []string{"a", "b"} {
		if _, err := images.Apply(user, uploadImage(name, 1<<30, "")); err != nil {
			t.Fatal(err)
		}
	}
	used("two uploads, at their files' sizes", 2, 2)

	if got := report("a", inspected(4<<30-1)); got.GetVirtualSize() != 4<<30-1 || ready(got) != computeapi.ReasonConverting {
		t.Errorf("a virtual size within the quota: stored %v, want it kept, converting", got)
	}
	used("the virtual size kept, in whole Gi", 5, 2)
	if got := report("a", inspected(9<<30)); got.GetVirtualSize() != 4<<30-1 {
		t.Errorf("virtual size %d once kept, want it kept at %d", got.GetVirtualSize(), 4<<30-1)
	}
	used("a second report of another virtual size", 5, 2)

	got := report("b", inspected(7<<30))
	if c := conditions.Find(got.GetConditions(), CustomImageReady); c.GetReason() != computeapi.ReasonQuotaExceeded ||
		c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || !strings.Contains(c.GetMessage(), "tenant \"acme\"'s disk limit is 10Gi") {
		t.Errorf("a virtual size past the tenant's limit: Ready %v, want False QuotaExceeded naming the limit", c)
	}
	if !conditions.IsTrue(got.GetConditions(), computeapi.CustomImageUploaded) {
		t.Errorf("the image over quota: conditions %v, want Uploaded still True", got.GetConditions())
	}
	used("the upload over quota, released", 4, 1)
	if got := report("b", inspected(7<<30)); ready(got) != computeapi.ReasonQuotaExceeded {
		t.Errorf("the node reporting again over QuotaExceeded: Ready %s, want it kept", ready(got))
	}

	// The project's own limit holds too.
	p, err := s.get(system, registry.Project, resource.Key{Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	proj := p.(*tenancyv1alpha1.Project)
	proj.Spec.Limits = &tenancyv1alpha1.Limits{Disk: proto.String("6Gi")}
	proj.Metadata.ResourceVersion = 0
	if _, err := s.backend(registry.Project).Apply(system, proj); err != nil {
		t.Fatal(err)
	}
	if _, err := images.Apply(user, uploadImage("c", 1<<30, "")); err != nil {
		t.Fatal(err)
	}
	got = report("c", inspected(3<<30))
	if c := conditions.Find(got.GetConditions(), CustomImageReady); c.GetReason() != computeapi.ReasonQuotaExceeded || !strings.Contains(c.GetMessage(), "project \"web\"'s disk limit is 6Gi") {
		t.Errorf("a virtual size past the project's limit: Ready %v, want QuotaExceeded naming it", c)
	}
	used("the second upload over quota, released", 4, 1)

	settled := inspected(0)
	settled.UsedSize, settled.DiskSize = "1Gi", "4Gi"
	settled.Conditions[1] = imageReady()
	report("a", settled)
	used("the kept upload Ready, settled at what it holds", 1, 1)
}

// TestUploadsReportedAtOnceAreCountedTogether: two virtual sizes reported
// at the same time, each of which fits alone, do not both fit.
func TestUploadsReportedAtOnceAreCountedTogether(t *testing.T) {
	s, system, user := withNode1(t, func(*Config) {})
	uploadingNode(t, s, system, "n1")
	images := s.backend(registry.CustomImage)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("10Gi")})
	for _, name := range []string{"a", "b"} {
		if _, err := images.Apply(user, uploadImage(name, 1<<30, "")); err != nil {
			t.Fatal(err)
		}
	}
	for range 20 {
		reasons := make(chan string, 2)
		for _, name := range []string{"a", "b"} {
			go func() {
				res, err := images.UpdateStatus(asNode(system, "n1"), resource.Key{Project: "web", Name: name}, &storagev1alpha1.CustomImageStatus{
					VirtualSize: 6 << 30, Conditions: []*metav1.Condition{uploadFailed(computeapi.ReasonConverting)},
				}, nil)
				if err != nil {
					reasons <- err.Error()
					return
				}
				reasons <- conditions.Find(res.(*storagev1alpha1.CustomImage).GetStatus().GetConditions(), CustomImageReady).GetReason()
			}()
		}
		got := []string{<-reasons, <-reasons}
		slices.Sort(got)
		if want := []string{computeapi.ReasonConverting, computeapi.ReasonQuotaExceeded}; !slices.Equal(got, want) {
			t.Fatalf("two 6Gi virtual sizes within 10Gi reported at once: Ready %v, want %v", got, want)
		}
		// Again, from the start.
		for _, name := range []string{"a", "b"} {
			if _, err := images.Delete(system, resource.Key{Project: "web", Name: name}, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := images.Apply(user, uploadImage(name, 1<<30, "")); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestAnUploadIsPlacedOnANodeThatTakesUploads: of two nodes, the upload
// goes to the one that takes uploads, though the other has more room.
func TestAnUploadIsPlacedOnANodeThatTakesUploads(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1", "n2"} })
	waitNode(t, s, "n1")
	waitNode(t, s, "n2")
	nodeWithRoom(t, s, system, "n1", 65536)
	uploadingNode(t, s, system, "n2")
	res, err := s.backend(registry.CustomImage).Apply(user, uploadImage("img", 1<<20, ""))
	if err != nil {
		t.Fatal(err)
	}
	if node := res.Object.(*storagev1alpha1.CustomImage).GetStatus().GetNode(); node != "n2" {
		t.Errorf("placed on %s, want n2, the node that takes uploads", node)
	}
}
