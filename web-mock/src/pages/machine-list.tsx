import { Link } from "@tanstack/react-router";
import { Filter, MoreHorizontal, Plus, Search } from "lucide-react";
import { useState } from "react";
import { Topbar } from "@/components/shell";
import { ApronArt } from "@/components/taxiway";
import { Button, Card, Kbd, SizeBadge, StatusDot, phaseLabel } from "@/components/ui";
import { cn } from "@/lib/cn";
import { currentProject, machines, MachinePhase, sizes, tenant, type MachineView } from "@/mock/data";

type StatusFilter = "all" | "running" | "stopped" | "failed";

const filters: { id: StatusFilter; label: string; match: (m: MachineView) => boolean }[] = [
  { id: "all", label: "All", match: () => true },
  {
    id: "running",
    label: "Running",
    match: (m) => m.phase === MachinePhase.RUNNING || m.phase === MachinePhase.CREATING || m.phase === MachinePhase.PENDING,
  },
  { id: "stopped", label: "Stopped", match: (m) => m.phase === MachinePhase.STOPPED },
  { id: "failed", label: "Failed", match: (m) => m.phase === MachinePhase.FAILED },
];

function Empty({ kind }: { kind: MachineView["kind"] }) {
  const noun = kind === "Container" ? "container" : "virtual machine";
  return (
    <Card className="flex flex-col items-center px-6 pb-10 pt-8 text-center">
      <ApronArt className="w-full max-w-lg" />
      <h2 className="mt-6 text-[15px] font-medium">No {noun}s in {currentProject.displayName} yet</h2>
      <p className="mt-1 max-w-sm text-[13px] text-muted">
        {kind === "Container"
          ? "A container boots any OCI image as its own microVM, usually in a few seconds."
          : "A virtual machine boots a full operating system from the platform's images."}
      </p>
      <Link to="/new" search={{ kind: kind === "Container" ? "container" : "vm" }} className="mt-5">
        <Button variant="primary">
          <Plus /> New {noun}
        </Button>
      </Link>
    </Card>
  );
}

export function MachineList({ kind, empty }: { kind: MachineView["kind"]; empty?: boolean }) {
  const [filter, setFilter] = useState<StatusFilter>("all");
  const [query, setQuery] = useState("");
  const title = kind === "Container" ? "Containers" : "Virtual machines";
  const ofKind = empty ? [] : machines.filter((m) => m.kind === kind);
  const rows = ofKind.filter(
    (m) => filters.find((f) => f.id === filter)!.match(m) && (!query || m.name.includes(query)),
  );

  return (
    <>
      <Topbar crumbs={[tenant.name, currentProject.displayName, title]} />
      <div className="flex flex-wrap items-end justify-between gap-4 px-4 md:px-8 pb-4 pt-6">
        <div>
          <h1 className="text-[22px] font-semibold tracking-tight">{title}</h1>
          <p className="mt-1 text-[13px] text-muted">
            {kind === "Container"
              ? "OCI images, each booted as its own microVM."
              : "Full operating systems booted from a disk image."}
          </p>
        </div>
        <Link to="/new" search={{ kind: kind === "Container" ? "container" : "vm" }}>
          <Button variant="primary">
            <Plus /> New {kind === "Container" ? "container" : "virtual machine"}
          </Button>
        </Link>
      </div>

      <div className="px-4 md:px-8 pb-10">
        {ofKind.length === 0 ? (
          <Empty kind={kind} />
        ) : (
        <>
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <label className="flex h-8 w-full items-center sm:w-64 gap-2 rounded-md border bg-surface px-2.5 text-[13px] focus-within:border-border-strong">
            <Search className="size-3.5 text-subtle" />
            <input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Filter by name"
              className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-subtle"
            />
            <Kbd>/</Kbd>
          </label>
          <div className="flex h-8 items-center rounded-md border bg-surface p-0.5">
            {filters.map((f) => (
              <button
                key={f.id}
                onClick={() => setFilter(f.id)}
                className={cn(
                  "flex h-full items-center gap-1.5 rounded px-2.5 text-[12.5px]",
                  filter === f.id ? "bg-surface-2 font-medium text-foreground" : "text-muted hover:text-foreground",
                )}
              >
                {f.label}
                <span className="font-mono text-[11px] text-subtle tabular">{ofKind.filter(f.match).length}</span>
              </button>
            ))}
          </div>
          <Button variant="ghost" size="sm" className="ml-1">
            <Filter /> Labels
          </Button>
        </div>

        <Card className="overflow-x-auto">
          <table className="w-full min-w-[820px] text-[13px]">
            <thead>
              <tr className="border-b bg-surface-2/60 text-left text-[11.5px] text-subtle">
                <th className="w-[28%] px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium">Status</th>
                <th className="px-4 py-2 font-medium">Size</th>
                <th className="px-4 py-2 font-medium">Addresses</th>
                <th className="px-4 py-2 font-medium">Host</th>
                <th className="px-4 py-2 font-medium">Created</th>
                <th className="w-10" />
              </tr>
            </thead>
            <tbody>
              {rows.map((m) => (
                <tr key={m.name} className="group border-b last:border-0 hover:bg-surface-2/60">
                  <td className="max-w-0 px-4 py-2.5">
                    <Link
                      to={kind === "Container" ? "/containers/$name" : "/vms/$name"}
                      params={{ name: m.name }}
                      className="block"
                    >
                      <span className="block truncate font-medium group-hover:text-accent">{m.name}</span>
                      <span className="block truncate font-mono text-[11.5px] text-subtle">{m.image}</span>
                    </Link>
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5">
                    <span className="inline-flex items-center gap-2">
                      <StatusDot phase={m.phase} />
                      {phaseLabel(m.phase)}
                    </span>
                    {m.phase === MachinePhase.FAILED && (
                      <span className="block text-[11.5px] text-fail">
                        {m.conditions.find((c) => c.type === "Ready")?.reason}
                      </span>
                    )}
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5">
                    <span className="flex items-center gap-2">
                      <SizeBadge size={sizes[m.size].label} />
                      <span className="text-muted tabular">
                        {sizes[m.size].cpu} vCPU · {sizes[m.size].memoryGi} GiB
                      </span>
                    </span>
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px]">
                    <span className="block">{m.interfaces[0]?.ipv4}</span>
                    <span className="block text-subtle">{m.interfaces[0]?.ipv6 ?? "no public IPv6"}</span>
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px] text-muted">{m.node}</td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-muted">{m.created.slice(0, 10)}</td>
                  <td className="px-2 py-2.5">
                    <Button variant="ghost" size="icon" aria-label="Actions">
                      <MoreHorizontal />
                    </Button>
                  </td>
                </tr>
              ))}
              {rows.length === 0 && (
                <tr>
                  <td colSpan={7} className="px-4 py-10 text-center text-muted">
                    Nothing matches.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </Card>
        <p className="mt-3 text-[12px] text-subtle">
          {rows.length} of {ofKind.length} · updates live
        </p>
        </>
        )}
      </div>
    </>
  );
}
