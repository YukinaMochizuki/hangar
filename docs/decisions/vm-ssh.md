# Logging in to virtual machines: hangar ssh and the platform's SSH CA

Status: accepted, 2026-10.

## Context

A `VirtualMachine` runs its image's own OS (`vm-boot.md`), whose users log
in over SSH. Its addresses are private to its networks, with no inbound
IPv4 from the internet, and the platform runs nothing inside it: what it
gives the guest goes through cloud-init (`vm-boot.md`). People and agents
reach the API through Cloudflare Access (`authentication.md`), and the
platform already relays a container's exec over a WebSocket to its node
(`node-tunnel.md`, `streaming-transport.md`). Static keys in the VM's spec
(`sshAuthorizedKeys`) work, but each must be set up, outlives whoever it
was for, and names no one.

## Decision

- **Short-lived certificates first, static keys second.** `hangar ssh
  vm NAME [-- COMMAND]` makes a key for this login alone, has the apiserver
  sign it, and runs OpenSSH's `ssh` with it: no key to set up. A VM's
  `sshAuthorizedKeys` keep working beside it.
- **One platform SSH CA.** Its private key is a file the apiserver alone
  reads, `/var/lib/hangar/state/ssh-ca`, mode 0600, kept in the vault and
  installed by hand (`docs/runbook/host-bootstrap.md`), not in SQLite; it
  never leaves the apiserver, nor is it logged. Without it the apiserver
  runs, signing fails saying so, and static keys work. Per-project CAs were
  rejected for now: their keys would need envelope encryption, which the
  platform does not have yet.
- **The certificate.** `SignSSHKey` (`VirtualMachineSSHService`) needs
  `compute.virtualmachines.ssh`, which `tenant.owner` and `platform.root`
  hold, and certifies a user key for one principal, the VM's uid, for five
  minutes (from one minute back, for a guest's clock), with the caller's
  identity as its key ID, and every extension but agent forwarding. It is
  for logging in: the session outlives it. The apiserver logs each
  signing, by key fingerprint, never the key.
- **The guest trusts the CA for its own uid.** The VM's vendor-data
  (`vm-boot.md`) carries, with a CA, a script beside its keys' cloud-config:
  it writes the CA's public key as `TrustedUserCAKeys`, and an
  `AuthorizedPrincipalsCommand` that gives the VM's uid for every user but
  root, so a certificate for this VM logs in as any of its users, and as no
  other VM's. A script and not cloud-config, because cloud-init's user-data
  replaces vendor-data's lists, `write_files` among them; a user-data can
  still turn vendor-data off. It runs once, as cloud-init's first boot
  ends, and leaves sshd as it was if `sshd -t` refuses the config. A guest
  takes the CA on its first boot only: a VM made without one, or under
  another, does not trust the new one.
- **Which user.** The image's `defaultUser` (`Image.spec.default_user`,
  such as `ubuntu`), or `hangar ssh -l USER`. A certificate is good for any
  user but root: whoever may log in to the VM may become any of its users
  through the default user's sudo anyway. Root's login stays what the
  image's own keys and sshd say.
- **The path in.** `hangar ssh-proxy vm NAME`, ssh's `ProxyCommand` (usable
  in an ssh_config too), relays its stdin and stdout over a WebSocket,
  `GET /v1/projects/{project}/virtualmachines/{name}:ssh` with the
  subprotocol `hangar.ssh.v1alpha1`, through Access to the apiserver, which
  checks the same permission and calls the VM's node over the tunnel
  (`SSH` on the node service). The node dials port 22 of the VM's first
  interface's address, from that network's gateway inside its namespace.
  The caller names a VM, never an address or a port: nothing else is
  reached, and no port of the host or the VM is opened.
- **Security groups do not apply.** The platform's own SSH path passes
  whatever the VM's groups say, as AWS SSM's does: one rule in each
  network's firewall lets TCP from the gateway's own address to a tap's
  port 22 through (`networking.md`). Only the namespace's own sockets send
  from that address; a guest cannot (the binding). RBAC and the
  minutes-long certificate are the gate.
- **Host keys on first use.** There are no host certificates yet. `hangar
  ssh` records a VM's host key on its first login in `known_hosts` beside
  the CLI's config (`~/.config/hangar/known_hosts`), under the VM's uid. A
  VM made again has a new uid, and so a new entry. A key that changes under
  the same uid, its guest's host keys made again in it or something else
  answering, is refused, naming `hangar ssh --forget-host-key vm NAME`.

## Consequences

- `hangar ssh` needs OpenSSH's `ssh` on the client.
- A VM's SSH comes up a few seconds after its cloud-init's first boot ends,
  not when the VM is Ready.
- A VM made again is a new entry in `known_hosts`; the old one's stays
  there, unused.
- Rotating the CA leaves existing VMs on the old one.
- No Windows guests, nor host certificates, yet.
