// Whether a machine, or a change to one, takes its project past a limit,
// read from the project's status: its limits, the smaller of its own and its
// tenant's, against its own usage (docs/decisions/console-design.md).

import { useQuery } from "@connectrpc/connect-query";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { quantityCPU, quantityGiB } from "@/lib/quantity";

export type Over = { key: string; label: string; need: number; left: number; limit: number; unit: string };

/** What a machine, or a change to one, takes of each project limit. */
export type Need = { machines: number; cpu: number; memoryGi: number; diskGi: number };

/** The project limits the need would go past, from Project.status; a need of nothing passes whatever the usage. */
export function useOverQuota(project: string) {
  const q = useQuery(ProjectService.method.getProject, { name: project });
  const st = q.data?.project?.status;
  return (n: Need): Over[] => {
    if (!st) return [];
    const checks = [
      { key: "machines", label: "machines", used: st.usage?.machines ?? 0, limit: st.limits?.machines, need: n.machines, unit: "" },
      { key: "cpu", label: "vCPU", used: quantityCPU(st.usage?.cpu || "0"), limit: quantityCPU(st.limits?.cpu), need: n.cpu, unit: " vCPU" },
      { key: "memory", label: "memory", used: quantityGiB(st.usage?.memory || "0"), limit: quantityGiB(st.limits?.memory), need: n.memoryGi, unit: " GiB" },
      { key: "disk", label: "disk", used: quantityGiB(st.usage?.disk || "0"), limit: quantityGiB(st.limits?.disk), need: n.diskGi, unit: " GiB" },
    ];
    return checks
      .filter((c) => c.need > 0 && c.limit !== undefined && Number.isFinite(c.limit) && c.used + c.need > c.limit!)
      .map((c) => ({ key: c.key, label: c.label, need: c.need, left: Math.max(0, c.limit! - c.used), limit: c.limit!, unit: c.unit }));
  };
}
