package driver_test

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestAStagedFileEndsInZeros: a staged file of any size is whole 4 KiB,
// with 4 KiB of zeros at least after the tenant's bytes.
func TestAStagedFileEndsInZeros(t *testing.T) {
	for size, want := range map[int64]int64{1: 12288, 4095: 12288, 4096: 12288, 4097: 16384, 1 << 20: 1<<20 + 8192} {
		got := driver.UploadedLength(size)
		if got != want || got%4096 != 0 || got-driver.UploadOffset-size < 4096 {
			t.Errorf("a file of %d staged in %d bytes, want %d", size, got, want)
		}
	}
}
