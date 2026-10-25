# A VM's TPM

Status: accepted, 2026-10.

## Context

Some guests want a TPM 2.0: Windows 11, disk encryption sealed to it
(systemd-cryptenroll, Clevis), and keys an application keeps in it. Cloud
Hypervisor (v53, as pinned) gives a guest a TPM 2.0 with a CRB interface
that it serves from swtpm, a separate process it talks to over a socket
(`--tpm socket=`). The TPM's state is a file swtpm writes, about 2 KiB.

## Decision

- **`spec.tpm.enabled` on a VirtualMachine, set when it is created.**
  Every field of `spec.tpm` is immutable: a TPM cannot be added, removed
  or replaced later. `spec.tpm.source` is reserved for starting a TPM from
  an existing state; nothing reads it yet, and a VM that sets it is
  refused.
- **The state belongs to the VM.** It is kept on the VM's node, across
  stops, restarts and host reboots, and deleted with the VM. It is never
  part of a CustomImage or a snapshot: it lives beside the VM's disks, not
  on them. A VM made again under the same name gets a new, empty TPM.
- **The state is encrypted with a key of its own**, 32 random bytes the
  apiserver makes when the VM is created and seals under the platform's
  KEK, as a Secret's data is (`docs/decisions/secrets.md`), bound to the
  VM's project, name and uid. The sealed key is kept in the VM's own
  `spec.tpm.sealed_key`, which the apiserver strips from every answer, to
  users and nodes alike, by get, list, watch and apply; a caller who
  writes one is refused. A field on the VM, not a Secret: it is made with
  the VM, deleted with it, never shared, and has no one who should read
  it. An apiserver without a KEK refuses a VM with a TPM, and refuses to
  start over one; the startup pass that seals Secrets again under a new
  KEK does the same for TPM keys.
- **The node gets the key for each start of the VM**, through the node
  socket's `VirtualTPMKeyService.ResolveVirtualTPMKey`, which answers only
  a node the apiserver runs, only for a machine with a TPM placed on that
  node, and only with the key of the VM whose uid controls that machine.
  It logs that it gave a key, never the key. The agent writes it to
  `/run/hangar/vtpm/<machine uid>`, root's 0600, before it starts the VM,
  and removes it once the VM is stopped or released; the sweep removes
  any no machine accounts for. A reboot empties `/run`, and the agent
  fetches the key again. The apiserver unreachable, the VM does not
  start, and the agent tries again as it does for anything else it needs.
- **swtpm is Hangar's own build**, from pinned Alpine packages, run by its
  own static musl loader from `/usr/local/lib/hangar/swtpm`
  (`docs/runbook/swtpm.md`). A VM's unit has an empty root and takes only
  static executables, and Ubuntu's AppArmor profile for `/usr/bin/swtpm`
  would refuse the VM's directory. The bundle's path is not that one, so
  the profile never attaches.
- **swtpm runs in the VM's unit, as the VM's user, with no capability**,
  as Cloud Hypervisor does (`docs/decisions/node-agent.md`). The launcher
  reads the key from the unit's credential (`LoadCredential=`) and hands
  it to swtpm on a pipe; the file swtpm could open never holds it. The
  state is in the VM's directory, `tpm/`, the VM's user's while it runs
  and root's while it is stopped, since a stopped VM's user is given to
  the next VM.

## Consequences

- **No measured boot.** The pinned firmware, `CLOUDHV.fd` of
  cloud-hypervisor/edk2 `ch-97eeb7b09`, is built without TPM support
  (`TPM2_ENABLE = FALSE`), so it measures nothing: PCRs 0 to 7 stay zero
  and there is no event log. The TPM can hold keys and seal data to its
  own auth, not to the boot chain. Measured boot needs a firmware built
  with `-D TPM2_ENABLE=TRUE`, which nobody has tested on Cloud Hypervisor.
- **The state is keyed by the machine's uid.** Only the apiserver writes
  Machines, one per VM for the VM's life. A new one for a live VM takes an
  administrator removing it from the store by hand, or a move to another
  node, which the platform does not do yet. Either gives the VM a new,
  empty TPM, and the guest loses what it sealed. Users cannot cause it.
- **swtpm and Cloud Hypervisor share a uid.** Someone who takes over Cloud
  Hypervisor can read and rewrite the TPM's state: encrypted, but they can
  put back an older copy, so the guest sees its TPM roll back. They already
  have the guest's memory and disks, so this adds little. A uid of its own
  for swtpm would remove it; it is not done.

## Known issues

Both are Cloud Hypervisor's (v53, and main as of 2026-10). Their fix, a
patched Cloud Hypervisor, is tracked in hangar#212.

- **A slow reply locks the guest's TPM.** Cloud Hypervisor waits 100 ms
  for swtpm's answer (cloud-hypervisor#9053). A slower one fails the
  command, and the guest's TPM then fails every command for the rest of
  that boot (`tpm_recv: error -5`); only a guest reboot recovers it. A
  guest that seals its disk key or its identity to the TPM can't afford
  that. An RSA-2048 `CreatePrimary` took 27 to 122 ms on an idle testbed,
  so it happens with no load at all; ECC keys and NV take a few
  milliseconds.
- **Cloud Hypervisor leaks a file descriptor into swtpm** with every
  control command after the first, 7 per guest boot. swtpm's
  RLIMIT_NOFILE is the host's `nr_open`, about a million, so a VM runs
  out only after some 150,000 reboots without a stop.
