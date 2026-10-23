// Package drivertest is the conformance suite of the node's host drivers:
// the contract of package driver as tests. It runs against the fake in CI
// and against the real drivers on the testbed, so the fake the hermetic
// tests rely on behaves as the host does (docs/decisions/testing-strategy.md).
package drivertest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// Host is one host's drivers, with what the suite needs to drive them. A
// part of the suite whose drivers a Host lacks is skipped, so a real driver
// runs it before the others exist.
type Host struct {
	Images     driver.Images
	DiskImages driver.DiskImages
	Storage    driver.Storage
	Network    driver.Network
	Hypervisor driver.Hypervisor
	Guest      driver.Guest
	// Publish makes an image pullable under a reference made of name, and
	// returns that reference and the image's digest.
	Publish func(t *testing.T, name string) (ref, digest string)
	// PublishPrivate publishes as Publish does, in a repository of its own
	// that asks for the login it returns.
	PublishPrivate func(t *testing.T, name string) (ref, digest string, login driver.Login)
	// Claim makes a public repository of its own, named after name, answer
	// for the published image of digest as a registry an attacker runs can:
	// a resolve of the digest, and what it knows of the image, but none of
	// its layers. It returns the reference by digest.
	Claim func(t *testing.T, name, digest string) (ref string)
	// Mirror publishes the image of digest, published before, in full in a
	// public repository of its own, named after name. It returns the
	// reference by digest.
	Mirror func(t *testing.T, name, digest string) (ref string)
	// PublishDiskImage serves a disk image, qcow2, holding a disk of size
	// bytes, and returns its URL and digest; DiskImages fetches from it.
	PublishDiskImage func(t *testing.T, name string, size int64) (url, digest string)
	// Withdraw makes the registry stop serving ref, as when it is down.
	Withdraw func(t *testing.T, ref string)
	// Reopen returns new instances of the image and storage drivers on the
	// same host, as the agent has after it restarts.
	Reopen func(t *testing.T) (driver.Images, driver.Storage)
	// Hold keeps a root disk in use, as a running VM does, until release.
	Hold func(t *testing.T, d driver.Disk) (release func())
	// HoldVolume keeps a volume in use, as a running VM does, until
	// release; HoldShareVolume a file share's volume, as its server does.
	HoldVolume      func(t *testing.T, v driver.Volume) (release func())
	HoldShareVolume func(t *testing.T, v driver.ShareVolume) (release func())
	// WriteVolume writes data into the block device path at offset at,
	// durably, and ReadVolume reads n bytes back from there. The custom
	// images part checks what a base holds only with both.
	WriteVolume func(t *testing.T, path string, at int64, data []byte)
	ReadVolume  func(t *testing.T, path string, at int64, n int) []byte
	// Exit ends the guest process of a running VM with code, as the
	// container's process exiting does.
	Exit func(t *testing.T, machineUID string, code int)
	// Run is what the guests run, which Exit knows how to end; zero runs
	// sleep infinity.
	Run driver.RunSpec
	// ReopenHypervisor, if set, returns a new instance of the hypervisor
	// driver on the same host, as the agent has after it restarts.
	ReopenHypervisor func(t *testing.T) driver.Hypervisor
	// AskedToStop reports whether the guest of the machine's VM, stopped,
	// was asked to stop its process before the VM went.
	AskedToStop func(t *testing.T, machineUID string) bool
	// BootDisk reads back what the machine's listed server's next boot
	// takes from its boot disk: its export, and the disk's file mode and
	// group, zero where the host has no file. The boot exports part runs
	// only with it.
	BootDisk func(t *testing.T, machineUID string) (export driver.ShareExport, mode os.FileMode, gid int)
	// Exec are the programs the guests can exec.
	Exec *ExecPrograms
	// Firmware says the hypervisor boots firmware VMs (VMSpec.Firmware).
	Firmware bool
	// IPv6Prefix is the prefix the network driver was given, invalid for
	// none; the suite's IPv6 part runs only with one.
	IPv6Prefix netip.Prefix
	// SharedIPv4 is the shared address the network driver was given,
	// invalid for none; the port forwards part runs only with one, and the
	// part without them only without.
	SharedIPv4 netip.Addr
	// ServeGuest has a guest on the interface echo what each TCP connection
	// to its address on the ports sends, until the test ends.
	ServeGuest func(t *testing.T, nic driver.Interface, ports ...uint16)
	// Reaches has the guest on the interface open a TCP connection to an
	// address of its network, and says whether what it sends is echoed,
	// within a couple of seconds. The file share servers' firewall part
	// runs only with it and ServeGuest.
	Reaches func(t *testing.T, from driver.Interface, to netip.AddrPort) bool
	// GuestIPv6 reads, from inside the guest of the machine's running VM,
	// what it holds of its public IPv6: each of the interfaces the platform
	// gave it, by MAC, with its global address and, if the default route is
	// by it, its gateway. The suite's guest IPv6 part runs only with it.
	GuestIPv6 func(t *testing.T, machineUID string) []driver.GuestIPv6
	// Connect has a guest on the interface open a TCP connection from
	// srcPort out of the host, and returns where to and the source seen
	// there. The NAT part runs only with it, and a Network that is a
	// driver.NATFlows.
	Connect func(t *testing.T, nic driver.Interface, srcPort uint16) (dst, seen netip.AddrPort)
}

// Run runs the suite, each test on a host from fresh.
func Run(t *testing.T, fresh func(t *testing.T) Host) {
	part := func(name string, test func(*testing.T, Host), needs func(Host) bool) {
		t.Run(name, func(t *testing.T) {
			h := fresh(t)
			if !needs(h) {
				t.Skip("the host lacks the drivers this part needs")
			}
			test(t, h)
		})
	}
	disks := func(h Host) bool { return h.Images != nil && h.Storage != nil }
	vms := func(h Host) bool {
		return disks(h) && h.Network != nil && h.Hypervisor != nil && h.Guest != nil && h.AskedToStop != nil
	}
	part("images", images, func(h Host) bool { return h.Images != nil })
	part("private images", privateImages, func(h Host) bool { return h.Images != nil && h.PublishPrivate != nil })
	part("repository confusion", repositoryConfusion, func(h Host) bool {
		return h.Images != nil && h.PublishPrivate != nil && h.Claim != nil && h.Mirror != nil
	})
	part("storage", storage, disks)
	part("root sizes", rootSizes, disks)
	part("deleting disks", deletingDisks, disks)
	part("reopening", reopening, disks)
	part("volumes", volumes, func(h Host) bool { return h.Storage != nil })
	part("share volumes", shareVolumes, func(h Host) bool { return h.Storage != nil })
	part("disk images", diskImages, func(h Host) bool {
		return h.Storage != nil && h.DiskImages != nil && h.PublishDiskImage != nil
	})
	part("custom images", customImages, func(h Host) bool { return h.Storage != nil })
	nets := func(h Host) bool { return h.Network != nil }
	part("network", network, nets)
	part("network changes", networkChanges, nets)
	part("network sockets", networkSockets, nets)
	part("dialing SSH", dialSSH, func(h Host) bool { return h.Network != nil && h.ServeGuest != nil })
	part("dialing edge ports", dialEdgePorts, func(h Host) bool { return h.Network != nil && h.ServeGuest != nil })
	part("network firewall", networkFirewall, nets)
	part("file share servers' firewall", shareServerFirewall, func(h Host) bool {
		return h.Network != nil && h.ServeGuest != nil && h.Reaches != nil
	})
	part("NAT flows", natFlows, func(h Host) bool {
		_, ok := h.Network.(driver.NATFlows)
		return ok && h.Connect != nil
	})
	part("port forwards", portForwards, func(h Host) bool { return nets(h) && h.SharedIPv4.IsValid() })
	part("no port forwards", noPortForwards, func(h Host) bool { return nets(h) && !h.SharedIPv4.IsValid() })
	part("network IPv6", networkIPv6, func(h Host) bool { return nets(h) && h.IPv6Prefix.IsValid() })
	part("network without IPv6", networkWithoutIPv6, func(h Host) bool { return nets(h) && !h.IPv6Prefix.IsValid() })
	part("hypervisor", hypervisor, vms)
	part("restart policies", restarts, vms)
	part("logs", logs, vms)
	part("exec", exec, func(h Host) bool { return vms(h) && h.Exec != nil })
	part("firmware", firmware, func(h Host) bool { return vms(h) && h.Firmware })
	part("file share servers", shareServers, vms)
	part("boot exports", bootExports, func(h Host) bool { return vms(h) && h.BootDisk != nil })
	part("guest IPv6", guestIPv6, func(h Host) bool {
		return vms(h) && h.IPv6Prefix.IsValid() && h.GuestIPv6 != nil && h.Exit != nil
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var anon = driver.Credentials(driver.Anonymous)

func images(t *testing.T, h Host) {
	ctx := t.Context()
	ref, digest := h.Publish(t, "app:1")
	got, err := h.Images.Resolve(ctx, ref, anon)
	must(t, err)
	if got != digest {
		t.Errorf("resolved %s, published %s", got, digest)
	}
	// A reference by digest resolves to that digest; ref ends in its tag.
	pinned := ref[:strings.LastIndex(ref, ":")] + "@" + digest
	if got, err := h.Images.Resolve(ctx, pinned, anon); err != nil || got != digest {
		t.Errorf("resolving %s: %s %v", pinned, got, err)
	}
	if _, err := h.Images.Config(ctx, ref, digest, anon); err != nil {
		t.Errorf("config: %v", err)
	}
	if c, ok := cachedImage(t, h.Images, digest); !ok || c.UsedAt.IsZero() || len(c.Blobs) == 0 {
		t.Errorf("a pulled digest is cached as %+v (listed: %t)", c, ok)
	}
	dir := t.TempDir()
	must(t, h.Images.Unpack(ctx, ref, digest, dir, anon))
	if entries, err := os.ReadDir(dir); err != nil || len(entries) == 0 {
		t.Errorf("unpacked nothing: %v %v", entries, err)
	}
	absent := strings.Replace(ref, "app:1", "absent:1", 1)
	if _, err := h.Images.Resolve(ctx, absent, anon); !errors.Is(err, driver.ErrImageNotFound) {
		t.Errorf("resolving an image never published: %v, want not found", err)
	}

	// A pulled digest is the host's until forgotten, registry or not.
	h.Withdraw(t, ref)
	before := time.Now()
	if _, err := h.Images.Config(ctx, ref, digest, anon); err != nil {
		t.Errorf("config of a pulled digest with the registry down: %v", err)
	}
	if c, _ := cachedImage(t, h.Images, digest); c.UsedAt.Before(before) {
		t.Errorf("read from the cache at %v, it is last used at %v", before, c.UsedAt)
	}
	if err := h.Images.Unpack(ctx, ref, digest, t.TempDir(), anon); err != nil {
		t.Errorf("unpacking a pulled digest with the registry down: %v", err)
	}
	must(t, h.Images.Forget(ctx, digest))
	must(t, h.Images.Forget(ctx, digest))
	if _, ok := cachedImage(t, h.Images, digest); ok {
		t.Error("a forgotten digest is listed in the cache")
	}
	if _, err := h.Images.Config(ctx, ref, digest, anon); err == nil {
		t.Error("a forgotten digest answered with the registry down")
	}
}

// cachedImage returns what the cache lists of digest, checking the room it
// says it has.
func cachedImage(t *testing.T, images driver.Images, digest string) (driver.CachedImage, bool) {
	t.Helper()
	c, err := images.Cached(t.Context())
	must(t, err)
	if c.Total <= 0 || c.Free < 0 || c.Free > c.Total {
		t.Errorf("the image cache's room: %d free of %d", c.Free, c.Total)
	}
	for _, i := range c.Images {
		if i.Digest == digest {
			return i, true
		}
	}
	return driver.CachedImage{}, false
}

// baseOf returns the base of digest as listed.
func baseOf(t *testing.T, s driver.Storage, digest string) (driver.Base, bool) {
	t.Helper()
	bases, err := s.Bases(t.Context())
	must(t, err)
	for _, b := range bases {
		if b.Digest == digest {
			return b, true
		}
	}
	return driver.Base{}, false
}

// digestsOf returns the digests of the bases listed.
func digestsOf(t *testing.T, s driver.Storage) []string {
	t.Helper()
	bases, err := s.Bases(t.Context())
	must(t, err)
	var out []string
	for _, b := range bases {
		out = append(out, b.Digest)
	}
	return out
}

// privateImages: a private repository answers only its login, by tag and by
// digest. What the host has pulled from a repository it answers from its
// cache for that repository, whatever the credentials: proving a machine's
// login is its caller's, by Resolve (docs/decisions/secrets.md).
func privateImages(t *testing.T, h Host) {
	ctx := t.Context()
	ref, digest, login := h.PublishPrivate(t, "private:1")
	right := driver.Credentials(func(context.Context, string) (driver.Login, error) { return login, nil })
	wrong := driver.Credentials(func(context.Context, string) (driver.Login, error) {
		return driver.Login{Username: login.Username, Password: login.Password + "-wrong"}, nil
	})
	pinned := ref[:strings.LastIndex(ref, ":")] + "@" + digest
	for _, c := range []struct {
		what  string
		creds driver.Credentials
	}{{"no login", anon}, {"a wrong login", wrong}} {
		for _, r := range []string{ref, pinned} {
			if _, err := h.Images.Resolve(ctx, r, c.creds); !errors.Is(err, driver.ErrUnauthorized) {
				t.Errorf("resolving %s with %s: %v, want unauthorized", r, c.what, err)
			}
		}
		if _, err := h.Images.Config(ctx, ref, digest, c.creds); !errors.Is(err, driver.ErrUnauthorized) {
			t.Errorf("pulling with %s: %v, want unauthorized", c.what, err)
		}
	}
	if got, err := h.Images.Resolve(ctx, ref, right); err != nil || got != digest {
		t.Fatalf("resolving with the login: %s %v", got, err)
	}
	if got, err := h.Images.Resolve(ctx, pinned, right); err != nil || got != digest {
		t.Errorf("resolving %s with the login: %s %v", pinned, got, err)
	}
	if _, err := h.Images.Config(ctx, ref, digest, right); err != nil {
		t.Fatalf("pulling with the login: %v", err)
	}
	if err := h.Images.Unpack(ctx, ref, digest, t.TempDir(), right); err != nil {
		t.Errorf("unpacking with the login: %v", err)
	}
	// The cache answers what it holds from the repository named, whoever
	// asks; the registry still refuses them, which is what proves a login.
	if _, err := h.Images.Config(ctx, ref, digest, anon); err != nil {
		t.Errorf("config of a pulled digest of its repository without a login: %v", err)
	}
	if _, err := h.Images.Resolve(ctx, pinned, anon); !errors.Is(err, driver.ErrUnauthorized) {
		t.Errorf("resolving a pulled digest without a login: %v, want unauthorized", err)
	}
}

// repositoryConfusion: content the host pulled from one repository is no
// other's. A repository that says it has the digest, as any registry can,
// gets nothing from the cache: it must serve the image in full itself, and
// one that cannot fails, before and after a pull from it is tried; one that
// does, a genuine mirror, gets it (docs/decisions/secrets.md).
func repositoryConfusion(t *testing.T, h Host) {
	ctx := t.Context()
	ref, digest, login := h.PublishPrivate(t, "private:1")
	right := driver.Credentials(func(context.Context, string) (driver.Login, error) { return login, nil })
	if _, err := h.Images.Config(ctx, ref, digest, right); err != nil {
		t.Fatalf("pulling with the login: %v", err)
	}
	evil := h.Claim(t, "evil", digest)
	// Saying it has the digest is all a resolve proves.
	if got, err := h.Images.Resolve(ctx, evil, anon); err != nil || got != digest {
		t.Fatalf("precondition: %s claims %s: %s %v", evil, digest, got, err)
	}
	for range 2 {
		if _, err := h.Images.Config(ctx, evil, digest, anon); err == nil {
			t.Errorf("config of %s, which serves no layer of it, came from the cache", evil)
		}
		if err := h.Images.Unpack(ctx, evil, digest, t.TempDir(), anon); err == nil {
			t.Errorf("unpacked %s, which serves no layer of it, from the cache", evil)
		}
	}
	mirror := h.Mirror(t, "mirror:1", digest)
	if _, err := h.Images.Config(ctx, mirror, digest, anon); err != nil {
		t.Errorf("config of a genuine mirror %s: %v", mirror, err)
	}
	if err := h.Images.Unpack(ctx, mirror, digest, t.TempDir(), anon); err != nil {
		t.Errorf("unpacking a genuine mirror %s: %v", mirror, err)
	}
	if _, err := h.Images.Config(ctx, evil, digest, anon); err == nil {
		t.Errorf("config of %s came from the cache once a mirror had it", evil)
	}
}

// base makes the base of a published image.
func base(t *testing.T, h Host, name string) string {
	t.Helper()
	ref, digest := h.Publish(t, name)
	must(t, h.Storage.EnsureBase(t.Context(), digest, 0, func(dir string) error {
		return h.Images.Unpack(t.Context(), ref, digest, dir, anon)
	}))
	return digest
}

// Sizes of root disks the suite asks for, above any base it makes.
const (
	gib       = int64(1) << 30
	rootSize  = 1 * gib
	grownSize = 2 * gib
)

func storage(t *testing.T, h Host) {
	ctx := t.Context()
	// Made at once from two places, a base is made once.
	ref, digest := h.Publish(t, "app:1")
	var fills atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Storage.EnsureBase(ctx, digest, 0, func(dir string) error {
				fills.Add(1)
				// Both callers are in by now. A real driver blocks the second
				// in its own lock, which nothing outside it can see.
				time.Sleep(50 * time.Millisecond) //nolint:forbidigo // real elapsed time: the other caller reaching a real driver's lock
				return h.Images.Unpack(ctx, ref, digest, dir, anon)
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := fills.Load(); n != 1 {
		t.Errorf("a base made at once from two places was filled %d times", n)
	}
	filled := false
	must(t, h.Storage.EnsureBase(ctx, digest, 0, func(string) error { filled = true; return nil }))
	if filled {
		t.Error("an existing base was filled again")
	}
	if bases := digestsOf(t, h.Storage); !slices.Equal(bases, []string{digest}) {
		t.Errorf("bases %v", bases)
	}
	if b, _ := baseOf(t, h.Storage, digest); b.Size <= 0 || b.UsedAt.IsZero() {
		t.Errorf("a base made is listed as %+v", b)
	}
	// A fill that fails leaves no base.
	_, failing := h.Publish(t, "failing:1")
	failed := errors.New("the fill failed")
	if err := h.Storage.EnsureBase(ctx, failing, 0, func(string) error { return failed }); !errors.Is(err, failed) {
		t.Errorf("a failing fill: %v, want its error", err)
	}
	if _, ok := baseOf(t, h.Storage, failing); ok {
		t.Error("a failed fill left a base")
	}
	// An image larger than the room left is refused before it is unpacked.
	c, err := h.Storage.Capacity(ctx)
	must(t, err)
	// It says how much it needs, where to unpack it or for its base.
	var full *driver.NoSpaceError
	if err := h.Storage.EnsureBase(ctx, failing, c.Free+1, func(string) error {
		t.Error("an image larger than the room left was unpacked")
		return nil
	}); !errors.As(err, &full) || !errors.Is(err, driver.ErrNoSpace) || full.Need <= full.Free {
		t.Errorf("an image larger than the room left: %v, want no space, more needed than free", err)
	}
	// A fill that runs out of room says more is needed where it unpacks.
	full = nil
	// It knows only that it needs more than was free, and aims at more.
	if err := h.Storage.EnsureBase(ctx, failing, 1<<20, func(dir string) error {
		return &os.PathError{Op: "write", Path: dir, Err: syscall.ENOSPC}
	}); !errors.As(err, &full) || full.Room != driver.ImageRoom ||
		full.AtLeast() <= full.Free || full.AtLeast() >= full.Need {
		t.Errorf("a fill out of room: %v (%+v), want at least more than free needed where it unpacks, and more aimed at", err, full)
	}
	if _, ok := baseOf(t, h.Storage, failing); ok {
		t.Error("a fill out of room left a base")
	}

	before := time.Now()
	root, err := h.Storage.EnsureRoot(ctx, "m1", digest, rootSize)
	must(t, err)
	if root.MachineUID != "m1" || root.Digest != digest || root.Path == "" || root.Size < rootSize {
		t.Errorf("root %+v", root)
	}
	if b, _ := baseOf(t, h.Storage, digest); b.UsedAt.Before(before) {
		t.Errorf("cloned from at %v, its base is last used at %v", before, b.UsedAt)
	}
	again, err := h.Storage.EnsureRoot(ctx, "m1", digest, rootSize)
	if err != nil || again != root {
		t.Errorf("ensuring the root again: %+v %v", again, err)
	}
	other := base(t, h, "app:2")
	if _, err := h.Storage.EnsureRoot(ctx, "m1", other, rootSize); err == nil {
		t.Error("a root disk of one image was taken for another")
	}
	if roots, err := h.Storage.Roots(ctx); err != nil || len(roots) != 1 || roots[0] != root {
		t.Errorf("roots %v %v", roots, err)
	}
	if err := h.Storage.DeleteBase(ctx, digest); err == nil {
		t.Error("deleted a base a root disk was cloned from")
	}

	c, err = h.Storage.Capacity(ctx)
	if err != nil || c.Total <= 0 || c.Free < 0 || c.Free > c.Total {
		t.Errorf("capacity %+v %v", c, err)
	}

	// A root disk going is its base's last use.
	before = time.Now()
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
	if b, _ := baseOf(t, h.Storage, digest); b.UsedAt.Before(before) {
		t.Errorf("a root disk of it deleted at %v, the base is last used at %v", before, b.UsedAt)
	}
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
	must(t, h.Storage.DeleteBase(ctx, digest))
	must(t, h.Storage.DeleteBase(ctx, digest))
	must(t, h.Storage.DeleteBase(ctx, other))
	if roots, _ := h.Storage.Roots(ctx); len(roots) != 0 {
		t.Errorf("roots left: %v", roots)
	}
	if bases, _ := h.Storage.Bases(ctx); len(bases) != 0 {
		t.Errorf("bases left: %v", bases)
	}
}

// rootOf returns the machine's root disk as listed.
func rootOf(t *testing.T, s driver.Storage, machine string) (driver.Disk, bool) {
	t.Helper()
	roots, err := s.Roots(t.Context())
	must(t, err)
	for _, r := range roots {
		if r.MachineUID == machine {
			return r, true
		}
	}
	return driver.Disk{}, false
}

func rootSizes(t *testing.T, h Host) {
	ctx := t.Context()
	digest := base(t, h, "app:1")

	// Below the base's size is too small, and makes nothing.
	if _, err := h.Storage.EnsureRoot(ctx, "m1", digest, 1); !errors.Is(err, driver.ErrDiskTooSmall) {
		t.Errorf("a root of one byte: %v, want too small", err)
	}
	if _, ok := rootOf(t, h.Storage, "m1"); ok {
		t.Error("a root too small was made")
	}

	root, err := h.Storage.EnsureRoot(ctx, "m1", digest, rootSize)
	must(t, err)
	// Asked smaller, it is not shrunk.
	smaller, err := h.Storage.EnsureRoot(ctx, "m1", digest, rootSize/2)
	if err != nil || smaller != root {
		t.Errorf("asked smaller: %+v %v, want %+v", smaller, err, root)
	}
	// Asked larger, it grows, and is listed so.
	grown, err := h.Storage.EnsureRoot(ctx, "m1", digest, grownSize)
	must(t, err)
	if grown.Size < grownSize || grown.Path != root.Path {
		t.Errorf("grown %+v from %+v", grown, root)
	}
	if listed, _ := rootOf(t, h.Storage, "m1"); listed != grown {
		t.Errorf("listed %+v, grown %+v", listed, grown)
	}
	// Even too small for the base, an existing root is only kept.
	if kept, err := h.Storage.EnsureRoot(ctx, "m1", digest, 1); err != nil || kept != grown {
		t.Errorf("asked one byte of a grown root: %+v %v", kept, err)
	}
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
	must(t, h.Storage.DeleteBase(ctx, digest))
}

func deletingDisks(t *testing.T, h Host) {
	ctx := t.Context()
	a := base(t, h, "app:1")
	b := base(t, h, "app:2")
	// A base no root was cloned from goes at once.
	must(t, h.Storage.DeleteBase(ctx, b))

	// A base goes only once its last root has.
	var roots []driver.Disk
	for _, m := range []string{"m1", "m2", "m3"} {
		r, err := h.Storage.EnsureRoot(ctx, m, a, rootSize)
		must(t, err)
		roots = append(roots, r)
	}
	if paths := map[string]bool{roots[0].Path: true, roots[1].Path: true, roots[2].Path: true}; len(paths) != 3 {
		t.Errorf("roots share devices: %+v", roots)
	}
	must(t, h.Storage.DeleteRoot(ctx, "m2"))
	if err := h.Storage.DeleteBase(ctx, a); err == nil {
		t.Error("deleted a base two roots were cloned from")
	}

	// A root in use stays until it is let go.
	if h.Hold != nil {
		release := h.Hold(t, roots[0])
		if err := h.Storage.DeleteRoot(ctx, "m1"); err == nil {
			t.Error("deleted a root in use")
		}
		if _, ok := rootOf(t, h.Storage, "m1"); !ok {
			t.Error("a root in use is gone after a failed delete")
		}
		release()
	}
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
	if err := h.Storage.DeleteBase(ctx, a); err == nil {
		t.Error("deleted a base a root was cloned from")
	}
	must(t, h.Storage.DeleteRoot(ctx, "m3"))
	must(t, h.Storage.DeleteBase(ctx, a))
	if bases, _ := h.Storage.Bases(ctx); len(bases) != 0 {
		t.Errorf("bases left: %v", bases)
	}
	// Deleting what was never made is not an error.
	must(t, h.Storage.DeleteRoot(ctx, "never"))
	_, never := h.Publish(t, "never:1")
	must(t, h.Storage.DeleteBase(ctx, never))
}

func reopening(t *testing.T, h Host) {
	if h.Reopen == nil {
		t.Skip("the host cannot reopen its drivers")
	}
	ctx := t.Context()
	ref, digest := h.Publish(t, "app:1")
	must(t, h.Storage.EnsureBase(ctx, digest, 0, func(dir string) error {
		return h.Images.Unpack(ctx, ref, digest, dir, anon)
	}))
	r1, err := h.Storage.EnsureRoot(ctx, "m1", digest, rootSize)
	must(t, err)
	r2, err := h.Storage.EnsureRoot(ctx, "m2", digest, grownSize)
	must(t, err)
	base, _ := baseOf(t, h.Storage, digest)
	cached, _ := cachedImage(t, h.Images, digest)

	// What one instance made, another finds, the registry gone, and when
	// it was last used.
	h.Withdraw(t, ref)
	images, storage := h.Reopen(t)
	if bases := digestsOf(t, storage); !slices.Equal(bases, []string{digest}) {
		t.Errorf("bases after reopening: %v", bases)
	}
	if b, _ := baseOf(t, storage, digest); !b.UsedAt.Equal(base.UsedAt) {
		t.Errorf("the base last used at %v, after reopening at %v", base.UsedAt, b.UsedAt)
	}
	if c, _ := cachedImage(t, images, digest); !c.UsedAt.Equal(cached.UsedAt) {
		t.Errorf("the cached digest last used at %v, after reopening at %v", cached.UsedAt, c.UsedAt)
	}
	roots, err := storage.Roots(ctx)
	must(t, err)
	slices.SortFunc(roots, func(a, b driver.Disk) int { return strings.Compare(a.MachineUID, b.MachineUID) })
	if !slices.Equal(roots, []driver.Disk{r1, r2}) {
		t.Errorf("roots after reopening: %+v, want %+v %+v", roots, r1, r2)
	}
	if _, err := images.Config(ctx, ref, digest, anon); err != nil {
		t.Errorf("config of a pulled digest after reopening: %v", err)
	}
	must(t, storage.EnsureBase(ctx, digest, 0, func(string) error {
		t.Error("a base made before reopening was filled again")
		return nil
	}))
	if again, err := storage.EnsureRoot(ctx, "m1", digest, rootSize); err != nil || again != r1 {
		t.Errorf("ensuring a root after reopening: %+v %v", again, err)
	}
	must(t, storage.DeleteRoot(ctx, "m1"))
	must(t, storage.DeleteRoot(ctx, "m2"))
	must(t, storage.DeleteBase(ctx, digest))
	must(t, images.Forget(ctx, digest))
}

// volumeOf returns the Disk's volume as listed.
func volumeOf(t *testing.T, s driver.Storage, disk string) (driver.Volume, bool) {
	t.Helper()
	vols, err := s.Volumes(t.Context())
	must(t, err)
	for _, v := range vols {
		if v.DiskUID == disk {
			return v, true
		}
	}
	return driver.Volume{}, false
}

func volumes(t *testing.T, h Host) {
	ctx := t.Context()
	v1, err := h.Storage.EnsureVolume(ctx, "d1", rootSize, "")
	must(t, err)
	if v1.DiskUID != "d1" || v1.Path == "" || v1.Size < rootSize {
		t.Errorf("volume %+v", v1)
	}
	if again, err := h.Storage.EnsureVolume(ctx, "d1", rootSize, ""); err != nil || again != v1 {
		t.Errorf("ensuring the volume again: %+v %v", again, err)
	}
	// Asked smaller, it is kept; asked larger, it grows, and is listed so.
	if kept, err := h.Storage.EnsureVolume(ctx, "d1", 1, ""); err != nil || kept != v1 {
		t.Errorf("asked smaller: %+v %v, want %+v", kept, err, v1)
	}
	grown, err := h.Storage.EnsureVolume(ctx, "d1", grownSize, "")
	must(t, err)
	if grown.Size < grownSize || grown.Path != v1.Path {
		t.Errorf("grown %+v from %+v", grown, v1)
	}
	if listed, ok := volumeOf(t, h.Storage, "d1"); !ok || listed != grown {
		t.Errorf("listed %+v %t, grown %+v", listed, ok, grown)
	}
	v2, err := h.Storage.EnsureVolume(ctx, "d2", rootSize, "")
	must(t, err)
	if v2.Path == v1.Path {
		t.Errorf("volumes share a device: %+v %+v", v1, v2)
	}
	// Volumes are not root disks.
	if roots, err := h.Storage.Roots(ctx); err != nil || len(roots) != 0 {
		t.Errorf("roots %v %v", roots, err)
	}
	// A volume in use stays until it is let go.
	if h.HoldVolume != nil {
		release := h.HoldVolume(t, v2)
		if err := h.Storage.DeleteVolume(ctx, "d2"); err == nil {
			t.Error("deleted a volume in use")
		}
		if _, ok := volumeOf(t, h.Storage, "d2"); !ok {
			t.Error("a volume in use is gone after a failed delete")
		}
		release()
	}
	// What one instance made, another finds.
	if h.Reopen != nil {
		_, storage := h.Reopen(t)
		vols, err := storage.Volumes(ctx)
		must(t, err)
		slices.SortFunc(vols, func(a, b driver.Volume) int { return strings.Compare(a.DiskUID, b.DiskUID) })
		if !slices.Equal(vols, []driver.Volume{grown, v2}) {
			t.Errorf("volumes after reopening: %+v, want %+v %+v", vols, grown, v2)
		}
	}
	must(t, h.Storage.DeleteVolume(ctx, "d1"))
	must(t, h.Storage.DeleteVolume(ctx, "d1"))
	must(t, h.Storage.DeleteVolume(ctx, "d2"))
	must(t, h.Storage.DeleteVolume(ctx, "never"))
	if vols, err := h.Storage.Volumes(ctx); err != nil || len(vols) != 0 {
		t.Errorf("volumes left: %v %v", vols, err)
	}
}

// diskImageSize is the disk the suite's disk images hold.
const diskImageSize = 64 << 20

func diskImages(t *testing.T, h Host) {
	ctx := t.Context()
	url, digest := h.PublishDiskImage(t, "cloud", diskImageSize)
	other := "sha256:" + strings.Repeat("0", 64)
	if _, err := h.DiskImages.Fetch(ctx, url, other); !errors.Is(err, driver.ErrDigestMismatch) {
		t.Errorf("fetching with another digest: %v, want a mismatch", err)
	}
	img, err := h.DiskImages.Fetch(ctx, url, digest)
	must(t, err)
	if img.Size() != diskImageSize {
		t.Errorf("the image holds %d bytes, want %d", img.Size(), diskImageSize)
	}
	// A fill that fails leaves no base.
	failed := errors.New("the fill failed")
	if err := h.Storage.EnsureImageBase(ctx, digest, img.Size(), func(string) error { return failed }); !errors.Is(err, failed) {
		t.Errorf("a failing fill: %v, want its error", err)
	}
	if bases, err := h.Storage.ImageBases(ctx); err != nil || len(bases) != 0 {
		t.Errorf("image bases after a failed fill: %v %v", bases, err)
	}
	must(t, h.Storage.EnsureImageBase(ctx, digest, img.Size(), func(dev string) error { return img.WriteTo(ctx, dev) }))
	must(t, img.Close())
	filled := false
	must(t, h.Storage.EnsureImageBase(ctx, digest, diskImageSize, func(string) error { filled = true; return nil }))
	if filled {
		t.Error("an existing image base was filled again")
	}
	if bases, err := h.Storage.ImageBases(ctx); err != nil || !slices.Equal(bases, []string{digest}) {
		t.Errorf("image bases %v %v", bases, err)
	}
	// Image bases are not OCI images' bases.
	if bases, err := h.Storage.Bases(ctx); err != nil || len(bases) != 0 {
		t.Errorf("bases %v %v", bases, err)
	}

	// A volume of it is no smaller than it, and grows.
	if _, err := h.Storage.EnsureVolume(ctx, "d1", diskImageSize/2, digest); !errors.Is(err, driver.ErrDiskTooSmall) {
		t.Errorf("a volume smaller than its image: %v, want too small", err)
	}
	if _, ok := volumeOf(t, h.Storage, "d1"); ok {
		t.Error("a volume too small was made")
	}
	v, err := h.Storage.EnsureVolume(ctx, "d1", rootSize, digest)
	must(t, err)
	if v.Digest != digest || v.Size < rootSize || v.Path == "" {
		t.Errorf("volume %+v", v)
	}
	if listed, ok := volumeOf(t, h.Storage, "d1"); !ok || listed != v {
		t.Errorf("listed %+v %t, made %+v", listed, ok, v)
	}
	if _, err := h.Storage.EnsureVolume(ctx, "d1", rootSize, ""); err == nil {
		t.Error("a volume of an image was taken for a blank one")
	}
	if _, err := h.Storage.EnsureVolume(ctx, "d2", rootSize, other); err == nil {
		t.Error("made a volume of a disk image with no base")
	}
	if err := h.Storage.DeleteImageBase(ctx, digest); err == nil {
		t.Error("deleted an image base a volume was cloned from")
	}
	if h.Reopen != nil {
		_, storage := h.Reopen(t)
		if bases, err := storage.ImageBases(ctx); err != nil || !slices.Equal(bases, []string{digest}) {
			t.Errorf("image bases after reopening: %v %v", bases, err)
		}
		if listed, ok := volumeOf(t, storage, "d1"); !ok || listed != v {
			t.Errorf("after reopening, listed %+v %t, made %+v", listed, ok, v)
		}
	}
	must(t, h.Storage.DeleteVolume(ctx, "d1"))
	must(t, h.Storage.DeleteImageBase(ctx, digest))
	must(t, h.Storage.DeleteImageBase(ctx, digest))
	if bases, err := h.Storage.ImageBases(ctx); err != nil || len(bases) != 0 {
		t.Errorf("image bases left: %v %v", bases, err)
	}
}

var testNet = driver.NetworkSpec{
	UID: "n1", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31"),
}

// eth0 is machine's eth0 on testNet, with a MAC of its own, as no two
// interfaces of a network may share one.
func eth0(machine string) driver.Interface {
	sum := sha256.Sum256([]byte(machine))
	return driver.Interface{
		MachineUID: machine, Name: "eth0", NetworkUID: testNet.UID, MAC: fmt.Sprintf("02:01:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3]),
		Address: netip.MustParsePrefix("10.0.0.2/24"), Gateway: netip.MustParseAddr("10.0.0.1"),
	}
}

func network(t *testing.T, h Host) {
	ctx := t.Context()
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	if nets, err := h.Network.Networks(ctx); err != nil || len(nets) != 1 || nets[0] != testNet {
		t.Errorf("networks %v %v", nets, err)
	}
	made, err := h.Network.EnsureInterface(ctx, eth0("m1"))
	must(t, err)
	if made.Device == "" {
		t.Error("an interface without a device")
	}
	if again, err := h.Network.EnsureInterface(ctx, eth0("m1")); err != nil || again != made {
		t.Errorf("ensuring the interface again: %+v %v", again, err)
	}
	if ifs, err := h.Network.Interfaces(ctx); err != nil || len(ifs) != 1 || ifs[0] != made {
		t.Errorf("interfaces %+v %v, want them as ensured", ifs, err)
	}
	// Machines made in the same instant still get taps of their own, with
	// names Linux takes.
	other := eth0("m1-and-a-longer-uid-sharing-its-prefix")
	other.Address = netip.MustParsePrefix("10.0.0.3/24")
	second, err := h.Network.EnsureInterface(ctx, other)
	must(t, err)
	for _, d := range []string{made.Device, second.Device} {
		if len(d) > 15 {
			t.Errorf("device %q is longer than Linux allows", d)
		}
	}
	if second.Device == made.Device {
		t.Errorf("two machines share the device %q", made.Device)
	}
	// One MAC's frames go to one tap.
	twin := eth0("m3")
	twin.MAC, twin.Address = strings.ToUpper(made.MAC), netip.MustParsePrefix("10.0.0.4/24")
	if _, err := h.Network.EnsureInterface(ctx, twin); !errors.Is(err, driver.ErrMACInUse) {
		t.Errorf("an interface given the MAC another of its network has: %v, want ErrMACInUse", err)
	}
	if ifs, _ := h.Network.Interfaces(ctx); len(ifs) != 2 {
		t.Errorf("after refusing a MAC, interfaces %+v", ifs)
	}
	must(t, h.Network.DeleteInterfaces(ctx, other.MachineUID))
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
	if ifs, _ := h.Network.Interfaces(ctx); len(ifs) != 0 {
		t.Errorf("interfaces left: %v", ifs)
	}
	if nets, _ := h.Network.Networks(ctx); len(nets) != 0 {
		t.Errorf("networks left: %v", nets)
	}
}

// networkChanges holds the host to the spec it is ensured to as the spec
// changes, and refuses interfaces no network on it can take.
func networkChanges(t *testing.T, h Host) {
	ctx := t.Context()
	other := driver.NetworkSpec{
		UID: "n2", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.2/31"),
	}
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	must(t, h.Network.EnsureNetwork(ctx, other))
	networks := func() map[string]driver.NetworkSpec {
		t.Helper()
		nets, err := h.Network.Networks(ctx)
		must(t, err)
		m := map[string]driver.NetworkSpec{}
		for _, n := range nets {
			m[n.UID] = n
		}
		return m
	}
	if nets := networks(); len(nets) != 2 || nets[testNet.UID] != testNet || nets[other.UID] != other {
		t.Errorf("two networks with the same range listed as %v", nets)
	}
	other.CIDR = netip.MustParsePrefix("10.1.0.0/16")
	other.Transit = netip.MustParsePrefix("100.64.0.4/31")
	must(t, h.Network.EnsureNetwork(ctx, other))
	if nets := networks(); nets[other.UID] != other {
		t.Errorf("a network ensured to another range and transit is listed as %+v", nets[other.UID])
	}

	nowhere := eth0("m1")
	nowhere.NetworkUID = "absent"
	if _, err := h.Network.EnsureInterface(ctx, nowhere); err == nil {
		t.Error("made an interface on a network the host does not have")
	}
	outside := eth0("m1")
	outside.Address = netip.MustParsePrefix("10.9.0.2/24")
	if _, err := h.Network.EnsureInterface(ctx, outside); err == nil {
		t.Error("made an interface with an address outside its network")
	}
	if ifs, _ := h.Network.Interfaces(ctx); len(ifs) != 0 {
		t.Errorf("refused interfaces are listed: %+v", ifs)
	}

	// An interface ensured again with another spec is listed once, as
	// ensured last, wherever that puts it.
	nic := eth0("m1")
	_, err := h.Network.EnsureInterface(ctx, nic)
	must(t, err)
	nic.MAC = "02:00:00:00:00:02"
	nic.NetworkUID = other.UID
	nic.Address = netip.MustParsePrefix("10.1.0.2/16")
	nic.Gateway = netip.MustParseAddr("10.1.0.1")
	moved, err := h.Network.EnsureInterface(ctx, nic)
	must(t, err)
	if ifs, err := h.Network.Interfaces(ctx); err != nil || len(ifs) != 1 || ifs[0] != moved {
		t.Errorf("an interface moved to another network is listed as %+v %v, want %+v", ifs, err, moved)
	}
	second := eth0("m1")
	second.Name = "eth1"
	second.MAC = "02:00:00:00:00:03"
	second, err = h.Network.EnsureInterface(ctx, second)
	must(t, err)
	if second.Device == moved.Device {
		t.Errorf("two interfaces of a machine share the device %q", moved.Device)
	}

	// A network deleted with interfaces in it takes them with it.
	must(t, h.Network.DeleteNetwork(ctx, other.UID))
	if ifs, err := h.Network.Interfaces(ctx); err != nil || len(ifs) != 1 || ifs[0] != second {
		t.Errorf("after deleting the network of one, interfaces %+v %v, want only %+v", ifs, err, second)
	}
	if nets := networks(); len(nets) != 1 || nets[testNet.UID] != testNet {
		t.Errorf("after deleting one network, %v", nets)
	}
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
	if nets := networks(); len(nets) != 0 {
		t.Errorf("networks left: %v", nets)
	}
}

// networkSockets opens sockets in a network's namespace, each tied to the
// instance of the namespace it was opened in.
func networkSockets(t *testing.T, h Host) {
	ctx := t.Context()
	dns := netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), 53)
	if got, err := h.Network.Instance(ctx, testNet.UID); err != nil || got != "" {
		t.Errorf("a network not on the host has instance %q %v", got, err)
	}
	if c, _, err := h.Network.ListenUDP(ctx, testNet.UID, dns); err == nil {
		_ = c.Close()
		t.Error("listened in a network not on the host")
	}

	must(t, h.Network.EnsureNetwork(ctx, testNet))
	first, err := h.Network.Instance(ctx, testNet.UID)
	must(t, err)
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	if again, err := h.Network.Instance(ctx, testNet.UID); err != nil || first == "" || again != first {
		t.Errorf("instances %q then %q %v, want one, kept by an ensure", first, again, err)
	}
	udp, in, err := h.Network.ListenUDP(ctx, testNet.UID, dns)
	must(t, err)
	if in != first {
		t.Errorf("a socket opened in instance %q, want %q", in, first)
	}
	tcp, in, err := h.Network.ListenTCP(ctx, testNet.UID, dns)
	must(t, err)
	if in != first {
		t.Errorf("a socket opened in instance %q, want %q", in, first)
	}
	must(t, tcp.Close())
	if c, _, err := h.Network.ListenUDP(ctx, testNet.UID, dns); err == nil {
		_ = c.Close()
		t.Error("two sockets on one address")
	}
	if _, _, err := h.Network.ListenUDP(ctx, testNet.UID, netip.AddrPortFrom(netip.MustParseAddr("10.9.0.1"), 53)); err == nil {
		t.Error("listened on an address the network does not have")
	}
	// A service answering clients that have no address yet, as DHCP,
	// listens on every address of the bridge.
	any, in, err := h.Network.ListenUDP(ctx, testNet.UID, netip.AddrPortFrom(netip.IPv4Unspecified(), 67))
	must(t, err)
	if in != first {
		t.Errorf("a socket opened in instance %q, want %q", in, first)
	}
	must(t, any.Close())
	// Every network's bridge holds the metadata service's address.
	md, _, err := h.Network.ListenTCP(ctx, testNet.UID, netip.AddrPortFrom(driver.MetadataAddr, 80))
	must(t, err)
	must(t, md.Close())
	if nets, err := h.Network.Networks(ctx); err != nil || len(nets) != 1 || nets[0] != testNet {
		t.Errorf("with the metadata address on its bridge, the network is listed as %v %v", nets, err)
	}

	// A network made again is another instance, while a socket of the old
	// one lives on.
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
	if got, err := h.Network.Instance(ctx, testNet.UID); err != nil || got != "" {
		t.Errorf("a deleted network has instance %q %v", got, err)
	}
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	second, err := h.Network.Instance(ctx, testNet.UID)
	must(t, err)
	if second == "" || second == first {
		t.Errorf("a network made again has instance %q, before %q", second, first)
	}
	again, in, err := h.Network.ListenUDP(ctx, testNet.UID, dns)
	must(t, err)
	if in != second {
		t.Errorf("a socket opened in instance %q, want %q", in, second)
	}
	must(t, again.Close())
	must(t, udp.Close())
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
}

// networkIPv6 gives interfaces IPv6 addresses of the host's prefix, and
// opens a network's packet sockets, each tied to its instance as other
// sockets are.
func networkIPv6(t *testing.T, h Host) {
	ctx := t.Context()
	at := func(id byte) netip.Addr {
		b := h.IPv6Prefix.Addr().As16()
		b[8], b[15] = 0x8a, id
		return netip.AddrFrom16(b)
	}
	if c, _, err := h.Network.ListenPackets(ctx, testNet.UID, driver.DHCPv6); err == nil {
		_ = c.Close()
		t.Error("opened a packet socket in a network not on the host")
	}
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	nic := eth0("m1")
	nic.IPv6 = at(1)
	made, err := h.Network.EnsureInterface(ctx, nic)
	must(t, err)
	if ifs, err := h.Network.Interfaces(ctx); err != nil || len(ifs) != 1 || ifs[0] != made || ifs[0].IPv6 != at(1) {
		t.Errorf("interfaces %+v %v, want one with %s", ifs, err, at(1))
	}
	nic.IPv6 = at(2)
	moved, err := h.Network.EnsureInterface(ctx, nic)
	must(t, err)
	if ifs, _ := h.Network.Interfaces(ctx); len(ifs) != 1 || ifs[0] != moved || ifs[0].IPv6 != at(2) {
		t.Errorf("an interface given another IPv6 address is listed as %+v", ifs)
	}
	// One address is delivered to one interface: another takes it only once
	// the first has let go.
	taker := eth0("m3")
	taker.MAC, taker.Address, taker.IPv6 = "02:00:00:00:00:03", netip.MustParsePrefix("10.0.0.4/24"), at(2)
	if _, err := h.Network.EnsureInterface(ctx, taker); !errors.Is(err, driver.ErrIPv6InUse) {
		t.Errorf("an interface given the IPv6 address another holds: %v, want ErrIPv6InUse", err)
	}
	nic.IPv6 = at(3)
	_, err = h.Network.EnsureInterface(ctx, nic)
	must(t, err)
	_, err = h.Network.EnsureInterface(ctx, taker)
	must(t, err)
	must(t, h.Network.DeleteInterfaces(ctx, "m3"))
	outside := eth0("m2")
	outside.MAC, outside.Address = "02:00:00:00:00:02", netip.MustParsePrefix("10.0.0.3/24")
	outside.IPv6 = netip.MustParseAddr("2001:db8:ffff:ffff::1")
	if h.IPv6Prefix.Contains(outside.IPv6) {
		t.Fatalf("the host's prefix %s holds the test's outside address", h.IPv6Prefix)
	}
	if _, err := h.Network.EnsureInterface(ctx, outside); err == nil {
		t.Error("made an interface with an IPv6 address outside the host's prefix")
	}

	first, err := h.Network.Instance(ctx, testNet.UID)
	must(t, err)
	var conns []driver.PacketConn
	for _, kind := range []driver.PacketKind{driver.RouterSolicitations, driver.DHCPv6} {
		c, in, err := h.Network.ListenPackets(ctx, testNet.UID, kind)
		must(t, err)
		if in != first {
			t.Errorf("a packet socket opened in instance %q, want %q", in, first)
		}
		conns = append(conns, c)
	}
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	c, in, err := h.Network.ListenPackets(ctx, testNet.UID, driver.DHCPv6)
	must(t, err)
	if second, _ := h.Network.Instance(ctx, testNet.UID); in != second || in == first {
		t.Errorf("a packet socket of a network made again opened in instance %q, the network's is %q, before %q", in, second, first)
	}
	must(t, c.Close())
	for _, c := range conns {
		must(t, c.Close())
	}
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
}

// networkWithoutIPv6 refuses IPv6 on a host without a prefix.
func networkWithoutIPv6(t *testing.T, h Host) {
	ctx := t.Context()
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	nic := eth0("m1")
	nic.IPv6 = netip.MustParseAddr("2001:db8:0:1::5")
	if _, err := h.Network.EnsureInterface(ctx, nic); err == nil {
		t.Error("made an interface with an IPv6 address on a host without a prefix")
	}
	if c, _, err := h.Network.ListenPackets(ctx, testNet.UID, driver.RouterSolicitations); err == nil {
		_ = c.Close()
		t.Error("opened a packet socket on a host without a prefix")
	}
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
}

// networkFirewall ensures a network's interfaces' policies: only on a
// network the host has, and ignoring interfaces it has not.
func networkFirewall(t *testing.T, h Host) {
	ctx := t.Context()
	open := secgroup.Policy{Ingress: []secgroup.Rule{{Protocol: secgroup.All, Peers: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}, EgressAll: true}
	if err := h.Network.EnsureFirewall(ctx, testNet.UID, nil); err == nil {
		t.Error("ensured the firewall of a network not on the host")
	}
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	_, err := h.Network.EnsureInterface(ctx, eth0("m1"))
	must(t, err)
	must(t, h.Network.EnsureFirewall(ctx, testNet.UID, []driver.InterfacePolicy{
		{MachineUID: "m1", Name: "eth0", Policy: open},
		{MachineUID: "elsewhere", Name: "eth0", Policy: open},
	}))
	// Past the bound, policies are refused; it counts only the
	// interfaces on the host.
	var peers []netip.Prefix
	for i := range 100 {
		peers = append(peers, netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 1, 0, byte(i)}), 32))
	}
	big := secgroup.Policy{Ingress: []secgroup.Rule{{Protocol: secgroup.TCP, Peers: peers,
		Ports: slices.Repeat([]secgroup.PortRange{{From: 80, To: 80}}, driver.MaxFirewallRules/len(peers)+1)}}}
	if err := h.Network.EnsureFirewall(ctx, testNet.UID, []driver.InterfacePolicy{{MachineUID: "m1", Name: "eth0", Policy: big}}); err == nil {
		t.Error("a firewall past its bound was made")
	}
	must(t, h.Network.EnsureFirewall(ctx, testNet.UID, []driver.InterfacePolicy{{MachineUID: "elsewhere", Name: "eth0", Policy: big}}))
	must(t, h.Network.EnsureFirewall(ctx, testNet.UID, nil))
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Network.DeleteNetwork(ctx, testNet.UID))
}

// vmSpec makes what a VM of machine needs and returns its spec.
func vmSpec(t *testing.T, h Host, machine string, restart driver.RestartPolicy) driver.VMSpec {
	t.Helper()
	ctx := t.Context()
	digest := base(t, h, "app:1")
	root, err := h.Storage.EnsureRoot(ctx, machine, digest, rootSize)
	must(t, err)
	must(t, h.Network.EnsureNetwork(ctx, testNet))
	nic, err := h.Network.EnsureInterface(ctx, eth0(machine))
	must(t, err)
	run := h.Run
	if len(run.Command) == 0 {
		run = driver.RunSpec{Command: []string{"sleep", "infinity"}}
	}
	return driver.VMSpec{
		MachineUID: machine, Machine: "test/" + machine, VCPUs: 1, MemoryMiB: 256,
		Root: root, Interfaces: []driver.Interface{nic},
		Run:     run,
		Restart: restart,
	}
}

func vmOf(t *testing.T, h Host, machine string) (driver.VM, bool) {
	t.Helper()
	return vmIn(t, h.Hypervisor, machine)
}

func vmIn(t *testing.T, hv driver.Hypervisor, machine string) (driver.VM, bool) {
	t.Helper()
	vms, err := hv.VMs(t.Context())
	must(t, err)
	for _, v := range vms {
		if v.MachineUID == machine {
			return v, true
		}
	}
	return driver.VM{}, false
}

// vmWait bounds a wait on a VM changing state, which on a real host
// boots or stops a guest.
const vmWait = 30 * time.Second

func hypervisor(t *testing.T, h Host) {
	ctx := t.Context()
	spec := vmSpec(t, h, "m1", driver.RestartAlways)
	vol, err := h.Storage.EnsureVolume(ctx, "d1", rootSize, "")
	must(t, err)
	spec.Disks = []driver.Volume{vol}
	// A mount is of one of the VM's volumes, at a target.
	for name, m := range map[string]driver.Mount{
		"of no volume of its": {Disk: 1, Name: "d1", Target: "/data"},
		"at no target":        {Disk: 0, Name: "d1"},
	} {
		bad := spec
		bad.Mounts = []driver.Mount{m}
		if err := h.Hypervisor.Start(ctx, bad); err == nil {
			t.Errorf("started a VM with a mount %s", name)
			must(t, h.Hypervisor.Stop(ctx, "m1"))
		}
	}
	must(t, h.Hypervisor.Start(ctx, spec))
	vm, ok := vmOf(t, h, "m1")
	if !ok || vm.State != driver.VMRunning || vm.SpecHash != spec.Hash() {
		t.Fatalf("VM %+v listed %t", vm, ok)
	}
	testutil.EventuallyWithin(t, vmWait, "the guest agent answering", func() bool { return h.Guest.Ready(ctx, "m1") == nil })

	must(t, h.Hypervisor.Start(ctx, spec))
	if again, _ := vmOf(t, h, "m1"); !again.Started.Equal(vm.Started) {
		t.Error("starting the same spec again restarted the VM")
	}
	other := spec
	other.MemoryMiB = 512
	if err := h.Hypervisor.Start(ctx, other); err == nil {
		t.Error("started another spec over a running VM")
	}
	if err := h.Storage.DeleteRoot(ctx, "m1"); err == nil {
		t.Error("deleted the root disk of a running VM")
	}
	if err := h.Network.DeleteInterfaces(ctx, "m1"); err == nil {
		t.Error("deleted the interfaces of a running VM")
	}
	if err := h.Hypervisor.DeleteLogs(ctx, "m1"); err == nil {
		t.Error("deleted the console log of a running VM")
	}
	if err := h.Storage.DeleteVolume(ctx, "d1"); err == nil {
		t.Error("deleted a volume a running VM has")
	}

	// What the host runs is read back from it, not remembered.
	if h.ReopenHypervisor != nil {
		fresh := h.ReopenHypervisor(t)
		got, ok := vmIn(t, fresh, "m1")
		if !ok || got.SpecHash != vm.SpecHash || got.State != driver.VMRunning || !got.Started.Equal(vm.Started) {
			t.Errorf("a new instance lists %+v %t, the first listed %+v", got, ok, vm)
		}
		must(t, fresh.Start(ctx, spec))
		if err := fresh.Start(ctx, other); err == nil {
			t.Error("a new instance started another spec over a running VM")
		}
		if again, _ := vmOf(t, h, "m1"); !again.Started.Equal(vm.Started) {
			t.Error("a new instance starting the same spec restarted the VM")
		}
	}

	must(t, h.Hypervisor.Stop(ctx, "m1"))
	must(t, h.Hypervisor.Stop(ctx, "m1"))
	if _, ok := vmOf(t, h, "m1"); ok {
		t.Error("a stopped VM is still listed")
	}
	if err := h.Guest.Ready(ctx, "m1"); err == nil {
		t.Error("the guest of a stopped VM answers")
	}
	if !h.AskedToStop(t, "m1") {
		t.Error("the VM was stopped without its guest being asked to stop its process")
	}
	// Stopped, it stays down, though its policy is always.
	testutil.Unchanged(t, "the stopped VM", 3*time.Second, func() (string, error) {
		if vm, ok := vmOf(t, h, "m1"); ok {
			return "", fmt.Errorf("came back: %+v", vm)
		}
		return "not listed", nil
	})
	// The log outlives the VM until the machine is gone.
	if logs, err := h.Hypervisor.Logs(ctx); err != nil || !slices.Contains(logs, "m1") {
		t.Errorf("logs after the VM stopped: %v %v", logs, err)
	}
	must(t, h.Hypervisor.DeleteLogs(ctx, "m1"))
	must(t, h.Hypervisor.DeleteLogs(ctx, "m1"))
	if logs, err := h.Hypervisor.Logs(ctx); err != nil || slices.Contains(logs, "m1") {
		t.Errorf("logs after deleting them: %v %v", logs, err)
	}
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
	must(t, h.Storage.DeleteVolume(ctx, "d1"))
}

func restarts(t *testing.T, h Host) {
	ctx := t.Context()
	for _, tt := range []struct {
		name    string
		policy  driver.RestartPolicy
		code    int
		restart bool
	}{
		{"always after success", driver.RestartAlways, 0, true},
		{"always after failure", driver.RestartAlways, 3, true},
		{"on failure after success", driver.RestartOnFailure, 0, false},
		{"on failure after failure", driver.RestartOnFailure, 3, true},
		{"never after success", driver.RestartNever, 0, false},
		{"never after failure", driver.RestartNever, 3, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := vmSpec(t, h, "m1", tt.policy)
			// A failed case leaves nothing for the next.
			t.Cleanup(func() {
				_ = h.Hypervisor.Stop(ctx, "m1")
				_ = h.Hypervisor.DeleteLogs(ctx, "m1")
				_ = h.Network.DeleteInterfaces(ctx, "m1")
				_ = h.Storage.DeleteRoot(ctx, "m1")
			})
			must(t, h.Hypervisor.Start(ctx, spec))
			first, _ := vmOf(t, h, "m1")
			testutil.EventuallyWithin(t, vmWait, "the guest agent answering", func() bool { return h.Guest.Ready(ctx, "m1") == nil })
			for len(h.Hypervisor.Changes()) > 0 { // what starting it signalled
				<-h.Hypervisor.Changes()
			}
			h.Exit(t, "m1", tt.code)
			select {
			case <-h.Hypervisor.Changes():
			case <-time.After(30 * time.Second):
				t.Fatal("the VM's exit was never signalled")
			}
			if tt.restart {
				testutil.EventuallyWithin(t, vmWait, "the VM started again", func() bool {
					vm, ok := vmOf(t, h, "m1")
					return ok && vm.State == driver.VMRunning && vm.Started.After(first.Started)
				})
			} else {
				testutil.EventuallyWithin(t, vmWait, "the VM listed as exited", func() bool {
					vm, ok := vmOf(t, h, "m1")
					return ok && vm.State == driver.VMExited && vm.ExitCode == tt.code
				})
				if h.ReopenHypervisor != nil {
					if vm, ok := vmIn(t, h.ReopenHypervisor(t), "m1"); !ok || vm.State != driver.VMExited || vm.ExitCode != tt.code {
						t.Errorf("a new instance lists the exited VM as %+v %t", vm, ok)
					}
				}
				// Starting the same spec again runs it again.
				must(t, h.Hypervisor.Start(ctx, spec))
				if vm, ok := vmOf(t, h, "m1"); !ok || vm.State != driver.VMRunning {
					t.Errorf("an exited VM started again is listed as %+v %t", vm, ok)
				}
			}
			must(t, h.Hypervisor.Stop(ctx, "m1"))
			must(t, h.Hypervisor.DeleteLogs(ctx, "m1"))
			must(t, h.Network.DeleteInterfaces(ctx, "m1"))
			must(t, h.Storage.DeleteRoot(ctx, "m1"))
		})
	}
}

// guestIPv6 changes a container's guest's public IPv6 while it runs: the
// guest boots with its boot disk's, which the VM lists as its interfaces as
// started, takes what SetIPv6 says, and boots again with its boot disk's.
func guestIPv6(t *testing.T, h Host) {
	ctx := t.Context()
	at := func(id byte) netip.Addr {
		b := h.IPv6Prefix.Addr().As16()
		b[8], b[15] = 0x8a, id
		return netip.AddrFrom16(b)
	}
	spec := vmSpec(t, h, "m1", driver.RestartAlways)
	nic := spec.Interfaces[0]
	nic.IPv6 = at(1)
	booted, err := h.Network.EnsureInterface(ctx, nic)
	must(t, err)
	spec.Interfaces = []driver.Interface{booted}
	must(t, h.Hypervisor.Start(ctx, spec))
	testutil.EventuallyWithin(t, vmWait, "the guest agent answering", func() bool { return h.Guest.Ready(ctx, "m1") == nil })
	holds := func(what string, want []driver.GuestIPv6) {
		t.Helper()
		testutil.EventuallyWithin(t, vmWait, what, func() (bool, string) {
			got := h.GuestIPv6(t, "m1")
			return slices.Equal(got, want), fmt.Sprintf("the guest holds %+v, want %+v", got, want)
		})
	}
	bootDisk := driver.GuestIPv6Of(spec.Interfaces)
	holds("the guest holding its boot disk's address", bootDisk)
	listed := func(what string) {
		t.Helper()
		vm, ok := vmOf(t, h, "m1")
		if !ok || !slices.Equal(vm.Interfaces, spec.Interfaces) || vm.SpecHash != spec.Hash() {
			t.Errorf("%s: the VM lists interfaces %+v, hash %s; it started with %+v, hash %s", what, vm.Interfaces, vm.SpecHash, spec.Interfaces, spec.Hash())
		}
	}
	listed("started")
	if h.ReopenHypervisor != nil {
		if vm, ok := vmIn(t, h.ReopenHypervisor(t), "m1"); !ok || !slices.Equal(vm.Interfaces, spec.Interfaces) {
			t.Errorf("a new instance lists interfaces %+v, the VM started with %+v", vm.Interfaces, spec.Interfaces)
		}
	}

	nic.IPv6 = at(2)
	movedNIC, err := h.Network.EnsureInterface(ctx, nic)
	must(t, err)
	moved := driver.GuestIPv6Of([]driver.Interface{movedNIC})
	must(t, h.Guest.SetIPv6(ctx, "m1", moved))
	holds("the guest holding the address it was told", moved)
	listed("told another address")
	must(t, h.Guest.SetIPv6(ctx, "m1", moved))
	holds("the guest told the same again", moved)
	none := []driver.GuestIPv6{{MAC: nic.MAC}}
	must(t, h.Guest.SetIPv6(ctx, "m1", none))
	holds("the guest told it has none", none)
	if err := h.Guest.SetIPv6(ctx, "m1", []driver.GuestIPv6{{MAC: "02:00:00:00:00:99", Address: at(3)}}); err == nil {
		t.Error("the guest took an address for an interface it has not")
	}
	if err := h.Guest.SetIPv6(ctx, "m1", []driver.GuestIPv6{{MAC: nic.MAC, Address: netip.MustParseAddr("192.0.2.1")}}); err == nil {
		t.Error("the guest took an IPv4 address as its IPv6 one")
	}

	// Booting again, it holds its boot disk's, whatever it was told.
	must(t, h.Guest.SetIPv6(ctx, "m1", moved))
	holds("the guest holding the address it was told, again", moved)
	before, _ := vmOf(t, h, "m1")
	h.Exit(t, "m1", 1)
	testutil.EventuallyWithin(t, vmWait, "the VM booting again", func() bool {
		vm, ok := vmOf(t, h, "m1")
		return ok && vm.State == driver.VMRunning && vm.Started.After(before.Started) && h.Guest.Ready(ctx, "m1") == nil
	})
	holds("the guest, booted again, holding its boot disk's address", bootDisk)

	must(t, h.Hypervisor.Stop(ctx, "m1"))
	if err := h.Guest.SetIPv6(ctx, "m1", moved); err == nil {
		t.Error("the guest of a stopped VM was told an address")
	}
	must(t, h.Hypervisor.DeleteLogs(ctx, "m1"))
	must(t, h.Network.DeleteInterfaces(ctx, "m1"))
	must(t, h.Storage.DeleteRoot(ctx, "m1"))
}
