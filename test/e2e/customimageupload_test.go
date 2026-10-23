package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
	"github.com/YukinaMochizuki/hangar/internal/uploadwire"
)

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// uploadImage makes the CustomImage name, uploaded from a file of size
// bytes and sha256 sum, once the node is Ready to be placed on, and
// returns its uid.
func (w *world) uploadImage(name string, size int64, sum string) string {
	w.t.Helper()
	testutil.Eventually(w.t, "the node Ready", func() (bool, string) {
		obj, err := w.root.Get(context.Background(), registry.Node, client.Key{Name: apiservertest.Node})
		if err != nil {
			return false, err.Error()
		}
		n := obj.(*computev1alpha1.Node)
		return conditions.IsTrue(n.GetStatus().GetConditions(), "Ready"), fmt.Sprint(n.GetStatus().GetConditions())
	})
	res, err := w.root.Create(context.Background(), registry.CustomImage, &storagev1alpha1.CustomImage{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &storagev1alpha1.CustomImageSpec{
			Source: &storagev1alpha1.CustomImageSource{Source: &storagev1alpha1.CustomImageSource_Upload{
				Upload: &storagev1alpha1.CustomImageUploadSource{Size: size, Sha256: sum},
			}},
			DefaultUser: "root",
		},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return res.Object.(*storagev1alpha1.CustomImage).GetMetadata().GetUid()
}

// putChunk sends the chunk of file at first, n bytes, with sum as its
// sha256 unless empty, to the image's :upload path, as root.
func (w *world) putChunk(name string, file []byte, first, n int64, sum string) (int64, error) {
	w.t.Helper()
	body := file[first : first+n]
	if sum == "" {
		sum = sumOf(body)
	}
	req, err := http.NewRequest(http.MethodPut, w.env.URL+uploadwire.Path(project, name), bytes.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set(auth.DevUserHeader, apiservertest.Root)
	req.Header.Set("Content-Range", uploadwire.ContentRange(first, n, int64(len(file))))
	req.Header.Set(uploadwire.ChunkSHA256, sum)
	return w.uploadAnswer(req)
}

// uploadedBytes asks the image's :upload path where its upload is.
func (w *world) uploadedBytes(name string) (int64, error) {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.env.URL+uploadwire.Path(project, name), nil)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set(auth.DevUserHeader, apiservertest.Root)
	return w.uploadAnswer(req)
}

func (w *world) uploadAnswer(req *http.Request) (int64, error) {
	w.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, uploadwire.ReadError(resp)
	}
	var st uploadwire.State
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		w.t.Fatal(err)
	}
	return st.ReceivedBytes, nil
}

func readyReason(ci *storagev1alpha1.CustomImage) string {
	return imageCondition(ci, computeapi.CustomImageReady).GetReason()
}

// staged lists what the agent's upload directory holds.
func (w *world) staged() []string {
	w.t.Helper()
	entries, err := os.ReadDir(w.uploadDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		w.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestACustomImageIsUploadedChunkByChunk: chunks are taken at the node's
// count, a wrong offset and a garbled chunk refused, the count reported,
// and the upload goes on, its hash right, after the agent restarts; the
// whole file, matching its sha256, is converted, the image is Ready with
// its disk's size, the staged file goes, and a VM boots from a disk of it.
func TestACustomImageIsUploadedChunkByChunk(t *testing.T) {
	w := newWorld(t)
	file := bytes.Repeat([]byte("nixos-25.05 "), 250)
	size := int64(len(file))
	uid := w.uploadImage("nixos", size, sumOf(file))

	if n, err := w.putChunk("nixos", file, 0, 1000, ""); err != nil || n != 1000 {
		t.Fatalf("the first chunk: %d %v", n, err)
	}
	var off *uploadwire.OffsetError
	if _, err := w.putChunk("nixos", file, 2000, 1000, ""); !errors.As(err, &off) || off.ReceivedBytes != 1000 {
		t.Errorf("a chunk at the wrong offset: %v, want a 409 at 1000", err)
	}
	if _, err := w.putChunk("nixos", file, 1000, 1000, sumOf([]byte("garbled"))); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a chunk not matching its sha256: %v, want invalid_argument", err)
	}
	if n, err := w.uploadedBytes("nixos"); err != nil || n != 1000 {
		t.Errorf("after the chunks refused: %d %v, want 1000", n, err)
	}
	ci := w.waitCustomImage("nixos", "reporting 1000 bytes", func(ci *storagev1alpha1.CustomImage) bool {
		return ci.GetStatus().GetReceivedBytes() == 1000 && readyReason(ci) == computeapi.ReasonAwaitingUpload
	})
	if exp := ci.GetStatus().GetUploadExpires().AsTime(); exp.Sub(ci.GetMetadata().GetCreateTime().AsTime()) != 24*time.Hour {
		t.Errorf("upload_expires %v, want 24h after the image was made", exp)
	}
	if c := imageCondition(ci, computeapi.CustomImageUploaded); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != computeapi.ReasonAwaitingUpload {
		t.Errorf("Uploaded %v, want False AwaitingUpload", c)
	}

	// The agent restarts: the upload goes on from what it kept.
	w.stopAgent()
	w.startAgent()
	testutil.Eventually(t, "the restarted agent's count", func() (bool, string) {
		n, err := w.uploadedBytes("nixos")
		return err == nil && n == 1000, fmt.Sprint(n, err)
	})
	if n, err := w.putChunk("nixos", file, 1000, size-1000, ""); err != nil || n != size {
		t.Fatalf("the rest after the restart: %d %v", n, err)
	}

	ci = w.waitCustomImage("nixos", "Ready", computeapi.CustomImageReadyNow)
	st := ci.GetStatus()
	if !conditions.IsTrue(st.GetConditions(), computeapi.CustomImageUploaded) || st.GetSha256() != sumOf(file) || st.GetReceivedBytes() != size {
		t.Errorf("status %v; want Uploaded with the file's sha256 and every byte", st)
	}
	if st.GetDiskSize() != "1Gi" || st.GetUsedSize() == "" {
		t.Errorf("disk_size %q and used_size %q; want the converted disk's", st.GetDiskSize(), st.GetUsedSize())
	}
	if w.host.Inspected(uid) != 1 || w.host.Converted(uid) != 1 {
		t.Errorf("inspected %d and converted %d times, want once each", w.host.Inspected(uid), w.host.Converted(uid))
	}
	key := driver.CustomImageBase(uid)
	if bases := w.host.Snapshot().ImageBases; !slices.Contains(bases, key) {
		t.Errorf("image bases %v, want %s", bases, key)
	}
	testutil.Eventually(t, "the staged file gone once Ready", func() (bool, string) {
		s := w.staged()
		return len(s) == 0, fmt.Sprint(s)
	})
	if _, err := w.putChunk("nixos", file, 0, 1000, ""); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a chunk of an image whose file is whole: %v, want failed_precondition", err)
	}

	w.applyVMOfCustomImage("box", "nixos", "2Gi", vmRunning)
	w.waitVMReason("box", "Running")
	root, _ := get[*storagev1alpha1.Disk](w, registry.Disk, "box-root")
	if vol := w.host.Snapshot().Volumes[root.GetMetadata().GetUid()]; vol.Digest != key {
		t.Errorf("the VM's root volume %+v, want a clone of %s", vol, key)
	}
}

// TestAnUploadThatFailsFailsForGood: a whole file not of its sha256, an
// upload not whole by its expiry, and files the node refuses fail with
// their reason, their staged file and any base gone, and take no chunk.
func TestAnUploadThatFailsFailsForGood(t *testing.T) {
	w := newIdleWorld(t)
	w.uploadExpiry = 2 * time.Second
	w.maxImageDisk = 1 << 30
	w.startAgent()
	file := bytes.Repeat([]byte("x"), 1500)
	failed := func(name, reason string) *storagev1alpha1.CustomImage {
		t.Helper()
		ci := w.waitCustomImage(name, "failed as "+reason, computeapi.CustomImageFailed)
		if got := readyReason(ci); got != reason {
			t.Errorf("%s failed as %s, want %s: %s", name, got, reason, imageCondition(ci, computeapi.CustomImageReady).GetMessage())
		}
		if _, err := w.putChunk(name, file, 0, 1, ""); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("a chunk of %s once failed: %v, want failed_precondition", name, err)
		}
		return ci
	}

	// Its file not of its sha256.
	uid := w.uploadImage("mismatch", int64(len(file)), sumOf([]byte("another file")))
	if _, err := w.putChunk("mismatch", file, 0, int64(len(file)), ""); err != nil {
		t.Fatal(err)
	}
	ci := failed("mismatch", computeapi.ReasonDigestMismatch)
	if c := imageCondition(ci, computeapi.CustomImageUploaded); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Errorf("Uploaded %v of a file not of its sha256", c)
	}
	if w.host.Inspected(uid) != 0 {
		t.Error("a file not of its sha256 was inspected")
	}

	// Not whole in time.
	w.uploadImage("late", int64(len(file)), "")
	if _, err := w.putChunk("late", file, 0, 500, ""); err != nil {
		t.Fatal(err)
	}
	failed("late", computeapi.ReasonUploadExpired)

	// What the converter says the file is, refused.
	for _, tt := range []struct {
		name, reason string
		info         *driver.ImageInfo
		convert      error
	}{
		{"datafile", computeapi.ReasonInvalidImage, &driver.ImageInfo{Format: "qcow2", VirtualSize: 1 << 20, DataFile: "/dev/vda"}, nil},
		{"backing", computeapi.ReasonInvalidImage, &driver.ImageInfo{Format: "qcow2", VirtualSize: 1 << 20, BackingFile: "base.qcow2"}, nil},
		{"vmdk", computeapi.ReasonInvalidImage, &driver.ImageInfo{Format: "vmdk", VirtualSize: 1 << 20}, nil},
		{"huge", computeapi.ReasonImageTooLarge, &driver.ImageInfo{Format: "qcow2", VirtualSize: 1<<30 + 1}, nil},
		{"broken", computeapi.ReasonConvertFailed, nil, errors.New("qemu-img convert: exit status 1")},
	} {
		w.host.SetImageInfo(tt.info)
		w.host.FailConvert(tt.convert)
		uid := w.uploadImage(tt.name, int64(len(file)), sumOf(file))
		if _, err := w.putChunk(tt.name, file, 0, int64(len(file)), ""); err != nil {
			t.Fatal(err)
		}
		failed(tt.name, tt.reason)
		if bases := w.host.Snapshot().ImageBases; slices.Contains(bases, driver.CustomImageBase(uid)) {
			t.Errorf("%s left its base", tt.name)
		}
	}
	w.host.SetImageInfo(nil)
	w.host.FailConvert(nil)
	testutil.Eventually(t, "every staged file gone", func() (bool, string) {
		s := w.staged()
		return len(s) == 0, fmt.Sprint(s)
	})
	testutil.Eventually(t, "failed uploads counting nothing", func() (bool, string) {
		u := w.projectUsage()
		return u.GetCustomImages() == 0 && (u.GetDisk() == "" || u.GetDisk() == "0"), fmt.Sprint(u)
	})
}

// TestADeletedUploadLeavesNothing: an upload's image deleted part-way, its
// staged file goes, and a chunk of it is refused.
func TestADeletedUploadLeavesNothing(t *testing.T) {
	w := newWorld(t)
	file := bytes.Repeat([]byte("y"), 2000)
	w.uploadImage("gone", int64(len(file)), "")
	if _, err := w.putChunk("gone", file, 0, 1000, ""); err != nil {
		t.Fatal(err)
	}
	if s := w.staged(); len(s) == 0 {
		t.Fatal("nothing staged")
	}
	if _, err := w.root.Delete(context.Background(), registry.CustomImage, client.Key{Project: project, Name: "gone"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the staged file gone with its image", func() (bool, string) {
		s := w.staged()
		return len(s) == 0, fmt.Sprint(s)
	})
	if _, err := w.putChunk("gone", file, 1000, 1000, ""); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a chunk of a deleted image: %v, want not_found", err)
	}
}

// holdGets keeps the agent's GetCustomImage calls from going out until
// release is closed, so its controller sees no image meanwhile.
type holdGets struct {
	next    http.RoundTripper
	release <-chan struct{}
}

func (h holdGets) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/GetCustomImage") {
		select {
		case <-h.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return h.next.RoundTrip(r)
}

// TestAChunkBeforeTheNodeSawItsImage: a chunk sent as soon as its image is
// made counts, though the node's controller has not looked at the image
// yet: the node finds the image itself.
func TestAChunkBeforeTheNodeSawItsImage(t *testing.T) {
	w := newIdleWorld(t)
	release := make(chan struct{})
	w.wrap = func(next http.RoundTripper) http.RoundTripper { return holdGets{next: next, release: release} }
	w.startAgent()
	defer close(release)
	file := bytes.Repeat([]byte("z"), 1000)
	w.uploadImage("early", int64(len(file)), "")
	if n, err := w.uploadedBytes("early"); err != nil || n != 0 {
		t.Errorf("where an upload the controller has not seen is: %d %v, want 0", n, err)
	}
	if n, err := w.putChunk("early", file, 0, 500, ""); err != nil || n != 500 {
		t.Errorf("a chunk the controller has not seen the image of: %d %v, want it counted", n, err)
	}
}

// TestAnUploadPastTheDiskQuotaIsNotConverted: the virtual size the node
// reads of a file counts toward the disk quota before any base is made;
// past the project's limit the image fails as QuotaExceeded, is never
// converted, leaves no base nor staged file, and counts nothing. One that
// fits is converted, and settles at what it holds.
func TestAnUploadPastTheDiskQuotaIsNotConverted(t *testing.T) {
	w := newWorld(t)
	obj, err := w.root.Get(context.Background(), registry.Project, client.Key{Name: project})
	if err != nil {
		t.Fatal(err)
	}
	p := obj.(*tenancyv1alpha1.Project)
	p.Spec.Limits = &tenancyv1alpha1.Limits{Disk: proto.String("2Gi")}
	p.Metadata.ResourceVersion = 0
	if _, err := w.root.Apply(context.Background(), registry.Project, p); err != nil {
		t.Fatal(err)
	}
	file := bytes.Repeat([]byte("q"), 1500)

	w.host.SetImageInfo(&driver.ImageInfo{Format: "qcow2", VirtualSize: 2<<30 + 1})
	uid := w.uploadImage("big", int64(len(file)), "")
	if _, err := w.putChunk("big", file, 0, int64(len(file)), ""); err != nil {
		t.Fatal(err)
	}
	ci := w.waitCustomImage("big", "failed", computeapi.CustomImageFailed)
	if c := imageCondition(ci, computeapi.CustomImageReady); c.GetReason() != computeapi.ReasonQuotaExceeded || !strings.Contains(c.GetMessage(), "disk limit is 2Gi") {
		t.Errorf("Ready %v, want QuotaExceeded naming the project's limit", c)
	}
	if !conditions.IsTrue(ci.GetStatus().GetConditions(), computeapi.CustomImageUploaded) || ci.GetStatus().GetVirtualSize() != 2<<30+1 {
		t.Errorf("status %v, want Uploaded and the virtual size read", ci.GetStatus())
	}
	testutil.Eventually(t, "the staged file gone and nothing counted", func() (bool, string) {
		s, u := w.staged(), w.projectUsage()
		return len(s) == 0 && u.GetCustomImages() == 0 && (u.GetDisk() == "" || u.GetDisk() == "0"), fmt.Sprint(s, u)
	})
	if w.host.Converted(uid) != 0 || slices.Contains(w.host.Snapshot().ImageBases, driver.CustomImageBase(uid)) {
		t.Errorf("converted %d times, bases %v: want no base made", w.host.Converted(uid), w.host.Snapshot().ImageBases)
	}

	w.host.SetImageInfo(&driver.ImageInfo{Format: "qcow2", VirtualSize: 2 << 30})
	w.uploadImage("fits", int64(len(file)), "")
	if _, err := w.putChunk("fits", file, 0, int64(len(file)), ""); err != nil {
		t.Fatal(err)
	}
	ci = w.waitCustomImage("fits", "Ready", computeapi.CustomImageReadyNow)
	testutil.Eventually(t, "the image counted at what it holds", func() (bool, string) {
		u := w.projectUsage()
		return u.GetCustomImages() == 1 && u.GetDisk() == ci.GetStatus().GetUsedSize(), fmt.Sprint(u, ci.GetStatus().GetUsedSize())
	})
}

// TestAFileReadAgainToAnotherSizeFails: after the agent restarts, the file
// is read again before it is converted; one found to hold another disk
// than the virtual size kept fails, rather than filling a base the quota
// did not count.
func TestAFileReadAgainToAnotherSizeFails(t *testing.T) {
	w := newWorld(t)
	file := bytes.Repeat([]byte("r"), 1500)
	w.host.SetImageInfo(&driver.ImageInfo{Format: "qcow2", VirtualSize: 1 << 30})
	release := w.host.HoldConverts()
	defer release()
	w.uploadImage("again", int64(len(file)), "")
	if _, err := w.putChunk("again", file, 0, int64(len(file)), ""); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the conversion under way", func() (bool, string) {
		return w.host.ConvertsUnderWay() == 1, fmt.Sprint(w.host.ConvertsUnderWay())
	})
	w.stopAgent()
	w.host.SetImageInfo(&driver.ImageInfo{Format: "qcow2", VirtualSize: 3 << 30})
	w.startAgent()
	ci := w.waitCustomImage("again", "failed", computeapi.CustomImageFailed)
	if c := imageCondition(ci, computeapi.CustomImageReady); c.GetReason() != computeapi.ReasonConvertFailed || !strings.Contains(c.GetMessage(), "not the 1073741824") {
		t.Errorf("Ready %v, want ConvertFailed naming the size kept", c)
	}
	if ci.GetStatus().GetVirtualSize() != 1<<30 {
		t.Errorf("virtual_size %d, want the one kept", ci.GetStatus().GetVirtualSize())
	}
}

// TestReadingAndConvertingAFileAreBoundedApart: reading what a file is
// has a bound of its own, --inspect-timeout, and converting it another,
// --convert-timeout; past either the image fails as ConvertFailed, saying
// which, and leaves no base.
func TestReadingAndConvertingAFileAreBoundedApart(t *testing.T) {
	w := newIdleWorld(t)
	w.inspectTimeout, w.convertTimeout = time.Second, 2*time.Second
	w.startAgent()
	file := bytes.Repeat([]byte("s"), 1500)
	for _, tt := range []struct {
		name, says string
		hold       func() func()
	}{
		{"stalls-reading", "reading the file took longer than 1s", w.host.HoldInspects},
		{"stalls-converting", "the conversion took longer than 2s", w.host.HoldConverts},
	} {
		release := tt.hold()
		uid := w.uploadImage(tt.name, int64(len(file)), "")
		if _, err := w.putChunk(tt.name, file, 0, int64(len(file)), ""); err != nil {
			t.Fatal(err)
		}
		ci := w.waitCustomImage(tt.name, "failed", computeapi.CustomImageFailed)
		release()
		if c := imageCondition(ci, computeapi.CustomImageReady); c.GetReason() != computeapi.ReasonConvertFailed || !strings.Contains(c.GetMessage(), tt.says) {
			t.Errorf("%s: Ready %v, want ConvertFailed saying %q", tt.name, c, tt.says)
		}
		if slices.Contains(w.host.Snapshot().ImageBases, driver.CustomImageBase(uid)) {
			t.Errorf("%s left its base", tt.name)
		}
	}
}

// TestANodeThatTakesNoUploads: a node without a converter says it takes no
// uploads, so none is placed on it; one placed there before still expires,
// rather than wait for ever.
func TestANodeThatTakesNoUploads(t *testing.T) {
	w := newIdleWorld(t)
	w.uploadExpiry = 3 * time.Second
	w.startAgent()
	file := bytes.Repeat([]byte("t"), 1500)
	w.uploadImage("before", int64(len(file)), "")
	if _, err := w.putChunk("before", file, 0, 500, ""); err != nil {
		t.Fatal(err)
	}

	w.stopAgent()
	w.uploadDir = ""
	w.startAgent()
	testutil.Eventually(t, "the node saying it takes no uploads", func() (bool, string) {
		obj, err := w.root.Get(context.Background(), registry.Node, client.Key{Name: apiservertest.Node})
		if err != nil {
			return false, err.Error()
		}
		n := obj.(*computev1alpha1.Node)
		return !n.GetStatus().GetCustomImageUploads() && conditions.IsTrue(n.GetStatus().GetConditions(), "Ready"), fmt.Sprint(n.GetStatus())
	})
	_, err := w.root.Create(context.Background(), registry.CustomImage, &storagev1alpha1.CustomImage{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "after"},
		Spec: &storagev1alpha1.CustomImageSpec{
			Source: &storagev1alpha1.CustomImageSource{Source: &storagev1alpha1.CustomImageSource_Upload{
				Upload: &storagev1alpha1.CustomImageUploadSource{Size: int64(len(file))},
			}},
			DefaultUser: "root",
		},
	})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no node takes uploads") {
		t.Errorf("an upload with no node taking uploads: %v, want failed_precondition saying so", err)
	}
	ci := w.waitCustomImage("before", "expired", computeapi.CustomImageFailed)
	if got := readyReason(ci); got != computeapi.ReasonUploadExpired {
		t.Errorf("the upload placed before failed as %s, want UploadExpired", got)
	}
}
