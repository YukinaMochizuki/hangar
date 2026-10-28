package computeapi_test

import (
	"slices"
	"strings"
	"testing"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

const gi = quantity.GiB

// TestADiskGrowsInSteps: a disk grows by its factor or its quantity, in
// whole Gi rounded up, and stops at its max.
func TestADiskGrowsInSteps(t *testing.T) {
	for _, tt := range []struct {
		step  string
		start uint64
		max   string
		want  []uint64
	}{
		{"", 2 * gi, "20Gi", []uint64{4, 8, 16, 20}},
		{"2x", 2 * gi, "20Gi", []uint64{4, 8, 16, 20}},
		{"3Gi", 2 * gi, "20Gi", []uint64{5, 8, 11, 14, 17, 20}},
		{"1.5x", 2 * gi, "10Gi", []uint64{3, 5, 8, 10}},
		// 1.1x of 2Gi is 2.2Gi: rounded up, it still grows.
		{"1.1x", 2 * gi, "4Gi", []uint64{3, 4}},
		// A start that is not whole Gi grows to whole Gi.
		{"1Gi", 1536 * quantity.MiB, "4Gi", []uint64{3, 4}},
		// A max that is not whole Gi is never passed: the disk stops below it.
		{"2x", 2 * gi, "5000Mi", []uint64{4}},
	} {
		dg, ok, err := computeapi.ParseDiskGrow(&computev1alpha1.DiskGrow{Max: tt.max, Step: tt.step}, tt.start, "f")
		if err != nil || !ok {
			t.Fatalf("step %q: %v %v", tt.step, ok, err)
		}
		size := tt.start
		var got []uint64
		for range 20 {
			next := dg.Next(size)
			if next == size {
				break
			}
			got = append(got, next/gi)
			size = next
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("step %q from %d: grew %v, want %v", tt.step, tt.start, got, tt.want)
		}
	}
}

// TestDiskGrowDefaults: an empty threshold is 80%, and growth is due at it,
// not below it.
func TestDiskGrowDefaults(t *testing.T) {
	dg, ok, err := computeapi.ParseDiskGrow(&computev1alpha1.DiskGrow{Max: "20Gi"}, 2*gi, "f")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if dg.Threshold != 80 {
		t.Errorf("threshold %d", dg.Threshold)
	}
	if dg.Due(79, 100) || !dg.Due(80, 100) || dg.Due(1, 0) {
		t.Error("due at the wrong fill")
	}
	if !dg.Due(^uint64(0), ^uint64(0)) {
		t.Error("a full disk at the largest size is not due")
	}
	if _, ok, err := computeapi.ParseDiskGrow(nil, 2*gi, "f"); ok || err != nil {
		t.Errorf("no DiskGrow: %v %v", ok, err)
	}
}

// TestDiskGrowRefusals: each field out of its bounds is refused, naming the
// field.
func TestDiskGrowRefusals(t *testing.T) {
	for _, tt := range []struct {
		g     *computev1alpha1.DiskGrow
		field string
	}{
		{&computev1alpha1.DiskGrow{}, "f.max"},
		{&computev1alpha1.DiskGrow{Max: "1Gi"}, "f.max"},
		{&computev1alpha1.DiskGrow{Max: "lots"}, "f.max"},
		{&computev1alpha1.DiskGrow{Max: "20Gi", Threshold: "80"}, "f.threshold"},
		{&computev1alpha1.DiskGrow{Max: "20Gi", Threshold: "49%"}, "f.threshold"},
		{&computev1alpha1.DiskGrow{Max: "20Gi", Threshold: "96%"}, "f.threshold"},
		{&computev1alpha1.DiskGrow{Max: "20Gi", Threshold: "80.5%"}, "f.threshold"},
		{&computev1alpha1.DiskGrow{Max: "20Gi", Step: "1x"}, "f.step"},
		{&computev1alpha1.DiskGrow{Max: "20Gi", Step: "17x"}, "f.step"},
		{&computev1alpha1.DiskGrow{Max: "20Gi", Step: "3/2x"}, "f.step"},
		{&computev1alpha1.DiskGrow{Max: "20Gi", Step: "512Mi"}, "f.step"},
		{&computev1alpha1.DiskGrow{Max: "20Gi", Step: "big"}, "f.step"},
	} {
		_, _, err := computeapi.ParseDiskGrow(tt.g, 2*gi, "f")
		if err == nil || !strings.HasPrefix(err.Error(), tt.field+":") {
			t.Errorf("%v: %v, want an error on %s", tt.g, err, tt.field)
		}
	}
	for _, ok := range []*computev1alpha1.DiskGrow{
		{Max: "2Gi"},
		{Max: "20Gi", Threshold: "50%", Step: "16x"},
		{Max: "20Gi", Threshold: "95%", Step: "1Gi"},
	} {
		if _, _, err := computeapi.ParseDiskGrow(ok, 2*gi, "f"); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
}
