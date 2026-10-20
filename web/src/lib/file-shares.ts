// FileShares as the console reads and changes them (docs/decisions/file-shares.md).
// Who uses a share is read from the declaring Containers' and VMs' specs as well
// as status.users, so a share the API refuses to delete is never shown unused.

import { clone, create, toJson, type JsonValue } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import {
  FileSharePhase,
  FileShareSchema,
  FileShareService,
  FileShareSpecSchema,
  FileShareSquash,
  type FileShare,
} from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { readModifyWrite } from "@/lib/actions";
import { createError } from "@/lib/create";
import { refetchInterval, type ListOpts, type MachineKind, type MachineView } from "@/lib/machines";
import { toYaml } from "@/lib/manifest";
import { quantityGiB } from "@/lib/quantity";
import { staleGrow, type GrowRequest, type Listing } from "@/lib/storage";

export const sharePerms = { apply: "storage.fileshares.apply", delete: "storage.fileshares.delete" } as const;

// The API's largest page: a project holds far fewer shares than this.
const pageSize = 1000;

/** useFileShares lists the project's file shares, asked again every few seconds. */
export function useFileShares(project: string, opts: ListOpts = {}): Listing<FileShare> {
  const q = useQuery(FileShareService.method.listFileShares, { project, pageSize }, { refetchInterval, ...opts });
  return {
    items: [...(q.data?.items ?? [])].sort((a, b) => shareName(a).localeCompare(shareName(b))),
    loading: q.isPending,
    error: q.error,
    refetch: () => void q.refetch(),
  };
}

export const shareName = (s: FileShare) => s.metadata?.name ?? "";

/** The networks a share is on: spec.networks, which empty means [default]. */
export function shareNetworks(s: FileShare): string[] {
  return s.spec?.networks.length ? s.spec.networks : ["default"];
}

/** The share's address on a network, once its server has one there. */
export const addressOn = (s: FileShare, network: string) => s.status?.addresses.find((a) => a.network === network)?.address || undefined;

/** The share's size as its node holds it, or as its spec asks before it is placed. */
export const shareGiB = (s: FileShare) => quantityGiB(s.status?.size || s.spec?.size);

export type PhaseWords = { label: string; tone: "ok" | "pending" | "off"; pulse?: boolean };

/** What the console shows of status.phase; a share the apiserver has not written a phase for is Pending, as it stores every new one. */
export function phaseWords(s: FileShare): PhaseWords {
  switch (s.status?.phase) {
    case FileSharePhase.AVAILABLE:
      return { label: "Available", tone: "off" };
    case FileSharePhase.IN_USE:
      return { label: "In use", tone: "ok" };
    default:
      return { label: "Pending", tone: "pending", pulse: true };
  }
}

export type Squash = "None" | "Root" | "All";

export const squashOf = (s: FileShare): Squash =>
  s.spec?.squash === FileShareSquash.ROOT ? "Root" : s.spec?.squash === FileShareSquash.ALL ? "All" : "None";

const squashEnum: Record<Squash, FileShareSquash> = { None: FileShareSquash.UNSPECIFIED, Root: FileShareSquash.ROOT, All: FileShareSquash.ALL };

export const squashWords: Record<Squash, { label: string; text: string }> = {
  None: { label: "None", text: "Every user keeps its own ID on the share, root included." },
  Root: { label: "Root", text: "Root in a machine is the anonymous user on the share; other users keep their IDs." },
  All: { label: "All", text: "Every user in a machine is one anonymous user and group on the share." },
};

/** "default", "default or data", "default, data or ops". */
export function orList(xs: string[], word = "or"): string {
  return xs.length <= 1 ? (xs[0] ?? "") : `${xs.slice(0, -1).join(", ")} ${word} ${xs[xs.length - 1]}`;
}

/** Sorts network names with default first, as the pages list them. */
export const byDefaultFirst = (a: string, b: string) => (a === "default" ? -1 : b === "default" ? 1 : a.localeCompare(b));

/** The options a VM mounts a share with: those containers are mounted with, and the short timeout (docs/decisions/file-shares.md). */
const mountOptions = (readOnly: boolean) => `${readOnly ? "ro," : ""}nosuid,nodev,timeo=50`;

/** The command a VM mounts the share with, from an address. */
export const mountCommand = (name: string, address: string | undefined, readOnly = false) =>
  `mount -t nfs4 -o ${mountOptions(readOnly)} ${address ?? "ADDRESS"}:/ /mnt/${name}`;

/**
 * The commands one VM mounts a share with: its NFSv4 client identity set to
 * its machine's ID first, so it is a client of its own and never takes
 * another guest's state, then the mount.
 */
export const vmMountCommands = (name: string, address: string | undefined, readOnly: boolean, id: string) =>
  [`modprobe nfs`, `echo ${id} > /sys/module/nfs/parameters/nfs4_unique_id`, mountCommand(name, address, readOnly)].join("\n");

/** A container's volumes entry naming the share. */
export const volumeSnippet = (name: string, target = "/data") => toYaml({ volumes: [{ source: name, sourceKind: "FileShare", target }] });

/** A VM's spec.fileShares entry naming the share. */
export const vmSnippet = (name: string, readOnly = false) => toYaml({ fileShares: [readOnly ? { name, readOnly: true } : { name }] });

type Json = { [k: string]: JsonValue };

/** The share as `hangar apply -f` takes it: its name, user labels and spec. */
export function shareYaml(s: FileShare): string {
  const json = toJson(FileShareSchema, s) as Json;
  const meta = (json.metadata ?? {}) as Json;
  const metadata: Json = { name: meta.name ?? "" };
  const labels = Object.entries((meta.labels ?? {}) as Json).filter(([k]) => !/\.hangarlab\.cloud\//.test(k));
  if (labels.length) metadata.labels = Object.fromEntries(labels);
  return toYaml({ apiVersion: "storage.hangarlab.cloud/v1alpha1", kind: "FileShare", metadata, spec: json.spec ?? {} });
}

/** A new share as the New file share page describes it; size is whole GiB as typed. */
export function newFileShare(
  project: string,
  name: string,
  f: { networks: string[]; size: string; squash: Squash; anonUid: string; anonGid: string },
): FileShare {
  const anon = f.squash === "All";
  return create(FileShareSchema, {
    metadata: { name, project },
    spec: {
      networks: f.networks,
      size: f.size === "" ? "" : `${f.size}Gi`,
      squash: squashEnum[f.squash],
      anonUid: anon && f.anonUid !== "" ? Number(f.anonUid) : undefined,
      anonGid: anon && f.anonGid !== "" ? Number(f.anonGid) : undefined,
    },
  });
}

/** The share as read, with its networks set to these, at the version read. */
export function withNetworks(s: FileShare, networks: string[]): FileShare {
  const o = clone(FileShareSchema, s);
  o.spec ??= create(FileShareSpecSchema);
  o.spec.networks = networks;
  return o;
}

/** One declaration of a share by a Container or VM, as its spec has it. */
export type Declaration = {
  kind: MachineKind;
  name: string;
  share: string;
  readOnly: boolean;
  /** A container's target. */
  path: string;
  /** The networks the machine has an interface on, as its spec asks; [default] when it names none. */
  networks: string[];
  /** The network its Machine mounts the share from, which the apiserver stores; empty until its Machine declares the share. */
  stored: string;
  /** Why a container is held back (Ready False, MachineHeldBack), such as its share being on no node yet. */
  heldBack?: string;
  /** Why a VM's shares are not mounted yet: its FileSharesOnNode condition when False. */
  notOnNode?: string;
  /** Whether nothing live declares it: the Container or VM is being deleted, and so is its Machine if that declares the share too. */
  deleting: boolean;
};

/**
 * The network a machine mounts a share from, as the apiserver chooses it: the
 * first of the machine's own networks, in order, that the share is on.
 */
export const mountNetwork = (machineNetworks: string[], shareNets: string[]) => machineNetworks.find((n) => shareNets.includes(n)) ?? "";

/** What a machine's spec declares of shares. */
export function declarationsOf(m: MachineView): Declaration[] {
  const ifaces = (m.container?.spec?.interfaces ?? m.vm?.spec?.interfaces ?? []).map((i) => i.network);
  const networks = ifaces.length ? [...new Set(ifaces)] : ["default"];
  const onMachine = (share: string) => m.machine?.spec?.fileShares.find((f) => f.fileShare === share);
  const stored = (share: string) => onMachine(share)?.network ?? "";
  // A Machine declaring the share is a declarer of its own, live until it too is being deleted.
  const deleting = (share: string) => m.deleting && !(onMachine(share) && !m.machine?.metadata?.deleteTime);
  const conds = m.container?.status?.conditions ?? m.vm?.status?.conditions ?? [];
  const ready = conds.find((c) => c.type === "Ready");
  const heldBack = m.kind === "Container" && ready?.status === ConditionStatus.FALSE && ready.reason === "MachineHeldBack" ? ready.message : undefined;
  const onNode = conds.find((c) => c.type === "FileSharesOnNode");
  const notOnNode = onNode?.status === ConditionStatus.FALSE ? onNode.message : undefined;
  const base = { kind: m.kind, name: m.name, networks, heldBack, notOnNode };
  if (m.container) {
    return (m.container.spec?.volumes ?? [])
      .filter((v) => v.sourceKind === "FileShare")
      .map((v) => ({ ...base, share: v.source, readOnly: v.readOnly, path: v.target, stored: stored(v.source), deleting: deleting(v.source) }));
  }
  return (m.vm?.spec?.fileShares ?? []).map((f) => ({ ...base, share: f.name, readOnly: f.readOnly, path: "", stored: stored(f.name), deleting: deleting(f.name) }));
}

/** One machine using a share: what declares it, and the network and address it mounts from. */
export type ShareUse = {
  kind: MachineKind;
  name: string;
  readOnly: boolean;
  path: string;
  /** The network it mounts from; empty when nothing says yet. */
  network: string;
  /** The share's address on that network; undefined until its server has one. */
  address?: string;
  heldBack?: string;
  notOnNode?: string;
  /** Whether a live Container or VM declares it, which the API refuses to delete the share over (a status.users entry alone does not). */
  live: boolean;
  /** Whether what declares it is being deleted: the share can go once it is gone. */
  deleting: boolean;
};

/**
 * The machines that use a share: status.users, and every declaration the
 * apiserver has not listed there yet. Each mounts from the network
 * status.users names, else as the apiserver chooses it (declarer.mountsFrom):
 * the network its Machine stores while the share is on it, else mountNetwork.
 */
export function shareUses(s: FileShare, decls: Declaration[]): ShareUse[] {
  const nets = shareNetworks(s);
  const mine = decls.filter((d) => d.share === shareName(s));
  const out: ShareUse[] = [];
  const seen = new Set<string>();
  const add = (u: Omit<ShareUse, "address">) => {
    const key = `${u.kind}/${u.name}`;
    if (seen.has(key)) return;
    seen.add(key);
    out.push({ ...u, address: u.network ? addressOn(s, u.network) : undefined });
  };
  for (const u of s.status?.users ?? []) {
    if (u.kind !== "Container" && u.kind !== "VirtualMachine") continue;
    const d = mine.find((x) => x.kind === u.kind && x.name === u.name);
    add({
      kind: u.kind,
      name: u.name,
      readOnly: u.readOnly,
      path: u.path,
      network: u.network,
      heldBack: d?.heldBack,
      notOnNode: d?.notOnNode,
      live: !!d && !d.deleting,
      deleting: !!d?.deleting,
    });
  }
  for (const d of mine) {
    add({
      kind: d.kind,
      name: d.name,
      readOnly: d.readOnly,
      path: d.path,
      network: d.stored ? (nets.includes(d.stored) ? d.stored : "") : mountNetwork(d.networks, nets),
      heldBack: d.heldBack,
      notOnNode: d.notOnNode,
      live: !d.deleting,
      deleting: d.deleting,
    });
  }
  return out.sort((a, b) => a.name.localeCompare(b.name));
}

/** The uses that mount from a network: removing it from the share is refused while there is one. */
export const usesVia = (uses: ShareUse[], network: string) => uses.filter((u) => u.network === network);

/**
 * useCreateFileShare creates a share with a create-only apply, which the API
 * refuses for a name that exists, saying so as the form does (lib/create.ts).
 */
export function useCreateFileShare() {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (fileShare: FileShare) => {
      try {
        await createClient(FileShareService, transport).applyFileShare({ fileShare, createOnly: true });
      } catch (err) {
        throw createError(err, "file share", fileShare.metadata?.name ?? "");
      }
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/**
 * useSetNetworks applies the share handed to mutate as it is, at the
 * resource version it carries: the networks chosen depend on the share as
 * the dialog showed it, so a newer one is never overwritten, and the API
 * answers Aborted (isStale in lib/network-actions.ts).
 */
export function useSetNetworks() {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (fileShare: FileShare) => createClient(FileShareService, transport).applyFileShare({ fileShare }),
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** useReloadShare reads the share as it is now, for a dialog to start again from. */
export function useReloadShare(project: string, name: string) {
  const transport = useTransport();
  return useMutation({
    mutationFn: async () => {
      const { fileShare } = await createClient(FileShareService, transport).getFileShare({ project, name });
      if (!fileShare) throw new Error(`${name} is gone.`);
      return fileShare;
    },
  });
}

/**
 * addNetworks adds networks to a share whatever else it holds: it reads the
 * share, adds those it is not on, and applies it back at the version read,
 * reading again when it changed in between. Nothing is applied when the
 * share is on all of them already. It returns the networks it added, so an
 * undo takes off only those.
 */
export async function addNetworks(transport: ReturnType<typeof useTransport>, project: string, name: string, networks: string[]): Promise<string[]> {
  const c = createClient(FileShareService, transport);
  let more: string[] = [];
  await readModifyWrite(
    async () => {
      const { fileShare } = await c.getFileShare({ project, name });
      if (!fileShare) throw new Error(`File share ${name} is gone.`);
      return fileShare;
    },
    (o) => {
      const now = shareNetworks(o);
      more = networks.filter((n) => !now.includes(n));
      o.spec ??= create(FileShareSpecSchema);
      o.spec.networks = [...now, ...more];
    },
    async (o) => {
      if (more.length) await c.applyFileShare({ fileShare: o });
    },
  );
  return more;
}

/**
 * removeNetworks takes networks off a share, read and applied back at the
 * version read, as addNetworks adds them. A network some machine mounts the
 * share from (mountedFrom, given the share as read) is never taken off: it
 * throws instead, applying nothing. Nothing is applied when the share is on
 * none of them.
 */
export async function removeNetworks(
  transport: ReturnType<typeof useTransport>,
  project: string,
  name: string,
  networks: string[],
  mountedFrom: (s: FileShare) => string[],
) {
  const c = createClient(FileShareService, transport);
  let changed = true;
  await readModifyWrite(
    async () => {
      const { fileShare } = await c.getFileShare({ project, name });
      if (!fileShare) throw new Error(`File share ${name} is gone.`);
      return fileShare;
    },
    (o) => {
      const now = shareNetworks(o);
      const held = networks.filter((n) => now.includes(n) && mountedFrom(o).includes(n));
      if (held.length) throw new Error(`${orList(held, "and")} stays on ${name}: a machine mounts it from there now.`);
      const rest = now.filter((n) => !networks.includes(n));
      changed = rest.length < now.length;
      o.spec ??= create(FileShareSpecSchema);
      o.spec.networks = rest;
    },
    async (o) => {
      if (changed) await c.applyFileShare({ fileShare: o });
    },
  );
}

/**
 * useGrowFileShare sets a share's size: it reads the share, applies it back
 * with only spec.size changed at the version read, and reads again when it
 * changed in between. A read that makes the request stale applies nothing.
 */
export function useGrowFileShare(project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (req: GrowRequest) => {
      const c = createClient(FileShareService, transport);
      await readModifyWrite(
        async () => {
          const { fileShare } = await c.getFileShare({ project, name });
          if (!fileShare) throw new Error(`${name} is gone.`);
          return fileShare;
        },
        (o) => {
          const stale = staleGrow(name, req, quantityGiB(o.spec?.size));
          if (stale) throw new Error(stale);
          o.spec ??= create(FileShareSpecSchema);
          o.spec.size = `${req.sizeGi}Gi`;
        },
        (o) => c.applyFileShare({ fileShare: o }),
      );
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** useDeleteFileShare deletes a share; the API refuses one a machine declares. */
export function useDeleteFileShare(project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      await createClient(FileShareService, transport).deleteFileShare({ project, name });
    },
    // Not awaited: the page that deleted the object goes on (to the list)
    // before the refetch finds it gone and says so.
    onSettled: () => void queryClient.invalidateQueries(),
  });
}
