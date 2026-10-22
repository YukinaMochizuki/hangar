# Machine shapes: rules of the platform's, with sizes as presets

Status: accepted, 2026-10. Supersedes the first version of this record,
under which a machine had to have the shape of a `MachineSize` (#177).

## Context

The console's create form offered four sizes, S to XL, whose cpu, memory and
disk were written into the console itself. The sizes moved to the API, as
`MachineSize`, the platform administrator's, and admission held every
tenant's machine to the shape of one: vCPUs were shared with no weight, so
a machine of 8 vCPUs and 1Gi took a large share of the CPUs for little
memory, and only a fixed list of shapes kept the sharing in check.

A fixed list fits small workloads badly: the smallest useful size was about
1 vCPU, 2Gi and 10Gi, where a bot or a cron job uses a fraction of that.
Once each machine's share of the CPUs follows its memory
(`node-agent.md`, "CPU shares"), no shape changes how far the host's CPUs
are shared, so the list is no longer what keeps that in check, and tenants
can choose a shape within rules instead.

## Decision

- **A machine's shape is held to rules, not to a list.** Admission refuses,
  with InvalidArgument, a Container or VirtualMachine whose:
  - memory is not a whole number of 256Mi steps, at least one;
  - vCPUs are fewer than `ceil(memory / 8Gi)` or more than
    `floor(memory / 512Mi)`, though one vCPU is always allowed: 512Mi to
    8Gi of memory per vCPU. This refuses 256Mi with 8 vCPUs and 32Gi with
    1;
  - root disk is below 2Gi: a container's `resources.disk` and a VM's
    `root_disk.from_image.size`, each 10Gi when empty. A VM booting a Disk
    it names is checked for cpu and memory only, as that Disk's size is its
    own. The disk is not tied to the memory, and no rule caps it: what a
    tenant may hold is its plan's disk limit.

  The refusal says which rule, and for a ratio the vCPUs the memory allows.
  The rules are constants of the apiserver (`checkShape` in
  `internal/apiserver/machinesizes.go`), mirrored by the console
  (`web/src/lib/machine-sizes.ts`). Quantities compare by value, with the
  defaults for what a spec leaves empty; admission writes nothing into the
  spec.
- **Only a new shape is checked**: a create, or an update that changes cpu,
  memory or disk by value. A machine outside the rules, made before them or
  by someone exempt, can still be started, stopped and relabelled. The
  bounds every machine has whoever writes it (at least 128Mi for a
  container and 256Mi for a VM, at least 1Gi of disk, the maximums) are
  checked as before, apart from these rules.
- **The exemption follows the permission to define sizes.** A caller the
  authorizer allows `compute.machinesizes.apply` at the platform is not
  checked: `platform.root`, whether a person, root on the host or a service
  token bound it at the platform; nor is the apiserver's own write. A
  tenant owner, and a service token of a tenant or project role, are
  checked.
- **A `MachineSize` is a preset.** It stays a global kind of the `compute`
  group, after OpenStack flavors and GCP machine types: a `display_name`,
  the `cpu`, `memory` and `disk` quantities, and an integer `order` the
  sizes are listed by. The console offers the sizes, and fills a machine's
  cpu and memory from the chosen one and its disk field's starting value
  from the size's disk. A size is a template, not a reference: the machine
  records no size, and changing or deleting a size changes no machine.
- **A size keeps to the rules a machine does**, as a preset of a shape a
  tenant may have: admission refuses a size whose cpu, memory and disk
  would be refused on a tenant's machine, besides a display name of 1 to
  63 characters and an order not negative, with no defaults.
- **Only `platform.root` applies or deletes a size**, through the role's
  grants; everyone allowed to use Hangar at all, people and service tokens
  alike, gets, lists and watches every size (`readableByAll`, as for
  `Image`). Nothing reports a size's status.
- **A field the apiserver's schema does not have is never stored.** Over
  the binary codec, which keeps such a field, admission refuses the size.
  Over JSON, the codec drops the field before admission sees the request,
  as for every kind, and the rest of the size is stored without it. The
  CLI decodes manifests strictly and refuses an unknown field before
  sending anything.
- **No size ships with Hangar**, and no size is made at start. Until the
  administrator applies some, the platform lists none, and the console
  offers a shape of the person's own alone. The rules hold either way.

## Consequences

- Admission reads nothing to check a shape: the rules are constants, where
  the list of sizes was one read of the store.
- The console's create and edit forms offer the sizes and a Custom choice,
  with memory and vCPU fields checked against the rules as they are typed
  (`console-design.md`). Which size a machine is, as the console's lists
  show it, is a match of the machine's cpu and memory against the listed
  sizes; a machine of a shape of its own matches none, and shows no size.
- `hangar get machinesizes` (short name `ms`) lists them, as for any kind;
  `hangar apply` writes them from a manifest, as root.
- The numbers of the sizes are the administrator's to choose; the repo holds
  only made-up sizes, in tests. The rules are the platform's code, and
  changing them is a change to the apiserver and the console together.
- Memory stays reserved in full and is never overcommitted, so a shape's
  memory is what the scheduler holds for it (`machine-model.md`). The
  CPU a machine is sure of under full load follows its memory; it can use
  idle CPU up to its vCPUs at any time.
