package computeapi_test

import (
	"slices"
	"strings"
	"testing"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

// TestPlacementFieldsCoverPlacement: a field added to Placement is checked
// like the others, since it reaches every tenant.
func TestPlacementFieldsCoverPlacement(t *testing.T) {
	var p computev1alpha1.Placement
	fields := p.ProtoReflect().Descriptor().Fields()
	var names []string
	for _, f := range computeapi.PlacementFields(&p) {
		names = append(names, f.Name)
	}
	for i := range fields.Len() {
		if name := string(fields.Get(i).Name()); !slices.Contains(names, name) {
			t.Errorf("Placement.%s is not among PlacementFields", name)
		}
	}
}

func TestCheckPlacementValue(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		ok          bool
	}{
		{"region", "", true},
		{"storage", "2 × NVMe SSD", true},
		{"cpu_model", strings.Repeat("é", computeapi.MaxPlacementLen), true},
		{"cpu_model", strings.Repeat("e", computeapi.MaxPlacementLen+1), false},
		{"site", "site\na", false},
		{"site", "site\x1b[2J", false},
		{"site", "site\u200ba", false},
		{"memory", "DDR4-3200 ECC", true},
		{"memory", "DDR4\t3200", false},
		{"network", "2 × 10 GbE", true},
		{"network", strings.Repeat("1 × 10 GbE, ", 6), false},
		{"chassis", "generic-1u-2x-u2", true},
		{"chassis", "Generic 1U", false},
		{"chassis", "-1u", false},
	} {
		if err := computeapi.CheckPlacementValue(tt.name, tt.value); (err == nil) != tt.ok {
			t.Errorf("%s %q: %v", tt.name, tt.value, err)
		}
	}
}
