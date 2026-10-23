// What the Disks and Images pages' tests share: an in-memory API holding
// disks, images, custom images and the machines a disk can be attached to,
// and answering which permissions the caller holds.

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { MachinePhase, MachineSchema, MachineService } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { ContainerService } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { VirtualMachineRunState, VirtualMachineSchema, VirtualMachineService } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import {
  DiskAttachmentRole,
  DiskPhase,
  DiskSchema,
  DiskService,
  type ApplyDiskRequest,
  type Disk,
  type DeleteDiskRequest,
} from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import {
  CustomImageSchema,
  CustomImageService,
  type ApplyCustomImageRequest,
  type CustomImage,
} from "@hangar/api/hangar/storage/v1alpha1/custom_image_pb";
import { ImageSchema, ImageService, type Image } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { rootDiskOf } from "@/lib/storage";
import { fakeApi } from "@/test/harness";

/** project is the project every test's objects are in. */
export const project = "demo";

type Cond = [ConditionStatus, string, string];

/** DiskOpts are what a test's disk differs in; an unset phase is Available, and Pending means not placed. */
export type DiskOpts = Partial<{
  size: string;
  have: string;
  image: string;
  phase: DiskPhase;
  machine: string;
  /**
   * What an attached disk is used as, by the Container's or VM's name; unset,
   * a disk on a machine is VM postgres's boot disk when it is its root disk
   * and its data disk (vdb) otherwise, as the apiserver writes it.
   */
  attachment: { kind: string; name: string; as: DiskAttachmentRole; device?: string; path?: string; readOnly?: boolean };
  rootOf: string;
  version: bigint;
  labels: Record<string, string>;
  ready: Cond;
  allocated: Cond;
}>;

/** disk is a Disk of the project, as the API returns it. */
export function disk(name: string, o: DiskOpts = {}): Disk {
  const cond = (type: string, c: Cond | undefined) => (c ? [{ type, status: c[0], reason: c[1], message: c[2] }] : []);
  return create(DiskSchema, {
    metadata: {
      name,
      project,
      uid: `uid-${name}`,
      resourceVersion: o.version ?? 3n,
      labels: o.labels ?? {},
      annotations: o.rootOf ? { [rootDiskOf]: `uid-${o.rootOf}` } : {},
      ownerReferences: o.rootOf ? [{ kind: "VirtualMachine", name: o.rootOf, uid: `uid-${o.rootOf}`, controller: true }] : [],
    },
    spec: { size: o.size ?? "20Gi", source: o.image ? { source: { case: "image", value: { name: o.image } } } : {} },
    status: {
      phase: o.phase ?? DiskPhase.AVAILABLE,
      node: o.phase === DiskPhase.PENDING ? "" : "node-a",
      size: o.have ?? (o.phase === DiskPhase.PENDING ? "" : (o.size ?? "20Gi")),
      machine: o.machine ?? "",
      attachment: o.machine
        ? {
            machine: o.machine,
            ...(o.attachment ?? { kind: "VirtualMachine", name: "postgres", as: o.rootOf ? DiskAttachmentRole.BOOT : DiskAttachmentRole.DATA, device: o.rootOf ? "vda" : "vdb" }),
          }
        : undefined,
      conditions: [...cond("Allocated", o.allocated), ...cond("Ready", o.ready)],
    },
  });
}

/** image is a global Image, as the API returns it. */
export function image(name: string, o: Partial<{ size: string; min: string; verified: [ConditionStatus, string, string] }> = {}): Image {
  return create(ImageSchema, {
    metadata: { name },
    spec: { url: `https://images.example.com/${name}.img`, sha256: "ab".repeat(32), os: "debian", version: "13", minDiskSize: o.min ?? "", defaultUser: "debian" },
    status: {
      diskSize: o.size ?? "",
      conditions: o.verified ? [{ type: "Verified", status: o.verified[0], reason: o.verified[1], message: o.verified[2] }] : [],
    },
  });
}

/** CustomImageState is how far a test's custom image is: captured, still capturing, or failed. */
export type CustomImageState = "ready" | "capturing" | "failed";

/** UploadStep is how far a test's uploaded image is: awaiting the rest of its file, converting, or failed with a reason. */
export type UploadStep = { awaiting: number } | "converting" | { failed: string; uploaded: boolean };

/** uploadedImage is a CustomImage of the project made for a file of `size` bytes to upload, at `step`. */
export function uploadedImage(name: string, size: number, step: UploadStep): CustomImage {
  const uploaded = step === "converting" || (typeof step === "object" && "failed" in step && step.uploaded);
  const ready =
    typeof step === "object" && "failed" in step
      ? { type: "Ready", status: ConditionStatus.FALSE, reason: step.failed, message: "the converter refused it" }
      : { type: "Ready", status: ConditionStatus.FALSE, reason: uploaded ? "Converting" : "AwaitingUpload", message: "" };
  return create(CustomImageSchema, {
    metadata: { name, project },
    spec: { source: { source: { case: "upload", value: { size: BigInt(size) } } }, os: "nixos", version: "24.11", defaultUser: "nixos" },
    status: {
      node: "node-a",
      receivedBytes: BigInt(typeof step === "object" && "awaiting" in step ? step.awaiting : size),
      conditions: [{ type: "Uploaded", status: uploaded ? ConditionStatus.TRUE : ConditionStatus.FALSE, reason: uploaded ? "Uploaded" : "AwaitingUpload", message: "" }, ready],
    },
  });
}

/** customImage is a CustomImage of the project captured from disk, as the API returns it. */
export function customImage(name: string, o: Partial<{ disk: string; state: CustomImageState; size: string; used: string }> = {}): CustomImage {
  const state = o.state ?? "ready";
  const ready =
    state === "ready"
      ? { type: "Ready", status: ConditionStatus.TRUE, reason: "Captured", message: "" }
      : state === "failed"
        ? { type: "Ready", status: ConditionStatus.FALSE, reason: "CaptureFailed", message: "zfs receive: out of space" }
        : { type: "Ready", status: ConditionStatus.FALSE, reason: "Capturing", message: "copying" };
  return create(CustomImageSchema, {
    metadata: { name, project },
    spec: { source: { source: { case: "disk", value: { name: o.disk ?? "web-root" } } }, os: "ubuntu", version: "24.04", defaultUser: "ubuntu" },
    status: {
      node: "node-a",
      diskSize: state === "ready" ? (o.size ?? "20Gi") : "",
      usedSize: state === "ready" ? (o.used ?? "2Gi") : "",
      conditions: [ready],
    },
  });
}

/** The machines a test's disks can be attached to: VM postgres (Machine vm-postgres), and those a Fake adds by name with whether each is stopped. */
function vmsAndMachines(deleting: boolean, more: Record<string, boolean>) {
  const vms = [create(VirtualMachineSchema, { metadata: { name: "postgres", project }, spec: {} })];
  const machines = [
    create(MachineSchema, {
      metadata: {
        name: "vm-postgres",
        project,
        ownerReferences: [{ kind: "VirtualMachine", name: "postgres", controller: true }],
        deleteTime: deleting ? { seconds: 1n } : undefined,
      },
    }),
  ];
  for (const [name, stopped] of Object.entries(more)) {
    vms.push(
      create(VirtualMachineSchema, {
        metadata: { name, project },
        spec: { runState: stopped ? VirtualMachineRunState.STOPPED : VirtualMachineRunState.RUNNING },
        status: { rootDisk: `${name}-root` },
      }),
    );
    machines.push(
      create(MachineSchema, {
        metadata: { name: `vm-${name}`, project, ownerReferences: [{ kind: "VirtualMachine", name, controller: true }] },
        status: { phase: stopped ? MachinePhase.STOPPED : MachinePhase.RUNNING },
      }),
    );
  }
  return { vms, machines };
}

/** Fake is what a test's API answers; unset lists are empty. */
export type Fake = {
  /** Whether Machine vm-postgres is being deleted. */
  machineDeleting?: boolean;
  disks?: () => Promise<{ items: Disk[] }>;
  images?: () => Promise<{ items: Image[] }>;
  /** The disk GetDisk finds, by name; NotFound for any other. A create-only apply of its name is refused. */
  get?: (name: string) => Disk | undefined;
  /** What TestPermissions answers the caller holds; unset, every permission asked. */
  perms?: () => Promise<string[]>;
  apply?: (req: ApplyDiskRequest) => { disk?: Disk; created: boolean } | Promise<{ disk?: Disk; created: boolean }>;
  del?: (req: DeleteDiskRequest) => void;
  customImages?: () => Promise<{ items: CustomImage[] }>;
  applyCustomImage?: (req: ApplyCustomImageRequest) => { customImage?: CustomImage; created: boolean };
  /** VMs besides postgres, by name, each with whether it is stopped; VM x runs as Machine vm-x with root disk x-root. */
  vms?: Record<string, boolean>;
};

/**
 * storageApi is an in-memory API holding disks, images, VM postgres and its
 * Machine vm-postgres, and answering which permissions the caller holds.
 */
export function storageApi(f: Fake) {
  const { vms, machines } = vmsAndMachines(!!f.machineDeleting, f.vms ?? {});
  return fakeApi((r) => {
    r.service(DiskService, {
      listDisks: () => (f.disks ? f.disks() : { items: [] }),
      getDisk: (req) => {
        const d = f.get?.(req.name);
        if (!d) throw new ConnectError(`disk ${req.name} not found`, Code.NotFound);
        return { disk: d };
      },
      applyDisk: (req) => {
        // A create-only apply of a name that exists is refused, as the API does.
        if (req.createOnly && f.get?.(req.disk?.metadata?.name ?? "")) {
          throw new ConnectError(`disk ${req.disk?.metadata?.name} already exists`, Code.AlreadyExists);
        }
        return f.apply ? f.apply(req) : { disk: req.disk, created: true };
      },
      deleteDisk: (req) => {
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
    r.service(ImageService, { listImages: () => (f.images ? f.images() : { items: [] }) });
    r.service(CustomImageService, {
      listCustomImages: () => (f.customImages ? f.customImages() : { items: [] }),
      applyCustomImage: (req) => (f.applyCustomImage ? f.applyCustomImage(req) : { customImage: req.customImage, created: true }),
      deleteCustomImage: () => ({}),
    });
    r.service(MachineService, { listMachines: () => ({ items: machines }) });
    r.service(VirtualMachineService, { listVirtualMachines: () => ({ items: vms }) });
    r.service(ContainerService, { listContainers: () => ({ items: [] }) });
    r.service(ProjectService, { listProjects: () => ({ items: [] }) });
    r.service(TenantService, { listTenants: () => ({ items: [] }) });
  });
}
