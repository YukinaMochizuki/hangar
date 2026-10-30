// Package computeapi is what the apiserver and the node agent must read
// alike in Machines, Nodes, Disks, FileShares, CustomImages and
// EndpointServices: what their status says, of which each writes a part and
// reads the other's, which Disks a Machine's spec names, which networks a
// FileShare is on, and which transit ports an EndpointService draws from.
package computeapi

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
)

// DefaultNetwork is the network every project has: the one a machine
// naming no interface joins, and a FileShare naming no network is on.
const DefaultNetwork = "default"

// The transit ports EndpointServices draw theirs from: below those
// PortForwards draw, which a network's namespace takes at the same transit
// address (docs/decisions/endpoint-services.md).
const (
	TransitPortsFrom    = 1024
	TransitPortsThrough = 9999
)

// ShareNetworks are the networks a FileShare is on: those its spec names,
// or DefaultNetwork if it names none.
func ShareNetworks(spec *storagev1alpha1.FileShareSpec) []string {
	if len(spec.GetNetworks()) == 0 {
		return []string{DefaultNetwork}
	}
	return spec.GetNetworks()
}

// VirtioDevice is how a Linux guest names the virtio disk the hypervisor
// attaches n-th, from 0: vda to vdz, then vdaa on.
func VirtioDevice(n int) string {
	name := ""
	for n++; n > 0; n = (n - 1) / 26 {
		name = string(rune('a'+(n-1)%26)) + name
	}
	return "vd" + name
}

// FirstContainerDisk is where a container machine's disks start among its
// virtio disks: after its root disk, vda, and its boot disk, vdb.
const FirstContainerDisk = 2

// MachineDisks names the Disks a machine has, in the order its node
// attaches them: the disk a firmware boot boots from, then spec.disks.
func MachineDisks(spec *computev1alpha1.MachineSpec) []string {
	var out []string
	if root := spec.GetFirmware().GetRootDisk(); root != "" {
		out = append(out, root)
	}
	for _, d := range spec.GetDisks() {
		out = append(out, d.GetDisk())
	}
	return out
}

// MaxDiskSerial is the longest serial a virtio-blk disk carries, in bytes:
// the virtio spec's, which Cloud Hypervisor refuses to boot past.
const MaxDiskSerial = 20

// DiskSerials are the serials a VirtualMachine's disks are given, in the
// order of names, the Disks MachineDisks lists, and uids, theirs: a disk's
// name if it fits MaxDiskSerial, else the last MaxDiskSerial hex digits of
// its uid, the random part of a UUIDv7. A name that is any of the disks'
// uid digits so taken gives way to its own, so no two serials are alike.
// The guest finds each at /dev/disk/by-id/virtio-<serial>; see
// docs/decisions/vm-boot.md.
func DiskSerials(names, uids []string) []string {
	tails := make([]string, len(uids))
	for i, uid := range uids {
		u := strings.ReplaceAll(uid, "-", "")
		tails[i] = u[max(0, len(u)-MaxDiskSerial):]
	}
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = tails[i]
		if len(name) <= MaxDiskSerial && !slices.Contains(tails, name) {
			out[i] = name
		}
	}
	return out
}

// CheckDiskSerial checks a disk's serial as DiskSerials makes them: at most
// MaxDiskSerial lowercase letters, digits and '-', which Cloud Hypervisor's
// option parser takes as they are, and the guest's udev names a link by
// unchanged; empty is none.
func CheckDiskSerial(s string) error {
	if len(s) > MaxDiskSerial || strings.Trim(s, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return fmt.Errorf("%q is not at most %d lowercase letters, digits and '-'", s, MaxDiskSerial)
	}
	return nil
}

// DiskInUseFinalizer keeps a deleted Disk until no machine lists it; the
// apiserver removes it then. A node removes a disk's volume only once it is
// gone, and reports DiskReleased.
const DiskInUseFinalizer = registry.Domain + "/disk-in-use"

// DependentsFinalizer keeps an object that cascades in place, marked as
// being deleted, until everything it owns is gone: a tenant's projects, a
// FileShare's server. A node lets go of a deleted share only once it is
// gone, and reports FileShareReleased.
const DependentsFinalizer = registry.Domain + "/dependents"

// Condition types. Each has one writer.
const (
	// NodeReady says a Node's agent is reporting. The agent sets it true with
	// every report; the apiserver turns it Unknown when the reports stop.
	NodeReady = "Ready"
	// MachineScheduled is the scheduler's: whether the machine has a node.
	// The scheduler also sets phase Pending on a machine no node has
	// reported on; the rest of a machine's status is its node's.
	MachineScheduled = "Scheduled"
	// MachineReady is the node's: whether the machine runs its current spec
	// with its guest agent answering. When false, its reason and message say
	// what stands in the way, for the machine's owner to pass on.
	MachineReady = "Ready"
	// MachineReleased is the node's: a deleted machine's VM, disks and
	// interfaces are gone from the host.
	MachineReleased = "Released"
	// DiskAllocated is the apiserver's: whether the disk's node holds room
	// for its spec.size, which status.size then says. The apiserver also
	// writes a disk's node, size, machine, attachment and phase.
	DiskAllocated = "Allocated"
	// DiskReady is the node's: the disk exists on the host at status.size.
	DiskReady = "Ready"
	// DiskReleased is the node's: a deleted disk is gone from the host.
	DiskReleased = "Released"
	// FileShareReleased is the node's: a deleted share's node holds nothing
	// of it any more.
	FileShareReleased = "Released"
	// ImageVerified is any node's: True once a node has fetched the image,
	// found it matches its sha256 and made a disk image of it; False, with
	// one of the Image reasons, when the image at that generation can never
	// be used. Nodes then try it again only once its spec changes.
	ImageVerified = "Verified"
	// CustomImageSnapshotted is the image's node's: True once the source
	// disk's volume is snapshotted, after which the disk is free again.
	CustomImageSnapshotted = "Snapshotted"
	// CustomImageUploaded is the image's node's, for an upload: True once
	// the whole file is in and matches its sha256, with status.sha256;
	// False with ReasonAwaitingUpload until then.
	CustomImageUploaded = "Uploaded"
	// CustomImageReady is the image's node's: True once the image's base is
	// made, with status.used_size; False with one of CustomImageFailures
	// once it failed for good, and with ReasonCapturing,
	// ReasonAwaitingUpload or ReasonConverting until then.
	CustomImageReady = "Ready"
)

// Reasons the node gives for CustomImageSnapshotted, CustomImageUploaded
// and CustomImageReady, but ReasonQuotaExceeded.
const (
	ReasonCapturing      = "Capturing"
	ReasonCaptured       = "Captured"
	ReasonCaptureFailed  = "CaptureFailed"
	ReasonAwaitingUpload = "AwaitingUpload"
	ReasonUploaded       = "Uploaded"
	ReasonConverting     = "Converting"
	ReasonConverted      = "Converted"
	// ReasonUploadExpired means the file was not whole by
	// status.upload_expires.
	ReasonUploadExpired = "UploadExpired"
	// ReasonDigestMismatch means the whole file does not match the
	// declared sha256.
	ReasonDigestMismatch = "DigestMismatch"
	// ReasonConvertFailed means converting a file the node took failed.
	ReasonConvertFailed = "ConvertFailed"
	// ReasonQuotaExceeded means the disk an uploaded file holds, its
	// status.virtual_size, does not fit the project's or the tenant's disk
	// quota. The apiserver, not the node, sets it, in place of the node's
	// report of the virtual size.
	ReasonQuotaExceeded = "QuotaExceeded"
)

// CustomImageFailures are the reasons a CustomImage's Ready is False for
// good: the image is never tried again. ReasonInvalidImage and
// ReasonImageTooLarge are an uploaded file the node refuses.
var CustomImageFailures = []string{
	ReasonCaptureFailed, ReasonUploadExpired, ReasonDigestMismatch,
	ReasonInvalidImage, ReasonImageTooLarge, ReasonConvertFailed, ReasonQuotaExceeded,
}

// Reasons an image can never be used, for ImageVerified.
const (
	ReasonHashMismatch  = "HashMismatch"
	ReasonInvalidImage  = "InvalidImage"
	ReasonImageTooLarge = "ImageTooLarge"
)

// Reasons the node gives for MachineReady.
const (
	ReasonRunning         = "Running"
	ReasonCreating        = "Creating"
	ReasonPulling         = "Pulling"
	ReasonBooting         = "Booting"
	ReasonStopped         = "Stopped"
	ReasonImagePullFailed = "ImagePullFailed"
	// ReasonImagePullUnauthorized means the registry refused the machine's
	// login, or asked for one it has none of.
	ReasonImagePullUnauthorized = "ImagePullUnauthorized"
	ReasonDiskFailed            = "DiskFailed"
	// ReasonDiskTooSmall means the machine asks for a root disk smaller than
	// its image.
	ReasonDiskTooSmall = "DiskTooSmall"
	// ReasonDiskGrowing means the machine's root disk, smaller than its
	// image, waits for its owner to grant it the size the image needs,
	// which its resources.disk_grow lets it grow to (status.disk_needed).
	ReasonDiskGrowing   = "DiskGrowing"
	ReasonNetworkFailed = "NetworkFailed"
	ReasonBootFailed    = "BootFailed"
	ReasonExited        = "Exited"
	// ReasonPoweredOff means a firmware machine's guest powered itself
	// off, which keeps it off until it is stopped and started; a firmware
	// machine that ended otherwise is Exited, and boots again.
	ReasonPoweredOff      = "PoweredOff"
	ReasonNetworkNotReady = "NetworkNotReady"
	// ReasonDiskNotReady means an attached disk is not on the node yet.
	ReasonDiskNotReady = "DiskNotReady"
	// ReasonImageNotReady means a disk's CustomImage cannot be made a disk
	// of here: it is gone, not Ready, or on another node.
	ReasonImageNotReady = "ImageNotReady"
	// ReasonExportFailed means a file share's server did not take the
	// export its node told it, and serves the one it had.
	ReasonExportFailed = "ExportFailed"
	// ReasonFileShareNotReady means a container waits to boot for a file
	// share it mounts: its server on the node is not running yet, or has
	// not yet taken an export holding the container.
	ReasonFileShareNotReady = "FileShareNotReady"
)

// ReasonStartDoesNotFit is a Container's or VirtualMachine's Ready and
// Stopped reason while it is asked to run but stays stopped: its node, where
// its disks are, has too little memory free for it. The apiserver, not the
// node, sets it, and starts the machine once there is room.
const ReasonStartDoesNotFit = "StartDoesNotFit"

// MaxSystemSerialNumber is the longest SMBIOS system serial number a
// firmware machine is given, in bytes.
const MaxSystemSerialNumber = 64

// CheckSystemSerialNumber checks a firmware machine's SMBIOS system serial
// number, as its VirtualMachine's spec.platform.serial_number: printable
// ASCII of at most MaxSystemSerialNumber bytes, or empty for the platform's.
func CheckSystemSerialNumber(s string) error {
	if len(s) > MaxSystemSerialNumber {
		return fmt.Errorf("%d bytes, at most %d", len(s), MaxSystemSerialNumber)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return fmt.Errorf("byte %d, %#02x, is not printable ASCII", i, s[i])
		}
	}
	return nil
}

// ParseIPv6Prefix reads the IPv6 prefix a Node reports: a global unicast
// /64 without host bits.
func ParseIPv6Prefix(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Addr().Zone() != "" || p.Bits() != 64 || p != p.Masked() || !p.Addr().IsGlobalUnicast() {
		return netip.Prefix{}, fmt.Errorf("%q is not a global IPv6 /64 such as 2001:db8:0:1::/64", s)
	}
	return p, nil
}
