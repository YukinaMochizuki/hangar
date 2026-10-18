// The command palette as drawn: the top bar's search control, and a combobox
// over a listbox of sections with what has not answered stated under them.
// What it lists, and what choosing does, is components/command-palette.tsx's.

import { CornerDownLeft, Search, type LucideIcon } from "lucide-react";
import { useEffect, useRef, type KeyboardEvent, type ReactNode } from "react";
import { Skeleton } from "@/components/states";
import { Kbd } from "@/components/ui";
import { cn } from "@/lib/cn";
import { matchAt } from "@/lib/palette";

/** Something the palette can open or do. */
export type Entry = {
  id: string;
  icon: LucideIcon;
  /** The name shown first: a display name, or the object's name. */
  title: string;
  /** The ID, in mono, when it differs from the title. */
  id2?: string;
  /** A short fact beside it: a state, a size, a tenant. */
  detail?: string;
  run: () => void;
};

/** A heading and the entries under it. */
export type Section = { key: string; title: string; entries: Entry[] };

/** A list the palette searches that has not answered: still loading, or failed with the API's message. */
export type SourceState = { key: string; noun: string; state: "loading" | "failed"; error?: string };

/** Whether the platform's shortcut is ⌘K rather than Ctrl+K. */
export function isMac(): boolean {
  return typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);
}

/** The control in the top bar: a field-like button on wide screens, an icon on a phone. */
export function SearchButton({ onOpen }: { onOpen: () => void }) {
  return (
    <>
      <button
        onClick={onOpen}
        aria-haspopup="dialog"
        aria-keyshortcuts={isMac() ? "Meta+K" : "Control+K"}
        className="hidden h-8 w-64 shrink-0 items-center gap-2 rounded-md border bg-surface px-2.5 text-[13px] text-subtle hover:border-border-strong md:flex"
      >
        <Search className="size-3.5" />
        <span className="flex-1 text-left">Search or jump to…</span>
        <Kbd>{isMac() ? "⌘" : "Ctrl"}</Kbd>
        <Kbd>K</Kbd>
      </button>
      <button aria-label="Search" aria-haspopup="dialog" onClick={onOpen} className="rounded-md p-1.5 text-muted hover:bg-surface-2 md:hidden">
        <Search className="size-4" />
      </button>
    </>
  );
}

/** text with the query's first occurrence in it marked. */
function Marked({ text, query }: { text: string; query: string }) {
  const at = matchAt(query, text);
  if (!at) return <>{text}</>;
  return (
    <>
      {text.slice(0, at[0])}
      <mark className="rounded-[2px] bg-accent-soft px-px text-foreground">{text.slice(at[0], at[1])}</mark>
      {text.slice(at[1])}
    </>
  );
}

const optionId = (id: string) => `palette-${id.replace(/[^a-zA-Z0-9_-]/g, "_")}`;

// What Tab may move between inside the palette.
const focusable = "input, button, [href], [tabindex]:not([tabindex='-1'])";

/**
 * The palette, open while it is mounted. Which entry is active, and what
 * choosing one does, is the caller's; the keys arrive as onMove and onChoose.
 */
export function PaletteView({
  onClose,
  query,
  onQuery,
  sections,
  sources,
  active,
  onActive,
  onMove,
  onChoose,
  noMatch,
  scopeNote,
}: {
  onClose: () => void;
  query: string;
  onQuery: (q: string) => void;
  sections: Section[];
  sources: SourceState[];
  active?: string;
  onActive: (id: string) => void;
  /** Moves the active entry by d, wrapping. */
  onMove: (d: number) => void;
  onChoose: (id: string) => void;
  /** What nothing matched in, when every source answered: "Demo, its projects and tenants, or the pages". */
  noMatch: string;
  /** What is searched, for the footer. */
  scopeNote: string;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const field = useRef<HTMLInputElement>(null);
  // Unmounting takes the dialog out of the top layer with it, and fires no close.
  useEffect(() => {
    const d = ref.current;
    if (d && !d.open) d.showModal();
    // The field takes focus once the dialog is open, where it can.
    field.current?.focus();
  }, []);
  // The active entry stays in view as the arrows move it.
  useEffect(() => {
    if (!active) return;
    const el = document.getElementById(optionId(active));
    el?.scrollIntoView?.({ block: "nearest" });
  }, [active]);

  const count = sections.reduce((n, s) => n + s.entries.length, 0);
  const q = query.trim();
  const waiting = sources.length > 0;

  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      onMove(e.key === "ArrowDown" ? 1 : -1);
    } else if (e.key === "Enter") {
      // An Enter that ends an IME composition commits the text; it opens nothing.
      if (e.nativeEvent.isComposing || e.keyCode === 229) return;
      e.preventDefault();
      if (active) onChoose(active);
    } else if (e.key === "Escape") {
      // The dialog's own cancel is not waited for, so Escape closes at once and alike everywhere.
      e.preventDefault();
      onClose();
    } else if (e.key === "Tab") {
      // Tab cycles through what can be focused inside, and never leaves.
      const all = [...(ref.current?.querySelectorAll<HTMLElement>(focusable) ?? [])].filter((x) => x.getClientRects().length > 0 || x.matches("input"));
      e.preventDefault();
      if (!all.length) return;
      const i = all.indexOf(document.activeElement as HTMLElement);
      all[(i + (e.shiftKey ? -1 : 1) + all.length) % all.length]!.focus();
    }
  };

  let empty: ReactNode = null;
  if (count === 0 && q) {
    empty = (
      <div className="px-4 py-10 text-center text-[13px] text-muted">
        {waiting ? <>Nothing that has loaded matches “{q}”.</> : <>Nothing in {noMatch} matches “{q}”.</>}
      </div>
    );
  }

  return (
    <dialog
      ref={ref}
      onClose={onClose}
      onCancel={(e) => e.preventDefault()}
      onKeyDown={onKeyDown}
      onClick={(e) => e.target === ref.current && onClose()}
      aria-label="Search"
      className={cn(
        "bg-surface p-0 text-foreground backdrop:bg-black/40",
        // A phone gets the whole screen; a wider one a panel near the top.
        "fixed inset-0 m-0 h-dvh max-h-none w-screen max-w-none",
        "md:inset-x-0 md:top-[12vh] md:bottom-auto md:mx-auto md:h-auto md:max-h-[min(560px,76vh)] md:w-[min(640px,calc(100vw-2rem))] md:rounded-lg md:border md:shadow-2xl",
      )}
    >
      <div className="flex h-full max-h-[inherit] flex-col">
        <div className="flex h-12 shrink-0 items-center gap-2.5 border-b px-4">
          <Search className="size-4 shrink-0 text-subtle" />
          <input
            ref={field}
            role="combobox"
            aria-expanded="true"
            aria-controls="palette-list"
            aria-activedescendant={active ? optionId(active) : undefined}
            aria-autocomplete="list"
            aria-label="Search or jump to"
            value={query}
            onChange={(e) => onQuery(e.target.value)}
            placeholder="Search or jump to…"
            spellCheck={false}
            autoComplete="off"
            className="min-w-0 flex-1 bg-transparent text-[14px] text-foreground outline-none placeholder:text-subtle"
          />
          <span className="hidden md:inline-flex">
            <Kbd>esc</Kbd>
          </span>
          <button onClick={onClose} className="-mr-1 rounded-md px-2 py-1 text-[13px] text-muted hover:bg-surface-2 md:hidden">
            Cancel
          </button>
        </div>

        <div id="palette-list" role="listbox" aria-label="Results" className="min-h-0 flex-1 overflow-y-auto py-1.5">
          {sections.map((s) =>
            s.entries.length === 0 ? null : (
              <div key={s.key} role="group" aria-labelledby={`palette-h-${s.key}`} className="pb-1">
                <div id={`palette-h-${s.key}`} className="px-4 pb-1 pt-2.5 text-[11px] font-medium uppercase tracking-wider text-subtle">
                  {s.title}
                </div>
                {s.entries.map((e) => {
                  const Icon = e.icon;
                  const on = e.id === active;
                  return (
                    <div
                      key={e.id}
                      id={optionId(e.id)}
                      role="option"
                      aria-selected={on}
                      onMouseMove={() => !on && onActive(e.id)}
                      onClick={() => onChoose(e.id)}
                      className={cn("mx-1.5 flex h-10 cursor-pointer items-center gap-3 rounded-md px-2.5 text-[13px] md:h-9", on && "bg-surface-2")}
                    >
                      <Icon className={cn("size-4 shrink-0", on ? "text-accent" : "text-subtle")} strokeWidth={1.75} />
                      <span className="min-w-0 truncate text-foreground">
                        <Marked text={e.title} query={q} />
                      </span>
                      {e.id2 && (
                        <span className="hidden min-w-0 truncate font-mono text-[11.5px] text-subtle sm:inline">
                          <Marked text={e.id2} query={q} />
                        </span>
                      )}
                      <span className="ml-auto shrink-0 pl-2 text-[11.5px] text-subtle">{e.detail}</span>
                      <CornerDownLeft className={cn("hidden size-3.5 shrink-0 text-muted md:block", !on && "invisible")} />
                    </div>
                  );
                })}
              </div>
            ),
          )}
          {empty}
          {waiting && (
            <div aria-live="polite" className={cn("mx-4 mt-1 space-y-1.5 border-t pt-2.5 pb-1.5", count === 0 && !q && "border-t-0")}>
              {sources.map((s) =>
                s.state === "loading" ? (
                  <div key={s.key} aria-busy="true" className="flex items-center gap-3 text-[12px] text-subtle">
                    <Skeleton className="h-2.5 w-16" />
                    <span>The {s.noun} are loading.</span>
                  </div>
                ) : (
                  <div key={s.key} className="text-[12px] text-subtle">
                    <span className="text-fail">The {s.noun} did not load:</span> <span className="font-mono text-[11.5px]">{s.error}</span>
                  </div>
                ),
              )}
            </div>
          )}
        </div>

        <div className="hidden h-9 shrink-0 items-center gap-4 border-t px-4 text-[11.5px] text-subtle md:flex">
          <span className="flex items-center gap-1.5">
            <Kbd>↑</Kbd>
            <Kbd>↓</Kbd> move
          </span>
          <span className="flex items-center gap-1.5">
            <Kbd>↵</Kbd> open
          </span>
          <span className="flex items-center gap-1.5">
            <Kbd>esc</Kbd> close
          </span>
          <span className="ml-auto truncate">{scopeNote}</span>
        </div>
      </div>
    </dialog>
  );
}
