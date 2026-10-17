package computeapi

import (
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
)

// MaxPlacementLen bounds each field of a Placement, in characters.
const MaxPlacementLen = 64

var chassisRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// PlacementField is one field of a Placement, by its proto name.
type PlacementField struct {
	Name  string
	Value *string
}

// PlacementFields lists p's fields, each pointing at its value.
func PlacementFields(p *computev1alpha1.Placement) []PlacementField {
	return []PlacementField{
		{"region", &p.Region}, {"site", &p.Site}, {"cpu_model", &p.CpuModel},
		{"cpu_generation", &p.CpuGeneration}, {"storage", &p.Storage}, {"chassis", &p.Chassis},
		{"memory", &p.Memory}, {"network", &p.Network},
	}
}

// CheckPlacementValue checks the value of a Placement's field name: at most
// MaxPlacementLen printable characters, and a chassis a DNS-1123 label.
// Empty is always valid.
func CheckPlacementValue(name, v string) error {
	if v == "" {
		return nil
	}
	if n := utf8.RuneCountInString(v); n > MaxPlacementLen {
		return fmt.Errorf("%s is %d characters, more than %d", name, n, MaxPlacementLen)
	}
	for _, r := range v {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("%s %q holds a character that is not printable", name, v)
		}
	}
	if name == "chassis" && !chassisRE.MatchString(v) {
		return fmt.Errorf("chassis %q is not a DNS-1123 label", v)
	}
	return nil
}

// CheckPlacement checks every field of p, which may be nil.
func CheckPlacement(p *computev1alpha1.Placement) error {
	if p == nil {
		return nil
	}
	for _, f := range PlacementFields(p) {
		if err := CheckPlacementValue(f.Name, *f.Value); err != nil {
			return err
		}
	}
	return nil
}
