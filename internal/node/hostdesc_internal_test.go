package node

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdlayher/netlink"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

// smbiosStruct makes one SMBIOS structure of typ, length bytes of formatted
// area (header included) set by set, followed by strings.
func smbiosStruct(typ byte, length int, set func(d []byte), strings ...string) []byte {
	d := make([]byte, length)
	d[0], d[1] = typ, byte(length)
	if set != nil {
		set(d)
	}
	if len(strings) == 0 {
		return append(d, 0, 0)
	}
	for _, s := range strings {
		d = append(append(d, s...), 0)
	}
	return append(d, 0)
}

// memArray is a Physical Memory Array for system memory with the given
// error correction.
func memArray(correction byte) []byte {
	return smbiosStruct(16, 0x0f, func(d []byte) { d[0x05], d[0x06] = 0x03, correction })
}

// dimm is a Memory Device of SMBIOS 2.7's length: sizeMB 0 is an empty
// slot, configured 0 unknown.
func dimm(sizeMB uint16, typ byte, speed, configured uint16) []byte {
	return smbiosStruct(17, 0x22, func(d []byte) {
		binary.LittleEndian.PutUint16(d[0x0c:], sizeMB)
		d[0x12] = typ
		binary.LittleEndian.PutUint16(d[0x15:], speed)
		binary.LittleEndian.PutUint16(d[0x20:], configured)
	}, "DIMM 0", "Samsung")
}

func table(structs ...[]byte) []byte {
	var t []byte
	for _, s := range structs {
		t = append(t, s...)
	}
	return append(t, smbiosStruct(127, 4, nil)...)
}

func TestMemoryKind(t *testing.T) {
	const ddr4, ddr5, ram = 0x1a, 0x22, 0x07
	for _, tt := range []struct {
		what  string
		table []byte
		want  string
		fails bool
	}{
		{"two ECC DDR4 modules and an empty slot",
			table(memArray(0x06), dimm(16384, ddr4, 3200, 3200), dimm(0, ddr4, 2133, 2133), dimm(16384, ddr4, 3200, 3200)),
			"DDR4-3200 ECC", false},
		{"modules run slower than they could, without ECC",
			table(memArray(0x03), dimm(8192, ddr4, 3200, 2933)),
			"DDR4-2933", false},
		{"modules of two speeds run at the slower",
			table(memArray(0x05), dimm(32768, ddr5, 4800, 4800), dimm(32768, ddr5, 5600, 4400)),
			"DDR5-4400 ECC", false},
		{"an SMBIOS 2.1 module, with no speed",
			table(smbiosStruct(17, 0x15, func(d []byte) { d[0x0c], d[0x12] = 0x00, 0x18; d[0x0d] = 0x10 })),
			"DDR3", false},
		{"a virtual machine's RAM", table(memArray(0x06), dimm(1024, ram, 0, 0)), "", false},
		// Use 0x07 is cache memory, whose error correction says nothing of
		// the system's.
		{"ECC that is not the system memory's",
			table(smbiosStruct(16, 0x0f, func(d []byte) { d[0x05], d[0x06] = 0x07, 0x06 }), memArray(0x03), dimm(8192, ddr4, 3200, 3200)),
			"DDR4-3200", false},
		{"two types", table(dimm(8192, ddr4, 3200, 0), dimm(8192, ddr5, 4800, 0)), "", true},
		{"a truncated table", table(dimm(8192, ddr4, 3200, 0))[:0x30], "", true},
	} {
		got, err := memoryKind(tt.table)
		if (err != nil) != tt.fails || got != tt.want {
			t.Errorf("%s: %q, %v; want %q, failing %v", tt.what, got, err, tt.want, tt.fails)
		}
	}
}

// netPort is an interface in a made-up /sys/class/net: rated is the most
// its driver says it supports, 0 for a driver that does not say, and link
// what sysfs gives as its link's speed, which must never count.
type netPort struct {
	physical, wireless bool
	typ, link          string
	rated              int
}

func classNet(t *testing.T, ports map[string]netPort) (string, func(string) (int, error)) {
	t.Helper()
	root := t.TempDir()
	for name, p := range ports {
		dir := filepath.Join(root, name)
		mustMkdir(t, dir)
		if p.physical {
			mustMkdir(t, filepath.Join(dir, "device"))
		}
		if p.wireless {
			mustMkdir(t, filepath.Join(dir, "wireless"))
		}
		for file, v := range map[string]string{"type": p.typ, "speed": p.link} {
			if v == "" {
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, file), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root, func(dev string) (int, error) {
		if p := ports[dev]; p.rated > 0 {
			return p.rated, nil
		}
		return 0, fmt.Errorf("%s: no supported link mode with a speed", dev)
	}
}

func TestNetworkPorts(t *testing.T) {
	for _, tt := range []struct {
		what  string
		ports map[string]netPort
		want  string
		fails bool
	}{
		{"physical Ethernet ports by rated speed, links up or down, beside what is not one", map[string]netPort{
			"eno1":   {physical: true, typ: "1", rated: 10000, link: "1000"},
			"eno2":   {physical: true, typ: "1", rated: 10000, link: "-1"},
			"enp3s0": {physical: true, typ: "1", rated: 2500},
			"br0":    {typ: "1", link: "10000"},
			"hm1234": {typ: "1", link: "10000"},
			"wlp2s0": {physical: true, wireless: true, typ: "1", link: "866"},
			"ibp1s0": {physical: true, typ: "32", link: "100000"},
			"lo":     {typ: "772"},
			"enp4s0": {physical: true, typ: "1", rated: 1000, link: "100"},
			"enp5s0": {physical: true, typ: "1", rated: 1000},
			"enp6s0": {physical: true, typ: "1", rated: 1000},
			"enp7s0": {physical: true, typ: "1", rated: 25000, link: "10000"},
		}, "3 × 1 GbE, 2 × 10 GbE, 1 × 25 GbE, 1 × 2.5 GbE", false},
		{"a port whose driver gives no rated speed, among rated ones", map[string]netPort{
			"eno1": {physical: true, typ: "1", rated: 10000},
			"eno2": {physical: true, typ: "1"},
			"eno3": {physical: true, typ: "1", rated: 10000, link: "-1"},
		}, "2 × 10 GbE", true},
		{"no port rated", map[string]netPort{
			"eno1": {physical: true, typ: "1"},
			"eno2": {physical: true, typ: "1", link: "10000"},
		}, "", true},
		{"none", map[string]netPort{"lo": {typ: "772"}}, "", false},
	} {
		dir, rated := classNet(t, tt.ports)
		got, err := networkPorts(dir, rated)
		if (err != nil) != tt.fails || got != tt.want {
			t.Errorf("%s: %q, %v; want %q, failing %v", tt.what, got, err, tt.want, tt.fails)
		}
		// The error names each port left out.
		for name, p := range tt.ports {
			if p.physical && !p.wireless && p.typ == "1" && p.rated == 0 && !strings.Contains(fmt.Sprint(err), name) {
				t.Errorf("%s: %v does not name %s", tt.what, err, name)
			}
		}
	}
}

// TestDescribePortsFitsAPlacement: a line too long for a Placement field is
// summed up, not dropped.
func TestDescribePortsFitsAPlacement(t *testing.T) {
	count := map[int]int{100: 1, 1000: 2, 2500: 1, 5000: 1, 10000: 4, 25000: 2, 40000: 1, 100000: 1}
	got := describePorts(count)
	if want := "13 Ethernet ports, up to 100 GbE"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
	if err := computeapi.CheckPlacementValue("network", got); err != nil {
		t.Error(err)
	}
	if got := describePorts(map[int]int{10000: 2, 100: 1}); got != "2 × 10 GbE, 1 × 100 Mb/s Ethernet" {
		t.Errorf("a short line is %q", got)
	}
}

// linkModesReply is the attributes of a LINKMODES_GET reply as the kernel
// writes them without compact bitsets: the modes the port supports, each
// named, and the speed the link runs at.
func linkModesReply(t *testing.T, linkSpeed uint32, modes ...string) []byte {
	t.Helper()
	ae := netlink.NewAttributeEncoder()
	ae.Nested(ethtoolALinkmodesHdr, func(h *netlink.AttributeEncoder) error {
		h.Uint32(1, 2) // the device's index
		h.String(ethtoolAHeaderDevName, "eno1")
		return nil
	})
	ae.Uint8(2, 1) // autoneg
	ae.Nested(ethtoolALinkmodesOurs, func(bs *netlink.AttributeEncoder) error {
		bs.Uint32(2, 102) // the bitset's size
		bs.Nested(ethtoolABitsetBits, func(bits *netlink.AttributeEncoder) error {
			for i, m := range modes {
				bits.Nested(ethtoolABitsetBitsBit, func(bit *netlink.AttributeEncoder) error {
					bit.Uint32(1, uint32(i))
					bit.String(ethtoolABitsetBitName, m)
					bit.Flag(3, i%2 == 0) // advertised or not, which does not matter
					return nil
				})
			}
			return nil
		})
		return nil
	})
	ae.Uint32(5, linkSpeed)
	b, err := ae.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRatedSpeedOf(t *testing.T) {
	for _, tt := range []struct {
		what  string
		reply []byte
		want  int
	}{
		{"a 10 GbE port linked at 1 Gb/s", linkModesReply(t, 1000,
			"10baseT/Full", "100baseT/Full", "1000baseT/Full", "Autoneg", "TP", "10000baseT/Full", "Pause"), 10000},
		{"a 25 GbE port that also does 10, its link down", linkModesReply(t, 0xffffffff,
			"25000baseSR/Full", "10000baseSR/Full", "FIBRE"), 25000},
		{"a 2.5 GbE port", linkModesReply(t, 2500, "2500baseT/Full", "1000baseT/Full"), 2500},
		{"a driver naming no speed", linkModesReply(t, 10000, "Autoneg", "TP"), 0},
	} {
		got, err := ratedSpeedOf(tt.reply)
		if got != tt.want || (err != nil) != (tt.want == 0) {
			t.Errorf("%s: %d, %v; want %d", tt.what, got, err, tt.want)
		}
	}
}

// TestLinkModesRequest: the request names the port in its header.
func TestLinkModesRequest(t *testing.T) {
	b, err := linkModesRequest("eno1").Encode()
	if err != nil {
		t.Fatal(err)
	}
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		t.Fatal(err)
	}
	var dev string
	for ad.Next() {
		if ad.Type() == ethtoolALinkmodesHdr {
			ad.Nested(func(h *netlink.AttributeDecoder) error {
				for h.Next() {
					if h.Type() == ethtoolAHeaderDevName {
						dev = h.String()
					}
				}
				return nil
			})
		}
	}
	if err := ad.Err(); err != nil || dev != "eno1" {
		t.Errorf("the request names %q, %v", dev, err)
	}
}
