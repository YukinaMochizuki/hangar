# Machine sizes: defined by the platform, served by the API

Status: accepted, 2026-10.

## Context

The console's create form offered four sizes, S to XL, whose cpu, memory and
disk were written into the console itself. A size is a statement about the
hosts, not about the console: how many vCPUs go with how much memory is a
choice about what the hosts can carry. The scheduler holds memory and disk
for each machine and never overcommits them, but vCPUs are shared, not
reserved (`pickNode` in `internal/apiserver/controllers.go`). So the ratio of
vCPUs to memory in a size decides how far the hosts' CPUs are shared, and
only the platform can choose it. The owner settled that the sizes are the
platform's alone: its administrator sets them, and no tenant or project can
change them.

## Decision

- **A `MachineSize` is a global kind of the `compute` group**, a listable
  resource after OpenStack flavors and GCP machine types. Its spec is a
  `display_name`, the `cpu`, `memory` and `disk` quantities, and an integer
  `order` the sizes are listed by.
- **Only `platform.root` applies or deletes one**, through the role's grants;
  everyone allowed to use Hangar at all, people and service tokens alike,
  gets, lists and watches every size (`readableByAll`, as for `Image`).
  Nothing reports a size's status: no node is let write it, and no role
  grants it.
- **Admission checks a size as a container's resources are checked**, but
  with no defaults: cpu whole and at least 1, memory at least 128Mi, disk at
  least 1Gi in whole Gi, each within a machine's maximums; a display name of
  1 to 63 characters; an order not negative.
- **A field the apiserver's schema does not have is never stored.** Over
  the binary codec, which keeps such a field, admission refuses the size.
  Over JSON, the codec drops the field before admission sees the request,
  as for every kind, and the rest of the size is stored without it. The
  CLI decodes manifests strictly and refuses an unknown field before
  sending anything.
- **No size ships with Hangar**, and no size is made at start. Until the
  administrator applies some, the platform lists none, and the console
  offers none.
- **A size is a template, not a reference.** A Container or VirtualMachine
  still states its own cpu, memory and disk; the console fills them from the
  chosen size, and the API records no size on the machine. Changing or
  deleting a size changes no machine.
- **A machine must have the shape of a size.** Admission refuses, with
  InvalidArgument, a Container or VirtualMachine whose cpu and memory are
  those of no size, or whose disk is below the size's whose cpu and memory
  it has; the refusal lists the sizes as `name: cpu / memory / disk`.
  Without it, the sizes would bind only the console: as vCPUs are shared, a
  machine of 8 vCPUs and 1Gi takes a large share of the CPUs for little
  memory, which defeats the ratio the sizes set. Quantities compare by
  value, with the defaults for what a spec leaves empty: `2000m` is `2`,
  `2048Mi` is `2Gi`. The disk is a container's `resources.disk` and a VM's
  `root_disk.from_image.size`, each 10Gi when empty, so a size of a larger
  disk needs it stated; admission writes nothing into the spec. A VM booting
  a Disk it names is checked for cpu and memory only, as that Disk's size is
  its own.
- **Only a new shape is checked**: a create, or an update that changes cpu,
  memory or disk by value. A machine that matches no size, made before the
  sizes changed or by someone exempt, can still be started, stopped and
  relabelled. With no size defined, nothing is refused.
- **The exemption follows the permission to define sizes.** A caller the
  authorizer allows `compute.machinesizes.apply` at the platform is not
  checked: `platform.root`, whether a person, root on the host or a service
  token bound it at the platform; nor is the apiserver's own write. Such a
  caller could define a size of any shape, so checking it would protect
  nothing. A tenant owner, and a service token of a tenant or project role,
  are checked.
- **Admission reads the sizes from the store**, one list of a few objects,
  and only for a new shape by a caller who is checked; nothing caches them.

## Consequences

- The console reads the sizes from the API (`web/src/lib/machine-sizes.ts`)
  and has none of its own. With no size defined, its create form says so
  and sends nothing (`console-design.md`).
- Which size a machine is, as the console's lists show it, is a match of the
  machine's cpu and memory against the listed sizes. A machine made before
  a size changed, or by someone exempt, may match none, and shows no size.
- `hangar get machinesizes` (short name `ms`) lists them, as for any kind;
  `hangar apply` writes them from a manifest, as root.
- The numbers themselves are the administrator's to choose; the repo holds
  only made-up sizes, in tests.
