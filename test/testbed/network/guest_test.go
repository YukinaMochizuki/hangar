//go:build testbed

package network

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
)

// guest stands in for a VM on a machine interface: a namespace whose own
// tap is joined to the interface's by copying frames, as the VMM does, and
// which holds the interface's tap open while plugged.
type guest struct {
	ns    string
	nic   driver.Interface
	inner *os.File
	host  *os.File
	stop  chan struct{}
	wg    sync.WaitGroup
}

func newGuest(t *testing.T, nic driver.Interface) *guest {
	t.Helper()
	g := &guest{ns: guestPrefix + nic.MachineUID + "-" + nic.Name, nic: nic}
	run(t, "ip", "netns", "add", g.ns)
	t.Cleanup(func() {
		g.unplug()
		if g.inner != nil {
			_ = g.inner.Close()
		}
		_ = exec.Command("ip", "netns", "del", g.ns).Run()
	})
	ns, err := netns.GetFromName(g.ns)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ns.Close() }()
	if err := inNS(ns, func() error {
		f, err := openTun("eth0")
		g.inner = f
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run(t, "ip", "-n", g.ns, "link", "set", "lo", "up")
	run(t, "ip", "-n", g.ns, "link", "set", "eth0", "address", nic.MAC, "up")
	run(t, "ip", "-n", g.ns, "addr", "add", nic.Address.String(), "dev", "eth0")
	run(t, "ip", "-n", g.ns, "route", "add", "default", "via", nic.Gateway.String())
	g.plug(t)
	return g
}

// openTun makes a tap for the calling thread's namespace, gone when closed,
// and returns it pollable, so closing it ends a read.
func openTun(name string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	ifr, err := unix.NewIfreq(name)
	if err == nil {
		ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
		err = unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr)
	}
	if err == nil {
		err = unix.SetNonblock(fd, true)
	}
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// plug opens the host's tap and starts copying frames, as a VM starting.
func (g *guest) plug(t *testing.T) {
	t.Helper()
	f, err := hostnet.OpenTap(g.nic)
	if err != nil {
		t.Fatalf("opening the tap of %s: %v", g.ns, err)
	}
	g.host = pollable(t, f)
	g.stop = make(chan struct{})
	g.wg.Add(2)
	go g.copy(g.inner, g.host)
	go g.copy(g.host, g.inner)
}

// unplug closes the host's tap, as a VM stopping.
func (g *guest) unplug() {
	if g.host == nil {
		return
	}
	close(g.stop)
	g.wg.Wait()
	_ = g.host.Close()
	g.host = nil
}

func (g *guest) copy(from, to *os.File) {
	defer g.wg.Done()
	buf := make([]byte, 65536+64)
	for {
		select {
		case <-g.stop:
			return
		default:
		}
		_ = from.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := from.Read(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if err != nil {
			select {
			case <-g.stop:
				return
			case <-time.After(10 * time.Millisecond): // a tap without carrier
				continue
			}
		}
		_, _ = to.Write(buf[:n])
	}
}

func pollable(t *testing.T, f *os.File) *os.File {
	t.Helper()
	raw, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fd int
	if err := raw.Control(func(old uintptr) { fd, err = unix.Dup(int(old)) }); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(fd), f.Name())
}

// reaches says whether the guest gets an answer to a ping of addr.
func (g *guest) reaches(addr string) bool {
	for range 3 {
		if exec.Command("ip", "netns", "exec", g.ns, "ping", "-c", "1", "-W", "1", addr).Run() == nil {
			return true
		}
	}
	return false
}

// seenAs returns the address the upstream sees the guest's request from.
func (g *guest) seenAs() (string, error) {
	out, err := exec.Command("ip", "netns", "exec", g.ns, "curl", "-sS", "-m", "3", upstreamURL).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// sendAs sends three marked pings to dst from the guest as src, an address
// it takes for them, as a VM can take any.
func (g *guest) sendAs(t *testing.T, src, dst string) {
	t.Helper()
	run(t, "ip", "-n", g.ns, "addr", "add", src+"/32", "dev", "eth0")
	defer run(t, "ip", "-n", g.ns, "addr", "del", src+"/32", "dev", "eth0")
	out, _ := exec.Command("ip", "netns", "exec", g.ns, "ping", "-c", "3", "-i", "0.2", "-W", "1", "-s", markSize, "-I", src, dst).CombinedOutput()
	if !strings.Contains(string(out), "3 packets transmitted") {
		t.Fatalf("%s did not send as %s: %s", g.ns, src, out)
	}
	time.Sleep(200 * time.Millisecond) // for the last reply
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
