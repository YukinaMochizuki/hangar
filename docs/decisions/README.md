# Decisions

One file per decision: the context that forced it, what was decided, and what
follows from it. Read the relevant one before proposing to change something it
covers; if the context has genuinely changed, update the file in the same PR as
the change.

| File | Decision |
|---|---|
| `monorepo.md` | Everything in one repo and one Go module |
| `machine-model.md` | Containers and VMs are both microVMs; one internal `Machine` |
| `storage-zfs.md` | ZFS zvols for machine disks, NFS for shared files, no virtio-fs |
| `api-versioning.md` | Proto packages and `apiVersion` are per group, mapped 1:1 |
| `api-rpc-shape.md` | One typed service per kind, buf STANDARD request/response wrappers |
| `apply-semantics.md` | Apply replaces the spec; field ownership; status subresource |
| `resource-codegen.md` | Resources are declared in proto; codegen and a test do the rest |
| `sqlite-store.md` | SQLite with a global revision and a replayable event log |
| `buf-breaking-wire-json.md` | WIRE_JSON breaking checks are enforced even in alpha |
| `tenancy-and-iam.md` | Tenant and Project; permissions, roles and bindings |
| `authentication.md` | Cloudflare Access for people and agents; peer credentials for the node |
| `networking.md` | Per-network namespaces, security groups, routed IPv6, one tunnel |
| `streaming-transport.md` | Bidirectional streams over WebSocket |
| `testing-strategy.md` | Test layers, fakes held honest by conformance tests, a disposable testbed |
| `node-agent.md` | The node agent keeps no state, reads the host back, and sweeps what nothing accounts for |
