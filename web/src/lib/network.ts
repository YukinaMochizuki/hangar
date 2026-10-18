// Networks, security groups, addresses and public endpoints as the console
// shows them (docs/decisions/networking.md, public-endpoints.md).

import { toJson, type DescMessage, type JsonValue, type MessageShape } from "@bufbuild/protobuf";
import { ConditionStatus, type Condition, type ObjectMeta } from "@hangar/api/hangar/meta/v1/meta_pb";
import { AddressSchema, type Address, type AddressTarget } from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { NetworkSchema, type Network } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import {
  PublicEndpointAccess,
  PublicEndpointSchema,
  type PublicEndpoint,
  type PublicEndpointTarget,
} from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import {
  SecurityGroupProtocol,
  SecurityGroupSchema,
  type SecurityGroup,
  type SecurityGroupPeer,
} from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { useQuery } from "@connectrpc/connect-query";
import { AddressService } from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { NetworkService } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { PublicEndpointService } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { SecurityGroupService } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { toYaml } from "@/lib/manifest";
import { refetchInterval, useMachines, type MachineKind } from "@/lib/machines";

/** One machine interface: its network, addresses and the groups it is in. */
export type Attachment = {
  kind: MachineKind;
  name: string;
  iface: string;
  network: string;
  ipv4: string;
  ipv6: string;
  securityGroups: string[];
};

export type ListState<T> = { items: T[]; loading: boolean; error: unknown; refetch: () => void };

// The API's largest page: a project holds far fewer objects than this.
const pageSize = 1000;

function listState<T>(q: { data?: { items: T[] }; isPending: boolean; error: unknown; refetch: () => unknown }): ListState<T> {
  return { items: q.data?.items ?? [], loading: q.isPending, error: q.error ?? null, refetch: () => void q.refetch() };
}

const byName = <T extends { metadata?: ObjectMeta }>(s: ListState<T>): ListState<T> => ({
  ...s,
  items: [...s.items].sort((a, b) => nameOf(a).localeCompare(nameOf(b))),
});

export function useNetworks(project: string): ListState<Network> {
  return byName(listState(useQuery(NetworkService.method.listNetworks, { project, pageSize }, { refetchInterval })));
}
export function useSecurityGroups(project: string): ListState<SecurityGroup> {
  return byName(listState(useQuery(SecurityGroupService.method.listSecurityGroups, { project, pageSize }, { refetchInterval })));
}
export function useAddresses(project: string): ListState<Address> {
  return byName(listState(useQuery(AddressService.method.listAddresses, { project, pageSize }, { refetchInterval })));
}
export function usePublicEndpoints(project: string): ListState<PublicEndpoint> {
  return byName(listState(useQuery(PublicEndpointService.method.listPublicEndpoints, { project, pageSize }, { refetchInterval })));
}

/**
 * Every machine interface of the project, as its Machine has it: the groups
 * it is in, "default" included when it is. A machine with no Machine yet has
 * no interfaces.
 */
export function useAttachments(project: string): ListState<Attachment> {
  const m = useMachines(project);
  return {
    loading: m.loading,
    error: m.error,
    refetch: m.refetch,
    items: m.items.flatMap((x) =>
      x.interfaces.map((i) => ({
        kind: x.kind,
        name: x.name,
        iface: i.name,
        network: i.network,
        ipv4: i.ipv4,
        ipv6: i.ipv6,
        securityGroups: i.securityGroups,
      })),
    ),
  };
}

/** "1 member", "3 members". */
export const count = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;

export const nameOf = (o: { metadata?: ObjectMeta }) => o.metadata?.name ?? "";

export function conditionOf(conds: Condition[] | undefined, type: string): Condition | undefined {
  return conds?.find((c) => c.type === type);
}

/** A condition's light: ok, waiting (false or unknown), or none yet. */
export type Light = "ok" | "pending" | "fail" | "off";

export function lightOf(c: Condition | undefined): Light {
  if (!c) return "pending";
  return c.status === ConditionStatus.TRUE ? "ok" : "pending";
}

/** The gateway of a range: its first usable address. */
export function gatewayOf(cidr: string): string {
  const [addr, bits] = cidr.split("/");
  const parts = addr.split(".").map(Number);
  if (parts.length !== 4 || parts.some((p) => !Number.isInteger(p) || p < 0 || p > 255) || !bits) return "";
  const n = ((parts[0] << 24) | (parts[1] << 16) | (parts[2] << 8) | parts[3]) >>> 0;
  const mask = Number(bits) === 0 ? 0 : (~0 << (32 - Number(bits))) >>> 0;
  const g = ((n & mask) + 1) >>> 0;
  return [g >>> 24, (g >>> 16) & 255, (g >>> 8) & 255, g & 255].join(".");
}

export const protocolLabel: Record<number, string> = {
  [SecurityGroupProtocol.UNSPECIFIED]: "–",
  [SecurityGroupProtocol.ALL]: "All",
  [SecurityGroupProtocol.TCP]: "TCP",
  [SecurityGroupProtocol.UDP]: "UDP",
  [SecurityGroupProtocol.ICMP]: "ICMP",
};

export function portsLabel(protocol: SecurityGroupProtocol, ports: string): string {
  if (protocol !== SecurityGroupProtocol.TCP && protocol !== SecurityGroupProtocol.UDP) return "–";
  return ports || "all";
}

export function peerLabel(p: SecurityGroupPeer | undefined): { text: string; group: boolean } {
  if (p?.peer.case === "group") return { text: p.peer.value, group: true };
  if (p?.peer.case === "cidr") return { text: p.peer.value, group: false };
  return { text: "–", group: false };
}

/** The machine an Address or PublicEndpoint names. */
export function targetOf(t: AddressTarget | PublicEndpointTarget | undefined): { kind: MachineKind; name: string } | undefined {
  if (t?.object.case === "container") return { kind: "Container", name: t.object.value };
  if (t?.object.case === "virtualMachine") return { kind: "VirtualMachine", name: t.object.value };
  return undefined;
}

export const accessLabel = (a: PublicEndpointAccess) => (a === PublicEndpointAccess.PUBLIC ? "Public" : "Private");

/** The container whose public port made an endpoint, if one did. */
export function ownerOf(e: PublicEndpoint): string | undefined {
  const ref = e.metadata?.ownerReferences.find((r) => r.kind === "Container" && r.controller);
  return ref?.name;
}

export const kindNoun = (k: MachineKind) => (k === "Container" ? "container" : "virtual machine");
export const cliKind = (k: MachineKind) => (k === "Container" ? "ct" : "vm");

type Json = { [k: string]: JsonValue };

/** What a person writes of an object, as `hangar apply -f` takes it. */
function manifest<D extends DescMessage>(schema: D, obj: MessageShape<D>, kind: string): string {
  const json = toJson(schema, obj) as Json;
  const meta = (json.metadata ?? {}) as Json;
  const metadata: Json = { name: meta.name ?? "" };
  const labels = Object.entries((meta.labels ?? {}) as Json).filter(([k]) => !/\.hangarlab\.cloud\//.test(k));
  if (labels.length) metadata.labels = Object.fromEntries(labels);
  return toYaml({ apiVersion: "network.hangarlab.cloud/v1alpha1", kind, metadata, spec: json.spec ?? {} });
}

export const networkYaml = (n: Network) => manifest(NetworkSchema, n, "Network");
export const securityGroupYaml = (g: SecurityGroup) => manifest(SecurityGroupSchema, g, "SecurityGroup");
export const addressYaml = (a: Address) => manifest(AddressSchema, a, "Address");
export const endpointYaml = (e: PublicEndpoint) => manifest(PublicEndpointSchema, e, "PublicEndpoint");
