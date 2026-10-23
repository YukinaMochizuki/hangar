// A disk's actions, on its page and in its row's menu: Grow, Create image
// (the New image page on this disk, which says whether it can be captured
// now), Edit labels and Delete, each shown once TestPermissions holds its
// permission.

import { useNavigate } from "@tanstack/react-router";
import type { Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { HardDrive, ImagePlus, Trash2 } from "lucide-react";
import { useState } from "react";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { editLabelsAction, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { DeleteDiskDialog, GrowDiskDialog } from "@/components/storage";
import { useCan } from "@/lib/permissions";
import { diskImage, useImages } from "@/lib/storage";

/** What a disk's actions need at its project. */
export const diskPerms = { apply: "storage.disks.apply", delete: "storage.disks.delete", capture: "storage.customimages.apply" } as const;

/** diskPerms, asked of TestPermissions at the project: one answer for the list, the page and the rows. */
export function useDiskCan(project: string) {
  return useCan({ level: "project", name: project }, [diskPerms.apply, diskPerms.delete, diskPerms.capture]);
}

/**
 * A disk's actions, by `can`, the answer of useDiskCan its list or page
 * asked; `after` follows a delete the API took, such as going to the list.
 */
export function useDiskActions(d: Disk, can: Can, after?: () => void): ActionSet {
  const [open, setOpen] = useState<"grow" | "delete" | "labels" | null>(null);
  const navigate = useNavigate();
  const project = d.metadata?.project ?? "";
  const images = useImages();
  const image = images.items.find((i) => i.metadata?.name === diskImage(d));
  const actions: ObjectAction[] = [];
  if (can(diskPerms.apply)) actions.push({ label: "Grow", icon: HardDrive, onSelect: () => setOpen("grow") });
  if (can(diskPerms.capture)) {
    const link = { to: "/projects/$project/new/custom-image", params: { project }, search: { disk: d.metadata?.name ?? "" } };
    actions.push({ label: "Create image", icon: ImagePlus, disabled: !!d.metadata?.deleteTime, link, onSelect: () => void navigate(link) });
  }
  if (can(diskPerms.delete)) actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: () => setOpen("delete") });
  const close = () => setOpen(null);
  return {
    actions,
    editLabels: editLabelsAction(can(diskPerms.apply), !!d.metadata?.deleteTime, () => setOpen("labels")),
    deleting: !!d.metadata?.deleteTime,
    dialogs: (
      <>
        {/* Mounted while closed, so what they say of the disk's machine has loaded when they open. */}
        <GrowDiskDialog key={String(open === "grow")} d={d} image={image} open={open === "grow"} onClose={close} />
        <DeleteDiskDialog d={d} open={open === "delete"} onClose={close} after={after} />
        {open === "labels" && <EditLabelsDialog kind="disk" object={d} onClose={close} />}
      </>
    ),
  };
}
