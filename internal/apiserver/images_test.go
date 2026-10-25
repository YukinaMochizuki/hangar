package apiserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

var ubuntuSHA = strings.Repeat("ab", 32)

func image(name string) *storagev1alpha1.Image {
	return &storagev1alpha1.Image{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec: &storagev1alpha1.ImageSpec{
			Url: "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img", Sha256: ubuntuSHA,
			Os: "ubuntu", Version: "24.04", Arch: "amd64", MinDiskSize: "4Gi",
		},
	}
}

func diskOf(name, size, img string) *storagev1alpha1.Disk {
	d := disk(name, size)
	d.Spec.Source = &storagev1alpha1.DiskSource{Source: &storagev1alpha1.DiskSource_Image{Image: &storagev1alpha1.ImageSource{Name: img}}}
	return d
}

// reportImage writes an image's status as a node does.
func reportImage(t *testing.T, s *Server, system context.Context, name, diskSize string, c *metav1.Condition) {
	t.Helper()
	obj, err := s.get(system, registry.Image, resource.Key{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	img := obj.(*storagev1alpha1.Image)
	c.ObservedGeneration = img.GetMetadata().GetGeneration()
	status := &storagev1alpha1.ImageStatus{DiskSize: diskSize, Conditions: conditions.Set(nil, c, time.Now())}
	if _, err := s.backend(registry.Image).UpdateStatus(asNode(system, "n1"), resource.Key{Name: name}, status, nil); err != nil {
		t.Fatal(err)
	}
}

func TestImageAdmission(t *testing.T) {
	t.Parallel()
	s, system, _ := running(t)
	b := s.backend(registry.Image)
	if _, err := b.Apply(system, image("ubuntu-24-04")); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*storagev1alpha1.ImageSpec){
		"an http URL":           func(s *storagev1alpha1.ImageSpec) { s.Url = "http://example.com/a.img" },
		"no URL":                func(s *storagev1alpha1.ImageSpec) { s.Url = "" },
		"a URL with a password": func(s *storagev1alpha1.ImageSpec) { s.Url = "https://u:p@example.com/a.img" },
		"no sha256":             func(s *storagev1alpha1.ImageSpec) { s.Sha256 = "" },
		"an uppercase sha256":   func(s *storagev1alpha1.ImageSpec) { s.Sha256 = strings.ToUpper(ubuntuSHA) },
		"a short sha256":        func(s *storagev1alpha1.ImageSpec) { s.Sha256 = ubuntuSHA[:63] },
		"arm64":                 func(s *storagev1alpha1.ImageSpec) { s.Arch = "arm64" },
		"a minimum below 1Gi":   func(s *storagev1alpha1.ImageSpec) { s.MinDiskSize = "512Mi" },
		"a minimum unreadable":  func(s *storagev1alpha1.ImageSpec) { s.MinDiskSize = "big" },
	} {
		img := image("bad")
		mutate(img.Spec)
		_, err := b.Apply(system, img)
		wantCodeOf(t, err, connect.CodeInvalidArgument, name)
	}
	ok := image("plain")
	ok.Spec.Arch, ok.Spec.MinDiskSize, ok.Spec.Os, ok.Spec.Version = "", "", "", ""
	if _, err := b.Apply(system, ok); err != nil {
		t.Errorf("an image with no metadata: %v", err)
	}
}

// TestOnlyAdministratorsWriteImages: the catalogue is the platform's;
// everyone allowed to use Hangar reads it, and nodes report on it.
func TestOnlyAdministratorsWriteImages(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	root := asRoot(t, s, system)
	b := s.backend(registry.Image)
	key := resource.Key{Name: "ubuntu-24-04"}
	token := auth.WithPrincipal(system, auth.Principal{Type: auth.ServiceToken, ID: "ci.access"})
	outsider := auth.WithPrincipal(system, auth.Principal{Type: auth.User, ID: "nobody@example.com"})

	// alice owns a tenant and is a member, which writes no image.
	_, err := b.Apply(user, image("ubuntu-24-04"))
	wantCodeOf(t, err, connect.CodePermissionDenied, "a tenant owner adding an image")
	if _, err := b.Apply(root, image("ubuntu-24-04")); err != nil {
		t.Fatalf("an administrator adding an image: %v", err)
	}
	for who, ctx := range map[string]context.Context{"a tenant owner": user, "a service token": token, "a node": asNode(system, "n1")} {
		if _, err := b.Get(ctx, key); err != nil {
			t.Errorf("%s reading an image: %v", who, err)
		}
		if list, err := b.List(ctx, resource.ListOptions{}); err != nil || len(list.Items) != 1 {
			t.Errorf("%s listing images: %v %v", who, list.Items, err)
		}
	}
	for who, ctx := range map[string]context.Context{"a tenant owner": user, "a service token": token} {
		changed := image("ubuntu-24-04")
		changed.Spec.Url = "https://evil.example/a.img"
		_, err := b.Apply(ctx, changed)
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" changing an image")
		_, err = b.Delete(ctx, key, nil)
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" deleting an image")
		_, err = b.UpdateStatus(ctx, key, &storagev1alpha1.ImageStatus{DiskSize: "1Gi"}, nil)
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" reporting on an image")
	}
	_, err = b.Get(outsider, key)
	wantCodeOf(t, err, connect.CodePermissionDenied, "someone not a platform member reading an image")

	// Any node reports on it: what it found holds on every node.
	for _, node := range []string{"n1", "n2"} {
		if _, err := b.UpdateStatus(asNode(system, node), key, &storagev1alpha1.ImageStatus{DiskSize: "3Gi"}, nil); err != nil {
			t.Errorf("node %s reporting on an image: %v", node, err)
		}
	}
	if _, err := b.Delete(root, key, nil); err != nil {
		t.Errorf("an administrator deleting an image: %v", err)
	}
}

func TestADiskFromAnImage(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	disks := s.backend(registry.Disk)
	if _, err := s.backend(registry.Image).Apply(system, image("ubuntu-24-04")); err != nil {
		t.Fatal(err)
	}

	_, err := disks.Apply(user, diskOf("a", "10Gi", "nope"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a disk of an image that does not exist")
	_, err = disks.Apply(user, diskOf("a", "10Gi", "Bad_Name"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a disk of an image by an invalid name")
	_, err = disks.Apply(user, diskOf("a", "3Gi", "ubuntu-24-04"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a disk below the image's minimum")
	if _, err := disks.Apply(user, diskOf("a", "4Gi", "ubuntu-24-04")); err != nil {
		t.Fatalf("a disk at the image's minimum: %v", err)
	}
	_, err = disks.Apply(user, disk("a", "4Gi"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "dropping a disk's image")

	// A node found the image holds 5Gi: new disks and resizes must take it,
	// but a disk kept as it is passes.
	reportImage(t, s, system, "ubuntu-24-04", "5Gi", &metav1.Condition{
		Type: computeapi.ImageVerified, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Made",
	})
	_, err = disks.Apply(user, diskOf("b", "4Gi", "ubuntu-24-04"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a disk below the image's own size")
	if _, err := disks.Apply(user, diskOf("a", "4Gi", "ubuntu-24-04")); err != nil {
		t.Errorf("a disk kept at its size after its image grew: %v", err)
	}
	if _, err := disks.Apply(user, diskOf("a", "6Gi", "ubuntu-24-04")); err != nil {
		t.Errorf("growing a disk past its image: %v", err)
	}

	// An image found at fault takes no new disk, until its spec changes.
	reportImage(t, s, system, "ubuntu-24-04", "", &metav1.Condition{
		Type: computeapi.ImageVerified, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: computeapi.ReasonHashMismatch,
		Message: "the file has another sha256",
	})
	_, err = disks.Apply(user, diskOf("c", "10Gi", "ubuntu-24-04"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a disk of an image found at fault")
	if err == nil || !strings.Contains(err.Error(), "HashMismatch") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	fixed := image("ubuntu-24-04")
	fixed.Spec.Sha256 = strings.Repeat("cd", 32)
	if _, err := s.backend(registry.Image).Apply(system, fixed); err != nil {
		t.Fatal(err)
	}
	if _, err := disks.Apply(user, diskOf("c", "10Gi", "ubuntu-24-04")); err != nil {
		t.Errorf("a disk of an image fixed since: %v", err)
	}

	// An image being deleted takes no new disk.
	if _, err := s.backend(registry.Image).Apply(system, &storagev1alpha1.Image{
		Metadata: &metav1.ObjectMeta{Name: "going", Finalizers: []string{"test/hold"}}, Spec: image("going").Spec,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Image).Delete(system, resource.Key{Name: "going"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = disks.Apply(user, diskOf("d", "10Gi", "going"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a disk of an image being deleted")
}
