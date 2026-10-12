package boot

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// superblock is an ext4 superblock with the fields growth reads.
func superblock(lo, hi, logSize, incompat uint32) []byte {
	sb := make([]byte, superblockSize)
	le := binary.LittleEndian
	le.PutUint32(sb[0x04:], lo)
	le.PutUint32(sb[0x18:], logSize)
	le.PutUint16(sb[0x38:], ext4Magic)
	le.PutUint32(sb[0x60:], incompat)
	le.PutUint32(sb[0x150:], hi)
	return sb
}

func TestExt4Blocks(t *testing.T) {
	for name, tt := range map[string]struct {
		sb                []byte
		blocks, blockSize uint64
	}{
		"4 KiB blocks":                      {superblock(16384, 0, 2, 0), 16384, 4096},
		"1 KiB blocks":                      {superblock(65536, 0, 0, 0), 65536, 1024},
		"64-bit counts":                     {superblock(5, 2, 2, incompat64bit|0x2), 2<<32 | 5, 4096},
		"the high half ignored when 32-bit": {superblock(5, 2, 2, 0x2), 5, 4096},
	} {
		t.Run(name, func(t *testing.T) {
			blocks, size, err := ext4Blocks(bytes.NewReader(tt.sb))
			if err != nil || blocks != tt.blocks || size != tt.blockSize {
				t.Errorf("got %d blocks of %d, %v; want %d of %d", blocks, size, err, tt.blocks, tt.blockSize)
			}
		})
	}
	bad := superblock(1, 0, 2, 0)
	bad[0x38] = 0
	if _, _, err := ext4Blocks(bytes.NewReader(bad)); err == nil {
		t.Error("read a superblock without ext4's magic")
	}
	if _, _, err := ext4Blocks(bytes.NewReader(superblock(1, 0, 40, 0))); err == nil {
		t.Error("read a block size of 2^40 KiB")
	}
	if _, _, err := ext4Blocks(bytes.NewReader(make([]byte, 100))); err == nil {
		t.Error("read a short superblock")
	}
}
