# VM images: the catalogue, applying it, and moving an image to a newer file

The platform's Images (`docs/decisions/vm-images.md`) are kept in
`deploy/catalog/images.yaml`: one `Image` per distribution release, each a
dated file of the distribution's own cloud image and its sha256. The file is
what production holds. Change an image there first, check it on a testbed,
then apply it.

## Apply the catalogue to production

On the host, as root through the admin socket (`docs/runbook/host-bootstrap.md`
says how the `local` context is set up, once):

```sh
sudo hangar --context local get images -o yaml > images-before.yaml
sudo hangar --context local apply -f /var/tmp/hangar/deploy-TAG/catalog/images.yaml
sudo hangar --context local get images
```

The file is in every release's `deploy.tar`, which
`hangar-fetch-release TAG DIGEST` unpacks to `/var/tmp/hangar/deploy-TAG/`
(`docs/runbook/host-bootstrap.md`). A checkout's copy works as well.

`apply` prints `image/NAME created` for a new image and `image/NAME applied`
for one that was there. Check `images-before.yaml`
against the file first: an image whose spec differs is replaced by what the
file says, so a field set on the host by hand and missing from the file is
lost. Applying an unchanged file changes nothing (no generation moves).

Nothing is downloaded until a disk is made of an image. A node fetches the
file then, and the image's status shows what it found: `Verified` True with
its `diskSize`, or `Verified` False with `HashMismatch`, `InvalidImage` or
`ImageTooLarge`. To check an image before anyone uses it, make a VM of it in
a project of your own and delete it afterwards.

The RHEL-family images (Rocky, AlmaLinux) are 10 GiB disks, against 3 to
3.5 GiB for Ubuntu and Debian. Each node keeps the base of every Image used
on it on the pool (sparse, so what it holds is what the image wrote), so
leave room for them (`docs/runbook/host-bootstrap.md`).

## Move an image to a newer file

The name stays the same: a disk names its image by name, and a new name
would be a new image. Only disks made after the change get the new file. A
disk keeps the base it was cloned from, and a running VM is not touched.

1. Find the newer file. Use a dated path, never `current`, `latest` or a
   minor-version alias, which move under the pin:

   | Image | Where the files are | Checksum and signature |
   |---|---|---|
   | `ubuntu-*` | `https://cloud-images.ubuntu.com/releases/<codename>/release-<date>/` | `SHA256SUMS`, `SHA256SUMS.gpg`, by the UEC Image Automatic Signing Key, `D2EB44626FDDC30B513D5BB71A5D6C4C7DB87C81` |
   | `debian-*` | `https://cloud.debian.org/images/cloud/<codename>/<date>-<build>/`, the `genericcloud` qcow2 | `SHA512SUMS`, not signed: take the sha256 of the file once it matches |
   | `rocky-*` | `https://dl.rockylinux.org/pub/rocky/<major.minor>/images/x86_64/`, the dated `GenericCloud-Base` | `<file>.CHECKSUM`, `<file>.CHECKSUM.asc`, by the release key of that major (`https://rockylinux.org/resources/gpg-key-info`) |
   | `almalinux-*` | `https://repo.almalinux.org/almalinux/<major.minor>/cloud/x86_64/images/`, the dated `GenericCloud` | `CHECKSUM`, `CHECKSUM.asc`, by the release key of that major (`https://almalinux.org/security/`) |

   Rocky and AlmaLinux move a minor release's files to their vaults once
   the next minor is out (`https://dl.rockylinux.org/vault/rocky/<major.minor>/`,
   `https://vault.almalinux.org/<major.minor>/`), so a pinned URL stops
   answering then. Disks made already keep working, but new ones fail to
   fetch. Move the image to the new minor's file, or point the URL at the
   vault.

2. Check the signature and take the sha256 from the checksum file, for
   example for Ubuntu:

   ```sh
   export GNUPGHOME=$(mktemp -d)
   gpg --keyserver hkps://keyserver.ubuntu.com --recv-keys D2EB44626FDDC30B513D5BB71A5D6C4C7DB87C81
   curl -fsSLO "$BASE/SHA256SUMS" && curl -fsSLO "$BASE/SHA256SUMS.gpg"
   gpg --verify SHA256SUMS.gpg SHA256SUMS
   grep server-cloudimg-amd64.img SHA256SUMS
   ```

   For Rocky and AlmaLinux, import the key from the distribution's own site
   and check that its fingerprint is the one that site publishes before
   `gpg --verify CHECKSUM.asc CHECKSUM`. Never take a sha256 from a file you
   downloaded from the same place without checking it.

3. Change `url`, `sha256` and, if it changed, `version` in
   `deploy/catalog/images.yaml`, and the note above the image (which checksum
   file, which key). `make check` runs `TestTheImageCatalogueApplies`
   (`internal/cli`), which applies the file and refuses an undated path.

4. Boot it on a testbed (`docs/runbook/testbed.md`, which says how to
   prepare one):

   ```sh
   make e2e-host-images IMAGES=ubuntu-26-04
   ```

   `IMAGES` takes names, comma-separated, or `all`. `TestTheImageCatalogue`
   (`test/testbed/flow/images_test.go`) downloads each file from its URL
   into the testbed's cache, checks it against its sha256, and serves it to
   a real apiserver and node. It then checks a VM of it end to end: it is
   Ready; its serial console has its boot; `hangar ssh` logs in as the
   image's `defaultUser` with the platform CA's certificate, and OpenSSH
   with a static key; cloud-init is done and set the hostname and the key;
   the root filesystem grew to the 20 GiB disk; `hangar exec` (the console
   terminal's `:exec`) returns a command's exit code; and `hangar stop`
   powers the guest off through its power button within the grace. Each
   image takes one to two minutes once its file is cached, and one whose
   login never comes fails after 15; `all` takes about 15 minutes.

5. Commit, then apply as above. Disks made from then on get the new file.
   Existing ones are as they were, and nodes sweep the old base once no disk
   is cloned from it.

## Add or remove an image

To add one, give it a name without dots (`<distribution>-<major>`, as
`debian-13`), then follow steps 1 to 5. To remove one,
`sudo hangar --context local delete image NAME`, then take it out of the
file. Admission refuses new disks of it from then on, and disks made of it
keep working.

## A project's own image: capture a stopped VM's disk

A tenant makes a golden image of a VM it set up as a `CustomImage`
(`docs/decisions/custom-images.md`): an independent copy of a stopped
Disk, such as the VM's root disk `NAME-root`, from which the project's
disks and VMs are then made. The platform copies the disk as it is and
changes nothing in it, so prepare the guest first. In the VM:

```sh
sudo cloud-init clean --machine-id --logs
sudo poweroff
```

`cloud-init clean` makes each VM of the image a first boot to cloud-init
(its own hostname, host keys and users), and `--machine-id` empties
`/etc/machine-id`, so each gets its own, and its own DHCP identity, rather
than the source's. Each VM of the image also has its own instance id, its
uid, which cloud-init takes as a new instance even without the clean, but
the machine id stays the source's then. Remove what else the software in it
must not share between copies.

Once the VM is stopped (`hangar get vm NAME` says `Stopped`; `hangar stop vm
NAME` if it is not), capture its disk:

```yaml
apiVersion: storage.hangarlab.cloud/v1alpha1
kind: CustomImage
metadata:
  project: PROJECT
  name: golden
spec:
  source:
    disk:
      name: NAME-root
  os: ubuntu
  version: "24.04"
  defaultUser: ubuntu
```

`hangar apply -f golden.yaml`, then `hangar get cimg golden -o yaml`.
`Snapshotted` True says the source disk is free again: the VM can start
while the copy goes on. `Ready` True, with `usedSize`, says the image can
be used; `Ready` False with `CaptureFailed` says why it failed, for good:
delete it and capture again. Make a VM of it as of a catalogue image, with
the project beside the name:

```yaml
spec:
  rootDisk:
    fromImage:
      image:
        name: golden
        project: PROJECT
      size: 20Gi
```

The image counts toward the tenant's `customImages` limit, and toward its
disk quota at its source's size while it is captured and at `usedSize` once
Ready. It lives on its source disk's node, and the node keeps it after the
image is deleted only while disks of it remain, made or still to be made,
which then count toward no quota of the image's.

## A project's own image: upload a file

A tenant also makes a `CustomImage` from a disk image file of its own, raw
or qcow2, such as a NixOS image built with nixos-generators
(`docs/decisions/custom-images.md`). As with a captured disk, prepare it
to boot as a VM here: it boots through the platform's UEFI firmware, so
it needs a bootloader on an EFI system partition (`nixos-generators -f
raw-efi`, or `qcow-efi`), and cloud-init, or another NoCloud datasource
reader, to take the user, keys and network its VMs are given. Then:

```sh
hangar image upload nixos.qcow2 --name nixos --os nixos --version 25.05 --default-user root
```

It makes the image with the file's size and sha256, and sends the file in
chunks of 64 MiB with a progress line; `-o json` prints the image instead.
A chunk that fails on the way is sent again; run the same command after an
interruption, and it goes on from where the node is, for up to 24 hours
after the image was made (`uploadExpires`). `--no-verify` sends no sha256
and does not read the file twice; each chunk is still checked. It
authenticates as every command does, `HANGAR_TOKEN` included, and needs a
role that may apply custom images, such as `project.editor`. An image of
the name made for another file is refused (exit 4): delete it, or choose
another `--name`.

Once the whole file is in, `Uploaded` is True with the `sha256` the node
computed, and the node converts the file in a converter VM of its own; the
host never reads it. `hangar get cimg nixos -o yaml` then shows `Ready`
True with `diskSize`, the disk the file holds, the smallest a disk of it
can be, and `usedSize`. `Ready` False says why it failed, for good:
`UploadExpired` (not whole in 24 hours), `DigestMismatch` (not the file
whose sha256 was sent), `InvalidImage` (not raw or qcow2, or naming a
backing or data file), `ImageTooLarge` (a disk over 256 GiB),
`QuotaExceeded` (the disk the file holds does not fit the disk quota), or
`ConvertFailed`; delete it and upload again. A file is at most 16 GiB.

The image reserves the file's size toward the disk quota from the moment it
is made; once the node has read the file, the disk it holds if larger,
`virtualSize`, which must fit the project's and the tenant's disk quota
before it is converted; and it counts as `usedSize` once Ready. Make VMs
of it as of a captured image, with the project beside the name.

The upload path is `GET` and `PUT
/v1/projects/PROJECT/customimages/NAME:upload`, for a client of its own:
`GET` answers `{"received_bytes": N, "chunk_size": 67108864, "size": S}`;
each `PUT` sends the bytes from `received_bytes` on, at most `chunk_size`,
with `Content-Range: bytes FIRST-LAST/S` and `X-Hangar-Chunk-SHA256: HEX`,
and answers `{"received_bytes": N}`. Errors are JSON `{"code", "message"}`;
three carry `received_bytes` too, and the chunk can be sent again: 409
`aborted` (not at `received_bytes`; send from there), 503 `unavailable`
with `Retry-After: 1` (another chunk of the upload is being written; wait
and send it again) and 500 `data_loss` (the bytes did not match their
sha256 on arrival; send it again). A 503 or 429 without it, the node not
connected or too many calls open, is worth retrying with a growing wait
too; 400, 401, 403, 404 and 413 are final.

## Distributions and the platform

What the platform needs of an image, and which catalogue image needed what:

- **cloud-init that reads NoCloud from the SMBIOS serial**, which names
  the metadata service as an HTTP seed with `dsmode=net`
  (`docs/decisions/vm-boot.md`). Debian 12's cloud-init 22.4 needs the
  `dsmode`; the others infer it.
- **sshd reading `/etc/ssh/sshd_config.d/*.conf`**, where vendor-data's
  script writes the CA's config (`docs/decisions/vm-ssh.md`). Every
  catalogue image includes it, and the script adds the `Include` if one
  does not. Rocky 9 and AlmaLinux 9 run it under SELinux, enforcing.
- **An EFI boot loader**, through `CLOUDHV.fd` without Secure Boot
  (`docs/decisions/vm-boot.md`). Every catalogue image has an EFI partition.

**Known, not yet handled: Rocky 10 and AlmaLinux 10 are left out.** On
Cloud Hypervisor their guests drop packets the network's namespace sends
with the checksum left to offload: DHCP, metadata, the SSH path. The
guest counts them as `TcpInCsumErrors`, so it never gets an address or
its seed. With the bridge's tx checksum offload turned off by hand
(`ethtool -K hn-br tx off` in the namespace), both passed
`TestTheImageCatalogue` on a testbed (2026-10-20). Nothing in the platform
does that, and the cause has not been looked into.
