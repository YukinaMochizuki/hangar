import { useEffect, useRef, type ReactNode } from "react";

/** A modal dialog over the page; Escape or a click outside closes it. */
export function Dialog({ open, onClose, title, children }: { open: boolean; onClose: () => void; title: string; children: ReactNode }) {
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
      onClose={onClose}
      onClick={(e) => e.target === ref.current && onClose()}
      aria-label={title}
      className="m-auto w-[min(440px,calc(100vw-2rem))] rounded-lg border bg-surface p-0 text-foreground shadow-2xl backdrop:bg-black/40"
    >
      {open && (
        <div className="p-5">
          <h2 className="text-[15px] font-semibold tracking-tight">{title}</h2>
          <div className="mt-2 text-[13px] leading-relaxed text-muted">{children}</div>
        </div>
      )}
    </dialog>
  );
}
