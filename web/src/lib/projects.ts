// The projects the member can see, and which one the console is in.

import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { NameService } from "@hangar/api/hangar/tenancy/v1alpha1/names_pb";
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
  const tenant = (name: string) => tenants.data?.items.find((t) => t.metadata?.name === name);
  // Every tenant the member sees: those it may list, and those of its
  // projects, so a tenant with no project is there too.
  const tenantNames = [
    ...new Set([...(tenants.data?.items ?? []).map((t) => t.metadata?.name ?? ""), ...items.map((p) => p.spec?.tenant ?? "")]),
  ]
    .filter(Boolean)
    .sort((a, b) => tenantName(a).localeCompare(tenantName(b)));
  return { projects: items, loading: projects.isPending, error: projects.error, refetch: projects.refetch, tenantName, tenant, tenantNames };
}

// The longest part of a suggested name taken from the display name, as the
// CLI's `hangar create` takes it, so a name and its suffix fit a hostname
// label with room to spare.
const maxSlug = 20;

/** The start of a project name made from a display name, as `hangar create` makes it. */
export function slug(s: string): string {
  let out = "";
  let dash = false;
  for (const c of s.toLowerCase().normalize("NFKD").replace(/\p{Mn}/gu, "")) {
    if (out.length >= maxSlug) break;
    if (/[a-z0-9]/.test(c)) {
      if (dash && out) out += "-";
      dash = false;
      out += c;
    } else {
      dash = true;
    }
  }
  return out.slice(0, maxSlug).replace(/-+$/, "");
}

/** Six random digits, the end of a suggested name such as my-demo-438212. */
export function randomSuffix(): string {
  return String(crypto.getRandomValues(new Uint32Array(1))[0] % 1_000_000).padStart(6, "0");
}

/** Whether a project name is free: never used, and not reserved. */
export function useNameFree(name: string) {
  const valid = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(name);
  const q = useQuery(NameService.method.checkName, { kind: "Project", name }, { enabled: valid, staleTime: 5_000, retry: false });
  return { valid, free: valid ? q.data?.available : undefined, checking: valid && q.isPending, error: valid ? q.error : null };
}

/**
 * Creates a project under a name checked to be free, with a create-only
 * apply: when someone took the name in between, the API refuses and their
 * project is left as it was.
 */
export function useCreateProject() {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (p: { name: string; tenant: string; displayName: string }) => {
      const c = createClient(ProjectService, transport);
      try {
        const res = await c.applyProject({
          project: { apiVersion: "tenancy.hangarlab.cloud/v1alpha1", kind: "Project", metadata: { name: p.name }, spec: { tenant: p.tenant, displayName: p.displayName } },
          createOnly: true,
        });
        return res.project;
      } catch (err) {
        if (err instanceof ConnectError && err.code === Code.AlreadyExists) {
          throw new ConnectError(`${p.name} was taken after its name was checked.`, Code.AlreadyExists);
        }
        throw err;
      }
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** A project's name for people: its display name, or its name. */
export function projectLabel(p: Project | undefined, fallback = ""): string {
  return p?.spec?.displayName || p?.metadata?.name || fallback;
}
