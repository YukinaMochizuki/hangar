// The machine form's Port forwards: the forwards that lead to the machine,
// and new ones to make with it. Each is a PortForward of its own, made or
// deleted when the form is applied (lib/port-forwards.ts, forwardPlan).

import type { PortForward } from "@hangar/api/hangar/network/v1alpha1/port_forward_pb";
import type { SecurityGroup } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { ArrowRight, Plus, X } from "lucide-react";
import { Input, Select } from "@/components/form";
import { Button } from "@/components/ui";
import { cn } from "@/lib/cn";
import type { MachineKind } from "@/lib/machines";
import { inboundOf, portLabel, portProblem, publicAddress, type ForwardProtocol, type ForwardRow } from "@/lib/port-forwards";

/** An interface of the machine as the form has it: eth0 first. */
export type FormNic = { network: string; groups: string[] };

const protocols: ForwardProtocol[] = ["TCP", "UDP"];

export function MachinePortForwards({
  kind,
  machine,
  rows,
  onChange,
  nics,
  forwards,
  groups,
}: {
  kind: MachineKind;
  machine: string;
  rows: ForwardRow[];
  onChange: (rows: ForwardRow[]) => void;
  nics: FormNic[];
  /** The project's forwards, for an existing row's public address. */
  forwards: PortForward[];
  /** The project's security groups, to say whether a port is let in. */
  groups: SecurityGroup[];
}) {
  const set = (i: number, r: Partial<ForwardRow>) => onChange(rows.map((x, j) => (j === i ? { ...x, ...r } : x)));
  const ifaces = nics.map((n, i) => ({ name: `eth${i}`, ...n }));
  // Whether the groups the form puts the interface in let the port in from anywhere.
  const letIn = (r: ForwardRow) => {
    const nic = ifaces.find((x) => x.name === r.iface);
    if (!nic || portProblem(r.port)) return true;
    const att = { kind, name: machine, iface: r.iface, network: nic.network, ipv4: "", ipv6: "", securityGroups: nic.groups };
    return inboundOf({ kind, name: machine, iface: r.iface }, r.protocol, Number(r.port), [att], groups).verdict !== "none";
  };
  return (
    <div className="max-w-xl space-y-2">
      {rows.map((r, i) => {
        const pf = r.existing ? forwards.find((f) => f.metadata?.name === r.existing) : undefined;
        const shut = !letIn(r);
        return (
          <div key={r.existing ?? `new-${i}`} className="rounded-lg border bg-surface px-3 py-2">
            <div className="flex items-center gap-2">
              {r.existing ? (
                <span className="flex h-8 min-w-0 flex-1 items-center gap-2 font-mono text-[12.5px]">
                  <span className="font-medium">{portLabel(Number(r.port), r.protocol)}</span>
                  {ifaces.length > 1 && <span className="text-muted">{r.iface}</span>}
                  <ArrowRight className="size-3.5 shrink-0 text-subtle" />
                  <span className="truncate text-muted">{(pf && publicAddress(pf)) ?? (pf?.status?.port ? `port ${pf.status.port}` : "random port")}</span>
                </span>
              ) : (
                <>
                  <div role="radiogroup" aria-label={`Protocol of port forward ${i + 1}`} className="grid h-8 w-28 shrink-0 grid-cols-2 rounded-md border border-border-strong p-0.5">
                    {protocols.map((p) => (
                      <button
                        key={p}
                        type="button"
                        role="radio"
                        aria-checked={r.protocol === p}
                        onClick={() => set(i, { protocol: p })}
                        className={cn("rounded font-mono text-[12px]", r.protocol === p ? "bg-accent-soft font-medium ring-1 ring-accent" : "text-muted hover:text-foreground")}
                      >
                        {p}
                      </button>
                    ))}
                  </div>
                  <Input
                    value={r.port}
                    onChange={(e) => set(i, { port: e.target.value.trim() })}
                    inputMode="numeric"
                    placeholder="Port"
                    aria-label={`Port of port forward ${i + 1}`}
                    aria-invalid={r.port !== "" && !!portProblem(r.port)}
                    className="h-8 w-24 tabular"
                  />
                  {ifaces.length > 1 && (
                    <Select value={r.iface} onChange={(e) => set(i, { iface: e.target.value })} aria-label={`Interface of port forward ${i + 1}`} className="h-8 w-auto">
                      {ifaces.map((x) => (
                        <option key={x.name} value={x.name}>
                          {x.name} · {x.network}
                        </option>
                      ))}
                    </Select>
                  )}
                  <ArrowRight className="size-3.5 shrink-0 text-subtle" />
                  <span className="min-w-0 flex-1 truncate font-mono text-[12.5px] text-subtle">random port</span>
                </>
              )}
              <Button type="button" variant="ghost" size="icon" aria-label={`Remove port forward ${i + 1}`} onClick={() => onChange(rows.filter((_, j) => j !== i))}>
                <X />
              </Button>
            </div>
            {shut && <div className="pb-0.5 pt-1 text-[12px] text-pending">No security group lets it in.</div>}
          </div>
        );
      })}
      <Button type="button" size="sm" onClick={() => onChange([...rows, { protocol: "TCP", port: "", iface: "eth0" }])}>
        <Plus /> Add port forward
      </Button>
    </div>
  );
}
