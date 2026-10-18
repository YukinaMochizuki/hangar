package computeapi_test

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

func TestVirtioDevice(t *testing.T) {
	for n, want := range map[int]string{
		0: "vda", 2: "vdc", 25: "vdz", 26: "vdaa", 27: "vdab", 51: "vdaz", 52: "vdba", 701: "vdzz", 702: "vdaaa",
	} {
		if got := computeapi.VirtioDevice(n); got != want {
			t.Errorf("virtio disk %d: %s, want %s", n, got, want)
		}
	}
}
