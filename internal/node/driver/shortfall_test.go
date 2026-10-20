package driver_test

import (
	"errors"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestAnUnpackShortfall: an unpack out of room is known to need a byte more
// than was free, and asks for its compressed size more.
func TestAnUnpackShortfall(t *testing.T) {
	const mib = 1 << 20
	out := errors.New("no space left on device")
	for name, tt := range map[string]struct {
		size, free, need int64
	}{
		"of 20 MiB":     {20 * mib, 22 * mib, 42 * mib},
		"of no size":    {0, 22 * mib, 22*mib + 1},
		"with no room":  {20 * mib, 0, 20 * mib},
		"of a big one":  {5 << 30, 100 * mib, 5<<30 + 100*mib},
		"of a tiny one": {1, 22 * mib, 22*mib + 1},
	} {
		t.Run(name, func(t *testing.T) {
			e := driver.UnpackShortfall("unpacking", tt.size, tt.free, out)
			if e.Room != driver.ImageRoom || e.AtLeast() != tt.free+1 || e.Need != tt.need || e.Free != tt.free || !errors.Is(e, driver.ErrNoSpace) {
				t.Errorf("%+v, want at least %d, asking %d", e, tt.free+1, tt.need)
			}
		})
	}
}
