// A project's disks: data alone, filtered by name and state, each with the
// machine it is attached to.

import { Link } from "@tanstack/react-router";
import { DiskPhase, type Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { Plus, Search } from "lucide-react";
import { useState } from "react";
import { useDiskActions, useDiskCan } from "@/components/disk-actions";
import { ObjectRowMenu, type Can } from "@/components/object-actions";
import { ListCount, ListFrame, PageHead } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { DiskSource, DiskState, UsedAs, usedAsText } from "@/components/storage";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { buttonVariants, Card } from "@/components/ui";
import { cn } from "@/lib/cn";
import { date } from "@/lib/machines";
import { gib, growingTo, growthBlocked, useDisks } from "@/lib/storage";

type Filter = "all" | "in-use" | "available" | "pending";

const filters: { id: Filter; label: string; match: (d: Disk) => boolean }[] = [
  { id: "all", label: "All", match: () => true },
  { id: "in-use", label: "In use", match: (d) => d.status?.phase === DiskPhase.IN_USE },
  { id: "available", label: "Available", match: (d) => d.status?.phase === DiskPhase.AVAILABLE },
  { id: "pending", label: "Pending", match: (d) => d.status?.phase === DiskPhase.PENDING },
];

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

/** A disk's row menu: the actions of its page. */
function DiskRowMenu({ d, can }: { d: Disk; can: Can }) {
  return <ObjectRowMenu name={d.metadata?.name ?? ""} set={useDiskActions(d, can)} />;
}

/** DiskList is the project's disks, with New disk. */
export function DiskList() {
  const project = useProjectName();
  const { items, loading, error, refetch } = useDisks(project);
  const disks = { items, loading, error, refetch };
  const [filter, setFilter] = useState<Filter>("all");
  const [query, setQuery] = useState("");
  // One answer for New disk and every row's menu; New disk shows only once storage.disks.apply is known to be held.
  const createPerm = useDiskCan(project);
  const canCreate = createPerm.can("storage.disks.apply");
  const rows = items.filter((d) => filters.find((f) => f.id === filter)!.match(d) && (!query || d.metadata?.name.includes(query)));

  const newButton = (
    <Link to="/projects/$project/new/disk" params={{ project }} className={buttonVariants({ variant: "primary" })}>
      <Plus /> New disk
    </Link>
  );

  return (
    <>
      <Topbar crumbs={["Disks"]} />
      <PageHead title="Disks" description="Block storage for virtual machines" action={canCreate && newButton} />

      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ ...disks, count: items.length }}
          noun="disks"
          empty={{ title: "No disks", text: "A disk keeps data apart from any machine: a virtual machine boots from one or attaches it, a container mounts it as a volume.", action: canCreate ? newButton : undefined }}
        >
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

          {/* On a phone, a row is the name, the size and what it is attached to. */}
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
                        {usedAsText(d) ? ` · ${usedAsText(d)}` : ""}
                      </span>
                    </Link>
                    <span className="shrink-0 text-right">
                      <DiskState d={d} />
                    </span>
                    <DiskRowMenu d={d} can={createPerm.can} />
                  </li>
                );
              })}
              {rows.length === 0 && <NoMatch noun="disk" />}
            </ul>
          </Card>
          <Card className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[820px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[26%]">Name</Th>
                  <Th>State</Th>
                  <Th>Size</Th>
                  <Th>Used as</Th>
                  <Th>Node</Th>
                  <Th>Created</Th>
                  <Th className="w-10 px-0" />
                </tr>
              </thead>
              <tbody>
                {rows.map((d) => {
                  const name = d.metadata?.name ?? "";
                  return (
                    <tr key={name} className={tbodyRow}>
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
                        <UsedAs d={d} project={project} />
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px] text-muted">
                        {d.status?.node || <span className="font-sans text-subtle">not placed yet</span>}
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 text-muted">{date(d.metadata?.createTime)?.toISOString().slice(0, 10)}</td>
                      <td className="px-2 py-2.5">
                        <DiskRowMenu d={d} can={createPerm.can} />
                      </td>
                    </tr>
                  );
                })}
                {rows.length === 0 && <NoMatch noun="disk" colSpan={7} />}
              </tbody>
            </table>
          </Card>
          <ListCount shown={rows.length} total={items.length} />
        </ListFrame>
      </div>
    </>
  );
}
