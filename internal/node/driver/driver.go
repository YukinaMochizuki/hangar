// Package driver is what the node agent needs from its host, as interfaces:
// image fetching, storage, networking, the hypervisor and the guest agent.
// The real drivers live in subpackages; driver/fake simulates a whole host
// for tests, with fault injection. Every object a driver makes on the host
// is named after the uid of the API object it serves, and every driver
// lists what it has made, so the agent keeps no state of its own: it reads
// the host back and removes what no API object accounts for. Drivers are
// safe for concurrent use: images are prepared while machines are built.
package driver

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// Errors the drivers wrap, for the agent to tell what went wrong.
var (
	// ErrNoSpace means storage has no room for what was asked.
	ErrNoSpace = errors.New("no space left")
	// ErrDiskTooSmall means a root disk was asked for smaller than its
	// image's base.
	ErrDiskTooSmall = errors.New("disk smaller than its image")
	// ErrImageNotFound means the registry has no such image.
	ErrImageNotFound = errors.New("image not found")
	// ErrUnauthorized means the registry refused the credentials, or asks
	// for some.
	ErrUnauthorized = errors.New("registry refused access")
	// ErrRegistryUnreachable means the registry could not be reached or
	// failed to answer.
	ErrRegistryUnreachable = errors.New("registry unreachable")
	// ErrIPv6InUse means an interface was asked for with the IPv6 address
	// another interface on the host holds.
	ErrIPv6InUse = errors.New("IPv6 address held by another interface")
	// ErrMACInUse means an interface was asked for with the MAC another
	// interface of its network, or the network's gateway, has.
	ErrMACInUse = errors.New("MAC held by another interface of the network")
	// ErrImageTooLarge means an image is beyond what the host unpacks.
	ErrImageTooLarge = errors.New("image too large")
	// ErrInvalidImage means an image holds what the host refuses to
	// unpack, such as a name climbing out of its root.
	ErrInvalidImage = errors.New("invalid image")
	// ErrDigestMismatch means what was downloaded is not what its digest
	// names.
	ErrDigestMismatch = errors.New("digest mismatch")
	// ErrBusy means what was asked waits on work under way; ask again
	// later.
	ErrBusy = errors.New("busy with work under way")
	// ErrNoCapture means a CustomImage has neither its base nor the
	// snapshot of its source to make one from.
	ErrNoCapture = errors.New("no snapshot to capture the image from")
)

// customImagePrefix starts the key of a CustomImage's base, which a
// sha256 digest, an Image's, never starts with.
const customImagePrefix = "customimage:"

// CustomImageBase is the key of the base of the CustomImage of uid: what
// Storage's disk image methods take in place of an Image's digest
// (docs/decisions/custom-images.md).
func CustomImageBase(uid string) string { return customImagePrefix + uid }

// CustomImageOf returns the uid of the CustomImage whose base key is key.
func CustomImageOf(key string) (string, bool) {
	uid, ok := strings.CutPrefix(key, customImagePrefix)
	return uid, ok && uid != ""
}

// Room is where a NoSpaceError found too little room.
type Room int

const (
	// UnknownRoom says nothing of where; the node agent takes it as
	// ImageRoom.
	UnknownRoom Room = iota
	// ImageRoom is where images are cached and unpacked, which
	// ImageCache.Free counts.
	ImageRoom
	// StorageRoom is where bases and disks are, which Storage.Capacity
	// counts.
	StorageRoom
)

// NoSpaceError is ErrNoSpace saying where, and how much room was wanted.
type NoSpaceError struct {
	What string
	Room Room
	// Need is the bytes that had to be free, and Free those that were:
	// making room is making Need less Free. Need is what to try with if
	// Least is less.
	Need, Free int64
	// Least is the bytes known to have to be free, at most Need; zero
	// means Need.
	Least int64
}

// AtLeast is Least, or Need if Least is zero.
func (e *NoSpaceError) AtLeast() int64 {
	if e.Least == 0 {
		return e.Need
	}
	return e.Least
}

func (e *NoSpaceError) Error() string {
	if e.AtLeast() < e.Need {
		return fmt.Sprintf("%s takes at least %d bytes, perhaps %d, %d are free: %v", e.What, e.AtLeast(), e.Need, e.Free, ErrNoSpace)
	}
	return fmt.Sprintf("%s takes %d bytes, %d are free: %v", e.What, e.Need, e.Free, ErrNoSpace)
}

func (e *NoSpaceError) Is(target error) bool { return target == ErrNoSpace }

// UnpackShortfall is the NoSpaceError of what, the unpack of an image of
// layers of size bytes compressed that ran out of the free bytes it had,
// failing with err: known to need more than free, it asks for size more
// at a try, as nothing it can measure of the unpack tells what its room
// will take (docs/decisions/node-agent.md).
func UnpackShortfall(what string, size, free int64, err error) *NoSpaceError {
	return &NoSpaceError{What: fmt.Sprintf("%s (%v)", what, err), Room: ImageRoom, Need: free + max(size, 1), Least: free + 1, Free: free}
}

// OutOfRoom says err is a write its filesystem, or its quota, had no room
// for.
func OutOfRoom(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// Credentials finds the login for a registry, named by its host as in
// docker login, such as ghcr.io or index.docker.io. A zero Login pulls
// anonymously.
type Credentials func(ctx context.Context, registry string) (Login, error)

// Login is a registry login, as the Docker config format holds one.
type Login struct {
	Username, Password string
	// IdentityToken is an OAuth refresh token, used instead of a password.
	IdentityToken string
}

// Anonymous pulls from every registry without logging in.
func Anonymous(context.Context, string) (Login, error) { return Login{}, nil }

// Repository is the repository an image reference names, with its
// registry, as the cache records where content came from: the one place
// references are compared so. docker.io/library/x, index.docker.io/library/x
// and x are one repository, index.docker.io/library/x; the host is lower
// case, without its scheme's default port (443, or 80 for a registry
// reached over plain HTTP). A tag or digest is left out, and a digest need
// not be well formed.
func Repository(ref string) (string, error) {
	repo, _, _ := strings.Cut(ref, "@")
	r, err := name.ParseReference(repo)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrImageNotFound, err)
	}
	host := strings.ToLower(r.Context().RegistryStr())
	reg, err := name.NewRegistry(host)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrImageNotFound, err)
	}
	switch reg.Scheme() {
	case "https":
		host = strings.TrimSuffix(host, ":443")
	case "http":
		host = strings.TrimSuffix(host, ":80")
	}
	// Docker Hub's other names, which go-containerregistry does not fold
	// once spelled otherwise.
	if host == "docker.io" || host == "registry-1.docker.io" {
		host = name.DefaultRegistry
	}
	// Parsed again, so Docker Hub's official images get their library/.
	if r, err = name.ParseReference(host + "/" + r.Context().RepositoryStr()); err != nil {
		return "", fmt.Errorf("%w: %w", ErrImageNotFound, err)
	}
	return r.Context().Name(), nil
}

// Images resolves and unpacks OCI images for the host's platform. The host
// keeps a pulled digest in its cache until forgotten, with when it was last
// used and the repositories (Repository) it was fetched from in full:
// Config and Unpack answer a digest from the cache only for a reference
// naming one of them.
// For any other, they first fetch the manifest, the config and every layer
// from the reference's repository with creds, each checked against its
// digest, even what the cache holds; a repository that cannot serve them
// fails the pull. Failed pulls wrap ErrImageNotFound, ErrUnauthorized,
// ErrRegistryUnreachable or ctx's error, or are a *NoSpaceError of
// ImageRoom when the cache has no room for the blobs. See
// docs/decisions/secrets.md.
type Images interface {
	// Resolve returns the digest the reference names now, as its registry
	// says: it proves nothing about the content a digest names.
	Resolve(ctx context.Context, ref string, creds Credentials) (digest string, err error)
	// Config returns how the image wants to be run. It and Unpack count as
	// a use of the digest.
	Config(ctx context.Context, ref, digest string, creds Credentials) (ImageConfig, error)
	// Unpack writes the image's root filesystem into the empty dir, never
	// outside it; an image larger than the host allows is ErrImageTooLarge.
	Unpack(ctx context.Context, ref, digest, dir string, creds Credentials) error
	// Cached lists what the cache keeps, and the room where it keeps it.
	Cached(ctx context.Context) (ImageCache, error)
	// Forget drops what the host keeps of a digest, its blobs no other
	// digest holds among them. While the digest is being pulled it fails
	// at once with ErrBusy.
	Forget(ctx context.Context, digest string) error
}

// ImageCache is what the host keeps of the images it pulled.
type ImageCache struct {
	Images []CachedImage
	// Total is the bytes the cache may take, its blobs and what else
	// shares their room, and Free what of it is left.
	Total, Free int64
}

// CachedImage is a digest in the cache.
type CachedImage struct {
	Digest string
	// UsedAt is when Config or Unpack last read it, kept on the host.
	UsedAt time.Time
	// Busy says it is being pulled, so Forget would fail with ErrBusy.
	Busy bool
	// Blobs are the sizes of the blobs it holds, by digest. Digests may
	// share blobs.
	Blobs map[string]int64
}

// ImageConfig is the part of an OCI image config a container runs by.
type ImageConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
	// Size is the bytes of the image's layers as its registry sends them,
	// compressed: the least unpacking it takes.
	Size int64
}

// DiskImages fetches disk images, such as a distribution's cloud image, for
// the storage to make bases of. Only images a platform administrator chose
// come through it (docs/decisions/vm-images.md). Failed fetches wrap
// ErrDigestMismatch, ErrImageTooLarge, ErrInvalidImage, ctx's error, or a
// failure to reach the server, which may pass.
type DiskImages interface {
	// Fetch downloads the image at the https url and checks it against its
	// digest, "sha256:" and 64 hex digits, and that it is a disk image the
	// host takes: raw or qcow2, naming no other file. The image is the
	// caller's until it closes it.
	Fetch(ctx context.Context, url, digest string) (FetchedImage, error)
}

// FetchedImage is a disk image fetched and checked.
type FetchedImage interface {
	// Size is the disk it holds, in bytes.
	Size() int64
	// WriteTo writes the disk onto the block device dev, at least Size
	// bytes and all zeros, as raw.
	WriteTo(ctx context.Context, dev string) error
	// Close removes what the fetch kept on the host.
	Close() error
}

// Storage holds disks: a read-only base per image, OCI or disk image, a root
// disk per machine cloned from an OCI image's, and a volume per API Disk,
// blank or cloned from a disk image's, which outlives the machines it is
// attached to. A disk image's base is an Image's, keyed by its sha256
// digest, or a CustomImage's, keyed by CustomImageBase and captured from a
// volume. Deleting what is not there is not an error.
type Storage interface {
	// EnsureBase makes a digest's base once, filling an empty dir with fill,
	// unless size (ImageConfig.Size, or zero) exceeds the room left; fill's
	// error is returned wrapped. Too little room, where dir is or for the
	// base, is a *NoSpaceError, of ImageRoom or StorageRoom; one where
	// fill ran out says what the image is likely to need there, and as its
	// Least only more than was free.
	EnsureBase(ctx context.Context, digest string, size int64, fill func(dir string) error) error
	// Bases lists the bases.
	Bases(ctx context.Context) ([]Base, error)
	// DeleteBase removes a base. It fails while a root disk cloned from it
	// exists, as a ZFS snapshot with clones does.
	DeleteBase(ctx context.Context, digest string) error
	// EnsureRoot returns the machine's root disk of digest, cloned if need
	// be, of at least size bytes: grown, never shrunk, and ErrDiskTooSmall
	// below the base. A root disk of another digest is an error.
	EnsureRoot(ctx context.Context, machineUID, digest string, size int64) (Disk, error)
	// Roots lists every machine's root disk.
	Roots(ctx context.Context) ([]Disk, error)
	// DeleteRoot removes a machine's root disk, a use of its base. It fails
	// while the machine's VM runs.
	DeleteRoot(ctx context.Context, machineUID string) error
	// EnsureImageBase makes the base of a disk image of digest once: a
	// volume of size bytes, which fill writes given its device, kept
	// read-only. fill's error is returned wrapped.
	EnsureImageBase(ctx context.Context, digest string, size int64, fill func(dev string) error) error
	// ImageBases lists the keys, digests and CustomImageBase keys, that
	// have a disk image's base.
	ImageBases(ctx context.Context) ([]string, error)
	// DeleteImageBase removes the disk image's base of key. It fails while
	// a volume cloned from it exists.
	DeleteImageBase(ctx context.Context, key string) error
	// SnapshotVolume takes, once, the snapshot of a Disk's volume that the
	// CustomImage of imageUID is captured from.
	SnapshotVolume(ctx context.Context, diskUID, imageUID string) error
	// CaptureImageBase makes, once, the base of the CustomImage of imageUID
	// from the snapshot SnapshotVolume took: a copy that holds nothing of
	// the volume, read-only. It then removes the snapshot, and returns the
	// base, as it does for one made before. With neither it fails with
	// ErrNoCapture; a failure leaves no base, and keeps the snapshot.
	CaptureImageBase(ctx context.Context, imageUID string) (ImageBase, error)
	// Captures lists the snapshots SnapshotVolume took that are still
	// there.
	Captures(ctx context.Context) ([]Capture, error)
	// DeleteCapture removes the snapshot taken for the CustomImage of
	// imageUID.
	DeleteCapture(ctx context.Context, imageUID string) error
	// FillImageBase makes, once, the base of the CustomImage of imageUID
	// from an uploaded file: a volume of size bytes, all zeros, that fill
	// writes given its device, then read-only and marked finished as a
	// captured base is. It returns the base, as it does for one made
	// before. A failure, fill's among them, leaves no base.
	FillImageBase(ctx context.Context, imageUID string, size int64, fill func(dev string) error) (ImageBase, error)
	// EnsureVolume returns a Disk's volume of at least size bytes, grown,
	// never shrunk. A volume made here is blank if digest is empty, and
	// otherwise a clone of the disk image's base of that key, a digest or
	// a CustomImageBase key, ErrDiskTooSmall below its size. A volume made
	// from another is an error.
	EnsureVolume(ctx context.Context, diskUID string, size int64, digest string) (Volume, error)
	// Volumes lists every Disk's volume.
	Volumes(ctx context.Context) ([]Volume, error)
	// DeleteVolume removes a Disk's volume. It fails while a VM has it, and
	// while a snapshot SnapshotVolume took of it is there.
	DeleteVolume(ctx context.Context, diskUID string) error
	// EnsureShareVolume returns a FileShare's volume of at least size
	// bytes: blank when made, grown, never shrunk. The host never reads it;
	// the share's server makes its filesystem (docs/decisions/file-shares.md).
	EnsureShareVolume(ctx context.Context, shareUID string, size int64) (ShareVolume, error)
	// ShareVolumes lists every FileShare's volume.
	ShareVolumes(ctx context.Context) ([]ShareVolume, error)
	// DeleteShareVolume removes a FileShare's volume. It fails while a VM
	// has it.
	DeleteShareVolume(ctx context.Context, shareUID string) error
	// Capacity returns how much the storage holds, and how much of it is
	// free.
	Capacity(ctx context.Context) (Capacity, error)
}

// Converter reads the files tenants upload for CustomImages, which the
// host never parses: only qemu-img in a converter VM of the file's own
// does, the platform kernel booting the converter initramfs, with no
// network, its agent answering the host alone over vsock
// (docs/decisions/custom-images.md). What it reports of a file is the
// guest's claim. A file qemu-img refuses is ErrInvalidImage.
type Converter interface {
	// Inspect returns what qemu-img info in a converter VM says of the
	// file, attached read-only.
	Inspect(ctx context.Context, imageUID string, file UploadedFile) (ImageInfo, error)
	// Convert has qemu-img convert in a converter VM write the file,
	// attached read-only and read as format, onto the block device dev,
	// all zeros, as raw.
	Convert(ctx context.Context, imageUID string, file UploadedFile, format, dev string) error
}

// UploadedFile is a file a tenant uploaded, as the node stages it: Size
// bytes at Offset of the file at Path, whole sectors of zeros before and
// after them.
type UploadedFile struct {
	Path         string
	Offset, Size int64
}

// UploadOffset is where a staged file holds the tenant's bytes: after
// 4 KiB of zeros. Cloud Hypervisor reads a disk's first and last bytes
// for a format's header, even when told the disk is raw, and refuses one
// it takes for qcow2, VHD or VHDX; so it is never handed the tenant's
// bytes at either end (docs/decisions/custom-images.md).
const UploadOffset = 4 << 10

// UploadedLength is the length of the staged file of size bytes: the
// zeros before, the bytes, zeros up to a 4 KiB boundary, and 4 KiB more.
func UploadedLength(size int64) int64 {
	end := UploadOffset + size
	return (end+UploadOffset-1)/UploadOffset*UploadOffset + UploadOffset
}

// ImageInfo is what qemu-img info says of a file.
type ImageInfo struct {
	Format string
	// VirtualSize is the size of the disk the file holds, in bytes.
	VirtualSize int64
	// BackingFile and DataFile name other files the image reads, if any.
	BackingFile, DataFile string
}

// Base is an OCI image's base.
type Base struct {
	Digest string
	// Size is the bytes it takes in the storage.
	Size int64
	// UsedAt is when it was last used, kept on the host: made, cloned
	// from, or a root disk cloned from it deleted.
	UsedAt time.Time
}

// Disk is a machine's root disk.
type Disk struct {
	MachineUID string
	// Digest is the image it was cloned from.
	Digest string
	// Path is the block device the VM boots from.
	Path string
	// Size is the disk's size in bytes, at least what was asked for. The
	// guest grows its filesystem to it as it boots.
	Size int64
}

// Volume is the block device of an API Disk.
type Volume struct {
	DiskUID string
	// Path is the block device a VM is given.
	Path string
	// Size is the volume's size in bytes, at least what was asked for.
	Size int64
	// Digest is the key of the disk image's base it was cloned from, a
	// digest or a CustomImageBase key; empty for a blank one.
	Digest string `json:",omitempty"`
	// Serial is the virtio-blk serial a firmware VM's guest sees the
	// volume by (computeapi.DiskSerials); empty is none.
	Serial string `json:",omitempty"`
}

// ImageBase is a CustomImage's base.
type ImageBase struct {
	// Key is its CustomImageBase key.
	Key string
	// Size is the size of the disk it holds, its source volume's, in bytes.
	Size int64
	// Used is the bytes of data it holds, as written, before compression.
	Used int64
}

// Capture is a snapshot of a Disk's volume taken for a CustomImage.
type Capture struct {
	ImageUID, DiskUID string
}

// ShareVolume is the block device of a FileShare, which only its server
// has.
type ShareVolume struct {
	ShareUID string
	// Path is the block device the server is given.
	Path string
	// Size is the volume's size in bytes, at least what was asked for. The
	// server grows its filesystem to it as it boots.
	Size int64
}

// Capacity is the storage's size and free space, in bytes.
type Capacity struct {
	Total, Free int64
}

// Network gives each API Network a namespace on the host and each machine
// interface a tap in it. See docs/decisions/networking.md. Deleting what is
// not there is not an error.
type Network interface {
	// EnsureNetwork makes the network's namespace, its link to the host on
	// the transit /31, and its NAT, or corrects them.
	EnsureNetwork(ctx context.Context, n NetworkSpec) error
	// Networks lists the networks on the host.
	Networks(ctx context.Context) ([]NetworkSpec, error)
	// DeleteNetwork removes a network. The caller makes sure no interface is
	// left in it; a driver need not check, and deleting a network in use
	// cuts its VMs off.
	DeleteNetwork(ctx context.Context, networkUID string) error
	// EnsureInterface makes the tap for a machine interface in its
	// network's namespace, or corrects it, and returns the interface with
	// its Device. An IPv6 address another interface holds is refused with
	// ErrIPv6InUse: one address is delivered to one interface. A MAC
	// another interface of the network, or its gateway, has is refused
	// with ErrMACInUse: one MAC's frames go to one tap.
	EnsureInterface(ctx context.Context, i Interface) (Interface, error)
	// Interfaces lists every machine interface on the host, as it was
	// ensured.
	Interfaces(ctx context.Context) ([]Interface, error)
	// DeleteInterfaces removes every interface of a machine. It fails while
	// the machine's VM runs.
	DeleteInterfaces(ctx context.Context, machineUID string) error
	// Changes signals that something outside the agent changed the host's
	// networking, such as networkd reconfiguring links, so the agent
	// checks everything again.
	Changes() <-chan struct{}

	// ListenUDP and ListenTCP open a socket only the network's machines
	// reach, and return the Instance it serves (docs/decisions/networking.md).
	ListenUDP(ctx context.Context, networkUID string, addr netip.AddrPort) (*net.UDPConn, string, error)
	ListenTCP(ctx context.Context, networkUID string, addr netip.AddrPort) (*net.TCPListener, string, error)
	// ListenPackets opens a socket on the bridge of a network with IPv6
	// that exchanges whole IPv6 packets of one kind with its machines, each
	// known by the MAC it sends from, which the interface's binding makes
	// its own; it returns the Instance it serves, as ListenUDP does.
	ListenPackets(ctx context.Context, networkUID string, kind PacketKind) (PacketConn, string, error)
	// Dial connects over TCP to addr, a machine's, from the network's
	// gateway in its namespace: the platform's own ways in, its SSH
	// (docs/decisions/vm-ssh.md) and public endpoints
	// (docs/decisions/public-endpoints.md). The firewall lets them in to
	// port 22 of every interface, and to an interface's EdgePorts.
	Dial(ctx context.Context, networkUID string, addr netip.AddrPort) (net.Conn, error)
	// DialAnyPort connects as Dial does, as a port forward
	// (docs/decisions/port-forward.md), which the firewall lets in to any
	// TCP port of an interface, but none of a file share's server.
	DialAnyPort(ctx context.Context, networkUID string, addr netip.AddrPort) (net.Conn, error)
	// Instance names what a network's sockets serve, until it is made again;
	// "" means there is nothing to serve.
	Instance(ctx context.Context, networkUID string) (string, error)

	// EnsureFirewall makes each machine interface of the network on the
	// host hold exactly its policy, replacing the network's whole set at
	// once; an interface given none, or one made since, receives and sends
	// nothing but its network's services and replies. A file share's
	// server's interface (Interface.FileShareServer) takes no policy and no
	// service: it receives only new TCP connections to ShareServerPort from
	// its ShareClients, and sends only replies on them; given none, or made
	// since, nothing at all. Policies of
	// interfaces not on the host are ignored. Policies of the interfaces on
	// the host larger than MaxFirewallRules together are refused, and the
	// firewall is then left as it was.
	EnsureFirewall(ctx context.Context, networkUID string, policies []InterfacePolicy) error
	// ShareRules reads back from the host the file share rules in force in
	// a network's firewall, by machine uid and interface name, such as
	// "uid/eth0": what each interface was given of an InterfacePolicy's
	// ShareClients and Shares, as its rules now stand.
	ShareRules(ctx context.Context, networkUID string) (map[string]ShareRules, error)

	// EnsureForwards makes the host forward exactly these ports of its
	// shared IPv4 address, from the internet, to machines, replacing the
	// whole set at once; a table already as wanted is not written. Forwards
	// of a network not on the host are ignored. A set holding a forward that
	// is not of TCP or UDP, of port 0 or SharedPortsFrom and above, to no
	// IPv4 address and port, of a network uid no network can have, or a
	// port of a protocol twice, is refused whole, and what the host forwards
	// is left as it was. A host without a shared address refuses any
	// forward, and takes an empty set. By the time it returns, no flow a
	// removed forward, or one whose target changed, let in still reaches
	// the machine it led to.
	EnsureForwards(ctx context.Context, forwards []Forward) error
	// Forwards reads back from the host the forwards in force, by protocol
	// and port.
	Forwards(ctx context.Context) ([]Forward, error)

	// EnsureEndpoints makes the host carry exactly these network endpoints'
	// traffic from their networks to their services' machines, replacing the
	// whole set at once; a table already as wanted is not written. An
	// endpoint whose network or service network is not on the host is
	// ignored. A set CheckEndpoints refuses is refused whole, and what the
	// host carries is left as it was. By the time it returns, no flow a
	// removed endpoint, or one whose target changed, let in still reaches the
	// machine it led to.
	EnsureEndpoints(ctx context.Context, endpoints []Endpoint) error
	// Endpoints reads back from the host the endpoints in force, in the order
	// SortEndpoints puts them.
	Endpoints(ctx context.Context) ([]Endpoint, error)
}

// SharedPortsFrom is the first port a network's TCP and UDP leave the
// shared address from; Forward.Port is below it, so a port at the shared
// address is either a machine's connection out or a forward into one,
// never both.
const SharedPortsFrom = 32768

// ForwardProtocol is the transport a Forward forwards.
type ForwardProtocol int

const (
	TCP ForwardProtocol = iota + 1
	UDP
)

func (p ForwardProtocol) String() string {
	switch p {
	case TCP:
		return "TCP"
	case UDP:
		return "UDP"
	}
	return fmt.Sprintf("protocol %d", int(p))
}

// Forward is one port of the host's shared IPv4 address forwarded to a
// machine interface (docs/decisions/networking.md, "Port forwards").
type Forward struct {
	Protocol ForwardProtocol
	// Port is the port at the shared address, which no two forwards of one
	// protocol share.
	Port uint16
	// NetworkUID is the network of the interface.
	NetworkUID string
	// To is the interface's IPv4 address and the port it is reached at.
	To netip.AddrPort
}

// NetworkUIDPattern is what a network uid may be: what can name a file,
// such as a network's namespace on the host.
var NetworkUIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// CheckForwards refuses a set of forwards no host holds, as
// Network.EnsureForwards says.
func CheckForwards(fs []Forward) error {
	seen := map[[2]int]bool{}
	for _, f := range fs {
		if f.Protocol != TCP && f.Protocol != UDP {
			return fmt.Errorf("forward of port %d: %s is neither TCP nor UDP", f.Port, f.Protocol)
		}
		if f.Port == 0 || f.Port >= SharedPortsFrom {
			return fmt.Errorf("%s port %d is not below %d, where the networks' connections leave from", f.Protocol, f.Port, SharedPortsFrom)
		}
		if !f.To.Addr().Is4() || f.To.Port() == 0 {
			return fmt.Errorf("%s port %d: %s is not an IPv4 address and port", f.Protocol, f.Port, f.To)
		}
		if !NetworkUIDPattern.MatchString(f.NetworkUID) {
			return fmt.Errorf("%s port %d: network uid %q is no network's", f.Protocol, f.Port, f.NetworkUID)
		}
		k := [2]int{int(f.Protocol), int(f.Port)}
		if seen[k] {
			return fmt.Errorf("%s port %d is forwarded twice", f.Protocol, f.Port)
		}
		seen[k] = true
	}
	return nil
}

// The transit ports an Endpoint may have, those EndpointServices draw.
const (
	TransitPortsFrom    = computeapi.TransitPortsFrom
	TransitPortsThrough = computeapi.TransitPortsThrough
)

// Endpoint is one network's way to one EndpointService
// (docs/decisions/endpoint-services.md): Port of Addr, an address of the
// network NetworkUID, leads through TransitPort of the service network's
// transit link to To, a machine's address and port on ServiceNetworkUID.
type Endpoint struct {
	Protocol   ForwardProtocol
	NetworkUID string
	Addr       netip.Addr
	Port       uint16
	// ServiceNetworkUID is the network of the service's machine.
	ServiceNetworkUID string
	// TransitPort is the service's port at its network's transit address,
	// which no two services of a protocol share.
	TransitPort uint16
	To          netip.AddrPort
}

// SortEndpoints puts endpoints in the order Network.Endpoints lists them:
// by network, address, protocol and port.
func SortEndpoints(es []Endpoint) {
	slices.SortFunc(es, func(a, b Endpoint) int {
		return cmp.Or(cmp.Compare(a.NetworkUID, b.NetworkUID), a.Addr.Compare(b.Addr),
			cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Port, b.Port))
	})
}

// CheckEndpoints refuses a set of endpoints no host holds: one not of TCP
// or UDP, at no IPv4 address and port, of a transit port outside
// TransitPortsFrom to TransitPortsThrough, to no IPv4 address and port, of a
// network uid no network can have, or from a network to itself; an address
// and port of a protocol twice in a network; or a transit port of a
// protocol of a service network leading to two targets.
func CheckEndpoints(es []Endpoint) error {
	at := map[string]bool{}
	transit := map[string]netip.AddrPort{}
	for _, e := range es {
		if e.Protocol != TCP && e.Protocol != UDP {
			return fmt.Errorf("endpoint %s:%d: %s is neither TCP nor UDP", e.Addr, e.Port, e.Protocol)
		}
		if !e.Addr.Is4() || e.Port == 0 {
			return fmt.Errorf("endpoint %s:%d is not an IPv4 address and port", e.Addr, e.Port)
		}
		if e.TransitPort < TransitPortsFrom || e.TransitPort > TransitPortsThrough {
			return fmt.Errorf("endpoint %s:%d: transit port %d is not from %d to %d", e.Addr, e.Port, e.TransitPort, TransitPortsFrom, TransitPortsThrough)
		}
		if !e.To.Addr().Is4() || e.To.Port() == 0 {
			return fmt.Errorf("endpoint %s:%d: %s is not an IPv4 address and port", e.Addr, e.Port, e.To)
		}
		for _, uid := range []string{e.NetworkUID, e.ServiceNetworkUID} {
			if !NetworkUIDPattern.MatchString(uid) {
				return fmt.Errorf("endpoint %s:%d: network uid %q is no network's", e.Addr, e.Port, uid)
			}
		}
		if e.NetworkUID == e.ServiceNetworkUID {
			return fmt.Errorf("endpoint %s:%d leads to its own network %s", e.Addr, e.Port, e.NetworkUID)
		}
		k := fmt.Sprintf("%s %s %s:%d", e.NetworkUID, e.Protocol, e.Addr, e.Port)
		if at[k] {
			return fmt.Errorf("%s %s:%d is an endpoint of network %s twice", e.Protocol, e.Addr, e.Port, e.NetworkUID)
		}
		at[k] = true
		t := fmt.Sprintf("%s %s %d", e.ServiceNetworkUID, e.Protocol, e.TransitPort)
		if to, ok := transit[t]; ok && to != e.To {
			return fmt.Errorf("%s transit port %d of network %s leads to both %s and %s", e.Protocol, e.TransitPort, e.ServiceNetworkUID, to, e.To)
		}
		transit[t] = e.To
	}
	return nil
}

// ShareRules are an interface's file share rules in force: of a server's,
// the clients it takes on ShareServerPort; of any other, the servers it
// reaches there.
type ShareRules struct {
	Clients, Servers []netip.Addr
}

// PacketKind is what IPv6 packets a PacketConn receives.
type PacketKind int

const (
	// RouterSolicitations are ICMPv6 router solicitations.
	RouterSolicitations PacketKind = iota + 1
	// DHCPv6 is UDP to the DHCPv6 server port, 547.
	DHCPv6
)

// PacketConn exchanges IPv6 packets, IPv6 header included, with a network's
// machines. A packet it reads is one a machine sent; one the host sent is
// never read back.
type PacketConn interface {
	// ReadPacket reads a packet into b and says which MAC sent it.
	ReadPacket(b []byte) (n int, from net.HardwareAddr, err error)
	// WritePacket sends packet b in a frame to the MAC to.
	WritePacket(b []byte, to net.HardwareAddr) error
	SetReadDeadline(t time.Time) error
	Close() error
}

// MaxFirewallRules bounds the policies one network's firewall on a host
// holds, as their secgroup.Policy.Size together.
const MaxFirewallRules = 20000

// InterfacePolicy is what one machine interface may receive and send.
type InterfacePolicy struct {
	MachineUID string
	// Name is the interface's name in the machine, such as eth0.
	Name   string
	Policy secgroup.Policy
	// EdgePorts are the TCP ports of the interface that public endpoints
	// reach, which Dial from the gateway passes whatever Policy says.
	EdgePorts []uint16
	// Shares are the addresses of the file share servers the interface
	// connects to on ShareServerPort whatever Policy says: those of the
	// shares its machine declares, on this network.
	Shares []netip.Addr
	// ShareClients, of a file share's server's interface, are the
	// addresses that may connect to it, the machines declaring its share on
	// this network; its Policy is ignored.
	ShareClients []netip.Addr
}

// ShareServerPort is the port a file share's server serves NFS on.
const ShareServerPort = 2049

// MetadataAddr is the metadata service's address in every network, as
// clouds have it; a network's bridge holds it beside the gateway.
var MetadataAddr = netip.MustParseAddr("169.254.169.254")

// GatewayIPv6 is the gateway's address on every network's bridge when its
// host has an IPv6 prefix: machines route their public /128 through it.
var GatewayIPv6 = netip.MustParseAddr("fe80::1")

// NetworkSpec is a network as the host has it.
type NetworkSpec struct {
	UID  string
	CIDR netip.Prefix
	// Transit is the network's /31 to the host; the host takes the first
	// address.
	Transit netip.Prefix
}

// Interface is one machine interface.
type Interface struct {
	MachineUID string
	// Name is the interface's name in the machine, such as eth0.
	Name       string
	NetworkUID string
	MAC        string
	// Address is the interface's address with its network's prefix length.
	Address netip.Prefix
	// Gateway is the first address of the network.
	Gateway netip.Addr
	// IPv6 is the interface's public address, a /128 of the host's
	// prefix, routed to it through GatewayIPv6; zero for none. It is no part
	// of a VMSpec's Hash.
	IPv6 netip.Addr `json:",omitzero"`
	// Device names the tap on the host, unique there and at most 15
	// characters, as Linux allows. The network driver chooses it; the
	// hypervisor driver opens it.
	Device string
	// FileShareServer makes it an interface of a file share's server, which
	// the firewall closes to all but its share's clients (EnsureFirewall).
	FileShareServer bool `json:",omitempty"`
}

// Hypervisor runs VMs. A VM exited for good stays listed, with its exit
// code, until it is stopped: an unlisted VM is one never started. Stopping
// what is not there is not an error.
//
// A VM's console log, what its container wrote, outlives it: through its
// restarts, a Stop and a Start again, until DeleteLogs, once the machine is
// gone. So does a TPM VM's TPM state, which goes with the log.
type Hypervisor interface {
	// Start runs the VM, or leaves it as it is if it already runs this exact
	// spec. A VM running another spec is an error: stop it first.
	Start(ctx context.Context, vm VMSpec) error
	// Stop stops a machine's VM and forgets it, keeping its console log. A
	// running guest is asked to stop its process first, as docker stop
	// does, and its VM is killed if it has not powered off in StopGrace and
	// a few seconds more. A stopped VM is never started again by its
	// restart policy.
	Stop(ctx context.Context, machineUID string) error
	// SetBootExport replaces, whole, the export on a listed server's boot
	// disk, which its next boot by its restart policy serves; the boot
	// running and SpecHash stay. Any other VM is refused (file-shares.md).
	SetBootExport(ctx context.Context, machineUID string, export ShareExport) error
	// VMs lists the VMs on the host, running or not.
	VMs(ctx context.Context) ([]VM, error)
	// Changes signals that a VM changed state on its own, such as exiting.
	Changes() <-chan struct{}
	// Logs lists the machines whose console logs, or TPM states, the host
	// keeps.
	Logs(ctx context.Context) ([]string, error)
	// DeleteLogs removes a machine's console log, and its TPM state. It
	// fails while the machine's VM is listed.
	DeleteLogs(ctx context.Context, machineUID string) error
	// PutTPMKey keeps the key a TPM VM's state is encrypted with, root's
	// alone, for its VM to start with: Start needs it, and so does every
	// restart by its restart policy, until RemoveTPMKey. TPMKeys lists the
	// machines it keeps a key of.
	PutTPMKey(ctx context.Context, machineUID string, key []byte) error
	RemoveTPMKey(ctx context.Context, machineUID string) error
	TPMKeys(ctx context.Context) ([]string, error)
	// ReadLog sends what the machine's console log holds, oldest first,
	// across every boot it keeps, in batches. A machine with no log has no
	// records. With Follow it then sends what is written, rotations
	// included, until ctx ends; a log made later is followed from its
	// start. It returns send's error, or ctx's once following.
	ReadLog(ctx context.Context, machineUID string, opts LogOptions, send func([]LogRecord) error) error
	// ResizeRoot tells a running container's VM that its root disk, grown
	// since the VM started, is now the size its device has, and returns that
	// size: the guest sees it at once, and grows its filesystem when asked
	// (Guest.GrowRoot). Telling it the size it has already is no error. A VM
	// started before it could be told, until it is started again, is
	// ErrNoLiveResize; it sees the size at its next boot.
	ResizeRoot(ctx context.Context, machineUID string) (int64, error)
	// ResizeVolume does for a running container's VM's volume, the n-th of
	// its spec's Disks from 0, what ResizeRoot does for its root disk: the
	// guest sees it at once, and grows its filesystem when asked
	// (Guest.GrowVolume). A VM started before it could be told is
	// ErrNoLiveResize.
	ResizeVolume(ctx context.Context, machineUID string, n int) (int64, error)
	// AttachConsole connects to a running firmware VM's interactive serial
	// console, which speaks hangar.node.v1's ConsoleRequest and
	// ConsoleResponse, each delimited by its length
	// (docs/decisions/serial-console.md). A VM not running, one started
	// before its console was interactive, until it is started again, or one
	// booting no firmware, is ErrNoConsole.
	AttachConsole(ctx context.Context, machineUID string) (net.Conn, error)
}

// ErrNoLiveResize is a VM whose disk's size is told it only as it boots.
var ErrNoLiveResize = errors.New("the VM sees its disk's new size only once started again")

// ErrNoConsole is a VM whose serial console takes no client. It is short:
// a WebSocket's close carries it in at most 123 bytes, after what the
// apiserver puts before it.
var ErrNoConsole = errors.New("its serial console takes clients once the VM is started again")

// LogOptions selects what ReadLog sends.
type LogOptions struct {
	Follow bool
	// Tail keeps only the last Tail lines of what the log holds, or every
	// line if it is AllLines: each line of output, and each exit, is one.
	// Zero sends only what is written from now, when following.
	Tail int64
	// Since, if not zero, drops records older than it, by the time the
	// guest gave them.
	Since time.Time
}

// AllLines is the Tail that keeps every line.
const AllLines = -1

// LogRecord is one record of a console log. The guest writes it, so all
// but Boot is the guest's claim (docs/decisions/guest-agent.md).
type LogRecord struct {
	// Boot numbers the VM's boots from 1, the same whatever part of the
	// log is read; 0 means unknown, for records written before boots were
	// numbered.
	Boot   int64
	Stream console.Stream
	Time   time.Time
	// Data is a Stdout, Stderr or Init record's.
	Data []byte
	// Exit is an Exit record's.
	Exit console.ExitStatus
}

// Bounds on what one read of a log holds: a batch, and the memory Tail
// keeps back before sending, the records' own included. A record holds at
// most console.MaxPayload.
const (
	MaxLogBatch = 256 << 10
	MaxLogTail  = 4 << 20
)

// machineUIDPattern is what names a machine on a host: a unit name and a
// directory name, short enough that a VM's socket paths fit a unix address.
var machineUIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// MaxMachineUID is the longest machine uid a host takes.
const MaxMachineUID = 64

// CheckMachineUID refuses what cannot name a machine on a host.
func CheckMachineUID(uid string) error {
	if !machineUIDPattern.MatchString(uid) {
		return fmt.Errorf("machine uid %q cannot name a VM", uid)
	}
	return nil
}

// StopGrace is how long a container's process has between SIGTERM and
// SIGKILL when its machine is stopped.
const StopGrace = 10 * time.Second

// StoppedSignal is the Signal of the Exit record that ends a firmware VM's
// boot the platform ended, pressing its power button for a Stop or the
// host's shutdown: SIGTERM, as a stop ends a container's process. A boot
// with no signal ended on its own: the guest powered off, or the VM
// crashed.
const StoppedSignal int32 = 15

// VMSpec is everything a VM is started with. Hash identifies it.
type VMSpec struct {
	MachineUID string
	// Machine names the machine, project/name, for logs.
	Machine   string
	VCPUs     int32
	MemoryMiB int64
	Root      Disk
	// Disks are attached after the disks the boot needs, in this order.
	// Left out of the hash when empty, so a VM started before there were
	// disks keeps its hash.
	Disks []Volume `json:",omitempty"`
	// Firmware boots the VM through the platform's firmware from Disks[0],
	// with its own kernel, as a VirtualMachine's: no Root, nothing to Run,
	// no guest agent, and its serial console is its console log, each boot
	// ended by an Exit record, 0 if the guest powered off, StoppedSignal if
	// the platform ended it (docs/decisions/vm-boot.md). Stop presses its
	// power button.
	Firmware bool `json:",omitempty"`
	// SystemSerialNumber is a firmware VM's SMBIOS system serial number,
	// printable ASCII of at most computeapi.MaxSystemSerialNumber bytes;
	// empty is the one pointing cloud-init at the metadata service. Left
	// out of the hash when empty, so a VM started before there was one
	// keeps its hash.
	SystemSerialNumber string `json:",omitempty"`
	// TPM gives a firmware VM a TPM 2.0, swtpm's, whose state outlives the
	// VM's stops and goes with its logs; the hypervisor driver needs the
	// VM's key to start it. Left out of the hash when false, so a VM started
	// before there were TPMs keeps its hash.
	TPM        bool `json:",omitempty"`
	Interfaces []Interface
	// Nameservers and Search are the guest's resolvers and search domains,
	// as its /etc/resolv.conf lists them.
	Nameservers []string
	Search      []string
	// Run is what the guest runs.
	Run RunSpec
	// Mounts are the Disks the guest mounts as volumes before Run starts.
	// Left out of the hash when empty, as Disks are.
	Mounts  []Mount `json:",omitempty"`
	Restart RestartPolicy
	// FileShareServer boots the VM as a FileShare's server, as hangar-init
	// is made one by its boot disk: no Root, Disks, Mounts or Run, a blank
	// disk at vda that it never reads, and its share's volume at vdc,
	// after its boot disk (docs/decisions/file-shares.md).
	FileShareServer *ShareServer `json:",omitempty"`
	// Converter boots the VM as a converter VM, from the converter
	// initramfs: no Root, Disks, Mounts, Run or Interfaces, a blank disk at
	// vda that it never reads, the file at vdc, read-only, and the target,
	// if any, at vdd (docs/decisions/custom-images.md).
	Converter *ConverterSpec `json:",omitempty"`
}

// ConverterSpec is what a converter VM is booted with.
type ConverterSpec struct {
	// File is the uploaded file, as staged: a regular file of root's, of
	// whole sectors, holding FileSize bytes of the tenant's at FileOffset.
	File                 string
	FileOffset, FileSize int64
	// Target is the block device the file is converted onto, a zvol's
	// /dev/zvol link; empty for a VM that only inspects the file.
	Target string `json:",omitempty"`
}

// ShareServer is what a FileShare's server is booted with.
type ShareServer struct {
	Volume ShareVolume
	// Export is what its boot disk gives it to start with. It is no part of
	// the spec's Hash: the server is told a new one while it runs
	// (Guest.SetExport), and a boot by its restart policy starts from the
	// one its VM was started with.
	Export ShareExport
}

// ShareExport is who may mount a share, and how.
type ShareExport struct {
	// Clients are each machine's address, once.
	Clients []ShareClient
	Squash  Squash
	// AnonUID and AnonGID are the user and group squashed users become.
	AnonUID, AnonGID uint32
}

// ShareClient is a machine allowed to mount a share.
type ShareClient struct {
	Address netip.Addr
	// ReadOnly is enforced by the server.
	ReadOnly bool
}

// Squash is which users of a share's clients become its anonymous user.
type Squash int

const (
	SquashNone Squash = iota + 1
	SquashRoot
	SquashAll
)

// Mount is one of a VM's Disks mounted in its guest as a volume. See
// docs/decisions/container-volumes.md.
type Mount struct {
	// Disk is the volume's index in the VM's Disks.
	Disk int
	// Name is the Disk's, for the guest's messages.
	Name     string
	Target   string
	ReadOnly bool   `json:",omitempty"`
	Nocopy   bool   `json:",omitempty"`
	Subpath  string `json:",omitempty"`
	// Share, if valid, makes it a FileShare mounted over NFS from its
	// server at this address, and Disk and Nocopy mean nothing; Name is
	// the share's.
	Share netip.Addr `json:",omitzero"`
}

// Hash identifies the spec: a VM started with a spec of another hash runs
// something else and must be started again.
func (s VMSpec) Hash() string {
	// A container's root disk and volumes grow while its VM runs
	// (Hypervisor.ResizeRoot, ResizeVolume), so their sizes are no part of
	// what the VM runs. A firmware VM sees a disk grown only as it boots.
	s.Root.Size = 0
	if !s.Firmware {
		s.Disks = slices.Clone(s.Disks)
		for i := range s.Disks {
			s.Disks[i].Size = 0
		}
	}
	return s.sizedHash()
}

// asStarted is s with its root disk's and, but for a firmware VM's, its
// volumes' sizes as vm was started with them, which a hash older nodes gave
// counted.
func (s VMSpec) asStarted(vm VM) VMSpec {
	s.Root.Size = vm.RootSize
	if !s.Firmware && len(vm.DiskSizes) == len(s.Disks) {
		s.Disks = slices.Clone(s.Disks)
		for i := range s.Disks {
			s.Disks[i].Size = vm.DiskSizes[i]
		}
	}
	return s
}

// sizedHash is the Hash nodes gave a spec before a root disk grew while
// its VM ran, which counted every disk's size; with the root disk's size
// zero, the Hash nodes gave before a container's volumes did.
func (s VMSpec) sizedHash() string {
	// The interfaces' IPv6 addresses change while a VM runs, so they are no
	// part of what it runs.
	s.Interfaces = slices.Clone(s.Interfaces)
	for i := range s.Interfaces {
		s.Interfaces[i].IPv6 = netip.Addr{}
	}
	return s.hash()
}

// noExport is the spec less what a share's server is told while it runs.
func (s VMSpec) noExport() VMSpec {
	if s.FileShareServer != nil {
		srv := *s.FileShareServer
		srv.Export = ShareExport{}
		s.FileShareServer = &srv
	}
	return s
}

// LegacyHash is the Hash nodes gave a spec before an interface's IPv6
// address could change while its VM ran, which counted the addresses, with
// those of started, the interfaces as the VM was started, and the root
// disk's size. A VM started by such a node keeps that hash, and runs the
// spec all the same.
func (s VMSpec) LegacyHash(started []Interface) string {
	s.Interfaces = slices.Clone(s.Interfaces)
	for i := range s.Interfaces {
		s.Interfaces[i].IPv6 = netip.Addr{}
		if n := slices.IndexFunc(started, func(o Interface) bool { return o.Name == s.Interfaces[i].Name }); n >= 0 {
			s.Interfaces[i].IPv6 = started[n].IPv6
		}
	}
	return s.hash()
}

// Runs reports whether vm, as the host lists it, runs s: by Hash, by a hash
// an older node gave, counting the disks' sizes as vm was started, the root
// disk's or not (sizedHash), or the IPv6 addresses it was started with too
// (LegacyHash), or by any of them of s without its volumes' serials, as a
// node before them started it. An upgrade restarts no VM; its next boot
// gives it the serials.
func (s VMSpec) Runs(vm VM) bool {
	specs := []VMSpec{s}
	bare := s
	bare.Disks = slices.Clone(s.Disks)
	serials := false
	for i := range bare.Disks {
		serials = serials || bare.Disks[i].Serial != ""
		bare.Disks[i].Serial = ""
	}
	if serials {
		specs = append(specs, bare)
	}
	for _, c := range specs {
		if vm.SpecHash == c.Hash() {
			return true
		}
		c = c.asStarted(vm)
		rootless := c
		rootless.Root.Size = 0
		if vm.SpecHash == c.sizedHash() || vm.SpecHash == c.LegacyHash(vm.Interfaces) || vm.SpecHash == rootless.sizedHash() {
			return true
		}
	}
	return false
}

func (s VMSpec) hash() string {
	b, err := json.Marshal(s.noExport())
	if err != nil {
		panic(fmt.Sprintf("encoding a VM spec: %v", err)) // every field encodes
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// RunSpec is the process hangar-init runs in the guest.
type RunSpec struct {
	Command    []string
	Env        []string
	WorkingDir string
	User       string
}

// RestartPolicy says when the hypervisor starts an exited VM again.
type RestartPolicy int

const (
	RestartAlways RestartPolicy = iota
	RestartOnFailure
	RestartNever
)

// VM is a VM as the host has it.
type VM struct {
	MachineUID string
	// SpecHash is the Hash of the spec it was started with.
	SpecHash string
	State    VMState
	// ExitCode is the guest process's, as hangar-init reports it, once the
	// VM has exited.
	ExitCode int
	// Started is when the running instance booted.
	Started time.Time
	// Run is what its guest runs, as the VM was started: what an exec in
	// it inherits.
	Run RunSpec
	// Interfaces are its interfaces as the VM was started, which a
	// container's guest takes from its boot disk at every boot.
	Interfaces []Interface
	// RootSize is its root disk's size as the VM was started; the disk may
	// have grown since (Hypervisor.ResizeRoot).
	RootSize int64
	// DiskSizes are the sizes of its Disks as the VM was started, in their
	// order; a container's may have grown since (Hypervisor.ResizeVolume).
	DiskSizes []int64
}

// VMState is where a VM is.
type VMState int

const (
	VMRunning VMState = iota + 1
	// VMExited means the VM stopped and its restart policy keeps it down.
	VMExited
)

// Guest talks to hangar-init inside a VM, which it finds by machine uid as
// the hypervisor driver started it.
type Guest interface {
	// Ready returns nil once the machine's guest agent answers. It is called
	// with a short deadline and must honour it.
	Ready(ctx context.Context, machineUID string) error
	// Stop asks the guest to stop its container's process: SIGTERM, then
	// SIGKILL after grace. The guest powers off once the process has ended;
	// Stop returns once the process is signalled, not when it ends.
	Stop(ctx context.Context, machineUID string, grace time.Duration) error
	// Exec runs a process in the machine's guest, until it ends or ctx
	// does, which kills its group. A guest out of reach fails Recv; a
	// program that cannot run exits 127 or 126, as in a shell.
	Exec(ctx context.Context, machineUID string, spec ExecSpec) (ExecSession, error)
	// SetIPv6 makes the public IPv6 address and IPv6 default route of each
	// of the guest's interfaces what ifaces say, while its container runs.
	// A guest that cannot, as one started before it could, fails with
	// ErrGuestCannot.
	SetIPv6(ctx context.Context, machineUID string, ifaces []GuestIPv6) error
	// RootUsage says how large a container's root filesystem is and how much
	// of it is used, as the guest claims. A guest that cannot, as one
	// started before it could, fails with ErrGuestCannot.
	RootUsage(ctx context.Context, machineUID string) (FSUsage, error)
	// GrowRoot has a container's guest grow its root filesystem, while it
	// runs, to the disk it sees (Hypervisor.ResizeRoot). A filesystem that
	// fills its disk already stays as it is. A guest that cannot, as one
	// started before it could, fails with ErrGuestCannot; it grows the
	// filesystem as it boots.
	GrowRoot(ctx context.Context, machineUID string) error
	// VolumeUsage and GrowVolume do for the filesystem of a container's
	// volume on device, as its boot disk names it (/dev/vdc on), what
	// RootUsage and GrowRoot do for its root. A guest that cannot, as one
	// started before it could, fails with ErrGuestCannot; one that mounts no
	// volume read-write from device fails otherwise.
	VolumeUsage(ctx context.Context, machineUID, device string) (FSUsage, error)
	GrowVolume(ctx context.Context, machineUID, device string) error
	// SetExport replaces the whole export of a FileShare's server. A server
	// refusing it keeps the export it had, and the error says why.
	SetExport(ctx context.Context, machineUID string, export ShareExport) error
}

// GuestIPv6 is one guest interface's public IPv6, by its MAC: an address
// and, on the interface holding it, the default route's gateway; zero for
// none.
type GuestIPv6 struct {
	MAC     string
	Address netip.Addr
	Gateway netip.Addr
}

// GuestIPv6Of is what a container's guest is given of its interfaces'
// public IPv6, on its boot disk or by SetIPv6: each interface's address, and
// the default route by the first interface alone.
func GuestIPv6Of(ifaces []Interface) []GuestIPv6 {
	out := make([]GuestIPv6, len(ifaces))
	for n, i := range ifaces {
		out[n] = GuestIPv6{MAC: i.MAC, Address: i.IPv6}
		if n == 0 && i.IPv6.IsValid() {
			out[n].Gateway = GatewayIPv6
		}
	}
	return out
}

// FSUsage is how large a guest's filesystem, its root or a volume's, is and
// how much of it is used, in bytes.
type FSUsage struct {
	Size, Used uint64
}

// ErrGuestCannot is a guest asked what it does not know how to do.
var ErrGuestCannot = errors.New("the guest cannot do this")

// ExecSpec is a process for Guest.Exec to run.
type ExecSpec struct {
	// Process is the whole process: nothing is taken from the container's.
	Process RunSpec
	// TTY runs it on a terminal of Size; its stdout and stderr are then one
	// stream, sent as stdout.
	TTY  bool
	Size WindowSize
}

// WindowSize is a terminal's size.
type WindowSize struct {
	Rows, Cols uint16
}

// ExecSession is an exec's process under way. Its input methods are called
// from one goroutine and Recv from another; they fail once the process has
// ended.
type ExecSession interface {
	// Stdin writes to the process's stdin. It waits while the guest holds
	// as much as it takes that the process has not read.
	Stdin(p []byte) error
	// CloseStdin closes the process's stdin; on a terminal it sends
	// end-of-file.
	CloseStdin() error
	Resize(size WindowSize) error
	Signal(sig syscall.Signal) error
	// Recv returns the process's next output, and last how it ended; then
	// io.EOF.
	Recv() (ExecOutput, error)
}

// ExecOutput is a piece of an exec's output, or how its process ended.
type ExecOutput struct {
	// Data is what the process wrote to stdout, or to stderr if Stderr.
	Data   []byte
	Stderr bool
	// Exit, on the last, is how the process ended: Code is 128 plus the
	// signal's number when a signal ended it.
	Exit *console.ExitStatus
}

// MaxExecOutput bounds an ExecOutput's data.
const MaxExecOutput = 64 << 10
