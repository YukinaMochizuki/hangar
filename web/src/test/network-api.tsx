// An in-memory API for the network pages' tests: two networks, a security
// group, two addresses, two public endpoints (one a container's), and a
// container whose Machine is on the default network. Gets and Applies
// answer from a server copy of the objects, which a test may make newer
// than the lists; an Apply carrying another resource version than the
// server's is refused with Aborted, as the apiserver does. A test replaces
// any method by passing its own handler. Documentation addresses only.

import { clone, create, type DescMessage, type MessageShape } from "@bufbuild/protobuf";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";
import { ContainerSchema, ContainerService } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { MachinePhase, MachineSchema, MachineService } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { VirtualMachineService } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { ConditionStatus, type ObjectMeta } from "@hangar/api/hangar/meta/v1/meta_pb";
import { AddressSchema, AddressService, type Address } from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { NetworkSchema, NetworkService, type Network } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import {
  PublicEndpointAccess,
  PublicEndpointSchema,
  PublicEndpointService,
  type PublicEndpoint,
} from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import {
  SecurityGroupProtocol,
  SecurityGroupSchema,
  SecurityGroupService,
  type SecurityGroup,
} from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { fakeApi } from "@/test/harness";

const ready = (type: string, ok = true, reason = "Ready") => ({ type, status: ok ? ConditionStatus.TRUE : ConditionStatus.FALSE, reason });
const { TCP, UDP, ICMP, ALL } = {
  TCP: SecurityGroupProtocol.TCP,
  UDP: SecurityGroupProtocol.UDP,
  ICMP: SecurityGroupProtocol.ICMP,
  ALL: SecurityGroupProtocol.ALL,
};

export const networks = (): Network[] => [
  create(NetworkSchema, {
    metadata: { name: "default", project: "demo", uid: "n-1", resourceVersion: 7n, labels: { tier: "base" } },
    spec: { ipv4Cidr: "10.0.0.0/24" },
    status: { transitIpv4: "100.64.0.0/31", conditions: [ready("Ready")] },
  }),
  create(NetworkSchema, {
    metadata: { name: "sandbox", project: "demo", uid: "n-2", resourceVersion: 8n, labels: { team: "agents" } },
    spec: { ipv4Cidr: "10.20.0.0/24" },
  }),
];

export const groups = (): SecurityGroup[] => [
  create(SecurityGroupSchema, {
    metadata: { name: "web", project: "demo", uid: "g-1", resourceVersion: 11n, generation: 3n, labels: { team: "a", tier: "edge" } },
    spec: {
      network: "default",
      ingress: [
        { protocol: TCP, ports: "80,443", from: { peer: { case: "cidr", value: "::/0" } } },
        { protocol: TCP, ports: "8080", from: { peer: { case: "group", value: "default" } } },
        { protocol: ICMP, from: { peer: { case: "cidr", value: "::/0" } } },
        { protocol: ALL, from: { peer: { case: "group", value: "web" } } },
      ],
      egress: [{ protocol: UDP, ports: "53", to: { peer: { case: "cidr", value: "192.0.2.53/32" } } }],
    },
  }),
];

export const addresses = (): Address[] => [
  create(AddressSchema, {
    metadata: { name: "api", project: "demo", uid: "a-1", resourceVersion: 21n, labels: { role: "front" } },
    spec: { target: { object: { case: "container", value: "api" }, interface: "eth0" } },
    status: { ipv6: "2001:db8::77b2/128", machine: "api-x1", conditions: [ready("Bound", true, "Bound")] },
  }),
  create(AddressSchema, {
    metadata: { name: "spare", project: "demo", uid: "a-2", resourceVersion: 22n, labels: { role: "spare" } },
    spec: {},
    status: { ipv6: "2001:db8::90e4/128", conditions: [ready("Bound", false, "Unbound")] },
  }),
];

export const endpoints = (): PublicEndpoint[] => [
  create(PublicEndpointSchema, {
    metadata: { name: "docs", project: "demo", uid: "e-1", resourceVersion: 31n, labels: { site: "docs" } },
    spec: { target: { object: { case: "container", value: "api" } }, port: 3000, access: PublicEndpointAccess.PRIVATE },
    status: { url: "https://docs-demo.example.com", hostname: "docs-demo.example.com", conditions: [ready("Ready")] },
  }),
  create(PublicEndpointSchema, {
    metadata: { name: "api-http", project: "demo", uid: "e-2", ownerReferences: [{ kind: "Container", name: "api", controller: true }] },
    spec: { target: { object: { case: "container", value: "api" } }, port: 8080, access: PublicEndpointAccess.PRIVATE },
    status: { url: "https://api-http-demo.example.com", conditions: [ready("Ready")] },
  }),
];

/** A container's Machine, its eth0 on a network in the groups named. */
export function machineOf(container: string, network: string, groups: string[]) {
  return create(MachineSchema, {
    metadata: { name: `${container}-x1`, project: "demo", ownerReferences: [{ kind: "Container", name: container, controller: true }] },
    spec: { interfaces: [{ name: "eth0", network, ipv4: "10.0.0.2/24", ipv6: "2001:db8::77b2/128", securityGroups: groups }] },
    status: { phase: MachinePhase.RUNNING },
  });
}

/** Containers, by name, for machineOf's Machines; listContainers answers with them. */
export const containersNamed = (...names: string[]) => names.map((name) => create(ContainerSchema, { metadata: { name, project: "demo" }, spec: { image: "nginx:1" } }));

const container = containersNamed("api")[0];
const machine = machineOf("api", "default", ["default", "web"]);

/** The objects the server holds, by kind. */
export type Server = { networks: Network[]; groups: SecurityGroup[]; addresses: Address[]; endpoints: PublicEndpoint[] };

/** Handlers a test replaces, by method name. */
export type Overrides = Partial<{
  listNetworks: () => unknown;
  listSecurityGroups: () => unknown;
  listAddresses: () => unknown;
  listPublicEndpoints: () => unknown;
  listMachines: () => unknown;
  listContainers: () => unknown;
  apply: (req: unknown) => unknown;
  del: (req: unknown) => unknown;
  /** TestPermissions' answer; unset, the caller holds every permission asked. */
  permissions: (req: { permissions: string[] }) => unknown;
  /** What the server holds, when it differs from the lists (newer, say). */
  server: Partial<Server>;
}>;

type Named = { metadata?: ObjectMeta };

/**
 * One kind's server side: Get by name; Apply refused with Aborted when it
 * carries a version other than the stored one, and when create-only with
 * AlreadyExists for a name held, else stored at the next version.
 */
function kindOn<D extends DescMessage>(schema: D, items: MessageShape<D>[]) {
  const find = (name: string) => items.findIndex((x) => (x as Named).metadata?.name === name);
  return {
    get(name: string) {
      const i = find(name);
      if (i < 0) throw new ConnectError(`${name} not found`, Code.NotFound);
      return clone(schema, items[i]);
    },
    apply(o: MessageShape<D>, createOnly: boolean) {
      const meta = (o as Named).metadata!;
      const i = find(meta.name);
      const at = i < 0 ? 0n : ((items[i] as Named).metadata?.resourceVersion ?? 0n);
      if (createOnly && meta.resourceVersion !== 0n) throw new ConnectError("a create-only apply takes no resourceVersion", Code.InvalidArgument);
      if (createOnly && i >= 0) throw new ConnectError(`${meta.name} already exists`, Code.AlreadyExists);
      if (i >= 0 && meta.resourceVersion !== 0n && meta.resourceVersion !== at) {
        throw new ConnectError(`${meta.name} is at resource version ${at}, not ${meta.resourceVersion}`, Code.Aborted);
      }
      const stored = clone(schema, o);
      (stored as Named).metadata!.resourceVersion = at + 1n;
      if (i < 0) items.push(stored);
      else items[i] = stored;
      return { stored: clone(schema, stored), created: i < 0 };
    },
  };
}

export type NetworkApi = ReturnType<typeof fakeApi> & { server: Server };

/** The fake API. */
export function networkApi(o: Overrides = {}): NetworkApi {
  const server: Server = {
    networks: o.server?.networks ?? networks(),
    groups: o.server?.groups ?? groups(),
    addresses: o.server?.addresses ?? addresses(),
    endpoints: o.server?.endpoints ?? endpoints(),
  };
  const nets = kindOn(NetworkSchema, server.networks);
  const sgs = kindOn(SecurityGroupSchema, server.groups);
  const addrs = kindOn(AddressSchema, server.addresses);
  const eps = kindOn(PublicEndpointSchema, server.endpoints);
  const deleted = (req: unknown) => (o.del ? o.del(req) : {});
  const api = fakeApi((r: ConnectRouter) => {
    r.service(NetworkService, {
      listNetworks: (() => (o.listNetworks ? o.listNetworks() : { items: networks() })) as never,
      getNetwork: (req) => ({ network: nets.get(req.name) }),
      applyNetwork: ((req: { network: Network; createOnly: boolean }) => {
        if (o.apply) return o.apply(req);
        const { stored, created } = nets.apply(req.network, req.createOnly);
        return { network: stored, created };
      }) as never,
      deleteNetwork: deleted as never,
    });
    r.service(SecurityGroupService, {
      listSecurityGroups: (() => (o.listSecurityGroups ? o.listSecurityGroups() : { items: groups() })) as never,
      getSecurityGroup: (req) => ({ securityGroup: sgs.get(req.name) }),
      applySecurityGroup: ((req: { securityGroup: SecurityGroup; createOnly: boolean }) => {
        if (o.apply) return o.apply(req);
        const { stored, created } = sgs.apply(req.securityGroup, req.createOnly);
        return { securityGroup: stored, created };
      }) as never,
      deleteSecurityGroup: deleted as never,
    });
    r.service(AddressService, {
      listAddresses: (() => (o.listAddresses ? o.listAddresses() : { items: addresses() })) as never,
      getAddress: (req) => ({ address: addrs.get(req.name) }),
      applyAddress: ((req: { address: Address; createOnly: boolean }) => {
        if (o.apply) return o.apply(req);
        const { stored, created } = addrs.apply(req.address, req.createOnly);
        return { address: stored, created };
      }) as never,
      deleteAddress: deleted as never,
    });
    r.service(PublicEndpointService, {
      listPublicEndpoints: (() => (o.listPublicEndpoints ? o.listPublicEndpoints() : { items: endpoints() })) as never,
      getPublicEndpoint: (req) => ({ publicEndpoint: eps.get(req.name) }),
      applyPublicEndpoint: ((req: { publicEndpoint: PublicEndpoint; createOnly: boolean }) => {
        if (o.apply) return o.apply(req);
        const { stored, created } = eps.apply(req.publicEndpoint, req.createOnly);
        return { publicEndpoint: stored, created };
      }) as never,
      deletePublicEndpoint: deleted as never,
    });
    r.service(ContainerService, { listContainers: (() => (o.listContainers ? o.listContainers() : { items: [container] })) as never });
    r.service(VirtualMachineService, { listVirtualMachines: () => ({ items: [] }) });
    r.service(MachineService, { listMachines: (() => (o.listMachines ? o.listMachines() : { items: [machine] })) as never });
    r.service(ProjectService, { listProjects: () => ({ items: [] }) });
    r.service(TenantService, { listTenants: () => ({ items: [] }) });
    r.service(AccountService, {
      testPermissions: ((req: { permissions: string[] }) =>
        o.permissions ? o.permissions(req) : { permissions: req.permissions }) as never,
    });
  });
  return Object.assign(api, { server });
}
