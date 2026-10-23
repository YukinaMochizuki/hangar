package node

import (
	"errors"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// TestWhatAConverterSaysOfAFileIsJudged: a file is taken only as raw or
// qcow2, naming no other file, holding a disk of some bytes up to the
// bound; each refusal is the image's, for good, with its reason.
func TestWhatAConverterSaysOfAFileIsJudged(t *testing.T) {
	const bound = 1 << 30
	for _, tt := range []struct {
		name   string
		info   driver.ImageInfo
		reason string
	}{
		{"raw", driver.ImageInfo{Format: "raw", VirtualSize: 512}, ""},
		{"qcow2 at the bound", driver.ImageInfo{Format: "qcow2", VirtualSize: bound}, ""},
		{"vmdk", driver.ImageInfo{Format: "vmdk", VirtualSize: 512}, computeapi.ReasonInvalidImage},
		{"a backing file", driver.ImageInfo{Format: "qcow2", VirtualSize: 512, BackingFile: "base.qcow2"}, computeapi.ReasonInvalidImage},
		{"a data file", driver.ImageInfo{Format: "qcow2", VirtualSize: 512, DataFile: "/dev/vda"}, computeapi.ReasonInvalidImage},
		{"no disk", driver.ImageInfo{Format: "qcow2", VirtualSize: 0}, computeapi.ReasonInvalidImage},
		{"a disk of less than nothing", driver.ImageInfo{Format: "raw", VirtualSize: -1}, computeapi.ReasonInvalidImage},
		{"past the bound", driver.ImageInfo{Format: "qcow2", VirtualSize: bound + 1}, computeapi.ReasonImageTooLarge},
	} {
		err := checkImageInfo(tt.info, bound)
		var s *stuck
		switch {
		case tt.reason == "" && err != nil:
			t.Errorf("%s: %v, want it taken", tt.name, err)
		case tt.reason != "" && (!errors.As(err, &s) || s.reason != tt.reason):
			t.Errorf("%s: %v, want it refused as %s", tt.name, err, tt.reason)
		}
	}
}
