// A tenant at a glance: its plan, what all its projects hold against the
// plan's limits, and each project with what it holds. The tenant's own pages
// are apart from any project's (docs/decisions/console-design.md).

import { useQuery } from "@connectrpc/connect-query";
import { Link } from "@tanstack/react-router";
import type { Project } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { RowMenu } from "@/components/menu";
import { ProjectLimitsDialog, useMaySetLimits } from "@/components/project-limits-dialog";
import { limitDims, limitValue, ownLimitCount, type LimitKey } from "@/lib/project-limits";
import { OtherLimits, computeTiles, otherTiles, QuotaTiles } from "@/components/quota";
import { Topbar, useTenantName } from "@/components/shell";
import { ErrorNotice, Skeleton } from "@/components/states";
import { Button, Card, CardHeader, CopyButton } from "@/components/ui";
import { createProject, NewProjectDialog } from "@/components/project-picker";
import { useCan } from "@/lib/permissions";
import { Gauge, Plus } from "lucide-react";
import { useState } from "react";
import { isNotFound, isPermissionDenied } from "@/lib/api";
import { projectLabel, useProjects } from "@/lib/projects";
import { formatNumber } from "@/lib/quantity";

/** What the project uses of a limit that sizes machines, with its own limit after a slash when it sets one. */
function UsageCell({ p, dim }: { p: Project; dim: LimitKey }) {
  const d = limitDims.find((x) => x.key === dim)!;
  const used = limitValue(d, p.status?.usage) ?? 0;
  const own = limitValue(d, p.spec?.limits);
  const unit = d.unit ? ` ${d.unit}` : "";
  return (
    <td className="px-4 py-2.5 text-right font-mono tabular">
      {formatNumber(used)}
      {own === undefined ? unit : <span className="text-subtle">{` / ${formatNumber(own)}${unit}`}</span>}
    </td>
  );
}

/** A project's actions: Edit limits, where the caller may change them; the cell stays, empty, where it may not. */
function ProjectRowMenu({ p, may }: { p: Project; may: boolean }) {
  const [open, setOpen] = useState(false);
  if (!may) return <span className="block size-8" />;
  return (
    <RowMenu label={`Actions for ${p.metadata?.name}`} actions={[{ label: "Edit limits", icon: Gauge, onSelect: () => setOpen(true) }]}>
      {() => open && <ProjectLimitsDialog project={p} onClose={() => setOpen(false)} />}
    </RowMenu>
  );
}

function Projects({ tenant }: { tenant: string }) {
  const { projects, loading, error, refetch } = useProjects();
  const own = projects.filter((p) => p.spec?.tenant === tenant);
  const [creating, setCreating] = useState(false);
  const mayCreate = useCan({ level: "tenant", name: tenant }, [createProject]).can(createProject);
  const maySetLimits = useMaySetLimits(tenant);
  return (
    <Card className="overflow-x-auto">
      <CardHeader
        title={
          <span className="flex items-baseline gap-2">
            Projects
            {!loading && !error && <span className="font-mono text-[11.5px] font-normal text-subtle">{own.length}</span>}
          </span>
        }
        action={
          mayCreate && (
            <Button size="sm" onClick={() => setCreating(true)}>
              <Plus /> New project
            </Button>
          )
        }
      />
      <NewProjectDialog open={creating} tenant={tenant} onClose={() => setCreating(false)} />
      {error ? (
        <ErrorNotice error={error} title="The projects did not load" onRetry={() => void refetch()} />
      ) : (
        <table className="w-full min-w-[600px] text-[13px]">
          <thead>
            <tr className="border-b bg-surface-2/60 text-left text-[11.5px] text-subtle">
              <th className="px-4 py-2 font-medium">Project</th>
              <th className="px-4 py-2 text-right font-medium">Machines</th>
              <th className="px-4 py-2 text-right font-medium">vCPU</th>
              <th className="px-4 py-2 text-right font-medium">Memory</th>
              <th className="px-4 py-2 text-right font-medium">Disk</th>
              <th className="w-12 px-2 py-2" aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {own.map((p) => {
              const name = p.metadata?.name ?? "";
              return (
                <tr key={name} className="group border-b last:border-0 hover:bg-surface-2/60">
                  <td className="px-4 py-2.5">
                    <Link to="/projects/$project" params={{ project: name }} className="block">
                      <span className="block truncate font-medium group-hover:text-accent">{projectLabel(p)}</span>
                      <span className="block truncate font-mono text-[11.5px] text-subtle">{name}</span>
                    </Link>
                  </td>
                  <UsageCell p={p} dim="machines" />
                  <UsageCell p={p} dim="cpu" />
                  <UsageCell p={p} dim="memory" />
                  <UsageCell p={p} dim="disk" />
                  <td className="px-2 py-1.5">
                    <ProjectRowMenu p={p} may={maySetLimits} />
                  </td>
                </tr>
              );
            })}
            {!loading && own.length === 0 && (
              <tr>
                <td colSpan={6} className="px-4 py-8 text-center text-muted">
                  No projects
                </td>
              </tr>
            )}
          </tbody>
        </table>
      )}
      {own.some((p) => ownLimitCount(p) > 0) && (
        <div className="border-t px-4 py-2 text-[11.5px] text-subtle">
          A number after a slash is the project's own limit, inside the plan's; Edit limits in a project's menu changes them.
        </div>
      )}
    </Card>
  );
}

export function TenantOverview() {
  const tenant = useTenantName();
  const q = useQuery(TenantService.method.getTenant, { name: tenant }, { refetchInterval: 15_000, retry: false });
  const t = q.data?.tenant;
  const missing = !!q.error && isNotFound(q.error);
  const denied = !!q.error && isPermissionDenied(q.error);
  const hidden = missing || denied;
  const { tenantName } = useProjects();

  return (
    <>
      <Topbar crumbs={["Overview"]} />
      <div className="flex items-end justify-between gap-6 px-4 pb-5 pt-6 md:px-8">
        <div className="min-w-0">
          {q.isPending ? (
            <Skeleton className="h-7 w-40" />
          ) : (
            <h1 className="truncate text-[22px] font-semibold tracking-tight">{t?.spec?.displayName || tenantName(tenant)}</h1>
          )}
          <div className="mt-1 flex items-center gap-1 font-mono text-[12px] text-muted">
            tenant {tenant}
            <CopyButton text={tenant} />
            {t?.status?.plan && <span className="ml-2 font-sans">Plan {t.status.plan}</span>}
          </div>
        </div>
      </div>
      <div className="space-y-5 px-4 pb-10 md:px-8">
        {missing ? (
          <p className="text-[13px] text-muted">
            No tenant named <span className="font-mono">{tenant}</span> is visible to this account.
          </p>
        ) : denied ? (
          <p className="text-[13px] text-muted">
            This account's roles do not include reading the tenant's plan and limits.
          </p>
        ) : q.error ? (
          <ErrorNotice error={q.error} title="The tenant did not load" onRetry={() => void q.refetch()} className="p-0" />
        ) : (
          <QuotaTiles tiles={computeTiles(t?.status?.limits, t?.status?.usage)} loading={q.isPending} />
        )}
        <div className="grid grid-cols-1 gap-5 xl:grid-cols-[minmax(0,1fr)_380px]">
          <div className="min-w-0">
            <Projects tenant={tenant} />
          </div>
          {!hidden && !q.error && <OtherLimits tiles={otherTiles(t?.status?.limits, t?.status?.usage, true)} loading={q.isPending} />}
        </div>
      </div>
    </>
  );
}
