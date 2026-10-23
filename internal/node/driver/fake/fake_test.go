package fake_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

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
// drivers are held to on the testbed, on a host with a shared address; the
// run with IPv6 has none.
func TestConformance(t *testing.T) {
	shared := netip.MustParseAddr("198.51.100.3")
	drivertest.Run(t, func(t *testing.T) drivertest.Host {
		h := fake.New()
		h.SetSharedIPv4(shared)
		d := host(h)
		d.SharedIPv4 = shared
		return d
	})
}

// host is the suite's view of h.
func host(h *fake.Host) drivertest.Host {
	published := 0
	return drivertest.Host{
		Images: h.Images(), DiskImages: h.DiskImages(), Storage: h.Storage(), Network: h.Network(), Hypervisor: h.Hypervisor(), Guest: h.Guest(),
		Converter: h.Converter(),
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
		HoldShareVolume:  func(_ *testing.T, v driver.ShareVolume) func() { return h.HoldShareVolume(v.ShareUID) },
		ReopenHypervisor: func(*testing.T) driver.Hypervisor { return h.Hypervisor() },
		BootDisk: func(_ *testing.T, uid string) (driver.ShareExport, os.FileMode, int) {
			e, _ := h.BootExport(uid)
			return e, 0, 0
		},
		AskedToStop: func(_ *testing.T, uid string) bool { return h.GuestStops(uid) > 0 },
		Firmware:    true,
		ServeGuest: func(_ *testing.T, nic driver.Interface, ports ...uint16) {
			for _, p := range ports {
				h.ServeGuest(nic.NetworkUID, netip.AddrPortFrom(nic.Address.Addr(), p), echo)
			}
		},
		Reaches: func(_ *testing.T, from driver.Interface, to netip.AddrPort) bool {
			c, err := h.GuestConnect(from, to)
			if err != nil {
				return false
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			b := make([]byte, 2)
			_, werr := c.Write([]byte("hi"))
			_, rerr := io.ReadFull(c, b)
			return werr == nil && rerr == nil && string(b) == "hi"
		},
		Connect: func(t *testing.T, nic driver.Interface, srcPort uint16) (netip.AddrPort, netip.AddrPort) {
			dst := netip.MustParseAddrPort("198.51.100.7:443")
			seen, err := h.Connect(nic, srcPort, dst)
			if err != nil {
				t.Fatal(err)
			}
			return dst, seen
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

// TestConnectDoesNotHoldTheHost: a FollowNAT whose handler asks the host
// for something while Connect sends more than its queue holds is not
// deadlocked: Connect sends off the host's lock.
func TestConnectDoesNotHoldTheHost(t *testing.T) {
	h := fake.New()
	n := h.Network()
	nw := driver.NetworkSpec{UID: "n1", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31")}
	if err := n.EnsureNetwork(t.Context(), nw); err != nil {
		t.Fatal(err)
	}
	nic, err := n.EnsureInterface(t.Context(), driver.Interface{MachineUID: "m1", Name: "eth0", NetworkUID: "n1", MAC: "02:00:00:00:00:01",
		Address: netip.MustParsePrefix("10.0.0.2/24"), Gateway: netip.MustParseAddr("10.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, got := make(chan struct{}), make(chan struct{}, 1000)
	go func() {
		_ = n.(driver.NATFlows).FollowNAT(ctx, driver.NATHandler{
			Started: func(time.Time) { close(started) },
			Flow: func(driver.NATFlow) {
				_, _ = n.Interfaces(ctx) // takes the host's lock
				got <- struct{}{}
			},
			Lost: func(driver.NATLost) {},
		})
	}()
	<-started
	done := make(chan struct{})
	go func() {
		for i := range 200 {
			_, _ = h.Connect(nic, uint16(40000+i), netip.MustParseAddrPort("198.51.100.7:443"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Connect and the follower deadlocked")
	}
}

// TestFillsUnderWayCountTogether: the fake's storage counts a base being
// filled at its whole size, as the pool's does, until it ends.
func TestFillsUnderWayCountTogether(t *testing.T) {
	h := fake.New()
	h.SetStorageSize(3 << 30)
	st := h.Storage()
	filling, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := st.FillImageBase(t.Context(), "img1", 2<<30, func(string) error {
			close(filling)
			<-release
			return nil
		})
		first <- err
	}()
	<-filling
	if _, err := st.FillImageBase(t.Context(), "img2", 2<<30, func(string) error { return nil }); !errors.Is(err, driver.ErrNoSpace) {
		t.Errorf("a second 2GiB fill into 3GiB while one is under way: %v, want ErrNoSpace", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	h.SetStorageSize(5 << 30)
	if _, err := st.FillImageBase(t.Context(), "img2", 2<<30, func(string) error { return nil }); err != nil {
		t.Errorf("the second fill once the first ended, in 3GiB more: %v", err)
	}
}
