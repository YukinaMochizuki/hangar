package computeapi

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
)

// What a DiskGrow's empty fields are, and the bounds of the ones set
// (docs/decisions/disk-autogrow.md).
const (
	DefaultDiskGrowThreshold = 80
	DefaultDiskGrowStep      = "2x"

	minDiskGrowThreshold = 50
	maxDiskGrowThreshold = 95
	maxDiskGrowFactor    = 16
	minDiskGrowStep      = quantity.GiB
)

// RootGrowth is the condition a container machine carries, and its
// container with it, once its root disk failed to grow while its VM ran:
// False, with ReasonVMCannotResize or ReasonGuestCannotGrow, until a later
// boot grows it, then True, with ReasonRootGrew. A machine whose root never
// failed to grow has none.
const RootGrowth = "RootGrowth"

// Reasons of RootGrowth.
const (
	// ReasonVMCannotResize is a VM started before it could be told its root
	// disk's size while it runs.
	ReasonVMCannotResize = "VMCannotResize"
	// ReasonGuestCannotGrow is a guest whose hangar-init can neither say how
	// full its root is nor grow it while it runs.
	ReasonGuestCannotGrow = "GuestCannotGrow"
	// ReasonRootGrew is a root disk grown while the VM runs, in its current
	// boot.
	ReasonRootGrew = "RootGrew"
)

// DiskGrowth is a DiskGrow read as numbers, with the defaults for what it
// leaves empty.
type DiskGrowth struct {
	// Max is the largest the disk grows to, in bytes.
	Max uint64
	// Threshold is how full the disk is, in percent, when it grows.
	Threshold uint64
	// Factor is what the size is multiplied by, if the step is one; else
	// Step is the bytes added.
	Factor *big.Rat
	Step   uint64
}

// ParseDiskGrow reads g, of a root disk that starts at disk bytes; field is
// its path in the spec, for errors. A nil g is no growth: ok is false.
func ParseDiskGrow(g *computev1alpha1.DiskGrow, disk uint64, field string) (dg DiskGrowth, ok bool, err error) {
	if g == nil {
		return DiskGrowth{}, false, nil
	}
	if g.GetMax() == "" {
		return DiskGrowth{}, false, fmt.Errorf("%s.max: required", field)
	}
	if dg.Max, err = quantity.Bytes(g.GetMax()); err != nil {
		return DiskGrowth{}, false, fmt.Errorf("%s.max: %w", field, err)
	}
	if dg.Max < disk {
		return DiskGrowth{}, false, fmt.Errorf("%s.max: %s is less than the disk's %s", field, g.GetMax(), quantity.FormatDisk(disk))
	}
	dg.Threshold = DefaultDiskGrowThreshold
	if t := g.GetThreshold(); t != "" {
		n, perr := strconv.ParseUint(strings.TrimSuffix(t, "%"), 10, 8)
		if !strings.HasSuffix(t, "%") || perr != nil || n < minDiskGrowThreshold || n > maxDiskGrowThreshold {
			return DiskGrowth{}, false, fmt.Errorf("%s.threshold: %q is not a whole percentage from %d%% to %d%%", field, t, minDiskGrowThreshold, maxDiskGrowThreshold)
		}
		dg.Threshold = n
	}
	step := g.GetStep()
	if step == "" {
		step = DefaultDiskGrowStep
	}
	if f, isFactor := strings.CutSuffix(step, "x"); isFactor {
		r, ok := new(big.Rat).SetString(f)
		if !ok || strings.ContainsAny(f, "/eE") || r.Cmp(big.NewRat(1, 1)) <= 0 || r.Cmp(big.NewRat(maxDiskGrowFactor, 1)) > 0 {
			return DiskGrowth{}, false, fmt.Errorf("%s.step: %q is not a factor over 1x and at most %dx", field, step, maxDiskGrowFactor)
		}
		dg.Factor = r
		return dg, true, nil
	}
	if dg.Step, err = quantity.Bytes(step); err != nil {
		return DiskGrowth{}, false, fmt.Errorf("%s.step: %q is neither a factor such as 2x nor a quantity: %w", field, step, err)
	}
	if dg.Step < minDiskGrowStep {
		return DiskGrowth{}, false, fmt.Errorf("%s.step: %s is less than 1Gi", field, step)
	}
	return dg, true, nil
}

// Next is the size a disk of size bytes grows to: one step, in whole Gi
// rounded up, and no more than Max in whole Gi rounded down, as a disk's
// size is whole Gi. It is size itself once the disk is there.
func (dg DiskGrowth) Next(size uint64) uint64 {
	limit := dg.Max / quantity.GiB * quantity.GiB
	if size >= limit {
		return size
	}
	var next uint64
	if dg.Factor != nil {
		v := new(big.Rat).Mul(new(big.Rat).SetInt(new(big.Int).SetUint64(size)), dg.Factor)
		q, m := new(big.Int).QuoRem(v.Num(), v.Denom(), new(big.Int))
		if m.Sign() != 0 {
			q.Add(q, big.NewInt(1))
		}
		if !q.IsUint64() {
			return limit
		}
		next = q.Uint64()
	} else {
		next = size + dg.Step
		if next < size {
			return limit
		}
	}
	if next >= limit {
		return limit
	}
	next = (next + quantity.GiB - 1) / quantity.GiB * quantity.GiB
	return min(next, limit)
}

// Due says whether a filesystem of size bytes with used of them in use is
// full enough to grow.
func (dg DiskGrowth) Due(used, size uint64) bool {
	if size == 0 {
		return false
	}
	// used*100 >= size*threshold, without overflowing.
	u := new(big.Int).Mul(new(big.Int).SetUint64(used), big.NewInt(100))
	s := new(big.Int).Mul(new(big.Int).SetUint64(size), new(big.Int).SetUint64(dg.Threshold))
	return u.Cmp(s) >= 0
}
