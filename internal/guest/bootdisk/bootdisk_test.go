package bootdisk_test

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/bootdisk"
)

func TestARoundTripOnAPaddedDisk(t *testing.T) {
	cfg := &guestv1.BootConfig{
		Process:     &guestv1.Process{Args: []string{"nginx", "-g", "daemon off;"}, Env: []string{"PATH=/usr/bin"}, User: "101:101"},
		Interfaces:  []*guestv1.Interface{{Mac: "02:00:00:00:00:01", Address: "10.0.0.5/24", Gateway: "10.0.0.1"}},
		Hostname:    "api",
		Nameservers: []string{"1.1.1.1"},
	}
	var disk bytes.Buffer
	if err := bootdisk.Write(&disk, cfg); err != nil {
		t.Fatal(err)
	}
	disk.Write(make([]byte, 4096)) // the rest of the block
	got, err := bootdisk.Read(&disk)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, cfg) {
		t.Errorf("read %v, wrote %v", got, cfg)
	}
}

func TestWhatIsNotABootDisk(t *testing.T) {
	for name, disk := range map[string]string{
		"empty":     "",
		"no magic":  strings.Repeat("x", 64),
		"truncated": "HANGARBOOT1\n\x00\x00\x00\x10abc",
		"too large": "HANGARBOOT1\n\x7f\x00\x00\x00",
	} {
		if _, err := bootdisk.Read(strings.NewReader(disk)); err == nil {
			t.Errorf("%s: read as a boot disk", name)
		}
	}

	// A well-formed disk under another magic, as a future format would be.
	var disk bytes.Buffer
	if err := bootdisk.Write(&disk, &guestv1.BootConfig{Hostname: "api"}); err != nil {
		t.Fatal(err)
	}
	other := bytes.Replace(disk.Bytes(), []byte("HANGARBOOT1"), []byte("HANGARBOOT2"), 1)
	if _, err := bootdisk.Read(bytes.NewReader(other)); err == nil {
		t.Error("another magic: read as a boot disk")
	}
}
