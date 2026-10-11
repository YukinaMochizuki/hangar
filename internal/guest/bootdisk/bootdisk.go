// Package bootdisk is the format of the small read-only disk a guest boots
// with: the node writes a BootConfig on it and hangar-init reads it back.
// It is the only thing the guest needs from the node to boot, so a guest
// boots again, after a crash or a restore, with the node gone.
package bootdisk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// magic opens every boot disk; a disk without it is not one.
var magic = []byte("HANGARBOOT1\n")

// MaxSize bounds the encoded config. Disks are padded to a whole block, so
// the reader takes the length from the header, not the disk's size.
const MaxSize = 1 << 20

// Write writes cfg as a boot disk's contents.
func Write(w io.Writer, cfg *guestv1.BootConfig) error {
	data, err := proto.Marshal(cfg)
	if err != nil {
		return err
	}
	if len(data) > MaxSize {
		return fmt.Errorf("boot config is %d bytes; at most %d fit", len(data), MaxSize)
	}
	var buf bytes.Buffer
	buf.Write(magic)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(data)))
	buf.Write(data)
	_, err = w.Write(buf.Bytes())
	return err
}

// Read reads a boot disk's contents.
func Read(r io.Reader) (*guestv1.BootConfig, error) {
	head := make([]byte, len(magic)+4)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, fmt.Errorf("reading the boot disk's header: %w", err)
	}
	if !bytes.Equal(head[:len(magic)], magic) {
		return nil, errors.New("not a boot disk: no magic")
	}
	n := binary.BigEndian.Uint32(head[len(magic):])
	if n > MaxSize {
		return nil, fmt.Errorf("boot config says it is %d bytes; at most %d fit", n, MaxSize)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("reading the boot config: %w", err)
	}
	cfg := &guestv1.BootConfig{}
	if err := proto.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("decoding the boot config: %w", err)
	}
	return cfg, nil
}
