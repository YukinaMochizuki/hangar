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
