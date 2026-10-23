// The kinds whose labels the console edits, each with what the Edit labels
// dialog needs of it: its noun, its manifest, and its Get and Apply, which
// the dialog calls at the resource version it read (applyObjectAt). A
// Secret is not among them: its values are write-only and every apply
// carries them (docs/decisions/secrets.md), so its labels cannot be applied
// back without its value.

import { clone, toJson, type DescMessage, type JsonValue, type MessageShape } from "@bufbuild/protobuf";
import { createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ContainerSchema, ContainerService, type Container } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { VirtualMachineSchema, VirtualMachineService, type VirtualMachine } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { ServiceTokenSchema, ServiceTokenService, type ServiceToken } from "@hangar/api/hangar/iam/v1alpha1/service_token_pb";
import type { ObjectMeta } from "@hangar/api/hangar/meta/v1/meta_pb";
import { AddressSchema, AddressService, type Address } from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { CustomDomainSchema, CustomDomainService, type CustomDomain } from "@hangar/api/hangar/network/v1alpha1/custom_domain_pb";
import { NetworkSchema, NetworkService, type Network } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { PortForwardSchema, PortForwardService, type PortForward } from "@hangar/api/hangar/network/v1alpha1/port_forward_pb";
import { PublicEndpointSchema, PublicEndpointService, type PublicEndpoint } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { SecurityGroupSchema, SecurityGroupService, type SecurityGroup } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { CustomImageSchema, CustomImageService, type CustomImage } from "@hangar/api/hangar/storage/v1alpha1/custom_image_pb";
import { DiskSchema, DiskService, type Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { FileShareSchema, FileShareService, type FileShare } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { ImageSchema, ImageService, type Image } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { toYaml } from "@/lib/manifest";

type Objects = {
  container: Container;
  vm: VirtualMachine;
  disk: Disk;
  fileShare: FileShare;
  network: Network;
  securityGroup: SecurityGroup;
  address: Address;
  publicEndpoint: PublicEndpoint;
  portForward: PortForward;
  customDomain: CustomDomain;
  serviceToken: ServiceToken;
  image: Image;
  customImage: CustomImage;
};

/** A kind whose labels the console edits. */
export type LabelKind = keyof Objects;
export type LabelObject<K extends LabelKind = LabelKind> = Objects[K];

type KindInfo<K extends LabelKind> = {
  schema: DescMessage;
  /** The API group and Kind its manifest names. */
  group: string;
  kind: string;
  noun: string;
  /** The CLI's name for the kind, as `hangar get` takes it. */
  cli: string;
  /** Platform-wide: no project in its commands or calls. */
  global?: boolean;
  get: (t: Transport, project: string, name: string) => Promise<Objects[K] | undefined>;
  apply: (t: Transport, o: Objects[K]) => Promise<unknown>;
};

export const labelKinds: { [K in LabelKind]: KindInfo<K> } = {
  container: {
    schema: ContainerSchema,
    group: "compute",
    kind: "Container",
    noun: "container",
    cli: "container",
    get: async (t, project, name) => (await createClient(ContainerService, t).getContainer({ project, name })).container,
    apply: (t, o) => createClient(ContainerService, t).applyContainer({ container: o }),
  },
  vm: {
    schema: VirtualMachineSchema,
    group: "compute",
    kind: "VirtualMachine",
    noun: "virtual machine",
    cli: "vm",
    get: async (t, project, name) => (await createClient(VirtualMachineService, t).getVirtualMachine({ project, name })).virtualMachine,
    apply: (t, o) => createClient(VirtualMachineService, t).applyVirtualMachine({ virtualMachine: o }),
  },
  disk: {
    schema: DiskSchema,
    group: "storage",
    kind: "Disk",
    noun: "disk",
    cli: "disk",
    get: async (t, project, name) => (await createClient(DiskService, t).getDisk({ project, name })).disk,
    apply: (t, o) => createClient(DiskService, t).applyDisk({ disk: o }),
  },
  fileShare: {
    schema: FileShareSchema,
    group: "storage",
    kind: "FileShare",
    noun: "file share",
    cli: "fileshare",
    get: async (t, project, name) => (await createClient(FileShareService, t).getFileShare({ project, name })).fileShare,
    apply: (t, o) => createClient(FileShareService, t).applyFileShare({ fileShare: o }),
  },
  network: {
    schema: NetworkSchema,
    group: "network",
    kind: "Network",
    noun: "network",
    cli: "network",
    get: async (t, project, name) => (await createClient(NetworkService, t).getNetwork({ project, name })).network,
    apply: (t, o) => createClient(NetworkService, t).applyNetwork({ network: o }),
  },
  securityGroup: {
    schema: SecurityGroupSchema,
    group: "network",
    kind: "SecurityGroup",
    noun: "security group",
    cli: "sg",
    get: async (t, project, name) => (await createClient(SecurityGroupService, t).getSecurityGroup({ project, name })).securityGroup,
    apply: (t, o) => createClient(SecurityGroupService, t).applySecurityGroup({ securityGroup: o }),
  },
  address: {
    schema: AddressSchema,
    group: "network",
    kind: "Address",
    noun: "address",
    cli: "addr",
    get: async (t, project, name) => (await createClient(AddressService, t).getAddress({ project, name })).address,
    apply: (t, o) => createClient(AddressService, t).applyAddress({ address: o }),
  },
  publicEndpoint: {
    schema: PublicEndpointSchema,
    group: "network",
    kind: "PublicEndpoint",
    noun: "public endpoint",
    cli: "ep",
    get: async (t, project, name) => (await createClient(PublicEndpointService, t).getPublicEndpoint({ project, name })).publicEndpoint,
    apply: (t, o) => createClient(PublicEndpointService, t).applyPublicEndpoint({ publicEndpoint: o }),
  },
  portForward: {
    schema: PortForwardSchema,
    group: "network",
    kind: "PortForward",
    noun: "port forward",
    cli: "pf",
    get: async (t, project, name) => (await createClient(PortForwardService, t).getPortForward({ project, name })).portForward,
    apply: (t, o) => createClient(PortForwardService, t).applyPortForward({ portForward: o }),
  },
  customDomain: {
    schema: CustomDomainSchema,
    group: "network",
    kind: "CustomDomain",
    noun: "custom domain",
    cli: "cdom",
    global: true,
    get: async (t, _project, name) => (await createClient(CustomDomainService, t).getCustomDomain({ name })).customDomain,
    apply: (t, o) => createClient(CustomDomainService, t).applyCustomDomain({ customDomain: o }),
  },
  serviceToken: {
    schema: ServiceTokenSchema,
    group: "iam",
    kind: "ServiceToken",
    noun: "service token",
    cli: "st",
    get: async (t, project, name) => (await createClient(ServiceTokenService, t).getServiceToken({ project, name })).serviceToken,
    apply: (t, o) => createClient(ServiceTokenService, t).applyServiceToken({ serviceToken: o }),
  },
  image: {
    schema: ImageSchema,
    group: "storage",
    kind: "Image",
    noun: "image",
    cli: "image",
    global: true,
    get: async (t, _project, name) => (await createClient(ImageService, t).getImage({ name })).image,
    apply: (t, o) => createClient(ImageService, t).applyImage({ image: o }),
  },
  customImage: {
    schema: CustomImageSchema,
    group: "storage",
    kind: "CustomImage",
    noun: "image",
    cli: "customimage",
    get: async (t, project, name) => (await createClient(CustomImageService, t).getCustomImage({ project, name })).customImage,
    apply: (t, o) => createClient(CustomImageService, t).applyCustomImage({ customImage: o }),
  },
};

type Json = { [k: string]: JsonValue };

/**
 * The object as `hangar apply -f` takes it: its name, labels and spec. All
 * its labels are written, the system's too, as the dialog sends them back.
 */
export function labelledYaml<K extends LabelKind>(kind: K, o: Objects[K]): string {
  const info = labelKinds[kind];
  const json = toJson(info.schema, o as MessageShape<DescMessage>) as Json;
  const meta = (json.metadata ?? {}) as Json;
  const metadata: Json = { name: meta.name ?? "" };
  if (meta.labels && Object.keys(meta.labels).length > 0) metadata.labels = meta.labels;
  return toYaml({ apiVersion: `${info.group}.hangarlab.cloud/v1alpha1`, kind: info.kind, metadata, spec: json.spec ?? {} });
}

/**
 * Applies an object as it is, at the resource version it carries: a change
 * made on what the person saw. A newer object makes the API answer Aborted,
 * and nothing is read again or written over it.
 */
export async function applyObjectAt<K extends LabelKind>(t: Transport, kind: K, o: Objects[K]): Promise<void> {
  // Without a version Apply is create-or-update, which would write over whatever is there.
  if (!o.metadata?.resourceVersion) throw new Error(`${o.metadata?.name ?? "The object"} carries no resource version to apply at.`);
  await labelKinds[kind].apply(t, o);
}

/** applyObjectAt as a mutation, the lists asked again once it settles. */
export function useApplyObjectAt<K extends LabelKind>(kind: K) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (o: Objects[K]) => applyObjectAt(transport, kind, o),
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** Reads an object as it is now, for a dialog to start again from. */
export function useReadObject<K extends LabelKind>(kind: K, project: string, name: string) {
  const transport = useTransport();
  return useMutation({
    mutationFn: async (): Promise<Objects[K]> => {
      const o = await labelKinds[kind].get(transport, project, name);
      if (!o) throw new Error(`${name} is gone.`);
      return o;
    },
  });
}

/** The object as read, with its labels alone replaced; its resource version is the one read. */
export function withLabels<K extends LabelKind>(kind: K, o: Objects[K], labels: Record<string, string>): Objects[K] {
  const out = clone(labelKinds[kind].schema, o as MessageShape<DescMessage>) as unknown as { metadata?: ObjectMeta };
  if (out.metadata) out.metadata.labels = { ...labels };
  return out as unknown as Objects[K];
}
