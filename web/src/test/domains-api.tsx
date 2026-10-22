// An in-memory API for the custom domain pages' tests: tenant acme with
// projects demo and shop, tenant zeta with one project; acme's domains
// example.com (verified, every project), example.org (waiting, shop only)
// and shop.example.net (held by another tenant, demo only), and one domain
// of zeta's. Endpoint web of demo serves at example.com and www.example.com;
// endpoint shop of shop waits at shop.example.org; docs of demo is private.
// Gets and Applies answer from a server copy that a test may make newer
// than the lists, as test/network-api.tsx does. Documentation names only.

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { ContainerSchema, ContainerService } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { MachineService } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { VirtualMachineService } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { CustomDomainSchema, CustomDomainService, type CustomDomain } from "@hangar/api/hangar/network/v1alpha1/custom_domain_pb";
import { PublicEndpointAccess, PublicEndpointSchema, PublicEndpointService, type PublicEndpoint } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { fakeApi } from "@/test/harness";
import { kindOn } from "@/test/network-api";

const verified = (ok: boolean, reason: string, message = "") => ({ type: "Verified", status: ok ? ConditionStatus.TRUE : ConditionStatus.FALSE, reason, message });

function domain(name: string, tenant: string, d: string, projects: string[], cond: ReturnType<typeof verified>, rv = 10n): CustomDomain {
  return create(CustomDomainSchema, {
    metadata: { name, uid: `cd-${name}`, resourceVersion: rv },
    spec: { tenant, domain: d, projects },
    status: {
      challengeTarget: `t-${name}.acme.example.net`,
      records: [
        { name: `_acme-challenge.${d}`, type: "CNAME", value: `t-${name}.acme.example.net`, purpose: "proves the domain is the tenant's" },
        { name: `*.${d}`, type: "CNAME", value: "cname.example.net", purpose: "brings the traffic to Hangar" },
      ],
      conditions: [cond],
    },
  });
}

export const domains = (): CustomDomain[] => [
  domain("example-com", "acme", "example.com", [], verified(true, "Verified")),
  domain("example-org", "acme", "example.org", ["shop"], verified(false, "ChallengeRecordMissing", "_acme-challenge.example.org must be a CNAME of t-example-org.acme.example.net")),
  domain("shop-example-net", "acme", "shop.example.net", ["demo"], verified(false, "HeldByAnotherTenant", "another tenant has verified example.net first")),
  domain("zeta-example", "zeta", "zeta.example", [], verified(true, "Verified")),
];

function endpoint(name: string, project: string, access: PublicEndpointAccess, hostnames: string[], serving: string[]): PublicEndpoint {
  return create(PublicEndpointSchema, {
    metadata: { name, project, uid: `ep-${name}`, resourceVersion: 20n },
    spec: { target: { object: { case: "container", value: "api" } }, port: 8080, access, hostnames },
    status: {
      hostname: `${name}-${project}.example.net`,
      url: `https://${name}-${project}.example.net`,
      hostnameUrls: serving.map((h) => `https://${h}`),
      conditions: hostnames.length && serving.length < hostnames.length ? [{ type: "Hostnames", status: ConditionStatus.FALSE, reason: "DomainNotVerified" }] : [],
    },
  });
}

export const endpoints = (): PublicEndpoint[] => [
  endpoint("web", "demo", PublicEndpointAccess.PUBLIC, ["example.com", "www.example.com"], ["example.com", "www.example.com"]),
  endpoint("docs", "demo", PublicEndpointAccess.PRIVATE, [], []),
  endpoint("shop", "shop", PublicEndpointAccess.PUBLIC, ["shop.example.org"], []),
];

const projects = [
  { metadata: { name: "demo" }, spec: { tenant: "acme", displayName: "Demo" } },
  { metadata: { name: "shop" }, spec: { tenant: "acme", displayName: "Shop" } },
  { metadata: { name: "lab" }, spec: { tenant: "zeta", displayName: "Lab" } },
];

/** Handlers a test replaces, by method name. */
export type DomainOverrides = Partial<{
  listCustomDomains: (req: { pageToken: string }) => unknown;
  listPublicEndpoints: (req: { project: string; pageToken: string }) => unknown;
  applyCustomDomain: (req: unknown) => unknown;
  deleteCustomDomain: (req: unknown) => unknown;
  /** TestPermissions' answer; unset, the caller holds every permission asked. */
  permissions: (req: { permissions: string[] }) => unknown;
  /** What the server holds, when it differs from the lists (newer, say). */
  server: Partial<{ domains: CustomDomain[]; endpoints: PublicEndpoint[] }>;
}>;

export function domainsApi(o: DomainOverrides = {}) {
  const server = { domains: o.server?.domains ?? domains(), endpoints: o.server?.endpoints ?? endpoints() };
  const cds = kindOn(CustomDomainSchema, server.domains);
  const eps = kindOn(PublicEndpointSchema, server.endpoints);
  const api = fakeApi((r) => {
    r.service(CustomDomainService, {
      listCustomDomains: ((req: { pageToken: string }) => (o.listCustomDomains ? o.listCustomDomains(req) : { items: domains() })) as never,
      getCustomDomain: (req) => ({ customDomain: cds.get(req.name) }),
      applyCustomDomain: ((req: { customDomain: CustomDomain; createOnly: boolean }) => {
        if (o.applyCustomDomain) return o.applyCustomDomain(req);
        const { stored, created } = cds.apply(req.customDomain, req.createOnly);
        return { customDomain: stored, created };
      }) as never,
      deleteCustomDomain: ((req: unknown) => (o.deleteCustomDomain ? o.deleteCustomDomain(req) : {})) as never,
    });
    r.service(PublicEndpointService, {
      // No project lists every project's, as the API lists what the caller may read.
      listPublicEndpoints: ((req: { project: string; pageToken: string }) =>
        o.listPublicEndpoints ? o.listPublicEndpoints(req) : { items: endpoints().filter((e) => !req.project || e.metadata?.project === req.project) }) as never,
      getPublicEndpoint: (req) => ({ publicEndpoint: eps.get(req.name) }),
      applyPublicEndpoint: ((req: { publicEndpoint: PublicEndpoint; createOnly: boolean }) => {
        const { stored, created } = eps.apply(req.publicEndpoint, req.createOnly);
        return { publicEndpoint: stored, created };
      }) as never,
    });
    r.service(ProjectService, { listProjects: () => ({ items: projects }) });
    r.service(TenantService, {
      listTenants: () => ({ items: [] }),
      getTenant: (req) => ({ tenant: { metadata: { name: req.name }, status: { limits: { domains: 5 }, usage: { domains: 3 } } } }),
    });
    r.service(ContainerService, { listContainers: () => ({ items: [create(ContainerSchema, { metadata: { name: "api", project: "demo" }, spec: { image: "nginx:1" } })] }) });
    r.service(VirtualMachineService, { listVirtualMachines: () => ({ items: [] }) });
    r.service(MachineService, { listMachines: () => ({ items: [] }) });
    r.service(AccountService, {
      testPermissions: ((req: { permissions: string[] }) => (o.permissions ? o.permissions(req) : { permissions: req.permissions })) as never,
    });
  });
  return Object.assign(api, { server });
}

export type DomainsApi = ReturnType<typeof domainsApi>;

export const refuse = (code: Code, msg: string) => () => {
  throw new ConnectError(msg, code);
};
