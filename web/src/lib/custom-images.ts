// A project's CustomImages as the console reads and changes them: images a
// project captured from one of its own stopped disks (docs/decisions/
// custom-images.md). Capturing, creating and deleting are the API's own
// calls, so its rules decide and its error is shown; the console only says
// beforehand which disks the API would refuse to capture, from what the
// disk and its machine report.

import { create } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { CustomImageSchema, CustomImageService, type CustomImage } from "@hangar/api/hangar/storage/v1alpha1/custom_image_pb";
import type { Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { createError } from "@/lib/create";
import { isLive, refetchInterval, type ListOpts, type MachineView } from "@/lib/machines";
import { quantityGiB } from "@/lib/quantity";
import type { ImageState, Listing } from "@/lib/storage";

// The API's largest page: a project holds far fewer images than this.
const pageSize = 1000;

/** useCustomImages lists the project's custom images, asked again every few seconds. */
export function useCustomImages(project: string, opts: ListOpts = {}): Listing<CustomImage> {
  const q = useQuery(CustomImageService.method.listCustomImages, { project, pageSize }, { refetchInterval, ...opts });
  return {
    items: [...(q.data?.items ?? [])].sort((a, b) => (a.metadata?.name ?? "").localeCompare(b.metadata?.name ?? "")),
    loading: q.isPending,
    error: q.error,
    refetch: () => void q.refetch(),
  };
}

/** sourceDisk is the disk a custom image was captured from. */
export function sourceDisk(ci: CustomImage): string | undefined {
  return ci.spec?.source?.source.case === "disk" ? ci.spec.source.source.value.name : undefined;
}

/**
 * customImageState is what the console shows of a custom image's Ready
 * condition: Capturing until its node says otherwise, then Ready, or
 * Failed for good (CaptureFailed), with the node's message.
 */
export function customImageState(ci: CustomImage): ImageState & { capturing: boolean } {
  if (ci.metadata?.deleteTime) return { label: "Deleting", tone: "off", capturing: false };
  const c = ci.status?.conditions.find((x) => x.type === "Ready");
  if (c?.status === ConditionStatus.TRUE) return { label: "Ready", tone: "ok", capturing: false };
  if (c?.reason === "CaptureFailed") return { label: "Failed", tone: "fail", reason: c.reason, message: c.message, capturing: false };
  return { label: "Capturing", tone: "off", message: c?.message, capturing: true };
}

/** customImageFloorGiB is the smallest disk a custom image fits on: the source disk's size, or min_disk_size if larger. */
export function customImageFloorGiB(ci: CustomImage | undefined): number {
  if (!ci) return 1;
  const own = quantityGiB(ci.status?.diskSize);
  const min = quantityGiB(ci.spec?.minDiskSize);
  return Math.max(1, Number.isFinite(own) ? Math.ceil(own) : 1, Number.isFinite(min) ? Math.ceil(min) : 1);
}

/** CaptureRefusal is why a disk cannot be captured now: a short label for its row, and the whole reason. */
export type CaptureRefusal = { label: string; reason: string };

/**
 * captureRefusal says why the API would refuse to capture a disk now, from
 * what the disk and its machine report: a disk never made on a node, or one
 * whose machine is not stopped. Undefined when it would be taken. The API
 * decides; this only keeps the form from offering what it refuses.
 */
export function captureRefusal(d: Disk, machines: MachineView[]): CaptureRefusal | undefined {
  if (d.metadata?.deleteTime) return { label: "Deleting", reason: "It is being deleted." };
  if (!d.status?.node) return { label: "Never used", reason: "It has not been made on a node yet: no machine has used it." };
  const machine = d.status.machine;
  if (!machine) return undefined;
  const m = machines.find((x) => x.machine?.metadata?.name === machine);
  const who = m ? m.name : machine;
  if (!m || isLive(m)) return { label: `${who} is running`, reason: `${who} has it and is not stopped. Stop ${who} first.` };
  return undefined;
}

/** newCustomImage is a custom image as the New image page describes it. */
export function newCustomImage(project: string, name: string, disk: string, os: string, version: string, defaultUser: string): CustomImage {
  return create(CustomImageSchema, {
    metadata: { name, project },
    spec: { source: { source: { case: "disk", value: { name: disk } } }, os, version, defaultUser },
  });
}

/** useCreateCustomImage creates a custom image with a create-only apply, which the API refuses for a name that exists. */
export function useCreateCustomImage() {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (customImage: CustomImage) => {
      try {
        await createClient(CustomImageService, transport).applyCustomImage({ customImage, createOnly: true });
      } catch (err) {
        throw createError(err, "image", customImage.metadata?.name ?? "");
      }
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** useDeleteCustomImage deletes a custom image; disks made of it stay. */
export function useDeleteCustomImage(project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      await createClient(CustomImageService, transport).deleteCustomImage({ project, name });
    },
    onSettled: () => void queryClient.invalidateQueries(),
  });
}
