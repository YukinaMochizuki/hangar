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
  before it, as a PublicEndpoint's hostname does. The one name the server
  makes is that of the tenant and project signing up opens (below): signing
  up is no apply, and happens once. Tenant and project names are claimed
  apart, by kind, so one name serves both.
- **Deleting a tenant or project deletes everything in it.** It is marked as
  being deleted, its contents (projects, resources, role bindings scoped to it)
  are deleted in the background, and only then does it go. A grace period
  before anything is deleted may come later.
- **Permissions, roles, bindings** (GCP IAM style):
  - Permissions are `<group>.<plural>.<verb>`, generated from resource
    declarations (`get`, `list`, `watch`, `apply`, `delete`, `updateStatus`);
    special actions are declared explicitly. Logs, exec and ssh are
    capabilities of the machine an object runs as, whatever its kind:
    `compute.machines.logs`, `compute.machines.exec` and
    `compute.machines.ssh`, checked in the project of the machine behind
    the Container or VirtualMachine the caller names. With them goes
    `network.publicendpoints.access`, using a private public endpoint
    (`public-endpoints.md`). The owner and editor roles and `platform.root`
    hold these reach permissions (`machine-model.md`, `node-tunnel.md`,
    `vm-ssh.md`). Holding them is no write to the Machine, which stays
    system-managed.
  - Roles bundle permissions, and a check compares the permission asked
    for with every bound role's, never a role's name. They are built in
    for now and may become resources later. Below the platform they follow
    GCP's basic roles:
    - `tenant.owner` holds everything in the tenant.
    - `tenant.viewer` reads everything in the tenant: get, list and watch,
      without logs, exec, ssh or endpoint access, since output can hold
      what the spec does not.
    - `project.owner` holds what `tenant.owner` does, in one project,
      including its RoleBindings and deleting the project itself, as a GCP
      project owner may.
    - `project.editor` changes what runs in the project, with logs, exec
      and ssh, which agents need. It does not change who may: it writes no
      RoleBinding, and no ServiceToken, since creating or rotating one
      hands out its secret and with it whatever the token's bindings grant,
      an owner's included. It reads both. Nor does it write the Project
      itself, since deleting it takes everything in it and its name for
      good.
    - `project.viewer` reads the project.

    A role names the one scope level it can be bound at, so whoever may
    write bindings in a project can grant only project roles there.
  - A `RoleBinding` (group `iam`) grants a role to a principal at a scope:
    the platform, a tenant, or a project. The authorizer caches bindings and
    keeps them current through a watch. Its subject is a person's email or
    a service token as `<project>/<name>` (`authentication.md`). A binding
    naming a token can be made only while the token exists, and only by
    someone who may read it; any other token is refused as one that does
    not exist, so a binding reveals no other tenant's tokens. The binding
    is owned by the token as well as by its scope, and cannot be changed
    to name anyone else: deleting the token deletes the binding, and the
    token's own deletion waits for it, so a later token of the same name
    starts with no bindings. Making such a binding, from its check to its
    write, and the token's cascade, from looking for its bindings to
    letting it go, hold one lock per token, so no binding is written
    unseen while its token goes. Its owner reference names the token's
    project, since the binding is global.
- **Platform membership gates everything.** Signing in (through Google,
  `authentication.md`) only proves who someone is, and only someone invited
  may sign in at all. A platform member holds a platform role
  (`platform.member`, `platform.sponsor` or `platform.root`, which nest in
  that order) and has signed up. Anyone else may do nothing at all, even
  inside a tenant someone shared with them. Service tokens and root on the
  host are not subject to membership: they hold only what is bound to them,
  or `platform.root`, and need no User.
- **Invitation, then onboarding.** An administrator invites a person by
  binding them a platform role on their email, before they ever sign in.
  With it goes an `Invitation` (group `iam`, global, named by the email in
  lower case), which holds what their User starts with: the `plan` their
  tenants get and `max_owned_tenants`, both optional, the plan one that
  exists and the cap at least 1. `hangar admin invite EMAIL [--plan P]
  [--max-tenants N] [--role R]` makes both, `hangar admin invitations`
  lists them and `hangar admin revoke EMAIL` deletes both. Only
  `platform.root` reads and writes Invitations; there is none for an email
  that has a User. The person then signs up once (`hangar signup --username
  NAME`, later the console's onboarding page), which creates their `User`
  (group `iam`), with their Invitation's values, and deletes the
  Invitation, so every Invitation is one not taken up yet. Until
  then every call refuses them and names `hangar signup`, except
  `AccountService` (`WhoAmI`, `SignUp`, `UpdateProfile`) and checking a
  username with `CheckName`. `WhoAmI` answers anyone signed in, so the
  console can choose between a waiting page, onboarding and the app.
- **Signing up leaves a person ready to work.** In the store transaction
  that creates the User, signing up opens a tenant the person owns (their
  `tenant.owner` binding, and `opened-by` naming them, so it counts toward
  their cap) with their plan, a project in it, and the project's `default`
  network; all of them commit, or none does. Tenant and project are both
  named `<username>-<5 characters>`, the characters from lowercase letters
  and digits but `0 o 1 l i`, drawn again while either name has ever been
  used; the username is cut to 20 characters, as `hangar create`'s slugs
  are. Their display names are the username and `Default`. `hangar signup`
  then makes the project the context's. Every write but the apiserver's
  own waits for the transaction, and the transaction holds the locks its
  writes take from before it starts, so no check outside it reads the store
  as it was without them.
- **A User is a person's account.** Its name is the username they choose:
  a DNS label, global, never reused (like a tenant name), immutable, and not
  a reserved word such as `admin` or `root`. `spec.email` is the email
  the identity provider verified for whoever signed up, never taken from a request and
  never changed. The person's `display_name` is theirs alone, changed
  through `UpdateProfile`: an administrator's apply that leaves it out
  keeps it, and one that sets another is refused. Only `platform.root` sets
  `max_owned_tenants`, the cap on tenants they may own (unset is the
  platform default), and `plan`, the Plan every tenant they open gets as
  its `spec.plan` when it is created (unset is the default plan); changing
  it changes no tenant already open. A person may name their own plan on a
  tenant they open, and no other. An apply of a tenant that leaves
  `spec.plan` empty keeps the stored plan, whoever applies it, so a
  manifest applies again unchanged; one that names another is refused but
  to `platform.root`, who moves a tenant back to the default plan by
  naming it, `plan: default` (`apply-semantics.md`). Only signing up
  creates a User, as the apiserver itself. Otherwise Users go through
  ordinary RBAC: their target is the platform, so only `platform.root`
  reads, lists or changes them, and a person reads their own through
  `WhoAmI`. Bindings stay keyed by email, so an invitation can come before
  the User.
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
- A tenant's `plan` is set by platform administrators only, but for the
  plan a new tenant takes from its opener's User; leaving it out of an
  apply changes nothing.
- `Update<Kind>Status` is granted by no role, not even `platform.root`: only
  the node and the apiserver itself write status. A node agent writes the
  status of its own `Node` and of the machines placed on it, nothing else;
  every other status is the apiserver's controllers'.
- Node agents read only part of some objects. A `Machine`'s cloud-init data
  (its guest's user-data and keys) reaches only the node it is on, which
  serves it to the guest; a `VirtualMachine`'s `userData` and
  `sshAuthorizedKeys` reach no node. Every answer to a node is cleared so,
  by its kind's `forNode` hook (`internal/apiserver/admission.go`). Node
  agents read no `Secret` at all, not even its metadata; a node gets a
  registry login only through the node-only RPC, for a machine placed on
  it (`secrets.md`). Nor do they read any `User` or `Invitation`, which
  hold people's emails and which no node needs.
- A `Secret`'s permissions are the generic ones, so the owner and editor
  roles (and `platform.root`) read its metadata and write it, and the viewer
  roles read its metadata; nobody, of any role, reads its values back.
  Naming a secret in a container needs no permission on the secret:
  whoever may apply a container in a project may name any secret in it,
  as in Kubernetes (`secrets.md`).
- No role grants apply or delete on system-managed kinds (`Machine`, `Node`);
  see `docs/decisions/apply-semantics.md`.
- **Sponsors** (people who fund the hardware) get `platform.sponsor`: aggregated
  platform data only (node capacity and utilization, per-tenant usage totals,
  cost and traffic), never the contents of other tenants' resources.
- **More quota is not a permission.** A tenant references a `Plan` (a
  platform-level resource defining quota limits), and a sponsor's tenant gets a
  larger plan. See "Quotas" below.
- Platform roles, from most to least powerful: root, maintainer (a future
  operations role whose access to tenant details is audited), sponsor, regular
  user.

### Quotas

The tenant is the team, as on DigitalOcean and Fly, so the budget is the
tenant's: what all its projects together hold, against its Plan.

- `Plan` is a global kind only platform administrators read and write. Its
  `spec.limits` caps cpu, memory, disk, machines, projects, networks,
  addresses, public endpoints and service tokens; a field left unset has no
  limit. A tenant with an empty `spec.plan` has the plan `default`, a
  trial's, which the apiserver makes the first time it starts on a store,
  with 8 cpu, 16Gi memory, 100Gi disk, 10 machines, 5 projects, 5
  networks, 5 addresses, 10 public endpoints and 10 service tokens. The
  default limits are data from then on: the administrator changes them
  with apply. Paying users get a larger plan, through their Invitation or
  their User. The default plan, and a plan a tenant, a User or an
  Invitation names, cannot be deleted.
- A tenant owner may give a project `spec.limits`, a budget inside the
  tenant's, such as one to cap an agent's project. It may not be above the
  tenant's limits, and the limits that hold for a project are the smaller of
  the two. Changing it needs apply on projects at the tenant (a grant on the
  tenant or the platform), so a role held on the project alone cannot raise
  its own budget, however broad.
- Containers and VMs count alike, as the machines they become, with the
  defaults and the rounding their controllers apply: what the node is asked
  for. cpu and memory count only machines asked to run; a stopped machine
  counts its disk and its place among machines (the AWS and GCP model), so
  starting one is checked and can be refused. Disk is containers' disks,
  Disks, and the root disks VMs have made from images. Such a root disk
  counts once, by its VM: the size the VM asks for, or, once the Disk
  exists, the larger of that and the Disk's spec and status sizes. So
  shrinking the VM's size frees nothing the disk still holds, and growing
  the Disk itself raises the VM's count and is checked. A project counts its
  `default` network from the moment it is created.
- Admission refuses, with `ResourceExhausted`, a write that would take a
  tenant or project past a limit, naming the limit, its value, the usage and
  what the write would bring it to. Only what the write raises is checked:
  a tenant over a lowered limit can still shrink and change anything else.
  An update counts as what it stores, with the stored uid, owner references
  and reserved annotations, so a manifest applied again unchanged raises
  nothing.
- The apiserver's own writes are not checked; what they add was already
  counted at the user's write that caused them. The default network counts
  from its project's creation. A VM's root disk counts from the VM's
  creation, at the VM's size, which the VM controller makes the Disk at.
- A tenant's `status` holds its plan, the plan's limits and its usage; a
  project's holds the limits that hold for it and its usage. A controller
  writes them, for people to read; admission counts from the store itself.
- The real per-account limit is the number of tenants one person opens,
  because every tenant gets a plan of its own. Each tenant records who
  opened it in the reserved annotation `hangarlab.cloud/opened-by`, which
  the apiserver sets at creation and no one else can set, change or drop;
  the tenant's bindings stay free to change. The live tenants someone opened
  are capped by their User's `spec.maxOwnedTenants`, which only
  `platform.root` sets, or else by the apiserver flag
  `--default-max-owned-tenants` (1, the tenant signing up opens), which
  also caps service tokens.
  Platform root is not capped. The cap is not a Plan field: a Plan is one
  tenant's budget.
- **Concurrent writes cannot together overshoot a limit** because every write
  that can raise a tenant's usage, but the apiserver's own, holds one
  apiserver-wide mutex (`Server.quota`) from counting the tenant's usage in
  the store to the write. This holds only while a single apiserver process
  writes the store. Running more than one apiserver needs another way, such
  as counting and writing in one store transaction, before it ships.
- The lock has a cost: every such write counts its tenant twice (before and
  after), listing each counted kind in each of the tenant's projects, and
  the writes are serialized across all tenants, not per tenant. That is
  cheap at the platform's size today; a tenant with many projects, or many
  tenants writing at once, makes it the place to look first, with a lock per
  tenant (the tenant-count cap would then need one per opener) as the next
  step.

## Consequences

Adding a role is data, not code. Visibility and quota can be granted
independently.
