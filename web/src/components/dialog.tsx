import { useEffect, useRef, type ReactNode } from "react";

/**
 * A modal dialog over the page; Escape or a click outside closes it.
 *
 * A locked dialog is closed only by what its own buttons do, which sets
 * open to false or unmounts it. The browser may still close the element
 * itself: Chromium cancels Escape a few times and then sends a cancel that
 * cannot be prevented, and a close. A locked dialog takes such a close as
 * not asked for: it opens the element again and keeps its state, so
 * nothing it shows is dropped.
 */
export function Dialog({
  open,
  onClose,
  title,
  children,
  wide,
  action,
  locked,
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  children: ReactNode;
  /** A picker or a table rather than a question. */
  wide?: boolean;
  /** A button beside the title. */
  action?: ReactNode;
  /** Closed by its own buttons alone, for what is lost once closed. */
  locked?: boolean;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) d.showModal();
    if (!open && d.open) d.close();
  }, [open]);
  return (
    <dialog
      ref={ref}
      onClose={() => {
        // While open is still true, the close is the browser's own, not this
        // dialog's: a locked dialog opens the element again.
        const d = ref.current;
        if (locked && open) {
          if (d?.isConnected && !d.open) d.showModal();
          return;
        }
        onClose();
      }}
      // Spares a locked dialog the close while the browser lets Escape be
      // cancelled; onClose above holds it open when it does not.
      onCancel={(e) => locked && e.preventDefault()}
      onClick={(e) => !locked && e.target === ref.current && onClose()}
      aria-label={title}
      className={`m-auto ${wide ? "w-[min(720px,calc(100vw-2rem))]" : "w-[min(440px,calc(100vw-2rem))]"} rounded-lg border bg-surface p-0 text-foreground shadow-2xl backdrop:bg-black/40`}
    >
      {open && (
        <div className="p-5">
          <div className="flex items-center justify-between gap-3">
            <h2 className="text-[15px] font-semibold tracking-tight">{title}</h2>
            {action}
          </div>
          <div className="mt-2 text-[13px] leading-relaxed text-muted">{children}</div>
        </div>
      )}
    </dialog>
  );
}
