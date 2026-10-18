// A list page's create dialog opened from its address: ?new=1 opens it, and
// closing it, creating, or an answer that the caller may not create takes the
// parameter away, so going back or reloading does not open it again.

import { useEffect, useState } from "react";

/** What a list page is told by its route: whether ?new=1 asked for its dialog, and how to take that back. */
export type NewProps = { startNew?: boolean; onNewClosed?: () => Promise<void> | void };

/** The search a list route takes: new=1, or nothing. */
export function newSearch(s: Record<string, unknown>): { new?: 1 } {
  return s.new === 1 || s.new === "1" ? { new: 1 } : {};
}

/** What a create dialog calls once it created: go opens the new object, after ?new=1 is gone. */
export type OnCreated = (go?: () => void) => void;

/**
 * Whether the page's create dialog is open: opened by its button, or by
 * ?new=1 once the caller may create. Closing it, or created, clears ?new=1;
 * so does settled (the permission answer came or failed) without may.
 */
export function useCreating({ startNew, onNewClosed }: NewProps, may: boolean, settled: boolean): [boolean, (open: boolean) => void, OnCreated] {
  const [creating, setCreating] = useState(false);
  const open = creating || (!!startNew && may);
  const refused = !!startNew && settled && !may;
  useEffect(() => {
    if (refused) void onNewClosed?.();
    // onNewClosed is a new function each render; the refusal is what is acted on.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refused]);
  const clear = () => {
    setCreating(false);
    return startNew ? onNewClosed?.() : undefined;
  };
  const set = (o: boolean) => {
    if (o) setCreating(true);
    else void clear();
  };
  // The object opens once ?new=1 is replaced away, so Back from it does not reopen the dialog.
  const created: OnCreated = (go) => void Promise.resolve(clear()).then(go);
  return [open, set, created];
}
