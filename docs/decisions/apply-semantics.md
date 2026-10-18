# Apply semantics and field ownership

Status: accepted, 2026-10.

## Context

The API is declarative: imperative CLI commands also end up building a
resource and applying it. Several writers touch an object: users, in-process
controllers, and the node, which runs as a separate process.

## Decision

- **Apply is an upsert that replaces the whole `spec`.** The spec is the desired
  state. There is no server-side apply or field-level merge.
- **Labels and annotations** belong to the user and are replaced too, except keys
  under any `*.hangarlab.cloud/` prefix. Those are reserved for the system: a user
  setting one is rejected, and system-set ones survive a user's apply.
  Hangar's own clients write their keys under `hangar/`, such as
  `hangar/intended-use` (`container-volumes.md`): the user's, as any
  other key, and set on the user's behalf.
- **Finalizers and owner references** belong to the system; their values in an
  apply are ignored. The system sets them on create: everything in a project
  is owned by the project, a project by its tenant, and a role binding by the
  tenant or project it is scoped to. Tenants and projects carry the
  `hangarlab.cloud/dependents` finalizer, so deleting one deletes its contents
  first. `uid` (UUIDv7), `create_time`, `resource_version` and
  `generation` are system-generated.
- **`generation`** increments only when the spec actually changes. Controllers
  report `status.observed_generation`, which is how `hangar wait` knows a change
  has taken effect.
- **Optimistic concurrency**: a write carrying `resource_version` is a
  compare-and-swap (`Aborted` on mismatch); one without it is last-write-wins,
  which keeps `apply -f` as convenient as kubectl.
- **Create-only**: every `Apply<Kind>Request` has `create_only`
  (`hangar apply --create-only`). With it, an object that exists is refused
  with `AlreadyExists` and nothing changes, so a caller that picked a name
  never replaces what someone else made under it; controllers create the
  same way. It is still an apply to RBAC, with no permission of its own, and
  a caller who may not write the existing object is refused for that,
  exactly as a plain apply would refuse them, before anything says the name
  is taken. It takes no `resource_version` (`InvalidArgument`).
- **Immutable fields** are declared in proto with
  `(hangar.meta.v1.field).immutable = true` and enforced by generic admission.
- **Names** are DNS-1123 labels (lowercase alphanumerics and `-`, at most 63
  characters), because they end up in hostnames and internal DNS. An
  `Invitation`'s is an email instead, as it never reaches a hostname.
- **Two spec fields survive an apply that leaves them empty**, because
  someone other than the applier owns them: a `User`'s `display_name`,
  which is the person's own, keeps its value when an administrator's apply
  leaves it out; and a `Tenant`'s `plan`, which is the platform's, keeps
  its value when anyone, platform root included, applies the tenant
  without one. Setting either to another value is still refused to all but
  their owner, and a tenant goes back to the default plan only by naming
  it, `plan: default`, which is stored as no plan
  (`docs/decisions/tenancy-and-iam.md`).
- **Delete** with finalizers present only sets `delete_time`; the object goes
  away when controllers have removed every finalizer. Children found through
  owner references (matched by uid, not name) are deleted by a background
  garbage collector, which also sweeps everything periodically.
- **Status is a subresource.** Every kind has an `Update<Kind>Status` RPC that
  only system principals (the node, controllers) may call. Users can never
  write status, and system writers never write spec.
- **System-managed kinds** (`Machine`, `Node`) have their spec written only by
  the apiserver's own controllers. People may at most read them.

## Consequences

Things the system creates on a user's behalf are separate objects with an owner
reference, never fields injected into the user's spec. The one exception is a
new tenant's `plan`, which comes from its opener's `User`; it survives the
opener's later applies as above, so their manifest applies again unchanged.
