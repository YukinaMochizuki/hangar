# Decisions

One file per decision: the context that forced it, what was decided, and what
follows from it. Read the relevant one before proposing to change something it
covers; if the context has genuinely changed, update the file in the same PR as
the change.

| File | Decision |
|---|---|
| `monorepo.md` | Everything in one repo and one Go module |
| `machine-model.md` | Containers and VMs are both microVMs; one internal `Machine` |
| `machine-placement.md` | A container's or VM's status carries its node's `Placement`, the host's hardware and nothing of its load |
| `storage-zfs.md` | ZFS zvols for machine disks, NFS for shared files, no virtio-fs |
| `file-shares.md` | `FileShare`: NFSv4.2 served by a server microVM per share from its own zvol, on one to eight networks; only declaring machines mount it, and the server can only reply |
| `container-volumes.md` | A container mounts Disks as volumes in Compose's long syntax, formatted and filled from the image in the guest; a Disk's status says what it is used as |
| `vm-images.md` | A platform catalogue of disk images, fetched by URL and sha256 and written raw onto a base |
| `custom-images.md` | `CustomImage`: a project's own disk image, captured from a stopped Disk as an independent copy on its node, held to the tenant's disk quota by what it holds |
| `machine-sizes.md` | A machine's shape is held to rules (256Mi steps of memory, 512Mi to 8Gi of it per vCPU, a disk of 2Gi or more); `MachineSize`s are the administrator's presets, read by all, never recorded on a machine |
| `api-versioning.md` | Proto packages and `apiVersion` are per group, mapped 1:1 |
| `api-rpc-shape.md` | One typed service per kind, buf STANDARD request/response wrappers |
| `apply-semantics.md` | Apply replaces the spec; field ownership; status subresource |
| `resource-codegen.md` | Resources are declared in proto; codegen and a test do the rest |
| `api-schema.md` | `DiscoveryService` serves the kinds and each kind's JSON Schema, generated from proto with its comments, docs named in them as links at the server's revision; `hangar api-resources` and `hangar explain` read it |
| `sqlite-store.md` | SQLite with a global revision and a replayable event log |
| `buf-breaking-wire-json.md` | WIRE_JSON breaking checks are enforced even in alpha |
| `tenancy-and-iam.md` | Tenant and Project; permissions, roles and bindings |
| `authentication.md` | Sign-in through Google and Hangar's own sessions and tokens for people and agents; peer credentials for the node, the edge and root on the host |
| `cloudflare-setup.md` | Cloudflare is DNS alone: DNS-only records at the host, converged by `hangar-cloudflare`, which changes only what its state file records |
| `networking.md` | Per-network namespaces, security groups, routed IPv6; networks reach only the front door's ports of the host |
| `streaming-transport.md` | Bidirectional streams over WebSocket |
| `testing-strategy.md` | Test layers, fakes held honest by conformance tests, a disposable testbed |
| `node-agent.md` | The node agent keeps no state, reads the host back, and sweeps what nothing accounts for |
| `guest-agent.md` | hangar-init: PID 1 from the initramfs, a boot disk, Connect over vsock, a framed console |
| `node-tunnel.md` | The apiserver calls a node over a tunnel the node opens; a container's logs are one stream across its boots; an exec runs as `docker exec` does, as long as its call |
| `guest-kernel.md` | One platform guest kernel and initramfs for containers: LTS only, allnoconfig + fragment, no modules, `quiet` |
| `vm-boot.md` | Virtual machines boot their image's own kernel through EDK2 firmware; serial console as log, power button to stop, no restart policy |
| `secrets.md` | Secrets are write-only and sealed at rest under a KEK file; nodes get a login per pull; a private image is shared only after the machine's own resolve |
| `serial-console.md` | A VM's serial console is interactive: its helper, as the VM's user, is Cloud Hypervisor's one serial client, logs as before, keeps the screen, and serves clients, many watching and one typing |
| `vm-ssh.md` | `hangar ssh`: minutes-long certificates of one platform CA for the VM's uid, over a WebSocket to port 22 from the network's gateway |
| `front-door.md` | `hangar-edge` is the only listener on :443 and :80, bound by systemd, with certificates over ACME DNS-01; routes by Host to the API, the console and the endpoints; per-client limits; a host firewall that opens 22, 80 and 443 alone |
| `public-endpoints.md` | `PublicEndpoint`: `<endpoint>-<project>.hangarlab.dev`, claimed for good; an unprivileged `hangar-edge` signs private ones' callers in itself and reaches machines through the node's dial socket |
| `monitoring.md` | Prometheus and Alertmanager on the host, on loopback; alerts to one Telegram chat; services serve `/metrics` on a loopback listener of their own with no tenant in a label; the console shows the operator fixed queries through `MonitoringService` |
| `console-design.md` | The web console: its look, gating by `WhoAmI`, the session cookie and the Origin check as its only credential and CSRF defence, a strict CSP, and its files shipped in the release and served by `hangar-edge` |
