import { useEffect, useRef, type ReactNode } from "react";

/** A modal dialog over the page; Escape or a click outside closes it. */
export function Dialog({
  open,
  onClose,
  title,
  children,
  wide,
  action,
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  children: ReactNode;
  /** A picker or a table rather than a question. */
  wide?: boolean;
  /** A button beside the title. */
  action?: ReactNode;
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
      onClose={onClose}
      onClick={(e) => e.target === ref.current && onClose()}
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
