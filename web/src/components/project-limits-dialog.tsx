// Edit limits: a project's own limits, a budget inside its tenant's plan
// (lib/project-limits.ts). Opened from the project's Overview and from each
// project's row on its tenant's Overview, and drawn only to whoever holds
// apply on projects at the tenant, which the API requires of the change.

import { useQuery } from "@connectrpc/connect-query";
import type { Project } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { Gauge } from "lucide-react";
import { useState } from "react";
import { Dialog } from "@/components/dialog";
import { FormEquivalent, Input } from "@/components/form";
import { FormFooter, StaleNotice } from "@/components/resource";
import { Button } from "@/components/ui";
import { isStale } from "@/lib/network-actions";
import { useCan } from "@/lib/permissions";
import {
  changeText,
  draftOf,
  limitDims,
  limitValue,
  readDraft,
  setLimitsCommand,
  setLimitsPermission,
  useApplyLimits,
  useReadProject,
  type Draft,
  type LimitDim,
} from "@/lib/project-limits";
import { formatNumber } from "@/lib/quantity";

/** Whether the caller may change the own limits of the tenant's projects; false while unknown. */
export function useMaySetLimits(tenant: string | undefined): boolean {
  return useCan(tenant ? { level: "tenant", name: tenant } : undefined, [setLimitsPermission]).can(setLimitsPermission);
}

/** Edit limits, as a button that opens the dialog; nothing where the caller may not change them. */
export function EditLimitsButton({ project, size, label }: { project: Project | undefined; size?: "sm"; label?: string }) {
  // Asked only once the project, and so its tenant, is known.
  return project ? <GatedEditLimits project={project} size={size} label={label} /> : null;
}

function GatedEditLimits({ project, size, label = "Edit limits" }: { project: Project; size?: "sm"; label?: string }) {
  const [open, setOpen] = useState(false);
  const may = useMaySetLimits(project.spec?.tenant);
  if (!may) return null;
  return (
    <>
      <Button size={size} onClick={() => setOpen(true)} aria-label={`Edit limits of ${project.metadata?.name}`}>
        <Gauge /> {label}
      </Button>
      {open && <ProjectLimitsDialog project={project} onClose={() => setOpen(false)} />}
    </>
  );
}

function LimitRow({ dim, value, onChange, plan, planKnown, used }: { dim: LimitDim; value: string; onChange: (v: string) => void; plan?: number; planKnown: boolean; used: number }) {
  const unit = dim.unit ? ` ${dim.unit}` : "";
  return (
    <div className="grid grid-cols-[minmax(0,1fr)_120px] items-center gap-x-3 py-1.5">
      <div className="min-w-0">
        <label htmlFor={`limit-${dim.key}`} className="block text-[13px] text-foreground">
          {dim.label}
        </label>
        <span className="block font-mono text-[11px] text-subtle">
          uses {formatNumber(used)}
          {unit}
          {planKnown && ` · plan ${plan === undefined ? "no limit" : formatNumber(plan)}`}
        </span>
      </div>
      <div className="relative">
        <Input
          id={`limit-${dim.key}`}
          inputMode="decimal"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder="plan's"
          className={dim.unit ? "pr-10" : undefined}
        />
        {dim.unit && <span className="pointer-events-none absolute inset-y-0 right-2.5 flex items-center font-mono text-[11.5px] text-subtle">{dim.unit}</span>}
      </div>
    </div>
  );
}

/**
 * The dialog: one field a limit, empty for none of the project's own, each
 * beside what the project uses and the plan's limit. It applies the project
 * as it opened on, with spec.limits alone changed (lib/project-limits.ts,
 * applyLimits); when its own limits changed meanwhile, Reload starts it
 * again from them.
 */
export function ProjectLimitsDialog({ project, onClose }: { project: Project; onClose: () => void }) {
  const [base, setBase] = useState(project);
  const name = base.metadata?.name ?? "";
  const [draft, setDraft] = useState<Draft>(() => draftOf(base.spec?.limits));
  const tq = useQuery(TenantService.method.getTenant, { name: base.spec?.tenant ?? "" }, { retry: false });
  const planKnown = tq.isSuccess;
  const plan = tq.data?.tenant?.status?.limits;
  const apply = useApplyLimits();
  const reload = useReadProject(name);
  const stale = isStale(apply.error);
  const { limits, changes, problems } = readDraft(base.spec?.limits, draft, plan);
  const usage = base.status?.usage;
  const startAgain = () =>
    reload.mutate(undefined, {
      onSuccess: (fresh) => {
        setBase(fresh);
        setDraft(draftOf(fresh.spec?.limits));
        apply.reset();
      },
    });
  const rows = (group: LimitDim["group"]) =>
    limitDims
      .filter((d) => d.group === group)
      .map((d) => (
        <LimitRow
          key={d.key}
          dim={d}
          value={draft[d.key]}
          onChange={(v) => setDraft({ ...draft, [d.key]: v })}
          plan={limitValue(d, plan)}
          planKnown={planKnown}
          used={limitValue(d, usage) ?? 0}
        />
      ));
  return (
    <Dialog open onClose={onClose} title={`Limits of ${base.spec?.displayName || name}`} wide>
      <p>
        A budget for this project inside its tenant's plan. An empty field sets no limit of the project's own, so the plan's holds; the
        limits in effect are the smaller of the two.
      </p>
      <div className="mt-4 grid grid-cols-1 gap-x-8 md:grid-cols-2">
        <section aria-label="Machine limits">
          <h3 className="mb-1 text-[11.5px] font-medium uppercase tracking-wide text-subtle">Machines</h3>
          {rows("machines")}
        </section>
        <section aria-label="Other limits" className="mt-4 md:mt-0">
          <h3 className="mb-1 text-[11.5px] font-medium uppercase tracking-wide text-subtle">Other limits</h3>
          {rows("other")}
        </section>
      </div>
      <div className="mt-4 text-[12.5px] text-foreground">{changes.length ? changes.map(changeText).join(" · ") : "No change"}</div>
      {changes.length > 0 && <FormEquivalent cli={[setLimitsCommand(name, limits, changes)]} />}
      {stale && <StaleNotice title={`The limits of ${name} changed since this dialog opened.`} reloading={reload.isPending} error={reload.error} onReload={startAgain} />}
      <FormFooter
        problems={problems}
        blocked={changes.length === 0 || stale}
        error={stale ? null : apply.error}
        failed="Changing the limits failed"
        pending={apply.isPending}
        pendingLabel="Applying…"
        label={
          <>
            <Gauge /> Apply
          </>
        }
        onCancel={onClose}
        onSubmit={() => apply.mutate({ project: base, limits }, { onSuccess: onClose })}
      />
    </Dialog>
  );
}
