// A project's containers, or its virtual machines: data alone, filtered by
// name and state, with each row's actions in its menu.

import { Link } from "@tanstack/react-router";
import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { Plus, Search } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { MachineRowMenu } from "@/components/machine-actions";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, Skeleton } from "@/components/states";
import { buttonVariants, Card, Kbd, SizeBadge, StatusDot, phaseLabel } from "@/components/ui";
import { cn } from "@/lib/cn";
import { machinePath, useMachines, type MachineKind, type MachineView } from "@/lib/machines";
import { sizeOf, useMachineSizes } from "@/lib/machine-sizes";
import { useMayCreateMachines } from "@/lib/permissions";
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

function LoadingTable() {
  return (
    <Card aria-busy="true" aria-label="Loading" className="divide-y">
      {Array.from({ length: 4 }, (_, i) => (
        <div key={i} className="flex items-center gap-6 px-4 py-3.5">
          <span className="w-[28%] space-y-1.5">
            <Skeleton className="h-3 w-28" />
            <Skeleton className="h-2.5 w-44" />
          </span>
          <Skeleton className="h-3 w-16" />
          <Skeleton className="hidden h-3 w-24 md:block" />
          <Skeleton className="hidden h-3 w-28 md:block" />
        </div>
      ))}
    </Card>
  );
}

export function MachineList({ kind }: { kind: MachineKind }) {
  const project = useProjectName();
  const { items, loading, error, refetch } = useMachines(project, kind);
  const { sizes } = useMachineSizes();
  const [filter, setFilter] = useState<StatusFilter>("all");
  const [query, setQuery] = useState("");
  const search = useRef<HTMLInputElement>(null);
  const title = kind === "Container" ? "Containers" : "Virtual machines";
  const noun = kind === "Container" ? "container" : "virtual machine";
  const rows = items.filter((m) => filters.find((f) => f.id === filter)!.match(m) && (!query || m.name.includes(query)));

  // "/" puts the cursor in the name filter, as on most consoles.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement;
      if (e.key === "/" && !["INPUT", "TEXTAREA"].includes(t.tagName)) {
        e.preventDefault();
        search.current?.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  // New shows only once the caller is known to hold the kind's apply permission.
  const may = useMayCreateMachines(project);
  const newLink = may[kind === "Container" ? "container" : "vm"] && (
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
      <div className="flex flex-wrap items-end justify-between gap-4 px-4 pb-4 pt-6 md:px-8">
        <div>
          <h1 className="text-[22px] font-semibold tracking-tight">{title}</h1>
          <p className="mt-1 text-[13px] text-muted">
            {kind === "Container" ? "OCI images, each booted as its own microVM." : "Full operating systems booted from a disk image."}
          </p>
        </div>
        {newLink}
      </div>

      <div className="px-4 pb-10 md:px-8">
        {loading ? (
          <LoadingTable />
        ) : error ? (
          <Card>
            <ErrorNotice error={error} title={`The ${noun}s did not load`} onRetry={refetch} />
          </Card>
        ) : items.length === 0 ? (
          <Card>
            <EmptyState title={`No ${noun}s`} action={newLink}>
              {kind === "Container"
                ? "A container boots an OCI image as its own microVM."
                : "A virtual machine boots a full operating system from one of the platform's images."}
            </EmptyState>
          </Card>
        ) : (
          <>
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <label className="flex h-8 w-full items-center gap-2 rounded-md border bg-surface px-2.5 text-[13px] focus-within:border-border-strong sm:w-64">
                <Search className="size-3.5 text-subtle" />
                <input
                  ref={search}
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="Name"
                  aria-label="Filter by name"
                  className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-subtle"
                />
                <Kbd>/</Kbd>
              </label>
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
                    <MachineRowMenu m={m} />
                  </li>
                ))}
                {rows.length === 0 && <li className="px-4 py-8 text-center text-[13px] text-muted">No {noun} matches.</li>}
              </ul>
            </Card>
            <Card className="hidden overflow-x-auto md:block">
              <table className="w-full min-w-[820px] text-[13px]">
                <thead>
                  <tr className="border-b bg-surface-2/60 text-left text-[11.5px] text-subtle">
                    <th className="w-[28%] px-4 py-2 font-medium">Name</th>
                    <th className="px-4 py-2 font-medium">State</th>
                    <th className="px-4 py-2 font-medium">Size</th>
                    <th className="px-4 py-2 font-medium">Addresses</th>
                    <th className="px-4 py-2 font-medium">Host</th>
                    <th className="px-4 py-2 font-medium">Created</th>
                    <th className="w-10" />
                  </tr>
                </thead>
                <tbody>
                  {rows.map((m) => {
                    const size = sizeOf(m, sizes);
                    const nic = m.interfaces[0];
                    return (
                      <tr key={m.name} className="group border-b last:border-0 hover:bg-surface-2/60">
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
                          <MachineRowMenu m={m} />
                        </td>
                      </tr>
                    );
                  })}
                  {rows.length === 0 && (
                    <tr>
                      <td colSpan={7} className="px-4 py-10 text-center text-muted">
                        No {noun} matches.
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </Card>
            <p className="mt-3 text-[12px] text-subtle">
              {rows.length} of {items.length}
            </p>
          </>
        )}
      </div>
    </>
  );
}
