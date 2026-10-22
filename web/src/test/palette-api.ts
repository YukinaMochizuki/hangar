// What the palette's and the list routes' tests share: an in-memory API with
// objects of every kind in project demo, its projects, tenants and members,
// and TestPermissions. Each list can be replaced, to hold it loading or fail.

import { create } from "@bufbuild/protobuf";
import { ContainerService } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { MachinePhase, MachineService } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { MachineCapabilityService } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { VirtualMachineService } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { AccountService, WhoAmIResponseSchema } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { MemberService } from "@hangar/api/hangar/iam/v1alpha1/member_pb";
import { RoleBindingService } from "@hangar/api/hangar/iam/v1alpha1/role_binding_pb";
import { ServiceTokenService } from "@hangar/api/hangar/iam/v1alpha1/service_token_pb";
import { ServiceTokenIssuerService } from "@hangar/api/hangar/iam/v1alpha1/service_token_issuer_pb";
import { AddressService } from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { CustomDomainService } from "@hangar/api/hangar/network/v1alpha1/custom_domain_pb";
import { NetworkService } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { PortForwardService } from "@hangar/api/hangar/network/v1alpha1/port_forward_pb";
import { PublicEndpointService } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { SecurityGroupService } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { SecretService } from "@hangar/api/hangar/secrets/v1alpha1/secret_pb";
import { DiskService } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { FileShareService } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { ImageService } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { NameService } from "@hangar/api/hangar/tenancy/v1alpha1/names_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { fakeApi } from "@/test/harness";

const project = "demo";
const meta = (name: string) => ({ name, project });
const owned = (kind: string, name: string, phase: MachinePhase) => ({
  metadata: { name: `${kind === "Container" ? "c" : "vm"}-${name}`, project, ownerReferences: [{ kind, name, controller: true }] },
  status: { phase },
});

/** The lists the API answers, by the RPC that lists them; each may be replaced. */
export const lists = {
  listContainers: () => ({ items: ["web", "web-worker", "api-web"].map((n) => ({ metadata: meta(n), spec: { image: "nginx" } })) }),
  listVirtualMachines: () => ({ items: [{ metadata: meta("postgres"), spec: {} }] }),
  listMachines: () => ({
    items: [
      owned("Container", "web", MachinePhase.RUNNING),
      owned("Container", "web-worker", MachinePhase.RUNNING),
      owned("Container", "api-web", MachinePhase.STOPPED),
      owned("VirtualMachine", "postgres", MachinePhase.RUNNING),
    ],
  }),
  listDisks: () => ({ items: [{ metadata: meta("pg-data"), spec: { size: "100Gi" }, status: { size: "100Gi" } }] }),
  listFileShares: () => ({ items: [{ metadata: meta("assets"), spec: { networks: ["backend"], size: "50Gi" }, status: { addresses: [{ network: "backend", address: "10.1.0.7" }] } }] }),
  listImages: () => ({ items: [{ metadata: { name: "debian-13" }, spec: {} }] }),
  listNetworks: () => ({ items: [{ metadata: meta("backend"), spec: { ipv4Cidr: "10.1.0.0/24" } }] }),
  listSecurityGroups: () => ({ items: [{ metadata: meta("ssh-admin"), spec: { network: "backend", ingress: [{}, {}], egress: [{}] } }] }),
  listAddresses: () => ({ items: [{ metadata: meta("front-v6"), status: { ipv6: "2001:db8::10" } }] }),
  listPublicEndpoints: () => ({ items: [{ metadata: meta("site-https"), status: { hostname: "site.example.com" } }] }),
  listPortForwards: () => ({ items: [{ metadata: meta("postgres-ssh"), status: { ipv4: "198.51.100.3", port: 23817 } }] }),
  listSecrets: () => ({ items: [{ metadata: meta("registry-login"), status: { keys: [".dockerconfigjson"] } }] }),
  listServiceTokens: () => ({ items: [{ metadata: meta("ci-deploy") }] }),
  listRoleBindings: () => ({
    items: [{ metadata: { name: "b1" }, spec: { role: "project.editor", subject: { subject: { case: "serviceToken" as const, value: "demo/ci-deploy" } }, scope: { target: { case: "project" as const, value: "demo" } } } }],
  }),
  listMembers: (scope: string) =>
    scope === "tenant"
      ? { members: [{ email: "owner@example.com", username: "olga", role: "tenant.owner" }] }
      : { members: [{ email: "ada@example.com", username: "ada", role: "project.owner" }, { email: "grace@example.com", username: "", role: "project.viewer" }] },
  listProjects: () => ({
    items: [
      { metadata: { name: "demo" }, spec: { tenant: "acme", displayName: "Demo" } },
      { metadata: { name: "web-staging" }, spec: { tenant: "acme", displayName: "Staging" } },
      { metadata: { name: "lab-1" }, spec: { tenant: "zeta", displayName: "Edge lab" } },
    ],
  }),
  listCustomDomains: () => ({
    items: [
      { metadata: { name: "example-com" }, spec: { tenant: "acme", domain: "example.com" }, status: { conditions: [{ type: "Verified", status: 1 }] } },
      { metadata: { name: "zeta-example" }, spec: { tenant: "zeta", domain: "zeta.example" } },
    ],
  }),
  listTenants: () => ({ items: [{ metadata: { name: "acme" }, spec: { displayName: "Acme" } }, { metadata: { name: "zeta" }, spec: { displayName: "Zeta Research" } }] }),
};

type Lists = typeof lists;
type Answer<K extends keyof Lists> = ReturnType<Lists[K]>;

export type PaletteFake = {
  /** Replacements for lists: a promise that never settles holds one loading, a rejection makes it fail. */
  [K in keyof Lists]?: (...a: Parameters<Lists[K]>) => Answer<K> | Promise<Answer<K>>;
} & {
  /** What TestPermissions answers the caller holds; unset, every permission asked. */
  perms?: (scope: string) => string[] | Promise<string[]>;
};

/** paletteApi is the in-memory API, with what f replaces. */
export function paletteApi(f: PaletteFake = {}) {
  const l = <K extends keyof Lists>(k: K) => (f[k] ?? lists[k]) as (...a: Parameters<Lists[K]>) => Answer<K>;
  return fakeApi((r) => {
    r.service(AccountService, {
      whoAmI: () => create(WhoAmIResponseSchema, { member: true, type: "user", id: "ada@example.com", user: { metadata: { name: "ada" }, spec: { displayName: "Ada" } } }),
      testPermissions: async (req) => {
        const t = req.scope?.target;
        const scope = t?.case === "platform" ? "platform" : t?.case === "tenant" ? `tenant:${t.value}` : `project:${t?.value}`;
        const held = f.perms ? await f.perms(scope) : req.permissions;
        return { permissions: req.permissions.filter((p) => held.includes(p)) };
      },
    });
    r.service(ContainerService, { listContainers: () => l("listContainers")() });
    r.service(VirtualMachineService, { listVirtualMachines: () => l("listVirtualMachines")() });
    r.service(MachineService, { listMachines: () => l("listMachines")() });
    // One line of output, then the stream ends, for whatever machine is asked.
    r.service(MachineCapabilityService, {
      async *logs() {
        yield { records: [{ boot: 1n, content: { case: "data" as const, value: new TextEncoder().encode("listening on :8080\n") } }] };
      },
    });
    // Creates answer as the API does, with the object made.
    r.service(DiskService, { listDisks: () => l("listDisks")(), applyDisk: (req) => ({ disk: req.disk, created: true }) });
    r.service(FileShareService, {
      listFileShares: () => l("listFileShares")(),
      applyFileShare: (req) => ({ fileShare: req.fileShare, created: true }),
    });
    r.service(ImageService, { listImages: () => l("listImages")() });
    r.service(NetworkService, { listNetworks: () => l("listNetworks")(), applyNetwork: (req) => ({ network: req.network, created: true }) });
    r.service(SecurityGroupService, {
      listSecurityGroups: () => l("listSecurityGroups")(),
      applySecurityGroup: (req) => ({ securityGroup: req.securityGroup, created: true }),
    });
    r.service(AddressService, { listAddresses: () => l("listAddresses")(), applyAddress: (req) => ({ address: req.address, created: true }) });
    r.service(PublicEndpointService, {
      listPublicEndpoints: () => l("listPublicEndpoints")(),
      applyPublicEndpoint: (req) => ({ publicEndpoint: req.publicEndpoint, created: true }),
    });
    r.service(PortForwardService, {
      listPortForwards: () => l("listPortForwards")(),
      applyPortForward: (req) => ({ portForward: req.portForward, created: true }),
    });
    r.service(SecretService, { listSecrets: () => l("listSecrets")(), applySecret: (req) => ({ secret: req.secret, created: true }) });
    r.service(ServiceTokenService, { listServiceTokens: () => l("listServiceTokens")() });
    r.service(ServiceTokenIssuerService, {
      createServiceToken: (req) => ({ serviceToken: { metadata: { name: req.name, project: req.project } }, secret: "hgr_example" }),
    });
    r.service(RoleBindingService, { listRoleBindings: () => l("listRoleBindings")() });
    r.service(MemberService, { listMembers: (req) => l("listMembers")(req.scope?.target.case ?? ""), addMember: () => ({}) });
    r.service(ProjectService, {
      listProjects: () => l("listProjects")(),
      getProject: async (req) => ({ project: (await l("listProjects")()).items.find((p) => p.metadata.name === req.name) }),
    });
    r.service(TenantService, { listTenants: () => l("listTenants")() });
    r.service(CustomDomainService, { listCustomDomains: () => l("listCustomDomains")() });
    r.service(NameService, { checkName: () => ({ available: true }) });
  });
}
