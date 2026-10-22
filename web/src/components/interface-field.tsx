// Choosing one interface of the project's machines: a machine, then, when it
// has more than one, which of its interfaces, named with its network. Used
// by addresses and port forwards.

import { create } from "@bufbuild/protobuf";
import { AddressTargetSchema } from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { FormField, Select } from "@/components/form";
import { useProjectName } from "@/components/shell";
import { cn } from "@/lib/cn";
import type { MachineKind } from "@/lib/machines";
import { useAttachments, type Attachment } from "@/lib/network";

/** An interface as kind/machine/interface, or "" for none. */
export type InterfacePick = string;

/** pickTarget is the target a pick stands for, as the API takes it; undefined for none. */
export function pickTarget(pick: InterfacePick) {
  const [kind, machine, iface] = pick.split("/") as [MachineKind, string | undefined, string | undefined];
  if (!machine) return undefined;
  return create(AddressTargetSchema, {
    object: { case: kind === "Container" ? "container" : "virtualMachine", value: machine },
    interface: iface || "eth0",
  });
}

type Machine = { kind: MachineKind; name: string; ifaces: { name: string; network: string }[] };

/** machinesOf groups the interfaces by machine, keeping their order; current is listed even when no machine has it. */
export function machinesOf(items: Attachment[], current?: InterfacePick): Machine[] {
  const out: Machine[] = [];
  const add = (kind: MachineKind, name: string, iface: string, network: string) => {
    let m = out.find((x) => x.kind === kind && x.name === name);
    if (!m) out.push((m = { kind, name, ifaces: [] }));
    if (!m.ifaces.some((i) => i.name === iface)) m.ifaces.push({ name: iface, network });
  };
  for (const a of items) add(a.kind, a.name, a.iface, a.network);
  if (current) {
    const [kind, name, iface] = current.split("/") as [MachineKind, string, string];
    if (name) add(kind, name, iface || "eth0", "");
  }
  return out;
}

const groups: { kind: MachineKind; label: string }[] = [
  { kind: "Container", label: "Containers" },
  { kind: "VirtualMachine", label: "Virtual machines" },
];

/**
 * InterfaceField is a machine select and, for a machine of several
 * interfaces, an interface select. Choosing a machine chooses its first
 * interface. current is the interface the object leads to now.
 */
export function InterfaceField({
  value,
  onChange,
  current,
  hint,
  none = "Choose a machine",
}: {
  value: InterfacePick;
  onChange: (p: InterfacePick) => void;
  current?: InterfacePick;
  hint?: string;
  /** The label of no machine. */
  none?: string;
}) {
  const project = useProjectName();
  const att = useAttachments(project);
  const machines = machinesOf(att.items, current);
  const [kind, name, iface] = value.split("/");
  const key = name ? `${kind}/${name}` : "";
  const chosen = machines.find((m) => `${m.kind}/${m.name}` === key);
  const several = !!chosen && chosen.ifaces.length > 1;
  return (
    <div>
      <div className={cn("grid grid-cols-1 gap-3", several && "sm:grid-cols-[1fr_200px]")}>
        <FormField label="Machine" className="mt-0">
          <Select
            aria-label="Machine"
            value={key}
            onChange={(e) => {
              const m = machines.find((x) => `${x.kind}/${x.name}` === e.target.value);
              onChange(m ? `${m.kind}/${m.name}/${m.ifaces[0]?.name ?? "eth0"}` : "");
            }}
          >
            <option value="">{att.loading ? "Loading…" : machines.length === 0 ? "No machines" : none}</option>
            {groups.map((g) => {
              const of = machines.filter((m) => m.kind === g.kind);
              return (
                of.length > 0 && (
                  <optgroup key={g.kind} label={g.label}>
                    {of.map((m) => (
                      <option key={m.name} value={`${m.kind}/${m.name}`}>
                        {m.name}
                      </option>
                    ))}
                  </optgroup>
                )
              );
            })}
          </Select>
        </FormField>
        {several && (
          <FormField label="Interface" className="mt-0">
            <Select aria-label="Interface" value={iface} onChange={(e) => onChange(`${kind}/${name}/${e.target.value}`)}>
              {chosen.ifaces.map((i) => (
                <option key={i.name} value={i.name}>
                  {i.name}
                  {i.network && ` · ${i.network}`}
                </option>
              ))}
            </Select>
          </FormField>
        )}
      </div>
      {hint && <span className="mt-1.5 block text-[11.5px] text-subtle">{hint}</span>}
    </div>
  );
}
