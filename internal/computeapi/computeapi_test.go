package computeapi_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

// TestDiskSerials: a name of up to 20 bytes is its disk's serial, a longer
// one gives the last 20 hex digits of the disk's uid, and so does a name
// that is another disk's such digits; every serial is one CheckDiskSerial
// takes.
func TestDiskSerials(t *testing.T) {
	const (
		u1 = "01a16ef7-7689-782a-ad71-35c14fc6c594"
		u2 = "01a16ef7-7689-782a-bd71-35c14fc6c595"
	)
	for _, tt := range []struct {
		names, want []string
	}{
		{[]string{"web-root", "data"}, []string{"web-root", "data"}},
		{[]string{"twenty-bytes-exactly", "twenty-one-bytes-long"}, []string{"twenty-bytes-exactly", "782abd7135c14fc6c595"}},
		// Two names past 20 bytes, of disks made in the same millisecond:
		// their uids differ only past the first 20 characters.
		{[]string{"a-very-long-root-disk-name", "another-very-long-data-disk"}, []string{"782aad7135c14fc6c594", "782abd7135c14fc6c595"}},
		// A name that is another disk's uid digits gives way.
		{[]string{"782aad7135c14fc6c594-root-of-it", "782aad7135c14fc6c594"}, []string{"782aad7135c14fc6c594", "782abd7135c14fc6c595"}},
	} {
		got := computeapi.DiskSerials(tt.names, []string{u1, u2})
		if !slices.Equal(got, tt.want) {
			t.Errorf("%q: serials %q, want %q", tt.names, got, tt.want)
		}
		for _, s := range got {
			if err := computeapi.CheckDiskSerial(s); err != nil {
				t.Errorf("%q: %v", tt.names, err)
			}
		}
	}
	for _, bad := range []string{"ABC", "a/b", "a,b", "a b", strings.Repeat("a", 21)} {
		if computeapi.CheckDiskSerial(bad) == nil {
			t.Errorf("CheckDiskSerial takes %q", bad)
		}
	}
}

func TestVirtioDevice(t *testing.T) {
	for n, want := range map[int]string{
		0: "vda", 2: "vdc", 25: "vdz", 26: "vdaa", 27: "vdab", 51: "vdaz", 52: "vdba", 701: "vdzz", 702: "vdaaa",
	} {
		if got := computeapi.VirtioDevice(n); got != want {
			t.Errorf("virtio disk %d: %s, want %s", n, got, want)
		}
	}
}
