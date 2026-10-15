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
  proves who someone is. A platform member holds a platform role
  (`platform.member`, `platform.sponsor` or `platform.root`, which nest in
  that order) and has signed up. Anyone else may do nothing at all, even
  inside a tenant someone shared with them. Service tokens and root on the
  host are not subject to membership: they hold only what is bound to them,
  or `platform.root`, and need no User.
- **Invitation, then onboarding.** An administrator invites a person by
  binding them a platform role on their email, before they ever sign in.
  The person then signs up once (`hangar signup --username NAME`, later the
  console's onboarding page), which creates their `User` (group `iam`). Until
  then every call refuses them and names `hangar signup`, except
  `AccountService` (`WhoAmI`, `SignUp`, `UpdateProfile`) and checking a
  username with `CheckName`. `WhoAmI` answers anyone signed in, so the
  console can choose between a waiting page, onboarding and the app.
- **A User is a person's account.** Its name is the username they choose:
  a DNS label, global, never reused (like a tenant name), immutable, and not
  a reserved word such as `admin` or `root`. `spec.email` is the email
  Access verified for whoever signed up, never taken from a request and
  never changed. The person's `display_name` is theirs alone, changed
  through `UpdateProfile`: an administrator's apply that leaves it out
  keeps it, and one that sets another is refused. Only `platform.root` sets
  `max_owned_tenants`, the cap on tenants they may own (unset is the
  platform default). Only signing up creates a User, as the apiserver
  itself. Otherwise Users go through ordinary RBAC: their target is the
  platform, so only `platform.root` reads, lists or changes them, and a
  person reads their own through `WhoAmI`. Bindings stay keyed by email,
  so an invitation can come before the User.
- **A User is never deleted.** No one deletes one through the API, not
  `platform.root` and not root on the host: as with the system-managed
  kinds, no role grants it. One email has one User, for good, and its
  username stays taken.
- **Removing someone is deleting their platform binding.** Their
  membership ends and their User stays; inviting them again restores it.
- Members may create tenants, and creating one grants its creator
  `tenant.owner` (a binding owned by the tenant, so it goes with it). A
  `--bootstrap-admin` flag grants `platform.root` once, on an empty store;
  that invites the first administrator, who then signs up like anyone else.
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
