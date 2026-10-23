// A custom image's actions, on its page and in its row's menu: New disk from
// it, for whoever may make disks in the project, while it is Ready; Edit
// labels and Delete, for whoever may apply or delete custom images there.
// Deleting one keeps the disks made of it.

import { useNavigate } from "@tanstack/react-router";
import type { CustomImage } from "@hangar/api/hangar/storage/v1alpha1/custom_image_pb";
import { Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { ConfirmDeleteDialog } from "@/components/delete-dialog";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { editLabelsAction, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { customImageState, useDeleteCustomImage } from "@/lib/custom-images";
import { useCan } from "@/lib/permissions";

/** What a custom image's actions need at its project. */
export const customImagePerms = {
  disk: "storage.disks.apply",
  apply: "storage.customimages.apply",
  delete: "storage.customimages.delete",
} as const;

/** customImagePerms, asked at the project: one answer for the list, the page and the rows. */
export function useCustomImageCan(project: string) {
  return useCan({ level: "project", name: project }, [customImagePerms.disk, customImagePerms.apply, customImagePerms.delete]);
}

/** A custom image's actions, by `can`; `after` follows a delete the API took. */
export function useCustomImageActions(ci: CustomImage, project: string, can: Can, after?: () => void): ActionSet {
  const name = ci.metadata?.name ?? "";
  const navigate = useNavigate();
  const [open, setOpen] = useState<"labels" | "delete" | null>(null);
  const del = useDeleteCustomImage(project, name);
  const actions: ObjectAction[] = [];
  if (can(customImagePerms.disk)) {
    const link = { to: "/projects/$project/new/disk", params: { project }, search: { customImage: name } };
    actions.push({
      label: "New disk from it",
      icon: Plus,
      primary: true,
      disabled: customImageState(ci).tone !== "ok",
      link,
      onSelect: () => void navigate(link),
    });
  }
  if (can(customImagePerms.delete)) actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: () => setOpen("delete") });
  const close = () => setOpen(null);
  return {
    actions,
    editLabels: editLabelsAction(can(customImagePerms.apply), !!ci.metadata?.deleteTime, () => setOpen("labels")),
    deleting: !!ci.metadata?.deleteTime,
    dialogs: (
      <>
        <ConfirmDeleteDialog
          open={open === "delete"}
          onClose={close}
          title={`Delete image ${name}`}
          name={name}
          consequence="No new disk can be made from it. Disks already made from it, and the machines booting them, keep working."
          cli={`hangar delete customimage ${name} -p ${project}`}
          del={del}
          after={after}
        />
        {open === "labels" && <EditLabelsDialog kind="customImage" object={ci} onClose={close} />}
      </>
    ),
  };
}
