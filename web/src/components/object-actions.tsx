// An object's actions, made once per kind and drawn in two places: its
// page's head, as buttons, and its row's "..." menu in the list. Each kind
// has one hook that makes them (useDiskActions and the like), so the head and
// the menu hold the same actions, open the same dialogs and are gated by the
// same answer of TestPermissions.

import { Link } from "@tanstack/react-router";
import type { LucideIcon } from "lucide-react";
import { Tag } from "lucide-react";
import type { ReactNode } from "react";
import { RowMenu, type MenuAction } from "@/components/menu";
import { Button, buttonVariants } from "@/components/ui";

export type ObjectAction = {
  label: string;
  icon: LucideIcon;
  onSelect: () => void;
  /** Drawn in the failure colour, as Delete is. */
  danger?: boolean;
  /** The head's one primary button, as Start is. */
  primary?: boolean;
  /** Off while it runs, as Starting… is. */
  pending?: string;
  /** Shown but off, for a reason the page states. */
  disabled?: boolean;
  /** Opens a page, which the head draws as a link to it. */
  link?: { to: string; params: Record<string, string>; search?: Record<string, string> };
};

/**
 * What a kind's hook gives for one object: the head's actions in the
 * head's order, Edit labels where the caller may change labels (drawn
 * beside the labels on the page, and in the menu before Delete), whether
 * the object is being deleted, and the dialogs the actions open, mounted
 * once wherever the actions are drawn.
 */
export type ActionSet = {
  actions: ObjectAction[];
  editLabels?: ObjectAction;
  deleting: boolean;
  dialogs: ReactNode;
};

/**
 * Whether the caller holds a permission, from the one answer of
 * TestPermissions that an object's list or page asked and hands to its
 * kind's hook, so the rows and the page ask no question of their own.
 */
export type Can = (permission: string) => boolean;

/**
 * The Edit labels action, opening the kind's labels dialog, where the
 * caller may change labels; none while the object is being deleted, which
 * the API refuses to change.
 */
export const editLabelsAction = (may: boolean, deleting: boolean, onSelect: () => void): ObjectAction | undefined =>
  may && !deleting ? { label: "Edit labels", icon: Tag, onSelect } : undefined;

/**
 * An object page's actions as buttons. While the object is being deleted
 * they are drawn off.
 */
export function HeadActions({ set }: { set: ActionSet }) {
  if (set.actions.length === 0) return null;
  return (
    // A group for assistive technology alone: its buttons lay out in their parent's row.
    <div role="group" aria-label="Actions" className="contents">
      {set.actions.map((a) => {
        const Icon = a.icon;
        const body = (
          <>
            <Icon /> {a.pending ?? a.label}
          </>
        );
        if (a.link && !set.deleting && !a.disabled) {
          return (
            <Link key={a.label} to={a.link.to} params={a.link.params} search={a.link.search} className={buttonVariants({ variant: a.primary ? "primary" : undefined })}>
              {body}
            </Link>
          );
        }
        return (
          <Button
            key={a.label}
            variant={a.danger ? "danger" : a.primary ? "primary" : undefined}
            disabled={set.deleting || !!a.pending || a.disabled}
            onClick={a.onSelect}
          >
            {body}
          </Button>
        );
      })}
    </div>
  );
}

/** The menu's actions: the head's, with Edit labels before Delete; none while the object is being deleted. */
export function menuActions(set: ActionSet): MenuAction[] {
  if (set.deleting) return [];
  const all = set.actions.map((a): MenuAction => ({ label: a.label, icon: a.icon, danger: a.danger, disabled: !!a.pending || a.disabled, onSelect: a.onSelect }));
  if (!set.editLabels) return all;
  const at = all.findIndex((a) => a.danger);
  const labels = { label: set.editLabels.label, icon: set.editLabels.icon, onSelect: set.editLabels.onSelect };
  return at < 0 ? [...all, labels] : [...all.slice(0, at), labels, ...all.slice(at)];
}

/**
 * A list row's "..." menu of the object's actions. A row the caller may do
 * nothing to has no menu; a row being deleted has one that says so.
 */
export function ObjectRowMenu({ name, set, children }: { name: string; set: ActionSet; children?: (anchor: HTMLElement | null) => ReactNode }) {
  const actions = menuActions(set);
  const any = set.actions.length > 0 || !!set.editLabels;
  return (
    <>
      {any ? (
        <RowMenu label={`Actions for ${name}`} actions={actions} empty="Being deleted: nothing to change.">
          {children}
        </RowMenu>
      ) : (
        <span className="block size-8" />
      )}
      {set.dialogs}
    </>
  );
}
