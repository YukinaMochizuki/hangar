// Disks and Images as the console reads and changes them. A disk names the
// Machine it is attached to, not the Container or VM; the page links to the
// one that owns that Machine, found by its owner reference
// (docs/decisions/machine-model.md). Creating, growing and deleting a disk
// are the API's own calls, so its rules decide and its error is shown;
// which of them the caller may make is lib/permissions.ts's to say.

import { create } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { DiskAttachmentRole, DiskSchema, DiskService, DiskSpecSchema, type Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { ImageService, type Image } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { readModifyWrite } from "@/lib/actions";
import { createError } from "@/lib/create";
import { refetchInterval, useMachines, type ListOpts, type MachineKind } from "@/lib/machines";
import { formatNumber, quantityGiB } from "@/lib/quantity";

/** rootDiskOf is the annotation a root disk made for a VM carries: the VM's UID. */
export const rootDiskOf = "hangarlab.cloud/virtual-machine";

// The API's largest page: a project holds far fewer disks than this.
const pageSize = 1000;

/** Listing is what a page reads: the objects, or why there are none yet. */
export type Listing<T> = { items: T[]; loading: boolean; error: unknown; refetch: () => void };

const byName = (a: { metadata?: { name: string } }, b: { metadata?: { name: string } }) =>
  (a.metadata?.name ?? "").localeCompare(b.metadata?.name ?? "");

/** useDisks lists the project's disks, asked again every few seconds. */
export function useDisks(project: string, opts: ListOpts = {}): Listing<Disk> {
  const q = useQuery(DiskService.method.listDisks, { project, pageSize }, { refetchInterval, ...opts });
  return {
    items: [...(q.data?.items ?? [])].sort(byName),
    loading: q.isPending,
    error: q.error,
    refetch: () => void q.refetch(),
  };
}

/** useImages lists the platform's images, which every member reads. */
export function useImages(opts: ListOpts = {}): Listing<Image> {
  const q = useQuery(ImageService.method.listImages, { pageSize }, { refetchInterval, ...opts });
  return {
    items: [...(q.data?.items ?? [])].sort(byName),
    loading: q.isPending,
    error: q.error,
    refetch: () => void q.refetch(),
  };
}

/** MachineRef is the Container or VM a Machine runs for, and whether that Machine is being deleted. */
export type MachineRef = { kind: MachineKind; name: string; deleting: boolean };

/**
 * useMachineOwners finds the Container or VM a Machine runs for, by the
 * Machine's name, as a disk's status.machine names it. Undefined while the
 * machines load, when they failed to, or for a Machine no object of the
 * project owns.
 */
export function useMachineOwners(project: string): (machine: string) => MachineRef | undefined {
  const { items } = useMachines(project);
  const owners = new Map<string, MachineRef>();
  for (const m of items) {
    const name = m.machine?.metadata?.name;
    if (name) owners.set(name, { kind: m.kind, name: m.name, deleting: !!m.machine?.metadata?.deleteTime });
  }
  return (machine) => owners.get(machine);
}

/** gib writes a size quantity in GiB, as the console shows it: "40 GiB". */
export function gib(q: string | undefined): string {
  const n = quantityGiB(q);
  return Number.isFinite(n) ? `${Number.isInteger(n) ? n : n.toFixed(1)} GiB` : "–";
}

/** rootDiskOwner is the VM a disk was made for as its root disk, from its annotation and owner. */
export function rootDiskOwner(d: Disk): string | undefined {
  const uid = d.metadata?.annotations[rootDiskOf];
  if (!uid) return undefined;
  return d.metadata?.ownerReferences.find((r) => r.uid === uid)?.name;
}

/** diskImage is the platform image a disk was copied from, or undefined for a blank disk or one of a custom image. */
export function diskImage(d: Disk): string | undefined {
  const src = d.spec?.source?.source;
  return src?.case === "image" && !src.value.project ? src.value.name : undefined;
}

/** diskCustomImage is the project's custom image a disk was copied from, if it was. */
export function diskCustomImage(d: Disk): string | undefined {
  const src = d.spec?.source?.source;
  return src?.case === "image" && src.value.project ? src.value.name : undefined;
}

/** ImageChoice is the image a boot disk is copied from: a platform image, or one of the project's custom images. */
export type ImageChoice = { name: string; custom: boolean };

/** growingTo is the size a disk is growing to: its spec asks for more than its node holds. */
export function growingTo(d: Disk): string | undefined {
  const want = quantityGiB(d.spec?.size);
  const have = quantityGiB(d.status?.size);
  return Number.isFinite(have) && want > have ? d.spec?.size : undefined;
}

/** diskFault is why a disk is not ready on its node: the disk's Ready condition, which its node writes, when False. */
export function diskFault(d: Disk): { reason: string; message: string } | undefined {
  const c = d.status?.conditions.find((x) => x.type === "Ready");
  return c?.status === ConditionStatus.FALSE ? { reason: c.reason, message: c.message } : undefined;
}

/**
 * growthBlocked is why a placed disk's growth is not granted: its Allocated
 * condition when False, such as ResizeDoesNotFit. A disk not yet placed is
 * Allocated False too (WaitingForFirstMachine), which blocks nothing.
 */
export function growthBlocked(d: Disk): { reason: string; message: string } | undefined {
  if (!d.status?.node) return undefined;
  const c = d.status.conditions.find((x) => x.type === "Allocated");
  if (c?.status === ConditionStatus.FALSE) return { reason: c.reason, message: c.message };
  // A growth by itself its machine asked for, waiting on quota or the node (docs/decisions/disk-autogrow.md).
  const g = d.status.conditions.find((x) => x.type === "DiskGrowBlocked");
  return g?.status === ConditionStatus.TRUE ? { reason: g.reason, message: g.message } : undefined;
}

/** growthOf says how a disk grows by itself: up to its max, at its threshold, by its step; undefined if it does not. */
export function growthOf(d: Disk): string | undefined {
  const g = d.spec?.grow;
  if (!g) return undefined;
  const max = quantityGiB(g.max);
  if (quantityGiB(d.status?.size) >= max) return `at its largest, ${formatNumber(max)} GiB`;
  return `up to ${formatNumber(max)} GiB, at ${g.threshold || "80%"} full, by ${g.step || "2x"}`;
}

/**
 * growFloorGiB is the smallest size a disk may be set to: what its node
 * holds, which never shrinks, and for a disk of an image the image's
 * smallest disk. A growth not yet granted can be withdrawn down to it.
 */
export function growFloorGiB(d: Disk, img: Image | undefined): number {
  const held = quantityGiB(d.status?.size);
  return Math.max(Number.isFinite(held) ? Math.ceil(held) : 1, diskImage(d) ? imageFloorGiB(img) : 1);
}

/** ImageState is what the console shows of an image's Verified condition. */
export type ImageState = { label: string; tone: "ok" | "fail" | "off" | "pending"; reason?: string; message?: string };

/** imageState is what nodes found of an image: its Verified condition. */
export function imageState(img: Image): ImageState {
  const v = img.status?.conditions.find((c) => c.type === "Verified");
  if (!v || v.status === ConditionStatus.UNKNOWN) return { label: "Not fetched yet", tone: "off" };
  if (v.status === ConditionStatus.TRUE) return { label: "Verified", tone: "ok" };
  return { label: "At fault", tone: "fail", reason: v.reason, message: v.message };
}

/** imageFloorGiB is the smallest disk an image fits on, in whole GiB: the larger of its own size and min_disk_size, as admission holds disks to. */
export function imageFloorGiB(img: Image | undefined): number {
  if (!img) return 1;
  const own = quantityGiB(img.status?.diskSize);
  const min = quantityGiB(img.spec?.minDiskSize);
  return Math.max(1, Number.isFinite(own) ? Math.ceil(own) : 1, Number.isFinite(min) ? Math.ceil(min) : 1);
}

/** DiskUse is what a disk is made for: a copy of an image to boot from, or blank for data. */
export type DiskUse = "boot" | "data";

/**
 * intendedUse is the label the New disk page writes, recording what the disk
 * was made for. It is the user's own label and restricts nothing: any disk
 * attaches either way.
 */
export const intendedUse = "hangar/intended-use";

/** madeAs is what a disk's intended-use label says it was made as, if it holds one the console knows. */
export function madeAs(d: Disk): DiskUse | undefined {
  const v = d.metadata?.labels[intendedUse];
  return v === "boot" || v === "data" ? v : undefined;
}

/** What a disk is used as now, as its status.attachment says (docs/decisions/container-volumes.md). */
export type DiskUsedAs = {
  role: "boot" | "data" | "volume";
  kind: MachineKind;
  /** The Container's or VM's name. */
  name: string;
  /** The device the guest sees, such as "vdc". */
  device: string;
  /** A volume's mount path. */
  path: string;
  readOnly: boolean;
};

const roles: Partial<Record<DiskAttachmentRole, DiskUsedAs["role"]>> = {
  [DiskAttachmentRole.BOOT]: "boot",
  [DiskAttachmentRole.DATA]: "data",
  [DiskAttachmentRole.VOLUME]: "volume",
};

/**
 * usedAs reads a disk's status.attachment, which the apiserver alone writes;
 * undefined when the disk is attached to nothing, or to something the
 * console has no page for.
 */
export function usedAs(d: Disk): DiskUsedAs | undefined {
  const a = d.status?.attachment;
  const role = a ? roles[a.as] : undefined;
  if (!a || !role || (a.kind !== "Container" && a.kind !== "VirtualMachine")) return undefined;
  return { role, kind: a.kind, name: a.name, device: a.device, path: a.path, readOnly: a.readOnly };
}

/**
 * newDisk is a disk as the New disk page describes it; size is whole GiB as
 * typed, image is for a boot disk alone. A custom image is named with the
 * disk's own project, the only one the API takes.
 */
export function newDisk(
  project: string,
  name: string,
  use: DiskUse,
  size: string,
  image: ImageChoice | undefined,
  grow?: { max: string; threshold: string; step: string },
): Disk {
  return create(DiskSchema, {
    metadata: { name, project, labels: { [intendedUse]: use } },
    spec: {
      size: size === "" ? "" : `${size}Gi`,
      grow,
      source:
        use === "boot" && image?.name
          ? { source: { case: "image", value: { name: image.name, project: image.custom ? project : "" } } }
          : {},
    },
  });
}

/**
 * useCreateDisk creates a disk with a create-only apply, which the API
 * refuses for a name that exists, saying so as the form does (lib/create.ts).
 */
export function useCreateDisk() {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (disk: Disk) => {
      try {
        await createClient(DiskService, transport).applyDisk({ disk, createOnly: true });
      } catch (err) {
        throw createError(err, "disk", disk.metadata?.name ?? "");
      }
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** GrowRequest is the size asked for, and the spec.size the dialog showed when it was asked. */
export type GrowRequest = { sizeGi: number; shownGi: number };

/**
 * staleGrow says why a size asked for against shownGi is not applied to a
 * disk whose spec now asks for freshGi: it is already that size, or the
 * change no longer goes the way it did when asked.
 */
export function staleGrow(name: string, { sizeGi, shownGi }: GrowRequest, freshGi: number): string | undefined {
  if (freshGi === sizeGi) return `${name} is already set to ${sizeGi} GiB; nothing is applied.`;
  if (Math.sign(sizeGi - freshGi) !== Math.sign(sizeGi - shownGi)) {
    return `${name} is now set to ${freshGi} GiB, not ${shownGi} GiB as shown; nothing is applied.`;
  }
  return undefined;
}

/**
 * useGrowDisk sets a disk's size: it reads the disk, applies it back with
 * only spec.size changed at the version read, and reads again when it
 * changed in between. A read that makes the request stale applies nothing.
 */
export function useGrowDisk(project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (req: GrowRequest) => {
      const c = createClient(DiskService, transport);
      await readModifyWrite(
        async () => {
          const { disk } = await c.getDisk({ project, name });
          if (!disk) throw new Error(`${name} is gone.`);
          return disk;
        },
        (o) => {
          const stale = staleGrow(name, req, quantityGiB(o.spec?.size));
          if (stale) throw new Error(stale);
          o.spec ??= create(DiskSpecSchema);
          o.spec.size = `${req.sizeGi}Gi`;
        },
        (o) => c.applyDisk({ disk: o }),
      );
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** useDeleteDisk deletes a disk; the API refuses one a live machine has. */
export function useDeleteDisk(project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      await createClient(DiskService, transport).deleteDisk({ project, name });
    },
    // Not awaited: the page that deleted the object goes on (to the list)
    // before the refetch finds it gone and says so.
    onSettled: () => void queryClient.invalidateQueries(),
  });
}
