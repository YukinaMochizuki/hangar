package node

import (
	"cmp"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// Generic netlink and ethtool netlink numbers, from linux/genetlink.h and
// linux/ethtool_netlink.h.
const (
	genlIDCtrl             = 0x10
	ctrlCmdGetFamily       = 3
	ctrlAttrFamilyID       = 1
	ctrlAttrFamilyName     = 2
	ethtoolGenlVersion     = 1
	ethtoolMsgLinkmodesGet = 4
	ethtoolAHeaderDevName  = 2
	ethtoolALinkmodesHdr   = 1
	ethtoolALinkmodesOurs  = 3
	ethtoolABitsetBits     = 3
	ethtoolABitsetBitsBit  = 1
	ethtoolABitsetBitName  = 2
)

// ethtool asks the kernel's ethtool netlink family about ports.
type ethtool struct {
	c      *netlink.Conn
	family netlink.HeaderType
}

func dialEthtool() (*ethtool, error) {
	c, err := netlink.Dial(unix.NETLINK_GENERIC, nil)
	if err != nil {
		return nil, fmt.Errorf("generic netlink: %w", err)
	}
	ae := netlink.NewAttributeEncoder()
	ae.String(ctrlAttrFamilyName, "ethtool")
	msgs, err := genlExecute(c, genlIDCtrl, ctrlCmdGetFamily, 1, ae)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("the ethtool netlink family: %w", err)
	}
	ad, err := netlink.NewAttributeDecoder(msgs[0].Data[4:])
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	e := &ethtool{c: c}
	for ad.Next() {
		if ad.Type() == ctrlAttrFamilyID {
			e.family = netlink.HeaderType(ad.Uint16())
		}
	}
	if err := ad.Err(); err != nil || e.family == 0 {
		_ = c.Close()
		return nil, cmp.Or(err, errors.New("the kernel has no ethtool netlink family"))
	}
	return e, nil
}

func (e *ethtool) Close() error { return e.c.Close() }

// ratedSpeed is the highest speed, in Mb/s, among the link modes the port
// dev supports: what it is built for, whatever its link does.
func (e *ethtool) ratedSpeed(dev string) (int, error) {
	msgs, err := genlExecute(e.c, e.family, ethtoolMsgLinkmodesGet, ethtoolGenlVersion, linkModesRequest(dev))
	if err != nil {
		return 0, fmt.Errorf("the link modes of %s: %w", dev, err)
	}
	s, err := ratedSpeedOf(msgs[0].Data[4:])
	if err != nil {
		return 0, fmt.Errorf("the link modes of %s: %w", dev, err)
	}
	return s, nil
}

// linkModesRequest asks for dev's link modes, by name: without the compact
// flag the kernel names each mode.
func linkModesRequest(dev string) *netlink.AttributeEncoder {
	ae := netlink.NewAttributeEncoder()
	ae.Nested(ethtoolALinkmodesHdr, func(h *netlink.AttributeEncoder) error {
		h.String(ethtoolAHeaderDevName, dev)
		return nil
	})
	return ae
}

func genlExecute(c *netlink.Conn, family netlink.HeaderType, cmd, version uint8, ae *netlink.AttributeEncoder) ([]netlink.Message, error) {
	attrs, err := ae.Encode()
	if err != nil {
		return nil, err
	}
	msgs, err := c.Execute(netlink.Message{
		Header: netlink.Header{Type: family, Flags: netlink.Request},
		Data:   append([]byte{cmd, version, 0, 0}, attrs...),
	})
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 || len(msgs[0].Data) < 4 {
		return nil, errors.New("no reply")
	}
	return msgs, nil
}

// ratedSpeedOf reads a link modes reply's attributes: the "ours" bitset
// lists every mode the port supports, by name (its value says which are
// advertised, which does not matter here). The negotiated speed the reply
// also carries is not read.
func ratedSpeedOf(attrs []byte) (int, error) {
	ad, err := netlink.NewAttributeDecoder(attrs)
	if err != nil {
		return 0, err
	}
	best := 0
	eachNested := func(d *netlink.AttributeDecoder, typ uint16, f func(*netlink.AttributeDecoder) error) {
		for d.Next() {
			if d.Type() == typ {
				d.Nested(f)
			}
		}
	}
	eachNested(ad, ethtoolALinkmodesOurs, func(bitset *netlink.AttributeDecoder) error {
		eachNested(bitset, ethtoolABitsetBits, func(bits *netlink.AttributeDecoder) error {
			eachNested(bits, ethtoolABitsetBitsBit, func(bit *netlink.AttributeDecoder) error {
				for bit.Next() {
					if bit.Type() == ethtoolABitsetBitName {
						best = max(best, modeSpeed(bit.String()))
					}
				}
				return nil
			})
			return nil
		})
		return nil
	})
	if err := ad.Err(); err != nil {
		return 0, err
	}
	if best == 0 {
		return 0, errors.New("the driver reports no supported link mode with a speed")
	}
	return best, nil
}

// modeSpeed is the speed in Mb/s a link mode's name begins with, such as
// 10000 for "10000baseT/Full"; 0 for a mode that is no speed, such as
// "Autoneg".
func modeSpeed(name string) int {
	speed, _, ok := strings.Cut(name, "base")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(speed)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
