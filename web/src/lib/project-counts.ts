// How many of each kind a project holds, by the label of the kind's page
// (components/pages-nav.ts), for the sidebar. A count is undefined while its
// list loads, or when it failed or is not the caller's to read: nothing is
// shown rather than a wrong number. The lists are the pages' own queries, so
// a page and its count share one answer. Images are the platform's, not the
// project's, and Members lists only the project's own bindings, not those
// it inherits from its tenant, so neither is counted.

import { useSecrets, useServiceTokens } from "@/lib/credentials";
import { useFileShares } from "@/lib/file-shares";
import { useMachines } from "@/lib/machines";
import { useAddresses, useNetworks, usePortForwards, usePublicEndpoints, useSecurityGroups } from "@/lib/network";
import { useDisks } from "@/lib/storage";

type Counted = { loading: boolean; error: unknown };

const of = (l: Counted & { items: unknown[] }) => (l.loading || l.error ? undefined : l.items.length);

/** The project's count of each kind, by its page's label. */
export function useProjectCounts(project: string): Record<string, number | undefined> {
  const containers = useMachines(project, "Container");
  const vms = useMachines(project, "VirtualMachine");
  return {
    Containers: of(containers),
    "Virtual machines": of(vms),
    Disks: of(useDisks(project)),
    "File shares": of(useFileShares(project)),
    Networks: of(useNetworks(project)),
    "Security groups": of(useSecurityGroups(project)),
    Addresses: of(useAddresses(project)),
    "Public endpoints": of(usePublicEndpoints(project)),
    "Port forwards": of(usePortForwards(project)),
    Secrets: of(useSecrets(project)),
    "Service tokens": of(useServiceTokens(project)),
  };
}
