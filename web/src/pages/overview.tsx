// A project at a glance: what it holds against its limits, its machines,
// the host they run on, and what changed last.

import { useQuery } from "@connectrpc/connect-query";
import { Link } from "@tanstack/react-router";
import type { Condition } from "@hangar/api/hangar/meta/v1/meta_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { ArrowRight, Plus } from "lucide-react";
import { ChassisArt } from "@/components/chassis-art";
import { PageBanner } from "@/components/notice";
import { computeTiles, OtherLimits, otherTiles, QuotaTiles } from "@/components/quota";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, Skeleton, SkeletonRows } from "@/components/states";
import { buttonVariants, Card, CardHeader, CopyButton, SizeBadge, StatusDot, phaseLabel } from "@/components/ui";
import { isNotFound, isPermissionDenied } from "@/lib/api";
import { cn } from "@/lib/cn";
import { ago, date, isLive, machinePath, sizeOf, useMachines, type MachineView } from "@/lib/machines";
import { hostFor } from "@/lib/placement";
import { formatNumber } from "@/lib/quantity";

function MachineRow({ m, project }: { m: MachineView; project: string }) {
  const size = sizeOf(m);
  return (
    <li>
      <Link
        to={machinePath(m.kind)}
        params={{ project, name: m.name }}
        className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-4 px-4 py-2.5 text-[13px] hover:bg-surface-2 md:grid-cols-[minmax(0,1fr)_88px_150px_96px]"
      >
        <span className="flex min-w-0 items-center gap-2.5">
          <StatusDot phase={m.phase} />
          <span className="shrink-0 font-medium">{m.name}</span>
          <span className="hidden truncate font-mono text-[11.5px] text-subtle md:block">{m.image}</span>
        </span>
        <span className="text-muted">{m.deleting ? "Deleting" : phaseLabel(m.phase)}</span>
        <span className="hidden items-center gap-2 text-muted md:flex">
          {size && <SizeBadge size={size.label} />}
          <span className="tabular">
            {formatNumber(m.cpu)} vCPU · {formatNumber(m.memoryGi)} GiB
          </span>
        </span>
        <span className="hidden text-right text-[12px] text-subtle md:block">{m.kind === "Container" ? "Container" : "VM"}</span>
      </Link>
    </li>
  );
}

function MachineTable({ project }: { project: string }) {
  const { items, loading, error, refetch } = useMachines(project);
  return (
    <Card>
      <CardHeader
        title="Machines"
        action={
          items.length > 0 && (
            <Link
              to="/projects/$project/containers"
              params={{ project }}
              className="flex items-center gap-1 text-[12px] text-muted hover:text-foreground"
            >
              All containers <ArrowRight className="size-3" />
            </Link>
          )
        }
      />
      {loading ? (
        <SkeletonRows rows={4} />
      ) : error ? (
        <ErrorNotice error={error} title="The machines did not load" onRetry={refetch} />
      ) : items.length === 0 ? (
        <EmptyState
          title="No machines"
          action={
            <Link to="/projects/$project/new" params={{ project }} className={buttonVariants({ variant: "primary" })}>
              <Plus /> New machine
            </Link>
          }
        >
          This project runs no containers and no virtual machines.
        </EmptyState>
      ) : (
        <ul className="divide-y">
          {items.map((m) => (
            <MachineRow key={`${m.kind}/${m.name}`} m={m} project={project} />
          ))}
        </ul>
      )}
    </Card>
  );
}

/** The host most of the project's machines run on, with what they take there. */
function HostCard({ project }: { project: string }) {
  const { items, loading } = useMachines(project);
  if (loading) return null;
  const counts = new Map<string, number>();
  for (const m of items) if (m.node) counts.set(m.node, (counts.get(m.node) ?? 0) + 1);
  const nodes = [...counts.entries()].sort((a, b) => b[1] - a[1]);
  if (nodes.length === 0) return null;
  // The host as any of the project's machines on it describes it; none has
  // a placement until the apiserver copies the node's.
  const mine = items.filter((m) => m.node === nodes[0][0]);
  const host = mine.map(hostFor).find((h) => h !== undefined);
  if (!host) return null;
  const live = mine.filter(isLive);
  const cpu = live.reduce((n, m) => n + (Number.isFinite(m.cpu) ? m.cpu : 0), 0);
  const mem = live.reduce((n, m) => n + (Number.isFinite(m.memoryGi) ? m.memoryGi : 0), 0);
  return (
    <Card className="overflow-hidden">
      <CardHeader title="Your machines run on" action={<span className="font-mono text-[12px] text-muted">{host.node}</span>} />
      <div className="bg-[radial-gradient(ellipse_at_50%_0%,var(--accent-soft),transparent_70%)] px-4 pb-2 pt-5 md:px-8">
        {/* The host itself, not any one machine: its LEDs show it serving. */}
        <ChassisArt chassis={host.art} status="running" />
      </div>
      <div className="border-t px-4 py-3">
        <div className="text-[14px] font-medium">{host.cpuModel || host.node}</div>
        {(host.region || host.site) && (
          <div className="text-[12.5px] text-muted">{[host.region, host.site].filter(Boolean).join(" · ")}</div>
        )}
        {nodes.length > 1 && (
          <div className="mt-1 text-[12px] text-subtle">
            {nodes.length - 1} more {nodes.length === 2 ? "host runs" : "hosts run"} machines of this project.
          </div>
        )}
      </div>
      <div className="grid grid-cols-3 border-t text-center">
        {[
          [`${live.length}`, "running"],
          [formatNumber(cpu), "vCPU"],
          [`${formatNumber(mem)} GiB`, "memory"],
        ].map(([v, l], i) => (
          <div key={l} className={cn("py-2.5", i > 0 && "border-l")}>
            <div className="font-mono text-[14px] font-medium tabular">{v}</div>
            <div className="text-[11px] text-muted">{l}</div>
          </div>
        ))}
      </div>
    </Card>
  );
}

const statusWord: Record<number, string> = {
  [ConditionStatus.TRUE]: "True",
  [ConditionStatus.FALSE]: "False",
  [ConditionStatus.UNKNOWN]: "Unknown",
};

function conditionTone(c: Condition): string {
  if (c.type === "Ready") return c.status === ConditionStatus.TRUE ? "bg-ok" : c.reason === "Stopped" ? "bg-off" : "bg-pending";
  return c.status === ConditionStatus.UNKNOWN ? "bg-pending" : "bg-off";
}

/** The latest condition changes of the project's machines, newest first. */
function RecentChanges({ project }: { project: string }) {
  const { items, loading, error, refetch } = useMachines(project);
  const changes = items
    .flatMap((m) => m.conditions.map((c) => ({ m, c, at: date(c.lastTransitionTime) })))
    .filter((x) => x.at)
    .sort((a, b) => b.at!.getTime() - a.at!.getTime())
    // A machine's condition shows once, though its object repeats it.
    .filter((x, i, all) => all.findIndex((y) => y.m === x.m && y.c.type === x.c.type) === i)
    .slice(0, 6);
  return (
    <Card>
      <CardHeader title="Recent changes" />
      {loading ? (
        <SkeletonRows rows={3} />
      ) : error ? (
        <ErrorNotice error={error} title="The changes did not load" onRetry={refetch} />
      ) : changes.length === 0 ? (
        <div className="px-4 py-3 text-[13px] text-muted">No changes recorded.</div>
      ) : (
        <ul className="divide-y">
          {changes.map(({ m, c, at }) => (
            <li key={`${m.kind}/${m.name}/${c.type}`} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
              <span className={cn("size-1.5 shrink-0 rounded-full", conditionTone(c))} />
              <span className="min-w-0 flex-1 truncate">
                <span className="font-mono text-[12px]">
                  {m.kind === "Container" ? "container" : "vm"}/{m.name}
                </span>{" "}
                <span className="text-muted">
                  {c.type} {statusWord[c.status] ?? "?"}
                  {c.reason && ` · ${c.reason}`}
                </span>
              </span>
              <span className="shrink-0 text-[12px] text-subtle">{ago(at)} ago</span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}


function FromTheCli({ project }: { project: string }) {
  const lines = [`hangar login`, `hangar get containers -p ${project}`];
  return (
    <Card className="overflow-hidden">
      <CardHeader title="From the CLI" action={<CopyButton text={lines.join("\n")} />} />
      <pre className="overflow-x-auto px-4 py-3 font-mono text-[12px] leading-6">
        {lines.map((l) => (
          <div key={l}>
            <span className="select-none text-accent">$ </span>
            {l}
          </div>
        ))}
      </pre>
    </Card>
  );
}

/**
 * A page banner for each host that has stopped reporting, which the
 * apiserver writes into the Ready condition of every container and VM on it.
 */
function SilentHosts({ project }: { project: string }) {
  const { items } = useMachines(project);
  const silent = new Map<string, { count: number; since?: Date }>();
  for (const m of items) {
    const own = m.container?.status?.conditions ?? m.vm?.status?.conditions ?? [];
    const c = own.find((x) => x.type === "Ready" && x.reason === "NodeNotReporting");
    if (!c || !m.node) continue;
    const s = silent.get(m.node) ?? { count: 0, since: date(c.lastTransitionTime) };
    s.count++;
    silent.set(m.node, s);
  }
  return (
    <>
      {[...silent.entries()].map(([node, s]) => (
        <PageBanner key={node} tone="warning" title={`${node} stopped reporting${s.since ? ` ${ago(s.since)} ago` : ""}`}>
          {s.count === 1 ? "The machine" : `The ${s.count} machines`} of this project on it may still run, but
          {s.count === 1 ? " its" : " their"} status is not current.
        </PageBanner>
      ))}
    </>
  );
}

export function Overview() {
  const project = useProjectName();
  const q = useQuery(ProjectService.method.getProject, { name: project }, { refetchInterval: 10_000 });
  const p = q.data?.project;

  // Either answer means this member sees no project of that name.
  if (q.error && (isNotFound(q.error) || isPermissionDenied(q.error))) {
    return (
      <>
        <Topbar crumbs={["Overview"]} />
        <EmptyState title="No such project">
          No project named <span className="font-mono">{project}</span> is visible to this account.
        </EmptyState>
      </>
    );
  }

  return (
    <>
      <Topbar crumbs={["Overview"]} />
      {p?.metadata?.deleteTime && (
        <PageBanner tone="warning" title="This project is being deleted">
          Everything in it is deleted with it; the project goes once it is empty.
        </PageBanner>
      )}
      <SilentHosts project={project} />
      {q.error && <ErrorNotice error={q.error} title="The project did not load" onRetry={() => void q.refetch()} className="px-4 pb-0 md:px-8" />}
      <div className="flex items-end justify-between gap-6 px-4 pb-5 pt-6 md:px-8">
        <div className="min-w-0">
          {q.isPending ? (
            <Skeleton className="h-7 w-40" />
          ) : (
            <h1 className="truncate text-[22px] font-semibold tracking-tight">{p?.spec?.displayName || project}</h1>
          )}
          <div className="mt-1 flex items-center gap-1 font-mono text-[12px] text-muted">
            {project}
            <CopyButton text={project} />
          </div>
        </div>
        <Link to="/projects/$project/new" params={{ project }} className={buttonVariants({ variant: "primary" })}>
          <Plus /> New machine
        </Link>
      </div>
      <div className="space-y-5 px-4 pb-10 md:px-8">
        <QuotaTiles tiles={computeTiles(p?.status?.limits, p?.status?.usage)} loading={q.isPending} />
        <div className="grid grid-cols-1 gap-5 xl:grid-cols-[minmax(0,1fr)_380px]">
          <div className="min-w-0 space-y-5">
            <MachineTable project={project} />
            <RecentChanges project={project} />
          </div>
          <div className="space-y-5">
            <HostCard project={project} />
            <OtherLimits tiles={otherTiles(p?.status?.limits, p?.status?.usage)} loading={q.isPending} />
            <FromTheCli project={project} />
          </div>
        </div>
      </div>
    </>
  );
}
