# Tenancy and authorization

Status: accepted, 2026-10.

## Context

The first users are a few friends, but Hangar is meant to grow into a public
cloud, and some people fund the hardware and should see more of it.

## Decision

- **Two levels.** A `Tenant` is who: members, identity, later billing. A
  `Project` is where resources live: quotas, networks, boundaries. A project
  records its tenant, which is immutable.
- **Tenant and project names are IDs, never reused.** Like GCP project IDs,
  they are chosen by the user, globally unique, and stay claimed after the
  object is deleted, so a name can never come to mean someone else's object.
  What people read is `display_name`, which is free-form, need not be unique,
  and can change. Tooling may suggest IDs with a numeric suffix.
- **Deleting a tenant or project deletes everything in it.** It is marked as
  being deleted, its contents (projects, resources, role bindings scoped to it)
  are deleted in the background, and only then does it go. A grace period
  before anything is deleted may come later.
- **Permissions, roles, bindings** (GCP IAM style):
  - Permissions are `<group>.<plural>.<verb>`, generated from resource
    declarations (`get`, `list`, `watch`, `apply`, `delete`, `updateStatus`);
    special actions such as `exec` are declared explicitly.
  - Roles bundle permissions. They are built in for now (`platform.root`,
    `platform.sponsor`, `tenant.owner`) and may become resources later.
  - A `RoleBinding` (group `iam`) grants a role to a principal at a scope:
    the platform, a tenant, or a project. The authorizer caches bindings and
    keeps them current through a watch.
- **Platform membership gates everything.** Signing in through Access only
  proves who someone is. A person with no platform role (`platform.member`,
  `platform.sponsor` or `platform.root`, which nest in that order) may do
  nothing at all, even inside a tenant someone shared with them; the console
  will show them a waiting page. Membership is a platform-scoped RoleBinding
  on their email, so an administrator can add people before they ever sign
  in. Service tokens are not subject to membership: they hold only what is
  bound to them.
- Members may create tenants, and creating one grants its creator
  `tenant.owner` (a binding owned by the tenant, so it goes with it). A
  `--bootstrap-admin` flag grants `platform.root` once, on an empty store.
- People are not resources: an identity is the email Access verified, as in
  GCP IAM. A `User` kind for profiles, keys and join requests may come with
  the console; bindings stay keyed by email so pre-binding keeps working.
- Roles carry the scope level they can be bound at, so a tenant owner cannot
  bind a platform role at all, and can grant roles only inside their tenant.
  A binding's scope is immutable: it belongs to that tenant or project.
- A tenant's `plan` is set by platform administrators only.
- `Update<Kind>Status` is granted by no role, not even `platform.root`: only
  the node and the apiserver itself write status. A node agent writes the
  status of its own `Node` and of the machines placed on it, nothing else;
  every other status is the apiserver's controllers'.
- No role grants apply or delete on system-managed kinds (`Machine`, `Node`);
  see `docs/decisions/apply-semantics.md`.
- **Sponsors** (people who fund the hardware) get `platform.sponsor`: aggregated
  platform data only (node capacity and utilization, per-tenant usage totals,
  cost and traffic), never the contents of other tenants' resources.
- **More quota is not a permission.** A tenant references a `Plan` (a
  platform-level resource defining quota limits), and a sponsor's tenant gets a
  larger plan. `Tenant.spec.plan` exists from the start; plans and quotas are
  enforced later.
- Platform roles, from most to least powerful: root, maintainer (a future
  operations role whose access to tenant details is audited), sponsor, regular
  user.

## Consequences

Adding a role is data, not code. Visibility and quota can be granted
independently.
