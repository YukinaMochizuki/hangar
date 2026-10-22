// A file share's actions, on its page and in its row's menu: Edit networks,
// Grow, Edit labels and Delete, each shown once TestPermissions holds its
// permission.

import type { FileShare } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { HardDrive, Network as NetworkIcon, Trash2 } from "lucide-react";
import { useState } from "react";
import { DeleteShareDialog, EditNetworksDialog, GrowShareDialog, useDeclarations } from "@/components/file-shares";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { editLabelsAction, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { shareNetworks, sharePerms, shareUses } from "@/lib/file-shares";
import { useAttachments } from "@/lib/network";

/**
 * A file share's actions, by `can`, the answer of useShareCan its list or
 * page asked; `after` follows a delete the API took. Its page
 * also opens Edit networks with a network added (editNetworks).
 */
export function useFileShareActions(s: FileShare, can: Can, after?: () => void): ActionSet & { editNetworks: (initial: string[]) => void } {
  const project = s.metadata?.project ?? "";
  const [open, setOpen] = useState<"grow" | "delete" | "labels" | null>(null);
  const [editing, setEditing] = useState<string[] | null>(null);
  const canApply = can(sharePerms.apply);
  const canDelete = can(sharePerms.delete);
  const decls = useDeclarations(project);
  const att = useAttachments(project);
  const networks = shareNetworks(s);
  const uses = shareUses(s, decls.items);
  const usesKnown = !decls.loading && !decls.error;
  const actions: ObjectAction[] = [];
  if (canApply) {
    actions.push({ label: "Edit networks", icon: NetworkIcon, onSelect: () => setEditing(networks) });
    actions.push({ label: "Grow", icon: HardDrive, onSelect: () => setOpen("grow") });
  }
  if (canDelete) actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: () => setOpen("delete") });
  const close = () => setOpen(null);
  return {
    actions,
    editLabels: editLabelsAction(canApply, !!s.metadata?.deleteTime, () => setOpen("labels")),
    deleting: !!s.metadata?.deleteTime,
    editNetworks: setEditing,
    dialogs: (
      <>
        {editing && (
          <EditNetworksDialog
            key={editing.join(",")}
            s={s}
            decls={decls.items}
            declsState={decls}
            attachments={att.items}
            onClose={() => setEditing(null)}
            initial={editing}
          />
        )}
        {open === "grow" && <GrowShareDialog s={s} uses={uses} usesKnown={usesKnown} open onClose={close} />}
        {open === "delete" && <DeleteShareDialog s={s} uses={uses} usesKnown={usesKnown} open onClose={close} after={after} />}
        {open === "labels" && <EditLabelsDialog kind="fileShare" object={s} onClose={close} />}
      </>
    ),
  };
}
