# Virtual machines boot their own kernel through firmware

Status: accepted, 2026-10.

## Context

A `VirtualMachine` runs a distribution's cloud image, as a VPS does
(`docs/decisions/machine-model.md`, `docs/decisions/vm-images.md`). A
container boots the platform's kernel and initramfs, with hangar-init as PID
1 (`docs/decisions/guest-kernel.md`). A VM's user expects what the
distribution ships: its kernel and modules, its updates through its package
manager, its own init, and nothing of ours inside. EC2, GCE and Azure boot an
image's own kernel through firmware; DigitalOcean and Linode, which once
booted kernels of their own, moved there too.

## Decision

- **A VM boots its root disk through UEFI firmware, with the image's own
  kernel.** The machine's boot is `firmware {root_disk}`; Cloud Hypervisor
  runs its EDK2 build, `CLOUDHV.fd` from cloud-hypervisor/edk2
  (`--firmware`), which boots the root disk's EFI loader (shim, then GRUB).
  The firmware is pinned by release and sha256 in
  `deploy/host/versions.env`, as Cloud Hypervisor is, installed by
  deploy/host's packages step at `/usr/local/lib/hangar/CLOUDHV.fd`
  (`hangar-node --vm-firmware`), bound read-only into each VM's unit and read
  there by its own user (`docs/decisions/node-agent.md`). The project
  publishes no checksums: the pinned sha256 is that of the release's file as
  it was first fetched.
- **Not rust-hypervisor-firmware.** It was built in and removed: it cannot
  hand GRUB 2.12, which Ubuntu 24.04 and later ship, the UEFI services it
  loads an initrd with (rust-hypervisor-firmware#382), so those images'
  kernels panic without their initramfs.
- **Not the platform kernel.** It would hold a VM to our config and our
  release cadence, need hangar-init or another init of ours inside, and break
  every image that expects its own kernel modules.
- **No Secure Boot or Windows yet.** The same EDK2 can be built with Secure
  Boot, or the full OVMF used, later, without changing the API.
- **Nothing of the platform runs inside.** Network and keys come through
  cloud-init: Cloud Hypervisor gives the VM the SMBIOS system serial number
  `ds=nocloud;s=http://169.254.169.254/nocloud/;dsmode=net`, which
  cloud-init reads as its NoCloud datasource, served by the metadata
  service of the VM's networks. Images are used unmodified. `dsmode=net`
  says the seed is read once the network is up. cloud-init 24.4 and later
  infer that from the http URL, but 22.4 (Debian 12's) takes `ds=nocloud`
  alone for a local seed, drops the seed its network stage fetched, and
  boots with no datasource. Every version reads a `dsmode` given there
  before inferring one. Not `ds=nocloud-net`, which newer cloud-init
  reports as deprecated.
- **A VM may have a serial number of its own instead.** Some operating
  systems take the SMBIOS system serial number as the device's identity,
  as hardware vendors set it. `spec.platform.serialNumber` replaces the
  NoCloud string with any printable ASCII of at most 64 bytes, not checked
  for uniqueness; changing it restarts the VM. Such a VM gives up what
  that string pointed at: its cloud-init finds no platform seed, so it gets
  no `userData`, keys or trust in the platform's SSH CA, and `hangar ssh`
  cannot log in to it, unless the image arranges those itself (a `CIDATA`
  disk is the other place cloud-init's NoCloud looks). Apply says so as a
  warning. Cloud Hypervisor gets the value inside double quotes, a quote
  in it doubled, so its option parser takes commas, brackets and
  surrounding spaces as part of it.
- **What cloud-init gets** (`networking.md`, the metadata service):
  `meta-data` with the VM's uid as `instance-id` and its name as
  `local-hostname`; `user-data`, the VM's `userData` exactly as written
  (at most 16 KiB; empty is served as an empty answer); and
  `vendor-data`, the platform's settings, a `#cloud-config` with the VM's
  `sshAuthorizedKeys` as `ssh_authorized_keys`, which cloud-init gives the
  image's default user (`ubuntu` on Ubuntu). The platform's settings go
  in vendor-data, never into the user's document: cloud-init applies
  user-data over vendor-data, so a user-data that sets its own keys, or
  turns vendor-data off, wins. With the platform's SSH CA, vendor-data
  also trusts it for the VM's uid (`vm-ssh.md`). A key holds only
  printable characters and tabs, as cloud-init's YAML parser refuses the
  whole vendor-data over some others; and the vendor-data, keys each
  quoted and the CA's script, fits the machine's 64 KiB, so a VM admitted
  always gets its machine.
- **First boot only.** cloud-init runs per instance, and the instance id
  is the VM's uid, so user-data and keys apply on the VM's first boot,
  not again on a restart, nor when its machine is made again. Changing
  `userData` or `sshAuthorizedKeys` restarts nothing: the new data is
  served, and read only by a new instance (a VM made again).
- **Its disks are Disks.** Its root disk is a `Disk`, made for it from an
  image (`<vm>-root`, deleted with it or retained, as its `deletePolicy`
  says) or one it names; data disks follow it, in order (`vda`, `vdb` and on).
  A VM has no platform boot disk and no virtio console.
- **Each disk carries a serial** the guest finds it by, as
  `/dev/disk/by-id/virtio-<serial>`, which an attach order does not move:
  the Disk's name if it is at most 20 bytes, the most virtio-blk carries
  (Cloud Hypervisor refuses to boot past it rather than cut it), and
  otherwise the last 20 hex digits of the Disk's uid. Not its first 20: a
  UUIDv7 starts with the time it was made in milliseconds, which two disks
  made together share. A name that is another of the VM's disks' uid
  digits gives way to its own, so no two are alike. There is no field for
  it: names and uids never change, so neither does a disk's serial. A VM a
  node started before disks had serials runs on through the node's upgrade
  (`driver.VMSpec.Runs`), and has them from its next boot. A container's
  disks have none: hangar-init mounts them by their place.
- **Root disks follow the delete policy, races included.** A root disk the
  VM made is annotated with the VM's uid. Under `DELETE` it carries an
  owner reference to the VM, which garbage collection follows, though the
  Project stays its controller, as it is of every Disk: GC follows any owner
  reference, controller or not. Under `RETAIN` it has none. A VM's delete
  is refused (Aborted) until the reference says what the policy does. And
  since a reconcile that read the VM before its policy changed, or before
  it was deleted, could otherwise undo what the delete was allowed on,
  Disk admission refuses (Aborted) a root disk being made, or its
  reference added or dropped, unless the VM is live and its policy as it
  is now says so; VirtualMachine shares Disk's write lock, so the check
  and the delete read the same VM.
- **Ready once its VM runs.** There is no agent in the guest to ask, so a
  VM's machine is Ready as soon as Cloud Hypervisor runs it; that the guest
  booted, or is reachable, is not known to the platform.
- **Its serial console is its log.** The launcher frames what the guest
  writes on its serial port into the VM's console log, timed by the host,
  numbered by boot, each boot ended by whether the guest powered off (0) or
  not (253), and whether the platform ended it, stopping the VM or shutting
  its host down (`stopped`): `hangar logs vm` reads it as `hangar logs`
  reads a container's. It is interactive too, through the VM's console
  helper (`serial-console.md`).
- **Stopping presses its power button**, an ACPI event through Cloud
  Hypervisor's API socket, by the driver and, at host shutdown, by the
  launcher; a guest still up after its grace (60 s by default,
  `chvm.Config.FirmwareStopGrace`) is powered off.
- **No restart policy.** Only a guest that powers itself off stays off, as
  an instance does on a cloud; stopping and starting the VM (`runState`)
  boots it again. A VM the platform turned off, its host shutting down or
  lost, boots again when the host is back, and one that crashes boots again,
  backing off as a container's restarts do. Its machine's restart policy is
  `on-failure`, which says exactly that: a guest powering off is the one
  boot that ends well. Its `Ready` reason then is `PoweredOff`; a crashed
  VM reads `Exited` until it boots again. A guest's own reboot is Cloud
  Hypervisor's, and keeps the VM running.

## Consequences

- A VM given its own serial number boots as an image without cloud-init
  does: the platform reaches nothing inside it.
- A VM's image must boot through EFI: distributions' cloud images do; images
  with only a BIOS boot loader do not.
- The platform cannot tell a VM that booted from one stuck at its boot
  loader; its serial console says.
- A grown disk, as a changed interface, restarts the VM (`storage-zfs.md`).
