package converter

import "testing"

// TestAFileMustFitItsDisk: a disk shorter than the file's offset and size
// together is refused, one that holds them taken.
func TestAFileMustFitItsDisk(t *testing.T) {
	for _, tt := range []struct {
		have, offset, size int64
		ok                 bool
	}{
		{8192, 4096, 4096, true},
		{8192, 4096, 4097, false},
		{4096, 4096, 1, false},
		{1 << 20, 4096, 1<<20 - 4096, true},
		{100, 4096, 1, false},
	} {
		if err := fits(tt.have, tt.offset, tt.size); (err == nil) != tt.ok {
			t.Errorf("a disk of %d, a file of %d at %d: %v, want taken %t", tt.have, tt.size, tt.offset, err, tt.ok)
		}
	}
	if _, err := MapFile("/dev/null", 1000, 10); err == nil {
		t.Error("a file at an offset not of whole sectors mapped")
	}
}
