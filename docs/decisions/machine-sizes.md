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

## Consequences

- The console reads the sizes from the API (`web/src/lib/machine-sizes.ts`)
  and has none of its own. With no size defined, its create form says so
  and sends nothing (`console-design.md`).
- Which size a machine is, as the console's lists show it, is a match of the
  machine's cpu and memory against the listed sizes. A machine made before
  a size changed, or by hand, may match none, and shows no size.
- `hangar get machinesizes` (short name `ms`) lists them, as for any kind;
  `hangar apply` writes them from a manifest, as root.
- The numbers themselves are the administrator's to choose; the repo holds
  only made-up sizes, in tests.
