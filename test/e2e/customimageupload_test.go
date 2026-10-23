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

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
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
