# Adding a resource kind

Go through this when adding a kind. `go test` (the envelope check in
`internal/apimachinery/schemacheck`) catches shape mistakes; this list covers
what it cannot.

## Schema

- [ ] The kind's file is `proto/hangar/<group>/<version>/<kind_snake>.proto`,
      holding the resource message, its `Spec` and `Status`, and its service.
      Copy an existing kind of the same scope; `container.proto` is
      project-scoped, `tenant.proto` is global.
- [ ] `option (hangar.meta.v1.resource)` sets `kind` (the message name), a
      lowercase `plural`, any `short_names`, and `scope`. Kind, plural and
      short names are unique across all kinds, because the CLI resolves them in
      one namespace.
- [ ] The scope is right: `SCOPE_PROJECT` for anything a tenant creates inside
      a project, `SCOPE_GLOBAL` only for platform-wide objects.
- [ ] Fields that must never change after creation carry
      `[(hangar.meta.v1.field).immutable = true]`.
- [ ] References to other objects say in their comment which kind they name.
- [ ] Nothing the system computes is in `Spec`. Derived state goes in `Status`;
      things the system creates on the user's behalf are separate objects with
      an owner reference (see `docs/decisions/apply-semantics.md`).
- [ ] Enums have an `_UNSPECIFIED` zero value, and the code treats it as a
      default or rejects it, explicitly.
- [ ] The comments on the message, its fields and its enum values are
      written for a user of the API: they are what `hangar explain` and the
      served schema show (`docs/decisions/api-schema.md`). A doc they need
      is named by its `docs/` path, which the schema links; nothing names a
      path under `internal/` or `cmd/`.

## Generated code

- [ ] `make gen` ran, and the result is committed: the Go and TypeScript
      messages, the registry entry, the handler shim, and the kind's JSON
      Schema under `gen/go/hangar/schema`, read through as a user would.

## Behaviour

- [ ] Admission rules that are specific to the kind are implemented and tested,
      including the rejection paths. A rule that reads other objects, such
      as "no other machine has this disk", holds the kind's `writeLocks`
      (among them the other kind's, the same lock) from the check to the
      write, and a test races writers to show only one wins. A store
      transaction that writes the kind (`Server.atomically`) holds them
      from before it starts to its end.
- [ ] If only the apiserver's controllers write the kind, `hooksFor` marks it
      `systemManaged` and its comment says so; if a node agent reports its
      status, `nodeOf` names the node allowed to (`anyNodeReports` lets every
      node, for what holds on every node, as an Image's). A global kind every
      tenant reads, as the Image catalogue, sets `readableByAll`. Tests show a
      caller with the broadest role is still refused.
- [ ] If a new object claims something global besides its name, which no
      other object may ever hold, as an endpoint's hostname does,
      `hooksFor` sets `claim`, and a test races creators of it.
- [ ] If the kind holds what node agents may not read (a guest's user-data,
      keys), `forNode` clears it from every answer to a node, and a test
      reads the object by get, list and watch as that node, another node and
      its owner (`docs/decisions/tenancy-and-iam.md`).
- [ ] If the kind holds what no one may read back, as a Secret's values,
      `redact` clears it from every answer, `seal` turns it into what is
      stored, and `hiddenFromNodes` keeps node agents from reading the kind
      at all; tests read it by get, list, watch, apply and delete, and grep
      the database file and the log for a canary
      (`docs/decisions/secrets.md`).

- [ ] A kind that runs as a machine is registered in `machineKinds`
      (`internal/apiserver/machinecapability.go`) with its machine's name and
      the capabilities it has, so logs, exec and ssh reach it under
      `compute.machines.*`; any other capability answers `Unimplemented`
      (`docs/decisions/machine-model.md`).

## Docs

- [ ] `docs/architecture.md` mentions the kind if it changes the picture.
