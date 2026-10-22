// An image's actions, on its page and in its row's menu: New disk from it,
// for whoever may make disks in the project, and Edit labels, for whoever
// may apply images, which are the platform's (normally its administrators
// alone). An image at fault makes no disk, so New disk from it is off.

import { useNavigate } from "@tanstack/react-router";
import type { Image } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { Plus } from "lucide-react";
import { useState } from "react";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { editLabelsAction, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { useCan, usePlatformCan } from "@/lib/permissions";
import { imageState } from "@/lib/storage";

/** What an image's actions need: making a disk at the project, applying images at the platform. */
export const imagePerms = { disk: "storage.disks.apply", apply: "storage.images.apply" } as const;

/**
 * The two questions an image's list and page ask, a disk at the project and
 * an image at the platform, as one Can.
 */
export function useImageCan(project: string): Can {
  const atProject = useCan({ level: "project", name: project }, [imagePerms.disk]);
  const atPlatform = usePlatformCan([imagePerms.apply]);
  return (p) => (p === imagePerms.apply ? atPlatform.can(p) : atProject.can(p));
}

/** An image's actions, by `can`, the answer of useImageCan its list or page asked. */
export function useImageActions(img: Image, project: string, can: Can): ActionSet {
  const name = img.metadata?.name ?? "";
  const navigate = useNavigate();
  const [labels, setLabels] = useState(false);
  const mayDisk = can(imagePerms.disk);
  const mayApply = can(imagePerms.apply);
  const actions: ObjectAction[] = [];
  if (mayDisk) {
    const link = { to: "/projects/$project/new/disk", params: { project }, search: { image: name } };
    actions.push({
      label: "New disk from it",
      icon: Plus,
      primary: true,
      disabled: imageState(img).tone === "fail",
      link,
      onSelect: () => void navigate(link),
    });
  }
  return {
    actions,
    editLabels: editLabelsAction(mayApply, !!img.metadata?.deleteTime, () => setLabels(true)),
    deleting: !!img.metadata?.deleteTime,
    dialogs: labels && <EditLabelsDialog kind="image" object={img} onClose={() => setLabels(false)} />,
  };
}
