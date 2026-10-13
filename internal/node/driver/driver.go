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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

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
	// ErrImageTooLarge means an image is beyond what the host unpacks.
	ErrImageTooLarge = errors.New("image too large")
	// ErrInvalidImage means an image holds what the host refuses to
	// unpack, such as a name climbing out of its root.
	ErrInvalidImage = errors.New("invalid image")
)

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

// Images resolves and unpacks OCI images for the host's platform, a pulled
// digest from the host's cache until forgotten. Failed pulls wrap
// ErrImageNotFound, ErrUnauthorized, ErrRegistryUnreachable or ctx's error.
type Images interface {
	// Resolve returns the digest the reference names now.
	Resolve(ctx context.Context, ref string, creds Credentials) (digest string, err error)
	// Config returns how the image wants to be run.
	Config(ctx context.Context, ref, digest string, creds Credentials) (ImageConfig, error)
	// Unpack writes the image's root filesystem into the empty dir, never
	// outside it; an image larger than the host allows is ErrImageTooLarge.
	Unpack(ctx context.Context, ref, digest, dir string, creds Credentials) error
	// Forget drops what the host keeps of a digest, once no base of it is
	// left.
	Forget(ctx context.Context, digest string) error
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

// Storage holds disks: a read-only base per image, a root disk per machine
// cloned from one, and a volume per API Disk, which outlives the machines it
// is attached to. Deleting what is not there is not an error.
type Storage interface {
	// EnsureBase makes a digest's base once, filling an empty dir with fill,
	// unless size (ImageConfig.Size, or zero) exceeds the room left; fill's
	// error is returned wrapped.
	EnsureBase(ctx context.Context, digest string, size int64, fill func(dir string) error) error
	// Bases lists the digests that have a base.
	Bases(ctx context.Context) ([]string, error)
	// DeleteBase removes a base. It fails while a root disk cloned from it
	// exists, as a ZFS snapshot with clones does.
	DeleteBase(ctx context.Context, digest string) error
	// EnsureRoot returns the machine's root disk of digest, cloned if need
	// be, of at least size bytes: grown, never shrunk, and ErrDiskTooSmall
	// below the base. A root disk of another digest is an error.
	EnsureRoot(ctx context.Context, machineUID, digest string, size int64) (Disk, error)
	// Roots lists every machine's root disk.
	Roots(ctx context.Context) ([]Disk, error)
	// DeleteRoot removes a machine's root disk. It fails while the machine's
	// VM runs.
	DeleteRoot(ctx context.Context, machineUID string) error
	// EnsureVolume returns a Disk's volume, made blank if need be, of at
	// least size bytes: grown, never shrunk.
	EnsureVolume(ctx context.Context, diskUID string, size int64) (Volume, error)
	// Volumes lists every Disk's volume.
	Volumes(ctx context.Context) ([]Volume, error)
	// DeleteVolume removes a Disk's volume. It fails while a VM has it.
	DeleteVolume(ctx context.Context, diskUID string) error
	// Capacity returns how much the storage holds, and how much of it is
	// free.
	Capacity(ctx context.Context) (Capacity, error)
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
	// its Device.
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
	// Instance names what a network's sockets serve, until it is made again;
	// "" means there is nothing to serve.
	Instance(ctx context.Context, networkUID string) (string, error)

	// EnsureFirewall makes each machine interface of the network on the
	// host hold exactly its policy, replacing the network's whole set at
	// once; an interface given none, or one made since, receives and sends
	// nothing but its network's services and replies. Policies of
	// interfaces not on the host are ignored. Policies of the interfaces on
	// the host larger than MaxFirewallRules together are refused, and the
	// firewall is then left as it was.
	EnsureFirewall(ctx context.Context, networkUID string, policies []InterfacePolicy) error
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
}

// MetadataAddr is the metadata service's address in every network, as
// clouds have it; a network's bridge holds it beside the gateway.
var MetadataAddr = netip.MustParseAddr("169.254.169.254")

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
	// Device names the tap on the host, unique there and at most 15
	// characters, as Linux allows. The network driver chooses it; the
	// hypervisor driver opens it.
	Device string
}

// Hypervisor runs VMs. A VM exited for good stays listed, with its exit
// code, until it is stopped: an unlisted VM is one never started. Stopping
// what is not there is not an error.
//
// A VM's console log, what its container wrote, outlives it: through its
// restarts, a Stop and a Start again, until DeleteLogs, once the machine is
// gone.
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
	// VMs lists the VMs on the host, running or not.
	VMs(ctx context.Context) ([]VM, error)
	// Changes signals that a VM changed state on its own, such as exiting.
	Changes() <-chan struct{}
	// Logs lists the machines whose console logs the host keeps.
	Logs(ctx context.Context) ([]string, error)
	// DeleteLogs removes a machine's console log. It fails while the
	// machine's VM is listed.
	DeleteLogs(ctx context.Context, machineUID string) error
	// ReadLog sends what the machine's console log holds, oldest first,
	// across every boot it keeps, in batches. A machine with no log has no
	// records. With Follow it then sends what is written, rotations
	// included, until ctx ends; a log made later is followed from its
	// start. It returns send's error, or ctx's once following.
	ReadLog(ctx context.Context, machineUID string, opts LogOptions, send func([]LogRecord) error) error
}

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

// StopGrace is how long a container's process has between SIGTERM and
// SIGKILL when its machine is stopped.
const StopGrace = 10 * time.Second

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
	Disks      []Volume `json:",omitempty"`
	Interfaces []Interface
	// Nameservers and Search are the guest's resolvers and search domains,
	// as its /etc/resolv.conf lists them.
	Nameservers []string
	Search      []string
	// Run is what the guest runs.
	Run     RunSpec
	Restart RestartPolicy
}

// Hash identifies the spec: a VM started with a spec of another hash runs
// something else and must be started again.
func (s VMSpec) Hash() string {
	b, err := json.Marshal(s)
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
}

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
