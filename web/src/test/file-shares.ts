// What the file share pages' tests share: an in-memory API holding file
// shares, networks, the containers and VMs that declare shares with their
// Machines, and what the New machine page reads, answering which
// permissions the caller holds.

import { clone, create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { ContainerSchema, ContainerService, type ApplyContainerRequest, type Container } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { MachineSchema, MachineService, type Machine } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { MachineSizeSchema, MachineSizeService } from "@hangar/api/hangar/compute/v1alpha1/machine_size_pb";
import { VirtualMachineSchema, VirtualMachineService, type VirtualMachine } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { NetworkService } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { SecurityGroupService } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { DiskService } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import {
  FileSharePhase,
  FileShareSchema,
  FileShareService,
  FileShareSquash,
  type ApplyFileShareRequest,
  type DeleteFileShareRequest,
  type FileShare,
} from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { ImageService } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { fakeApi, type Api } from "@/test/harness";

/** Whether every TestPermissions the page asked has been answered, as a page's controls wait on it. */
export const permissionsAnswered = (api: Api) =>
  api.client
    .getQueryCache()
    .getAll()
    .filter((q) => JSON.stringify(q.queryKey).toLowerCase().includes("testpermissions"))
    .every((q) => q.state.status === "success");

/** Waits until the page has asked TestPermissions and the answer came. */
export async function settledPermissions(api: Api, waitFor: (f: () => void) => Promise<unknown>) {
  await waitFor(() => {
    if (!api.calls.includes("TestPermissions") || !permissionsAnswered(api)) throw new Error("TestPermissions not answered yet");
  });
}

export const project = "demo";

export type ShareOpts = Partial<{
  networks: string[];
  size: string;
  have: string;
  squash: FileShareSquash;
  anon: [number, number];
  phase: FileSharePhase;
  addresses: Record<string, string>;
  users: { kind: string; name: string; network: string; readOnly?: boolean; path?: string }[];
  version: bigint;
}>;

/** share is a FileShare of the project, as the API returns it; unset, it is Pending with no address, as before its server runs. */
export function share(name: string, o: ShareOpts = {}): FileShare {
  return create(FileShareSchema, {
    metadata: { name, project, uid: `uid-${name}`, resourceVersion: o.version ?? 5n },
    spec: { networks: o.networks ?? [], size: o.size ?? "50Gi", squash: o.squash ?? FileShareSquash.UNSPECIFIED, anonUid: o.anon?.[0], anonGid: o.anon?.[1] },
    status: {
      phase: o.phase ?? FileSharePhase.PENDING,
      size: o.have ?? "",
      addresses: Object.entries(o.addresses ?? {}).map(([network, address]) => ({ network, address })),
      users: (o.users ?? []).map((u) => ({ kind: u.kind, name: u.name, machine: `m-${u.name}`, network: u.network, readOnly: !!u.readOnly, path: u.path ?? "" })),
    },
  });
}

type Vol = { share: string; target: string; readOnly?: boolean };

/** container is a Container on the networks given ([default] when none), mounting the shares given, with a Machine on them. */
export function container(name: string, networks: string[] = [], volumes: Vol[] = [], heldBack?: string, deleting = false): Container {
  return create(ContainerSchema, {
    metadata: { name, project, uid: `uid-${name}`, deleteTime: deleting ? { seconds: 1n } : undefined },
    spec: {
      image: "nginx:1",
      interfaces: networks.map((n) => ({ network: n, securityGroups: ["default"] })),
      volumes: volumes.map((v) => ({ source: v.share, sourceKind: "FileShare", target: v.target, readOnly: !!v.readOnly })),
    },
    status: { conditions: heldBack ? [{ type: "Ready", status: ConditionStatus.FALSE, reason: "MachineHeldBack", message: heldBack }] : [] },
  });
}

/** vm is a VirtualMachine on the networks given, declaring the shares given. */
export function vm(name: string, networks: string[] = [], shares: { name: string; readOnly?: boolean }[] = [], notOnNode?: string): VirtualMachine {
  return create(VirtualMachineSchema, {
    metadata: { name, project, uid: `uid-${name}` },
    spec: { interfaces: networks.map((n) => ({ network: n, securityGroups: ["default"] })), fileShares: shares.map((s) => ({ name: s.name, readOnly: !!s.readOnly })) },
    status: { conditions: notOnNode ? [{ type: "FileSharesOnNode", status: ConditionStatus.FALSE, reason: "FileShareNotOnNode", message: notOnNode }] : [] },
  });
}

/** machineOf is the Machine a container or VM runs as, with an interface on each of its networks. */
export function machineOf(kind: "Container" | "VirtualMachine", o: Container | VirtualMachine, ip = 10): Machine {
  const nets = o.spec?.interfaces.length ? o.spec.interfaces.map((i) => i.network) : ["default"];
  return create(MachineSchema, {
    metadata: { name: `m-${o.metadata?.name}`, project, uid: `muid-${o.metadata?.name}`, ownerReferences: [{ kind, name: o.metadata?.name ?? "", controller: true }] },
    spec: { interfaces: nets.map((n, i) => ({ name: `eth${i}`, network: n, ipv4: `10.${nets.indexOf(n)}.0.${ip}/24`, securityGroups: ["default"] })) },
  });
}

export const networks = [
  { metadata: { name: "default", project }, spec: { ipv4Cidr: "10.0.0.0/24" } },
  { metadata: { name: "data", project }, spec: { ipv4Cidr: "10.1.0.0/24" } },
  { metadata: { name: "ops", project }, spec: { ipv4Cidr: "10.2.0.0/24" } },
];

export type Fake = {
  shares?: FileShare[];
  /** Makes ListFileShares answer this instead. */
  sharesAnswer?: () => Promise<{ items: FileShare[] }>;
  containers?: Container[];
  vms?: VirtualMachine[];
  /** What TestPermissions answers the caller holds; unset, every permission asked. */
  perms?: () => string[] | Promise<string[]>;
  /** Sees each share apply first; a throw refuses it, and nothing is stored. */
  apply?: (req: ApplyFileShareRequest) => void;
  del?: (req: DeleteFileShareRequest) => void;
  /** Sees each container apply first; a throw refuses it. */
  applyContainer?: (req: ApplyContainerRequest) => void;
  /** Makes ListMachines fail with this. */
  machinesFail?: ConnectError;
};

/**
 * sharesApi is the in-memory API; each container and VM runs as a Machine.
 * A share applied is stored at the next resource version, so a later Get and
 * List read it, as from the apiserver.
 */
export function sharesApi(f: Fake = {}) {
  const containers = f.containers ?? [];
  const vms = f.vms ?? [];
  const machines = [...containers.map((c, i) => machineOf("Container", c, 20 + i)), ...vms.map((v, i) => machineOf("VirtualMachine", v, 40 + i))];
  const stored = [...(f.shares ?? [])];
  const shares = () => stored;
  return fakeApi((r) => {
    r.service(FileShareService, {
      listFileShares: () => (f.sharesAnswer ? f.sharesAnswer() : { items: shares() }),
      getFileShare: (req) => {
        const s = shares().find((x) => x.metadata?.name === req.name);
        if (!s) throw new ConnectError(`file share ${req.name} not found`, Code.NotFound);
        return { fileShare: s };
      },
      applyFileShare: (req) => {
        if (req.createOnly && shares().some((x) => x.metadata?.name === req.fileShare?.metadata?.name)) {
          throw new ConnectError(`file share ${req.fileShare?.metadata?.name} already exists`, Code.AlreadyExists);
        }
        f.apply?.(req);
        const s = clone(FileShareSchema, req.fileShare!);
        const i = stored.findIndex((x) => x.metadata?.name === s.metadata?.name);
        s.metadata!.resourceVersion = (i < 0 ? 0n : (stored[i]!.metadata?.resourceVersion ?? 0n)) + 1n;
        if (i < 0) stored.push(s);
        else stored[i] = s;
        return { fileShare: s, created: !!req.createOnly };
      },
      deleteFileShare: (req) => {
        f.del?.(req);
        return {};
      },
    });
    r.service(AccountService, {
      testPermissions: async (req) => {
        const held = f.perms ? await f.perms() : req.permissions;
        return { permissions: req.permissions.filter((p) => held.includes(p)) };
      },
    });
    r.service(NetworkService, { listNetworks: () => ({ items: networks }) });
    r.service(SecurityGroupService, { listSecurityGroups: () => ({ items: [] }) });
    r.service(ContainerService, {
      listContainers: () => ({ items: containers }),
      applyContainer: (req) => {
        f.applyContainer?.(req);
        return { container: req.container, created: true };
      },
    });
    r.service(VirtualMachineService, { listVirtualMachines: () => ({ items: vms }) });
    r.service(MachineService, {
      listMachines: () => {
        if (f.machinesFail) throw f.machinesFail;
        return { items: machines };
      },
    });
    r.service(DiskService, { listDisks: () => ({ items: [] }) });
    r.service(ImageService, { listImages: () => ({ items: [] }) });
    r.service(MachineSizeService, {
      listMachineSizes: () => ({ items: [create(MachineSizeSchema, { metadata: { name: "s" }, spec: { displayName: "S", cpu: "1", memory: "2Gi", disk: "10Gi", order: 1 } })] }),
    });
    r.service(ProjectService, {
      getProject: () => ({ project: { metadata: { name: project }, status: { limits: {}, usage: {} } } }),
      listProjects: () => ({ items: [] }),
    });
    r.service(TenantService, { listTenants: () => ({ items: [] }) });
  });
}
