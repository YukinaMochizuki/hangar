// Every kind the Edit labels dialog edits sends, through its own Apply, the
// object as read with only metadata.labels changed, at the resource version
// read: a spec, a status or a version lost on the way would be written over
// what is stored.

import { clone, create, equals, type DescMessage, type MessageShape } from "@bufbuild/protobuf";
import { createRouterTransport } from "@connectrpc/connect";
import { ContainerSchema, ContainerService } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { VirtualMachineSchema, VirtualMachineService } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { ServiceTokenSchema, ServiceTokenService } from "@hangar/api/hangar/iam/v1alpha1/service_token_pb";
import { AddressSchema, AddressService } from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { NetworkSchema, NetworkService } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { PublicEndpointAccess, PublicEndpointSchema, PublicEndpointService } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { SecurityGroupProtocol, SecurityGroupSchema, SecurityGroupService } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { DiskSchema, DiskService } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { FileShareSchema, FileShareService } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { ImageSchema, ImageService } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { describe, expect, it } from "vitest";
import { applyObjectAt, labelKinds, withLabels, type LabelKind, type LabelObject } from "@/lib/object-kinds";

const meta = (name: string, global = false) => ({
  name,
  ...(global ? {} : { project: "demo" }),
  uid: `uid-${name}`,
  resourceVersion: 41n,
  generation: 3n,
  labels: { app: "web", "storage.hangarlab.cloud/root-disk-of": "uid-db" },
  annotations: { note: "kept" },
});

/** One object of each kind, with a spec and a status, as Get would answer it. */
const read: { [K in LabelKind]: () => LabelObject<K> } = {
  container: () =>
    create(ContainerSchema, { metadata: meta("web"), spec: { image: "nginx:1", resources: { cpu: "1", memory: "512Mi" } }, status: { observedGeneration: 3n } }),
  vm: () =>
    create(VirtualMachineSchema, {
      metadata: meta("db"),
      spec: { resources: { cpu: "2", memory: "4Gi" }, rootDisk: { source: { case: "disk", value: "db-root" } } },
      status: { rootDisk: "db-root" },
    }),
  disk: () => create(DiskSchema, { metadata: meta("pg"), spec: { size: "20Gi" }, status: { size: "20Gi", node: "node-a" } }),
  fileShare: () => create(FileShareSchema, { metadata: meta("media"), spec: { networks: ["default"], size: "5Gi" }, status: { node: "node-a" } }),
  network: () => create(NetworkSchema, { metadata: meta("backend"), spec: { ipv4Cidr: "10.1.0.0/24" }, status: { transitIpv4: "100.64.0.2/31" } }),
  securityGroup: () =>
    create(SecurityGroupSchema, {
      metadata: meta("web-sg"),
      spec: { network: "default", ingress: [{ protocol: SecurityGroupProtocol.TCP, ports: "443", from: { peer: { case: "cidr", value: "::/0" } } }] },
    }),
  address: () =>
    create(AddressSchema, {
      metadata: meta("front"),
      spec: { target: { object: { case: "container", value: "web" }, interface: "eth0" } },
      status: { ipv6: "2001:db8::1/128" },
    }),
  publicEndpoint: () =>
    create(PublicEndpointSchema, {
      metadata: meta("docs"),
      spec: { target: { object: { case: "container", value: "web" } }, port: 3000, access: PublicEndpointAccess.PRIVATE },
      status: { url: "https://docs-demo.example.com" },
    }),
  serviceToken: () => create(ServiceTokenSchema, { metadata: meta("ci"), spec: { description: "deploys" }, status: { secretPrefix: "hgr_ab" } }),
  image: () =>
    create(ImageSchema, { metadata: meta("debian-13", true), spec: { url: "https://images.example.com/d.img", sha256: "ab".repeat(32), os: "debian" } }),
};

const schemas: { [K in LabelKind]: DescMessage } = {
  container: ContainerSchema,
  vm: VirtualMachineSchema,
  disk: DiskSchema,
  fileShare: FileShareSchema,
  network: NetworkSchema,
  securityGroup: SecurityGroupSchema,
  address: AddressSchema,
  publicEndpoint: PublicEndpointSchema,
  serviceToken: ServiceTokenSchema,
  image: ImageSchema,
};

/** A transport whose every kind's Apply records the object it was sent. */
function recording() {
  const sent: unknown[] = [];
  const keep = <T>(o: T) => (sent.push(o), {});
  const transport = createRouterTransport((r) => {
    r.service(ContainerService, { applyContainer: (q) => keep(q.container) });
    r.service(VirtualMachineService, { applyVirtualMachine: (q) => keep(q.virtualMachine) });
    r.service(DiskService, { applyDisk: (q) => keep(q.disk) });
    r.service(FileShareService, { applyFileShare: (q) => keep(q.fileShare) });
    r.service(NetworkService, { applyNetwork: (q) => keep(q.network) });
    r.service(SecurityGroupService, { applySecurityGroup: (q) => keep(q.securityGroup) });
    r.service(AddressService, { applyAddress: (q) => keep(q.address) });
    r.service(PublicEndpointService, { applyPublicEndpoint: (q) => keep(q.publicEndpoint) });
    r.service(ServiceTokenService, { applyServiceToken: (q) => keep(q.serviceToken) });
    r.service(ImageService, { applyImage: (q) => keep(q.image) });
  });
  return { transport, sent };
}

describe.each(Object.keys(labelKinds) as LabelKind[])("labels of a %s", (kind) => {
  it("are applied over the object as read, its labels alone changed, at the version read", async () => {
    const was = read[kind]();
    const labels = { tier: "db", "storage.hangarlab.cloud/root-disk-of": "uid-db" };
    const { transport, sent } = recording();
    await applyObjectAt(transport, kind, withLabels(kind, was, labels));
    expect(sent).toHaveLength(1);
    const want = clone(schemas[kind], was as MessageShape<DescMessage>) as LabelObject;
    want.metadata!.labels = labels;
    const got = sent[0] as MessageShape<DescMessage>;
    expect(equals(schemas[kind], got, want as MessageShape<DescMessage>)).toBe(true);
    expect((got as LabelObject).metadata?.resourceVersion).toBe(41n);
    // The object as read is left as it was.
    expect(was.metadata?.labels).toEqual(meta("x").labels);
  });

  it("is not applied without the version it was read at", async () => {
    const was = read[kind]();
    was.metadata!.resourceVersion = 0n;
    const { transport, sent } = recording();
    await expect(applyObjectAt(transport, kind, withLabels(kind, was, {}))).rejects.toThrow(/carries no resource version/);
    expect(sent).toHaveLength(0);
  });
});
