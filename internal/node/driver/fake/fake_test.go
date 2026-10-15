package fake_test

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/drivertest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// TestConformanceWithIPv6 holds the fake to the contract on a host with an
// IPv6 prefix, its guests' IPv6 too.
func TestConformanceWithIPv6(t *testing.T) {
	prefix := netip.MustParsePrefix("2001:db8:0:1::/64")
	drivertest.Run(t, func(t *testing.T) drivertest.Host {
		h := fake.New()
		h.SetIPv6Prefix(prefix)
		d := host(h)
		d.IPv6Prefix = prefix
		d.GuestIPv6 = func(_ *testing.T, uid string) []driver.GuestIPv6 { return h.GuestIPv6(uid) }
		return d
	})
}

// TestConformance holds the fake to the drivers' contract, which the real
// drivers are held to on the testbed.
func TestConformance(t *testing.T) {
	drivertest.Run(t, func(t *testing.T) drivertest.Host { return host(fake.New()) })
}

// host is the suite's view of h.
func host(h *fake.Host) drivertest.Host {
	published := 0
	return drivertest.Host{
		Images: h.Images(), DiskImages: h.DiskImages(), Storage: h.Storage(), Network: h.Network(), Hypervisor: h.Hypervisor(), Guest: h.Guest(),
		Publish: func(_ *testing.T, name string) (string, string) {
			published++
			digest := fmt.Sprintf("sha256:%d", published)
			h.AddImage(name, digest, driver.ImageConfig{Cmd: []string{"true"}})
			return name, digest
		},
		PublishPrivate: func(_ *testing.T, name string) (string, string, driver.Login) {
			published++
			ref, digest := "private.example/"+name, fmt.Sprintf("sha256:%d", published)
			login := driver.Login{Username: "user", Password: "pass"}
			h.AddImage(ref, digest, driver.ImageConfig{Cmd: []string{"true"}})
			h.MakePrivate(ref, login)
			return ref, digest, login
		},
		Claim: func(_ *testing.T, name, digest string) string {
			published++
			return h.ClaimDigest(fmt.Sprintf("evil.example/%s%d", name, published), digest)
		},
		Mirror: func(_ *testing.T, name, digest string) string {
			published++
			ref := fmt.Sprintf("mirror.example/%d%s", published, name)
			h.AddImage(ref, digest, driver.ImageConfig{Cmd: []string{"true"}})
			return ref[:strings.LastIndex(ref, ":")] + "@" + digest
		},
		PublishDiskImage: func(_ *testing.T, name string, size int64) (string, string) {
			published++
			url, digest := "https://images.example/"+name, fmt.Sprintf("sha256:%064x", published)
			h.AddDiskImage(url, digest, size)
			return url, digest
		},
		Withdraw:         func(_ *testing.T, ref string) { h.FailPull(ref, errors.New("registry down")) },
		Reopen:           func(*testing.T) (driver.Images, driver.Storage) { return h.Images(), h.Storage() },
		Hold:             func(_ *testing.T, d driver.Disk) func() { return h.HoldDisk(d.MachineUID) },
		HoldVolume:       func(_ *testing.T, v driver.Volume) func() { return h.HoldVolume(v.DiskUID) },
		ReopenHypervisor: func(*testing.T) driver.Hypervisor { return h.Hypervisor() },
		AskedToStop:      func(_ *testing.T, uid string) bool { return h.GuestStops(uid) > 0 },
		Firmware:         true,
		ServeGuest: func(_ *testing.T, nic driver.Interface, ports ...uint16) {
			for _, p := range ports {
				h.ServeGuest(nic.NetworkUID, netip.AddrPortFrom(nic.Address.Addr(), p), echo)
			}
		},
		Exit: func(t *testing.T, machineUID string, code int) {
			if err := h.Exit(machineUID, code); err != nil {
				t.Fatal(err)
			}
		},
		Exec: &drivertest.ExecPrograms{
			Output: func(stdout, stderr string, code int) []string {
				return []string{"exit", strconv.Itoa(code), stdout, stderr}
			},
			Cat:     []string{"cat"},
			Size:    []string{"size"},
			Show:    []string{"show", "HANGAR_EXEC"},
			Wait:    func(marker string) []string { return []string{"wait", marker} },
			Running: func(marker string) []string { return []string{"running", marker} },
		},
	}
}

// echo sends back what a connection sends, as a guest's echo server.
func echo(c net.Conn) {
	defer func() { _ = c.Close() }()
	_, _ = io.Copy(c, c)
}
