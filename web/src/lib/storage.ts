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
import { DiskSchema, DiskService, DiskSpecSchema, type Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { ImageService, type Image } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { readModifyWrite } from "@/lib/actions";
import { createError } from "@/lib/create";
import { refetchInterval, useMachines, type MachineKind } from "@/lib/machines";
import { quantityGiB } from "@/lib/quantity";

/** rootDiskOf is the annotation a root disk made for a VM carries: the VM's UID. */
export const rootDiskOf = "hangarlab.cloud/virtual-machine";

// The API's largest page: a project holds far fewer disks than this.
const pageSize = 1000;

/** Listing is what a page reads: the objects, or why there are none yet. */
export type Listing<T> = { items: T[]; loading: boolean; error: unknown; refetch: () => void };

const byName = (a: { metadata?: { name: string } }, b: { metadata?: { name: string } }) =>
  (a.metadata?.name ?? "").localeCompare(b.metadata?.name ?? "");

/** useDisks lists the project's disks, asked again every few seconds. */
export function useDisks(project: string): Listing<Disk> {
  const q = useQuery(DiskService.method.listDisks, { project, pageSize }, { refetchInterval });
  return {
    items: [...(q.data?.items ?? [])].sort(byName),
    loading: q.isPending,
    error: q.error,
    refetch: () => void q.refetch(),
  };
}

/** useImages lists the platform's images, which every member reads. */
export function useImages(): Listing<Image> {
  const q = useQuery(ImageService.method.listImages, { pageSize }, { refetchInterval });
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

/** diskImage is the image a disk was copied from, or undefined for a blank disk. */
export function diskImage(d: Disk): string | undefined {
  return d.spec?.source?.source.case === "image" ? d.spec.source.source.value.name : undefined;
}

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
  return c?.status === ConditionStatus.FALSE ? { reason: c.reason, message: c.message } : undefined;
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
export type ImageState = { label: string; tone: "ok" | "fail" | "off"; reason?: string; message?: string };

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

/** newDisk is a disk as the New disk form describes it. */
export function newDisk(project: string, name: string, sizeGi: number, image: string): Disk {
  return create(DiskSchema, {
    metadata: { name, project },
    spec: {
      size: `${sizeGi}Gi`,
      source: image ? { source: { case: "image", value: { name: image } } } : {},
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
    onSettled: () => queryClient.invalidateQueries(),
  });
}
