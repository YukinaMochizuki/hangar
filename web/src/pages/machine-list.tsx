// A project's containers, or its virtual machines: data alone, filtered by
// name and state, with each row's actions in its menu.

import { Link } from "@tanstack/react-router";
import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { Plus } from "lucide-react";
import { useState } from "react";
import { MachineRowMenu } from "@/components/machine-actions";
import { ListCount, ListFrame, NameFilter, PageHead } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { buttonVariants, Card, SizeBadge, StatusDot, phaseLabel } from "@/components/ui";
import { cn } from "@/lib/cn";
import { machinePath, useMachines, type MachineKind, type MachineView } from "@/lib/machines";
import { sizeOf, useMachineSizes } from "@/lib/machine-sizes";
import { machinePermKey, machinePerms, useMachineCan } from "@/lib/permissions";
import { formatNumber } from "@/lib/quantity";

type StatusFilter = "all" | "running" | "stopped" | "failed";

const filters: { id: StatusFilter; label: string; match: (m: MachineView) => boolean }[] = [
  { id: "all", label: "All", match: () => true },
  {
    id: "running",
    label: "Running",
    match: (m) => [MachinePhase.RUNNING, MachinePhase.CREATING, MachinePhase.PENDING].includes(m.phase) && !m.stopped,
  },
  { id: "stopped", label: "Stopped", match: (m) => m.stopped || m.phase === MachinePhase.STOPPED },
  { id: "failed", label: "Failed", match: (m) => m.phase === MachinePhase.FAILED },
];

/** An address without its prefix length. */
export function bare(addr: string): string {
  return addr.split("/")[0];
}

/** Why a machine is not ready, in the API's words. */
export function notReadyReason(m: MachineView): string | undefined {
  const own = m.container?.status?.conditions ?? m.vm?.status?.conditions ?? [];
  return own.find((c) => c.type === "Ready")?.reason;
}

export function MachineStatusCell({ m }: { m: MachineView }) {
  return (
    <>
      <span className="inline-flex items-center gap-2">
        <StatusDot phase={m.phase} />
        {m.deleting ? "Deleting" : phaseLabel(m.phase)}
      </span>
      {m.phase === MachinePhase.FAILED && <span className="block text-[11.5px] text-fail">{notReadyReason(m)}</span>}
    </>
  );
}

export function MachineList({ kind }: { kind: MachineKind }) {
  const project = useProjectName();
  const { items, loading, error, refetch } = useMachines(project, kind);
  const { sizes } = useMachineSizes();
  const [filter, setFilter] = useState<StatusFilter>("all");
  const [query, setQuery] = useState("");
  const title = kind === "Container" ? "Containers" : "Virtual machines";
  const noun = kind === "Container" ? "container" : "virtual machine";
  const rows = items.filter((m) => filters.find((f) => f.id === filter)!.match(m) && (!query || m.name.includes(query)));

  // One answer for New and every row's menu; New shows only once the kind's apply permission is known to be held.
  const { can } = useMachineCan(project);
  const mayApply = can(machinePerms[machinePermKey(kind)]);
  const newLink = mayApply && (
    <Link
      to="/projects/$project/new"
      params={{ project }}
      search={{ kind: kind === "Container" ? "container" : "vm" }}
      className={buttonVariants({ variant: "primary" })}
    >
      <Plus /> New {noun}
    </Link>
  );

  return (
    <>
      <Topbar crumbs={[title]} />
      <PageHead
        title={title}
        description={kind === "Container" ? "Images from a registry, running" : "Full operating systems"}
        action={newLink}
      />

      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ loading, error, refetch, count: items.length }}
          noun={`${noun}s`}
          empty={{
            title: `No ${noun}s`,
            action: newLink,
          }}
        >
          <div className="mb-3 flex flex-wrap items-center gap-2">
            <NameFilter value={query} onChange={setQuery} className="mb-0" />
            <div role="radiogroup" aria-label="State" className="flex h-8 items-center overflow-x-auto rounded-md border bg-surface p-0.5">
              {filters.map((f) => (
                <button
                  key={f.id}
                  role="radio"
                  aria-checked={filter === f.id}
                  onClick={() => setFilter(f.id)}
                  className={cn(
                    "flex h-full items-center gap-1.5 rounded px-2.5 text-[12.5px]",
                    filter === f.id ? "bg-surface-2 font-medium text-foreground" : "text-muted hover:text-foreground",
                  )}
                >
                  {f.label}
                  <span className="font-mono text-[11px] text-subtle tabular">{items.filter(f.match).length}</span>
                </button>
              ))}
            </div>
          </div>

          {/* On a phone, a row is the name, the image and the state. */}
          <Card className="md:hidden">
            <ul className="divide-y">
              {rows.map((m) => (
                <li key={m.name} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                  <Link to={machinePath(kind)} params={{ project, name: m.name }} className="min-w-0 flex-1">
                    <span className="block truncate font-medium">{m.name}</span>
                    <span className="block truncate font-mono text-[11.5px] text-subtle">{m.image}</span>
                  </Link>
                  <span className="shrink-0 text-right">
                    <MachineStatusCell m={m} />
                  </span>
                  <MachineRowMenu m={m} can={can} />
                </li>
              ))}
              {rows.length === 0 && <NoMatch noun={noun} />}
            </ul>
          </Card>
          <Card className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[820px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[28%]">Name</Th>
                  <Th>State</Th>
                  <Th>Size</Th>
                  <Th>Addresses</Th>
                  <Th>Host</Th>
                  <Th>Created</Th>
                  <Th className="w-10 px-0" />
                </tr>
              </thead>
              <tbody>
                {rows.map((m) => {
                  const size = sizeOf(m, sizes);
                  const nic = m.interfaces[0];
                  return (
                    <tr key={m.name} className={tbodyRow}>
                      <td className="max-w-0 px-4 py-2.5">
                        <Link to={machinePath(kind)} params={{ project, name: m.name }} className="block">
                          <span className="block truncate font-medium group-hover:text-accent">{m.name}</span>
                          <span className="block truncate font-mono text-[11.5px] text-subtle">{m.image}</span>
                        </Link>
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5">
                        <MachineStatusCell m={m} />
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5">
                        <span className="flex items-center gap-2">
                          {size && <SizeBadge size={size.label} />}
                          <span className="text-muted tabular">
                            {formatNumber(m.cpu)} vCPU · {formatNumber(m.memoryGi)} GiB
                          </span>
                        </span>
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px]">
                        {nic?.ipv4 ? <span className="block">{bare(nic.ipv4)}</span> : <span className="block text-subtle">no address yet</span>}
                        {nic?.ipv6 && <span className="block text-subtle">{bare(nic.ipv6)}</span>}
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px] text-muted">
                        {m.node || <span className="font-sans text-subtle">not placed</span>}
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 text-muted">{m.created?.toISOString().slice(0, 10)}</td>
                      <td className="px-2 py-2.5">
                        <MachineRowMenu m={m} can={can} />
                      </td>
                    </tr>
                  );
                })}
                {rows.length === 0 && <NoMatch noun={noun} colSpan={7} />}
              </tbody>
            </table>
          </Card>
          <ListCount shown={rows.length} total={items.length} />
        </ListFrame>
      </div>
    </>
  );
}
