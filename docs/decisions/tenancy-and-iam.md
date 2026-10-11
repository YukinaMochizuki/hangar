# Tenancy and authorization

Status: accepted, 2026-10.

## Context

The first users are a few friends, but Hangar is meant to grow into a public
cloud, and some people fund the hardware and should see more of it.

## Decision

- **Two levels.** A `Tenant` is who: members, identity, later billing. A
  `Project` is where resources live: quotas, networks, boundaries. Project
  names are globally unique and record their tenant, which is immutable.
- **Permissions, roles, bindings** (GCP IAM style):
  - Permissions are `<group>.<plural>.<verb>`, generated from resource
    declarations (`get`, `list`, `watch`, `apply`, `delete`, `updateStatus`);
    special actions such as `exec` are declared explicitly.
  - Roles bundle permissions. They are built in for now (`platform.root`,
    `platform.sponsor`, `tenant.owner`) and may become resources later.
  - A `RoleBinding` (group `iam`) grants a role to a principal at a scope:
    the platform, a tenant, or a project. The authorizer caches bindings and
    keeps them current through a watch.
- Creating a tenant grants its creator `tenant.owner`. A `--bootstrap-admin`
  flag creates the first `platform.root` binding once.
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
