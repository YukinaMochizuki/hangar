// What a member does to networks, security groups, addresses and public
// endpoints. Each is the API's own call, so the API's rules decide and its
// error is what the console shows where the action was asked for. Creating
// is a create-only Apply, which never replaces (lib/create.ts). A change
// that sets one value reads the object and applies it back at the version
// read, as start and stop do (lib/actions.ts); one made on what a dialog
// showed applies at the version the dialog opened from, and a newer object
// refuses it.

import { Code, ConnectError, createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { AddressService, type Address } from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { NetworkService, type Network } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { PublicEndpointService, type PublicEndpoint } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { SecurityGroupService, type SecurityGroup } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { readModifyWrite } from "@/lib/actions";
import { createError } from "@/lib/create";

type Objects = {
  network: Network;
  securityGroup: SecurityGroup;
  address: Address;
  publicEndpoint: PublicEndpoint;
};
export type NetworkKind = keyof Objects;

export const nouns: Record<NetworkKind, string> = {
  network: "network",
  securityGroup: "security group",
  address: "address",
  publicEndpoint: "public endpoint",
};

const plurals: Record<NetworkKind, string> = {
  network: "networks",
  securityGroup: "securitygroups",
  address: "addresses",
  publicEndpoint: "publicendpoints",
};

/** The permission a verb on a kind needs at the project, as TestPermissions names it. */
export const permission = (kind: NetworkKind, verb: "apply" | "delete") => `network.${plurals[kind]}.${verb}`;

type Calls<T> = {
  get: (project: string, name: string) => Promise<T>;
  /** Create-only refuses an existing object with AlreadyExists, changing nothing. */
  apply: (o: T, createOnly?: boolean) => Promise<{ created: boolean }>;
  del: (project: string, name: string) => Promise<unknown>;
};

const gone = (name: string) => new Error(`${name} is gone.`);

/** The four kinds' Get, Apply and Delete, alike. */
function callsOf<K extends NetworkKind>(kind: K, t: Transport): Calls<Objects[K]> {
  switch (kind) {
    case "network": {
      const c = createClient(NetworkService, t);
      return {
        get: async (project, name) => (await c.getNetwork({ project, name })).network ?? Promise.reject(gone(name)),
        apply: (o, createOnly = false) => c.applyNetwork({ network: o as Network, createOnly }),
        del: (project, name) => c.deleteNetwork({ project, name }),
      } as Calls<Objects[K]>;
    }
    case "securityGroup": {
      const c = createClient(SecurityGroupService, t);
      return {
        get: async (project, name) => (await c.getSecurityGroup({ project, name })).securityGroup ?? Promise.reject(gone(name)),
        apply: (o, createOnly = false) => c.applySecurityGroup({ securityGroup: o as SecurityGroup, createOnly }),
        del: (project, name) => c.deleteSecurityGroup({ project, name }),
      } as Calls<Objects[K]>;
    }
    case "address": {
      const c = createClient(AddressService, t);
      return {
        get: async (project, name) => (await c.getAddress({ project, name })).address ?? Promise.reject(gone(name)),
        apply: (o, createOnly = false) => c.applyAddress({ address: o as Address, createOnly }),
        del: (project, name) => c.deleteAddress({ project, name }),
      } as Calls<Objects[K]>;
    }
    default: {
      const c = createClient(PublicEndpointService, t);
      return {
        get: async (project, name) => (await c.getPublicEndpoint({ project, name })).publicEndpoint ?? Promise.reject(gone(name)),
        apply: (o, createOnly = false) => c.applyPublicEndpoint({ publicEndpoint: o as PublicEndpoint, createOnly }),
        del: (project, name) => c.deletePublicEndpoint({ project, name }),
      } as Calls<Objects[K]>;
    }
  }
}

/**
 * Creates the object handed to mutate with one create-only Apply, so an
 * object of its name, made since the form's list loaded, is refused rather
 * than replaced; that refusal is said in the form's words (lib/create.ts).
 */
export function useCreate<K extends NetworkKind>(kind: K, project: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (o: Objects[K]) => {
      const name = o.metadata?.name ?? "";
      if (o.metadata) o.metadata.project = project;
      try {
        await callsOf(kind, transport).apply(o, true);
      } catch (err) {
        throw createError(err, nouns[kind], name);
      }
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** Changes an object's spec as read, and applies it back at that version. */
export function useChange<K extends NetworkKind>(kind: K, project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (change: (o: Objects[K]) => void) => {
      const c = callsOf(kind, transport);
      await readModifyWrite(() => c.get(project, name), change, (o) => c.apply(o));
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/**
 * Applies the object handed to mutate as it is, at the resource version it
 * carries: for a change made on what the person saw, which must not land on
 * a newer object. A newer one makes the API answer Aborted (isStale).
 */
export function useApplyAt<K extends NetworkKind>(kind: K) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (o: Objects[K]) => callsOf(kind, transport).apply(o),
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** Whether a write met a newer version of the object than it was made on. */
export function isStale(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.Aborted;
}

/** Reads the object as it is now, for a dialog to start again from. */
export function useReload<K extends NetworkKind>(kind: K, project: string, name: string) {
  const transport = useTransport();
  return useMutation({ mutationFn: () => callsOf(kind, transport).get(project, name) });
}

export function useDelete(kind: NetworkKind, project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () => callsOf(kind, transport).del(project, name),
    // Not awaited: the page that deleted the object goes on (to the list)
    // before the refetch finds it gone and says so.
    onSettled: () => void queryClient.invalidateQueries(),
  });
}
