// Custom domains as the console shows them (docs/decisions/custom-domains.md):
// a tenant's domain, whether it is verified and why not, the projects that
// may serve under it, and an endpoint's hostnames under one; and what a
// member does to them.

import { Code, ConnectError, createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { CustomDomainService, type CustomDomain } from "@hangar/api/hangar/network/v1alpha1/custom_domain_pb";
import { CustomDomainCheckService } from "@hangar/api/hangar/network/v1alpha1/custom_domain_check_pb";
import { PublicEndpointService, type PublicEndpoint } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import type { Tone } from "@/components/ui";
import { refetchInterval, type ListOpts } from "@/lib/machines";
import { conditionOf, nameOf, type ListState } from "@/lib/network";
import { useCan } from "@/lib/permissions";
import { useProjects } from "@/lib/projects";

/** What a domain's controls need at its tenant: New, Edit projects and labels (apply), and Delete. */
export const domainPerms = { apply: "network.customdomains.apply", delete: "network.customdomains.delete" } as const;

/** domainPerms, asked of TestPermissions at the tenant, once for a page and its rows. */
export function useDomainCan(tenant: string) {
  return useCan({ level: "tenant", name: tenant }, [domainPerms.apply, domainPerms.delete]);
}

const pageSize = 1000;

/**
 * Every page of a list. A list across projects or of a global kind is cut
 * into pages before the API leaves out what the caller may not read, so a
 * page may come back short, or empty, with more after it.
 */
async function everyPage<T>(page: (token: string) => Promise<{ items: T[]; nextPageToken: string }>): Promise<T[]> {
  const out: T[] = [];
  let token = "";
  do {
    const res = await page(token);
    out.push(...res.items);
    token = res.nextPageToken;
  } while (token);
  return out;
}

const listDomains = (t: Transport) => {
  const c = createClient(CustomDomainService, t);
  return everyPage((pageToken) => c.listCustomDomains({ pageSize, pageToken }));
};

const listEndpoints = (t: Transport) => {
  const c = createClient(PublicEndpointService, t);
  return everyPage((pageToken) => c.listPublicEndpoints({ project: "", pageSize, pageToken }));
};

/** The query keys of the two lists, which every page and the palette share. */
export const domainListKey = ["hangar", "customDomains"] as const;
export const endpointListKey = ["hangar", "publicEndpoints", "every project"] as const;

const byDomain = (a: CustomDomain, b: CustomDomain) => domainOf(a).localeCompare(domainOf(b));

function listState<T>(q: { data?: T[]; isPending: boolean; error: unknown; refetch: () => unknown }, keep: (x: T) => boolean = () => true): ListState<T> {
  return { items: (q.data ?? []).filter(keep), loading: q.isPending, error: q.error ?? null, refetch: () => void q.refetch() };
}

/**
 * The custom domains the caller may read: a tenant's for whoever reads the
 * tenant, and those that let a project for that project's people.
 */
function useReadableDomains(opts: ListOpts = {}) {
  const transport = useTransport();
  return useQuery({ queryKey: domainListKey, queryFn: () => listDomains(transport).then((items) => items.sort(byDomain)), refetchInterval, ...opts });
}

/** The tenant's custom domains, by domain. */
export function useCustomDomains(tenant: string, opts: ListOpts = {}): ListState<CustomDomain> {
  return listState(useReadableDomains(opts), (d) => d.spec?.tenant === tenant);
}

/**
 * The custom domains a project's endpoints may serve under: those of its
 * tenant that let it. Until the project's tenant is known, none.
 */
export function useProjectDomains(project: string): ListState<CustomDomain> {
  const q = useReadableDomains();
  const { projects, loading, error } = useProjects();
  const tenant = projects.find((p) => p.metadata?.name === project)?.spec?.tenant;
  const s = listState(q, (d) => d.spec?.tenant === tenant && allows(d, project));
  return { ...s, loading: s.loading || loading, error: s.error ?? error ?? null };
}

/**
 * The public endpoints of the tenant's projects the caller may read, from
 * one list across every project, each known to be the tenant's by its
 * project.
 */
export function useTenantEndpoints(tenant: string, opts: ListOpts = {}): ListState<PublicEndpoint> {
  const transport = useTransport();
  const q = useQuery({ queryKey: endpointListKey, queryFn: () => listEndpoints(transport), refetchInterval, ...opts });
  const { projects, loading, error } = useProjects();
  const ofTenant = new Set(projects.filter((p) => p.spec?.tenant === tenant).map((p) => p.metadata?.name ?? ""));
  const s = listState(q, (e) => ofTenant.has(e.metadata?.project ?? ""));
  return { ...s, loading: s.loading || loading, error: s.error ?? error ?? null };
}

const taken = (name: string) => `A custom domain named ${name} exists. Names are unique on the platform.`;

/**
 * Creates the custom domain handed to mutate with one create-only Apply. Its
 * name is unique across every tenant, and no name check covers the kind, so
 * a name taken, by another tenant too, is known only from this refusal.
 */
export function useCreateDomain() {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (d: CustomDomain) => {
      try {
        await createClient(CustomDomainService, transport).applyCustomDomain({ customDomain: d, createOnly: true });
      } catch (err) {
        if (err instanceof ConnectError && err.code === Code.AlreadyExists) throw new ConnectError(taken(d.metadata?.name ?? ""), Code.AlreadyExists);
        throw err;
      }
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** Deletes the custom domain; the API refuses while an endpoint serves under it. */
export function useDeleteDomain(name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () => createClient(CustomDomainService, transport).deleteCustomDomain({ name }),
    // Not awaited: the page that deleted it goes on to the list first.
    onSettled: () => void queryClient.invalidateQueries(),
  });
}

/**
 * Looks for the domain's challenge record now, at its own nameservers, and
 * puts the domain the API answers with into the list the pages read, so
 * the page shows what was found without waiting for the list's next read.
 */
export function useCheckDomain(name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => (await createClient(CustomDomainCheckService, transport).checkCustomDomain({ name })).customDomain,
    onSuccess: (d) => {
      if (!d) return;
      queryClient.setQueryData<CustomDomain[]>(domainListKey, (items) => items?.map((x) => (nameOf(x) === nameOf(d) ? d : x)));
    },
  });
}

export const domainOf = (d: CustomDomain) => d.spec?.domain ?? "";

/** Whether a project's endpoints may serve under d: an empty list lets every project of the tenant. */
export const allows = (d: CustomDomain, project: string) => (d.spec?.projects.length ?? 0) === 0 || !!d.spec?.projects.includes(project);

export type DomainState = { label: string; tone: Tone; reason?: string; message?: string };

/** Whether d is verified, and when it is not, why, as its Verified condition says. */
export function domainState(d: CustomDomain): DomainState {
  const c = conditionOf(d.status?.conditions, "Verified");
  if (c?.status === ConditionStatus.TRUE) return { label: "Verified", tone: "ok" };
  const why = { reason: c?.reason, message: c?.message };
  switch (c?.reason) {
    case "HeldByAnotherTenant":
      return { label: "Held by another tenant", tone: "fail", ...why };
    case "LookupFailed":
      return { label: "Lookup failed", tone: "pending", ...why };
    default:
      return { label: "Waiting for DNS", tone: "pending", ...why };
  }
}

export const isVerified = (d: CustomDomain) => domainState(d).tone === "ok";

/**
 * The domain of `domains` a hostname is under, as the apiserver finds it
 * (customdomain.Of): the domain itself or one label below it, the longest
 * when two serve it.
 */
export function domainFor(hostname: string, domains: CustomDomain[]): CustomDomain | undefined {
  return domains
    .filter((d) => {
      const apex = domainOf(d);
      if (hostname === apex) return true;
      if (!hostname.endsWith(`.${apex}`)) return false;
      return !hostname.slice(0, -apex.length - 1).includes(".");
    })
    .sort((a, b) => domainOf(b).length - domainOf(a).length)[0];
}

/** One endpoint's hostname under a domain. */
export type DomainUse = { endpoint: PublicEndpoint; hostname: string; serving: boolean };

/** The hostnames of endpoints that are under d, among the tenant's domains. */
export function usesOf(d: CustomDomain, domains: CustomDomain[], endpoints: PublicEndpoint[]): DomainUse[] {
  return endpoints.flatMap((e) =>
    (e.spec?.hostnames ?? [])
      .filter((h) => nameOf(domainFor(h, domains) ?? {}) === nameOf(d))
      .map((h) => ({ endpoint: e, hostname: h, serving: !!e.status?.hostnameUrls.includes(`https://${h}`) })),
  );
}

/** The CustomDomain name suggested for a domain: its dots made dashes, as a DNS label. */
export function suggestName(domain: string): string {
  return domain
    .toLowerCase()
    .replace(/\.+$/, "")
    .replace(/\./g, "-")
    .replace(/[^a-z0-9-]/g, "")
    .replace(/^-+/, "")
    .slice(0, 63)
    .replace(/-+$/, "");
}

const label = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** Why a domain as typed is not one the API takes, judged by its form alone; undefined when it may be. */
export function domainProblem(domain: string): string | undefined {
  if (!domain) return "The domain is empty.";
  if (domain !== domain.toLowerCase()) return "The domain is not in lower case.";
  if (domain.endsWith(".")) return "The domain ends in a dot.";
  const labels = domain.split(".");
  if (labels.length < 2 || !labels.every((l) => label.test(l)) || domain.length > 253) {
    return "The domain is not a hostname such as example.com.";
  }
  return undefined;
}

/** A custom hostname as its form holds it: the label below the domain, "" for the domain itself. */
export type HostnameRow = { label: string; domain: string };

export const hostnameOf = (r: HostnameRow) => (r.label ? `${r.label}.${r.domain}` : r.domain);

/** An endpoint's spec.hostnames as rows, each under the domain that serves it. */
export function rowsOf(hostnames: string[], domains: CustomDomain[]): HostnameRow[] {
  return hostnames.map((h) => {
    const d = domainFor(h, domains);
    const apex = d ? domainOf(d) : h.slice(h.indexOf(".") + 1);
    return { label: h === apex ? "" : h.slice(0, -apex.length - 1), domain: apex };
  });
}

/** Why the rows cannot be sent, each a fact. */
export function rowProblems(rows: HostnameRow[]): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const r of rows) {
    if (!r.domain) {
      out.push("A hostname has no domain chosen.");
      continue;
    }
    if (r.label && !label.test(r.label)) out.push(`${r.label} is not a DNS label: lowercase letters, digits and “-”, at most 63.`);
    const h = hostnameOf(r);
    if (seen.has(h)) out.push(`${h} is named twice.`);
    seen.add(h);
  }
  return out;
}
