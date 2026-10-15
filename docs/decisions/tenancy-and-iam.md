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
  and can change.
- **Clients suggest IDs; the server only checks them.** As the GCP console
  does, `hangar create` (and later the console) makes an ID from the display
  name and six random digits, such as `my-demo-438212`, and asks
  `NameService.CheckName` whether it is free: no object has it now and none
  ever had it. The check needs only platform membership and says nothing of
  who holds a name. The server does not generate names, because apply finds
  an object by its name: a generated one would make every apply of the same
  file a new object. A name taken between the check and the create fails
  the create, or, when the caller may change the object that took it, turns
  it into an update, which `hangar create` reports as an error. The slug is cut to 20
  characters, so an ID leaves room in a hostname label for what is put
  before it, as a PublicEndpoint's hostname does.
- **Deleting a tenant or project deletes everything in it.** It is marked as
  being deleted, its contents (projects, resources, role bindings scoped to it)
  are deleted in the background, and only then does it go. A grace period
  before anything is deleted may come later.
- **Permissions, roles, bindings** (GCP IAM style):
  - Permissions are `<group>.<plural>.<verb>`, generated from resource
    declarations (`get`, `list`, `watch`, `apply`, `delete`, `updateStatus`);
    special actions are declared explicitly, such as
    `compute.containers.logs` and `compute.containers.exec`,
    `compute.virtualmachines.logs` and `compute.virtualmachines.ssh`, which
    `tenant.owner` and `platform.root` hold (`node-tunnel.md`, `vm-ssh.md`).
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
- Node agents read only part of some objects. A `Machine`'s cloud-init data
  (its guest's user-data and keys) reaches only the node it is on, which
  serves it to the guest; a `VirtualMachine`'s `userData` and
  `sshAuthorizedKeys` reach no node. Every answer to a node is cleared so,
  by its kind's `forNode` hook (`internal/apiserver/admission.go`).
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
