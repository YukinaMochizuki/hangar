// A project's disks: data alone, filtered by name and state, each with the
// machine it is attached to.

import { Link } from "@tanstack/react-router";
import { DiskPhase, type Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { Plus, Search } from "lucide-react";
import { useState } from "react";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, Skeleton } from "@/components/states";
import { DiskSource, DiskState, MachineLink, NewDiskDialog } from "@/components/storage";
import { Button, Card } from "@/components/ui";
import { cn } from "@/lib/cn";
import { date } from "@/lib/machines";
import { useCan } from "@/lib/permissions";
import { gib, growingTo, growthBlocked, rootDiskOwner, useDisks, useImages, useMachineOwners } from "@/lib/storage";

type Filter = "all" | "in-use" | "available" | "pending";

const filters: { id: Filter; label: string; match: (d: Disk) => boolean }[] = [
  { id: "all", label: "All", match: () => true },
  { id: "in-use", label: "In use", match: (d) => d.status?.phase === DiskPhase.IN_USE },
  { id: "available", label: "Available", match: (d) => d.status?.phase === DiskPhase.AVAILABLE },
  { id: "pending", label: "Pending", match: (d) => d.status?.phase === DiskPhase.PENDING },
];

/** LoadingTable is a list page's table while it loads. */
export function LoadingTable() {
  return (
    <Card aria-busy="true" aria-label="Loading" className="divide-y">
      {Array.from({ length: 4 }, (_, i) => (
        <div key={i} className="flex items-center gap-6 px-4 py-3.5">
          <span className="w-[28%] space-y-1.5">
            <Skeleton className="h-3 w-28" />
            <Skeleton className="h-2.5 w-36" />
          </span>
          <Skeleton className="h-3 w-16" />
          <Skeleton className="hidden h-3 w-20 md:block" />
          <Skeleton className="hidden h-3 w-24 md:block" />
        </div>
      ))}
    </Card>
  );
}

/**
 * DiskSize shows the size a disk has on its node, the size it is growing to,
 * and why that growth is not granted when it is not.
 */
export function DiskSize({ d }: { d: Disk }) {
  const to = growingTo(d);
  const blocked = growthBlocked(d);
  return (
    <span className="tabular">
      {gib(d.status?.size || d.spec?.size)}
      {to && (
        <span className="block text-[11.5px] text-pending">
          growing to {gib(to)}
          {blocked && <span className="text-fail"> · {blocked.reason}</span>}
        </span>
      )}
    </span>
  );
}

/** Attached shows the machine a disk is attached to, and whether it is that VM's root disk. */
export function Attached({ d, project }: { d: Disk; project: string }) {
  const owners = useMachineOwners(project);
  if (!d.status?.machine) return <span className="text-subtle">none</span>;
  const root = rootDiskOwner(d);
  return (
    <span>
      <MachineLink project={project} machine={d.status.machine} />
      {root && owners(d.status.machine)?.name === root && <span className="text-[11.5px] text-subtle"> · root disk</span>}
    </span>
  );
}

/** DiskList is the project's disks, with New disk. */
export function DiskList() {
  const project = useProjectName();
  const { items, loading, error, refetch } = useDisks(project);
  const images = useImages();
  const owners = useMachineOwners(project);
  const disks = { items, loading, error, refetch };
  const [filter, setFilter] = useState<Filter>("all");
  const [query, setQuery] = useState("");
  const [creating, setCreating] = useState(false);
  // New disk shows only once the caller is known to hold storage.disks.apply.
  const canCreate = useCan({ level: "project", name: project }, ["storage.disks.apply"]).can("storage.disks.apply");
  const rows = items.filter((d) => filters.find((f) => f.id === filter)!.match(d) && (!query || d.metadata?.name.includes(query)));

  const newButton = (
    <Button variant="primary" onClick={() => setCreating(true)}>
      <Plus /> New disk
    </Button>
  );

  return (
    <>
      <Topbar crumbs={["Disks"]} />
      <div className="flex flex-wrap items-end justify-between gap-4 px-4 pb-4 pt-6 md:px-8">
        <div>
          <h1 className="text-[22px] font-semibold tracking-tight">Disks</h1>
          <p className="mt-1 text-[13px] text-muted">Durable block devices, each attached to one machine at a time.</p>
        </div>
        {canCreate && newButton}
      </div>

      <div className="px-4 pb-10 md:px-8">
        {loading ? (
          <LoadingTable />
        ) : error ? (
          <Card>
            <ErrorNotice error={error} title="The disks did not load" onRetry={refetch} />
          </Card>
        ) : items.length === 0 ? (
          <Card>
            <EmptyState title="No disks" action={canCreate ? newButton : undefined}>
              A disk outlives the machines it is attached to.
            </EmptyState>
          </Card>
        ) : (
          <>
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <label className="flex h-8 w-full items-center gap-2 rounded-md border bg-surface px-2.5 text-[13px] focus-within:border-border-strong sm:w-64">
                <Search className="size-3.5 text-subtle" />
                <input
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="Name"
                  aria-label="Filter by name"
                  className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-subtle"
                />
              </label>
              <div role="radiogroup" aria-label="State" className="flex h-8 items-center overflow-x-auto rounded-md border bg-surface p-0.5">
                {filters.map((f) => (
                  <button
                    key={f.id}
                    role="radio"
                    aria-checked={filter === f.id}
                    onClick={() => setFilter(f.id)}
                    className={cn(
                      "flex h-full items-center gap-1.5 whitespace-nowrap rounded px-2.5 text-[12.5px]",
                      filter === f.id ? "bg-surface-2 font-medium text-foreground" : "text-muted hover:text-foreground",
                    )}
                  >
                    {f.label}
                    <span className="font-mono text-[11px] text-subtle tabular">{items.filter(f.match).length}</span>
                  </button>
                ))}
              </div>
            </div>

            {/* On a phone, a row is the name, the size and where it is attached. */}
            <Card className="md:hidden">
              <ul className="divide-y">
                {rows.map((d) => {
                  const name = d.metadata?.name ?? "";
                  return (
                    <li key={name} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                      <Link to="/projects/$project/disks/$name" params={{ project, name }} className="min-w-0 flex-1">
                        <span className="block truncate font-medium">{name}</span>
                        <span className="block truncate text-[11.5px] text-subtle">
                          {gib(d.status?.size || d.spec?.size)}
                          {d.status?.machine ? ` · on ${owners(d.status.machine)?.name ?? d.status.machine}` : ""}
                        </span>
                      </Link>
                      <span className="shrink-0 text-right">
                        <DiskState d={d} />
                      </span>
                    </li>
                  );
                })}
                {rows.length === 0 && <li className="px-4 py-8 text-center text-[13px] text-muted">No disk matches.</li>}
              </ul>
            </Card>
            <Card className="hidden overflow-x-auto md:block">
              <table className="w-full min-w-[820px] text-[13px]">
                <thead>
                  <tr className="border-b bg-surface-2/60 text-left text-[11.5px] text-subtle">
                    <th className="w-[26%] px-4 py-2 font-medium">Name</th>
                    <th className="px-4 py-2 font-medium">State</th>
                    <th className="px-4 py-2 font-medium">Size</th>
                    <th className="px-4 py-2 font-medium">Attached to</th>
                    <th className="px-4 py-2 font-medium">Node</th>
                    <th className="px-4 py-2 font-medium">Created</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((d) => {
                    const name = d.metadata?.name ?? "";
                    return (
                      <tr key={name} className="group border-b last:border-0 hover:bg-surface-2/60">
                        <td className="max-w-0 px-4 py-2.5">
                          <Link to="/projects/$project/disks/$name" params={{ project, name }} className="block">
                            <span className="block truncate font-medium group-hover:text-accent">{name}</span>
                          </Link>
                          <span className="block truncate text-[11.5px]">
                            <DiskSource d={d} project={project} link={false} />
                          </span>
                        </td>
                        <td className="whitespace-nowrap px-4 py-2.5">
                          <DiskState d={d} />
                        </td>
                        <td className="whitespace-nowrap px-4 py-2.5">
                          <DiskSize d={d} />
                        </td>
                        <td className="whitespace-nowrap px-4 py-2.5">
                          <Attached d={d} project={project} />
                        </td>
                        <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px] text-muted">
                          {d.status?.node || <span className="font-sans text-subtle">not placed yet</span>}
                        </td>
                        <td className="whitespace-nowrap px-4 py-2.5 text-muted">{date(d.metadata?.createTime)?.toISOString().slice(0, 10)}</td>
                      </tr>
                    );
                  })}
                  {rows.length === 0 && (
                    <tr>
                      <td colSpan={6} className="px-4 py-10 text-center text-muted">
                        No disk matches.
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
      <NewDiskDialog key={String(creating)} open={creating} onClose={() => setCreating(false)} project={project} images={images.items} disks={disks} />
    </>
  );
}
