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

## Generated code

- [ ] `make gen` ran, and the result is committed: the Go and TypeScript
      messages, the registry entry, and the handler shim.

## Behaviour

- [ ] Admission rules that are specific to the kind are implemented and tested,
      including the rejection paths. A rule that reads other objects, such
      as "no other machine has this disk", holds the kind's `writeLock`
      (and the other kind's, the same lock) from the check to the write, and
      a test races writers to show only one wins.
- [ ] If only the apiserver's controllers write the kind, `hooksFor` marks it
      `systemManaged` and its comment says so; if a node agent reports its
      status, `nodeOf` names the node allowed to. Tests show a caller with the
      broadest role is still refused.

## Docs

- [ ] `docs/architecture.md` mentions the kind if it changes the picture.
