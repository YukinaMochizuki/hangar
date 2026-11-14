// A project's own limits, its spec.limits, as the Edit limits dialog edits
// them: a budget inside its tenant's plan (docs/decisions/tenancy-and-iam.md).
// A field left empty sets no limit of the project's own, so the plan's holds.
// The plan alone limits projects, domains and machine calls, so they are not
// here.

import { clone, create, equals } from "@bufbuild/protobuf";
import { createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { LimitsSchema, type Limits, type Usage } from "@hangar/api/hangar/tenancy/v1alpha1/plan_pb";
import { ProjectSchema, ProjectService, type Project } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { isStale } from "@/lib/network-actions";
import { formatNumber, parseQuantity } from "@/lib/quantity";

/**
 * What changing a project's own limits takes, at its tenant: apply on
 * projects, the permission creating one takes too.
 */
export const setLimitsPermission = "tenancy.projects.apply";

export type LimitKey =
  | "cpu"
  | "memory"
  | "disk"
  | "machines"
  | "networks"
  | "addresses"
  | "publicEndpoints"
  | "portForwards"
  | "serviceTokens"
  | "fileShares"
  | "customImages"
  | "networkEndpoints";

export type LimitDim = {
  key: LimitKey;
  label: string;
  /** The flag of `hangar project set-limits`. */
  flag: string;
  /** cpu is in vCPUs, size in GiB, count a whole number. */
  kind: "cpu" | "size" | "count";
  unit?: string;
  /** The four that size machines, or the counted kinds. */
  group: "machines" | "other";
};

export const limitDims: LimitDim[] = [
  { key: "machines", label: "Machines", flag: "machines", kind: "count", group: "machines" },
  { key: "cpu", label: "vCPU", flag: "cpu", kind: "cpu", group: "machines" },
  { key: "memory", label: "Memory", flag: "memory", kind: "size", unit: "GiB", group: "machines" },
  { key: "disk", label: "Disk", flag: "disk", kind: "size", unit: "GiB", group: "machines" },
  { key: "networks", label: "Networks", flag: "networks", kind: "count", group: "other" },
  { key: "addresses", label: "Addresses", flag: "addresses", kind: "count", group: "other" },
  { key: "publicEndpoints", label: "Public endpoints", flag: "public-endpoints", kind: "count", group: "other" },
  { key: "portForwards", label: "Port forwards", flag: "port-forwards", kind: "count", group: "other" },
  { key: "serviceTokens", label: "Service tokens", flag: "service-tokens", kind: "count", group: "other" },
  { key: "fileShares", label: "File shares", flag: "file-shares", kind: "count", group: "other" },
  { key: "customImages", label: "Custom images", flag: "custom-images", kind: "count", group: "other" },
  { key: "networkEndpoints", label: "Network endpoints", flag: "network-endpoints", kind: "count", group: "other" },
];

/** What each field of the dialog holds, as typed: "" for no limit of the project's own. */
export type Draft = Record<LimitKey, string>;

const GiB = 2 ** 30;

/**
 * A limit as a number in the dialog's unit (vCPUs, GiB, a count);
 * undefined when it is not set, NaN when it does not parse.
 */
export function limitValue(dim: LimitDim, l: Limits | Usage | undefined): number | undefined {
  const v = l?.[dim.key];
  if (v === undefined || v === "") return undefined;
  if (typeof v === "number") return v;
  const n = parseQuantity(v);
  return dim.kind === "size" ? n / GiB : n;
}

/** The limits as the dialog's fields show them. */
export function draftOf(l: Limits | undefined): Draft {
  const out = {} as Draft;
  for (const d of limitDims) {
    const v = limitValue(d, l);
    // To three decimal places, so a size in GiB that is not whole, such as 1.5, shows as it is.
    out[d.key] = v === undefined ? "" : Number.isFinite(v) ? String(+v.toFixed(3)) : String(l?.[d.key] ?? "");
  }
  return out;
}

/** A field's text as the quantity or count the API takes, or why it is not one. */
function toLimit(dim: LimitDim, text: string): { value?: string | number; problem?: string } {
  const t = text.trim();
  if (!/^[0-9]+(\.[0-9]+)?$/.test(t)) return { problem: `${dim.label} must be a number.` };
  const n = Number(t);
  switch (dim.kind) {
    case "count":
      if (!Number.isInteger(n) || n > 2 ** 31 - 1) return { problem: `${dim.label} must be a whole number.` };
      return { value: n };
    case "cpu": {
      const milli = n * 1000;
      if (Math.abs(milli - Math.round(milli)) > 1e-6) return { problem: "vCPU is finer than a thousandth." };
      return { value: Number.isInteger(n) ? String(n) : `${Math.round(milli)}m` };
    }
    case "size": {
      const mi = n * 1024;
      if (Math.abs(mi - Math.round(mi)) > 1e-6) return { problem: `${dim.label} is finer than a MiB.` };
      return { value: Number.isInteger(n) ? `${n}Gi` : `${Math.round(mi)}Mi` };
    }
  }
}

export type LimitChange = { dim: LimitDim; from?: number; to?: number };

/**
 * The limits a draft amounts to, made on those it started from: a field not
 * changed keeps the text the API wrote, so nothing moves that was not
 * edited. With them, what changed, and why it cannot be applied, if it
 * cannot: a field that is no number, or a limit above the plan's, which the
 * API refuses. plan is undefined when the caller may not read it.
 */
export function readDraft(
  base: Limits | undefined,
  draft: Draft,
  plan: Limits | undefined,
): { limits: Limits; changes: LimitChange[]; problems: string[] } {
  const start = draftOf(base);
  const limits = base ? clone(LimitsSchema, base) : create(LimitsSchema);
  const changes: LimitChange[] = [];
  const problems: string[] = [];
  for (const d of limitDims) {
    const text = draft[d.key].trim();
    if (text === start[d.key]) continue;
    const from = limitValue(d, base);
    if (text === "") {
      (limits as Record<string, unknown>)[d.key] = undefined;
      changes.push({ dim: d, from });
      continue;
    }
    const { value, problem } = toLimit(d, text);
    if (problem) {
      problems.push(problem);
      continue;
    }
    (limits as Record<string, unknown>)[d.key] = value;
    const to = Number(text);
    changes.push({ dim: d, from, to });
    const cap = limitValue(d, plan);
    if (cap !== undefined && to > cap) problems.push(`${d.label} ${formatNumber(to)} is above the plan's ${formatNumber(cap)}${d.unit ? ` ${d.unit}` : ""}.`);
  }
  return { limits, changes, problems };
}

/** A change as the dialog states it: "vCPU 8 → 4", "Memory no limit → 8 GiB". */
export function changeText(c: LimitChange): string {
  const v = (n?: number) => (n === undefined ? "plan's" : `${formatNumber(n)}${c.dim.unit ? ` ${c.dim.unit}` : ""}`);
  return `${c.dim.label} ${v(c.from)} → ${v(c.to)}`;
}

/** The `hangar project set-limits` command that makes the same changes. */
export function setLimitsCommand(project: string, limits: Limits, changes: LimitChange[]): string {
  const set = changes.filter((c) => c.to !== undefined).map((c) => `--${c.dim.flag} ${limits[c.dim.key]}`);
  const unset = changes.filter((c) => c.to === undefined).map((c) => c.dim.flag);
  return ["hangar project set-limits", project, ...set, ...(unset.length ? [`--unset ${unset.join(",")}`] : [])].join(" ");
}

/** Whether a project has limits of its own, and how many. */
export function ownLimitCount(p: Project | undefined): number {
  return limitDims.filter((d) => limitValue(d, p?.spec?.limits) !== undefined).length;
}

/** How many times applyLimits reads the project anew before it gives up. */
const tries = 5;

/**
 * Applies the project as the dialog opened on it, with its own limits
 * alone replaced, at the resource version it was read at. The usage
 * controller writes a project's status whenever what its tenant holds
 * changes, which moves that version: when the project read anew has the
 * same own limits, the change is made on it instead. When they changed, the
 * API's Aborted stands, for the dialog to state.
 */
export async function applyLimits(t: Transport, opened: Project, limits: Limits): Promise<void> {
  const c = createClient(ProjectService, t);
  const name = opened.metadata?.name ?? "";
  const was = opened.spec?.limits ?? create(LimitsSchema);
  let cur = opened;
  for (let i = 0; ; i++) {
    // Without a version Apply is create-or-update, which would write over whatever is there.
    if (!cur.metadata?.resourceVersion) throw new Error(`${name} carries no resource version to apply at.`);
    const obj = clone(ProjectSchema, cur);
    if (obj.spec) obj.spec.limits = limits;
    try {
      await c.applyProject({ project: obj });
      return;
    } catch (err) {
      if (!isStale(err) || i + 1 >= tries) throw err;
      const fresh = (await c.getProject({ name })).project;
      if (!fresh || !equals(LimitsSchema, fresh.spec?.limits ?? create(LimitsSchema), was)) throw err;
      cur = fresh;
    }
  }
}

/** applyLimits as a mutation, every query asked again once it settles. */
export function useApplyLimits() {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ project, limits }: { project: Project; limits: Limits }) => applyLimits(transport, project, limits),
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** Reads the project as it is now, for the dialog to start again from. */
export function useReadProject(name: string) {
  const transport = useTransport();
  return useMutation({
    mutationFn: async (): Promise<Project> => {
      const p = (await createClient(ProjectService, transport).getProject({ name })).project;
      if (!p) throw new Error(`${name} is gone.`);
      return p;
    },
  });
}
