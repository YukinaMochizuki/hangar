// The projects the member can see, and which one the console is in.

import { useQuery } from "@connectrpc/connect-query";
import { ProjectService, type Project } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";

const lastProjectKey = "hangar.project";

export function rememberProject(name: string) {
  localStorage.setItem(lastProjectKey, name);
}

/** The project to open when none is named: the last one used, if it is still there. */
export function pickProject(projects: Project[]): Project | undefined {
  const last = localStorage.getItem(lastProjectKey);
  return projects.find((p) => p.metadata?.name === last) ?? projects[0];
}

export function useProjects() {
  const projects = useQuery(ProjectService.method.listProjects, { pageSize: 1000 }, { refetchInterval: 30_000 });
  // Tenant names are only shown; a member who may not list tenants still
  // sees every project, under its tenant's name.
  const tenants = useQuery(TenantService.method.listTenants, { pageSize: 1000 }, { retry: false });
  const items = [...(projects.data?.items ?? [])].sort((a, b) =>
    (a.spec?.displayName || a.metadata?.name || "").localeCompare(b.spec?.displayName || b.metadata?.name || ""),
  );
  const tenantName = (name: string) => tenants.data?.items.find((t) => t.metadata?.name === name)?.spec?.displayName || name;
  return { projects: items, loading: projects.isPending, error: projects.error, refetch: projects.refetch, tenantName };
}

/** A project's name for people: its display name, or its name. */
export function projectLabel(p: Project | undefined, fallback = ""): string {
  return p?.spec?.displayName || p?.metadata?.name || fallback;
}
