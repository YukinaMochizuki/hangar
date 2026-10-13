package hostnet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// The driver's namespaces and links carry Prefix in their names; how they
// are named is in docs/decisions/networking.md.
const (
	Prefix = "hn"
	// nsPrefix names a network's namespace, hn-<network uid>, as ip netns
	// shows it.
	nsPrefix = Prefix + "-"
	// vethPrefix names the root end of a network's veth pair.
	vethPrefix = Prefix + "v"
	// tapPrefix names a machine interface's tap.
	tapPrefix = Prefix + "t"
	// Inside a namespace the names are fixed: everything there is the driver's.
	nsUplink = Prefix + "-up"
	nsBridge = Prefix + "-br"
	// Table is the nft table, family ip, in the root namespace and in each
	// network's.
	Table = "hangar"
	// FirewallTable is the bridge-family table in each network's namespace
	// holding its interfaces' security groups.
	FirewallTable = "hangar_sg"
	// hashLen hex digits of the hash follow the three-character prefix.
	hashLen = 12
	// nameAttempts bounds the names tried for one object when the first are
	// taken by others.
	nameAttempts = 16
	// aliasMax is the longest link alias Linux keeps (IFALIASZ less the NUL).
	aliasMax = 255
	// transitRange is where every network's transit /31 comes from; the
	// host maps it to its public address.
	transitRange = "100.64.0.0/10"
)

// uidPattern is what a network uid may be to name a namespace file.
var uidPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// NamespaceOf returns the name of a network's namespace under /run/netns.
func NamespaceOf(networkUID string) string { return nsPrefix + networkUID }

// linkName returns the attempt-th name for an object of key under prefix p:
// the first is the hash of the key, the next ones hash it with a counter.
func linkName(p, key string, attempt int) string {
	if attempt > 0 {
		key = fmt.Sprintf("%s#%d", key, attempt)
	}
	sum := sha256.Sum256([]byte(key))
	return p + hex.EncodeToString(sum[:])[:hashLen]
}

// gatewayMAC is the MAC of a network's gateway: locally administered,
// unicast, and the same whenever the network's bridge is made.
func gatewayMAC(networkUID string) net.HardwareAddr { return macOf("gateway/" + networkUID) }

// vethMACs are the MACs of a network's veth ends, kept as the gateway's is.
// A link made with one is not given another by udev's MACAddressPolicy.
func vethMACs(networkUID string) (host, inner net.HardwareAddr) {
	return macOf("veth/host/" + networkUID), macOf("veth/ns/" + networkUID)
}

func macOf(key string) net.HardwareAddr {
	sum := sha256.Sum256([]byte(key))
	return net.HardwareAddr{0x02, sum[0], sum[1], sum[2], sum[3], sum[4]}
}

// freeName returns the first name for key under p that taken does not hold.
func freeName(p, key string, taken func(string) bool) (string, error) {
	for attempt := range nameAttempts {
		if n := linkName(p, key, attempt); !taken(n) {
			return n, nil
		}
	}
	return "", fmt.Errorf("no free link name for %s after %d attempts", key, nameAttempts)
}

// vethMeta is the alias of a network's root veth end.
type vethMeta struct {
	Network string `json:"network"`
}

// tapMeta is the alias of a tap: the interface as it was ensured, but for
// its network, which is the namespace the tap is in.
type tapMeta struct {
	Machine string       `json:"machine"`
	Name    string       `json:"name"`
	MAC     string       `json:"mac"`
	Address netip.Prefix `json:"ipv4"`
	Gateway netip.Addr   `json:"gateway"`
}

func encodeAlias(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if len(b) > aliasMax {
		return "", fmt.Errorf("link alias of %d bytes is longer than Linux keeps (%d)", len(b), aliasMax)
	}
	return string(b), nil
}

func tapMetaOf(i driver.Interface) tapMeta {
	return tapMeta{Machine: i.MachineUID, Name: i.Name, MAC: i.MAC, Address: i.Address, Gateway: i.Gateway}
}

// decodeTap reads a tap's alias; ok is false for one the driver did not
// finish making.
func decodeTap(alias string) (tapMeta, bool) {
	var m tapMeta
	if err := json.Unmarshal([]byte(alias), &m); err != nil || m.Machine == "" || m.Name == "" {
		return tapMeta{}, false
	}
	return m, true
}

func decodeVeth(alias string) (string, bool) {
	var m vethMeta
	if err := json.Unmarshal([]byte(alias), &m); err != nil || !uidPattern.MatchString(m.Network) {
		return "", false
	}
	return m.Network, true
}

func (m tapMeta) iface(networkUID, device string) driver.Interface {
	return driver.Interface{
		MachineUID: m.Machine, Name: m.Name, NetworkUID: networkUID, MAC: m.MAC,
		Address: m.Address, Gateway: m.Gateway, Device: device,
	}
}

func checkUID(uid string) error {
	if !uidPattern.MatchString(uid) {
		return fmt.Errorf("network uid %q cannot name a namespace", uid)
	}
	return nil
}

func checkNetwork(n driver.NetworkSpec) error {
	if err := checkUID(n.UID); err != nil {
		return err
	}
	c, t := n.CIDR, n.Transit
	switch {
	case !c.IsValid() || !c.Addr().Is4() || c.Bits() > 30 || c != c.Masked():
		return fmt.Errorf("network %s: range %s is not an IPv4 range of /30 or wider", n.UID, c)
	case !t.IsValid() || !t.Addr().Is4() || t.Bits() != 31 || t != t.Masked():
		return fmt.Errorf("network %s: transit %s is not an IPv4 /31", n.UID, t)
	case !netip.MustParsePrefix(transitRange).Contains(t.Addr()):
		return fmt.Errorf("network %s: transit %s is outside %s", n.UID, t, transitRange)
	}
	return nil
}

func checkInterface(i driver.Interface) error {
	if err := checkUID(i.NetworkUID); err != nil {
		return err
	}
	if i.MachineUID == "" || i.Name == "" || strings.ContainsAny(i.MachineUID+i.Name, "/#") {
		return fmt.Errorf("interface %q of machine %q needs a machine uid and a name without / or #", i.Name, i.MachineUID)
	}
	if hw, err := net.ParseMAC(i.MAC); err != nil || len(hw) != 6 {
		return fmt.Errorf("interface %s of machine %s: MAC %q is not an Ethernet address", i.Name, i.MachineUID, i.MAC)
	}
	if !i.Address.IsValid() || !i.Address.Addr().Is4() || !i.Gateway.Is4() {
		return fmt.Errorf("interface %s of machine %s needs an IPv4 address and gateway", i.Name, i.MachineUID)
	}
	return nil
}

// errNoNetwork is returned for an interface whose network the host lacks.
var errNoNetwork = errors.New("no such network on the host")
