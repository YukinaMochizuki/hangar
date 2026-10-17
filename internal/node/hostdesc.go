package node

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

// smbiosTable is the host's SMBIOS structure table, which the kernel
// exposes whole, root-readable, whether or not dmi-sysfs is loaded.
const smbiosTable = "/sys/firmware/dmi/tables/DMI"

// hostMemory describes this host's memory by kind.
func hostMemory() (string, error) {
	table, err := os.ReadFile(smbiosTable)
	if err != nil {
		return "", err
	}
	return memoryKind(table)
}

// hostNetwork describes this host's network ports by their rated speed, as
// their drivers report it over ethtool netlink.
func hostNetwork() (string, error) {
	e, err := dialEthtool()
	if err != nil {
		return "", err
	}
	defer func() { _ = e.Close() }()
	return networkPorts("/sys/class/net", e.ratedSpeed)
}

// smbiosStructure is one structure of an SMBIOS table: its type and its
// formatted area, header included, so offsets are the specification's.
type smbiosStructure struct {
	typ  byte
	data []byte
}

// smbiosStructures splits an SMBIOS structure table.
func smbiosStructures(table []byte) ([]smbiosStructure, error) {
	var out []smbiosStructure
	for len(table) >= 4 {
		typ, n := table[0], int(table[1])
		if n < 4 || n > len(table) {
			return nil, fmt.Errorf("SMBIOS structure of type %d has length %d with %d bytes left", typ, n, len(table))
		}
		// Each structure's strings end with two NULs, and so does an empty set.
		end := bytes.Index(table[n:], []byte{0, 0})
		if end < 0 {
			return nil, fmt.Errorf("SMBIOS structure of type %d has unterminated strings", typ)
		}
		out = append(out, smbiosStructure{typ: typ, data: table[:n]})
		table = table[n+end+2:]
		if typ == 127 { // end of table
			break
		}
	}
	return out, nil
}

// word reads the little-endian word at off, and whether the structure is
// long enough to have it.
func (s smbiosStructure) word(off int) (uint16, bool) {
	if len(s.data) < off+2 {
		return 0, false
	}
	return binary.LittleEndian.Uint16(s.data[off:]), true
}

// smbiosMemoryTypes names the Memory Device (type 17) memory types a host
// may have.
var smbiosMemoryTypes = map[byte]string{
	0x12: "DDR", 0x13: "DDR2", 0x18: "DDR3", 0x1a: "DDR4", 0x22: "DDR5",
	0x1b: "LPDDR", 0x1c: "LPDDR2", 0x1d: "LPDDR3", 0x1e: "LPDDR4", 0x23: "LPDDR5",
}

// memoryKind describes the installed memory in an SMBIOS table by type,
// slowest speed and whether it is ECC, such as "DDR4-3200 ECC"; never how
// much there is or in how many modules. Memory of a type it has no name
// for, such as a virtual machine's "RAM", is not described.
func memoryKind(table []byte) (string, error) {
	structs, err := smbiosStructures(table)
	if err != nil {
		return "", err
	}
	ecc := false
	kinds := map[string]bool{}
	var speed uint16
	for _, s := range structs {
		switch s.typ {
		case 16: // Physical Memory Array: use at 0x05, error correction at 0x06.
			if len(s.data) > 0x06 && s.data[0x05] == 0x03 && s.data[0x06] >= 0x05 && s.data[0x06] <= 0x07 {
				ecc = true
			}
		case 17: // Memory Device: size at 0x0c, type at 0x12, speeds at 0x15 and 0x20.
			size, ok := s.word(0x0c)
			if !ok || size == 0 || len(s.data) <= 0x12 {
				continue // an empty slot
			}
			kind, known := smbiosMemoryTypes[s.data[0x12]]
			if !known {
				continue
			}
			kinds[kind] = true
			v, _ := s.word(0x15)
			if c, ok := s.word(0x20); ok && c != 0 && c != 0xffff {
				v = c // the speed it runs at, rather than the most it could
			}
			if v != 0 && v != 0xffff && (speed == 0 || v < speed) {
				speed = v
			}
		}
	}
	switch len(kinds) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", errors.New("the SMBIOS table lists installed memory of more than one type")
	}
	var b strings.Builder
	for k := range kinds {
		b.WriteString(k)
	}
	if speed != 0 {
		fmt.Fprintf(&b, "-%d", speed)
	}
	if ecc {
		b.WriteString(" ECC")
	}
	return b.String(), nil
}

// networkPorts counts the physical Ethernet ports under classNet,
// /sys/class/net on a host, by the speed rated gives each, the most it
// supports, such as "2 × 10 GbE". Every port counts, its link up or not,
// but one rated cannot read: it is left out, and said in the error.
func networkPorts(classNet string, rated func(dev string) (int, error)) (string, error) {
	entries, err := os.ReadDir(classNet)
	if err != nil {
		return "", err
	}
	count := map[int]int{}
	var unrated []error
	for _, e := range entries {
		dir := filepath.Join(classNet, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "device")); err != nil {
			continue // virtual: a bridge, tap, veth or bond
		}
		if _, err := os.Stat(filepath.Join(dir, "wireless")); err == nil {
			continue
		}
		if typ, _ := os.ReadFile(filepath.Join(dir, "type")); strings.TrimSpace(string(typ)) != "1" {
			continue // not Ethernet
		}
		mbps, err := rated(e.Name())
		if err != nil {
			unrated = append(unrated, err)
			continue
		}
		count[mbps]++
	}
	return describePorts(count), errors.Join(unrated...)
}

// describePorts writes ports counted by speed, the most numerous first, or,
// past what a Placement field holds, how many there are and the fastest.
func describePorts(count map[int]int) string {
	speeds := make([]int, 0, len(count))
	total := 0
	for s, n := range count {
		speeds = append(speeds, s)
		total += n
	}
	slices.SortFunc(speeds, func(a, b int) int { return cmp.Or(count[b]-count[a], b-a) })
	parts := make([]string, len(speeds))
	for i, s := range speeds {
		parts[i] = fmt.Sprintf("%d × %s", count[s], ethernetSpeed(s))
	}
	line := strings.Join(parts, ", ")
	if utf8.RuneCountInString(line) <= computeapi.MaxPlacementLen {
		return line
	}
	return fmt.Sprintf("%d Ethernet ports, up to %s", total, ethernetSpeed(slices.Max(speeds)))
}

// ethernetSpeed names a speed in Mb/s.
func ethernetSpeed(mbps int) string {
	if mbps >= 1000 {
		return strconv.FormatFloat(float64(mbps)/1000, 'f', -1, 64) + " GbE"
	}
	return strconv.Itoa(mbps) + " Mb/s Ethernet"
}
