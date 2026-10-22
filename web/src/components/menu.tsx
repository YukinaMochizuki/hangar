// A row's "..." menu, and the popovers tied to a control. Each is drawn in a
// layer on the document's body, placed at its control, so a list's card,
// which scrolls its table sideways and so clips what overflows it, never
// cuts one off. A layer opens below its control, or above it when the
// viewport has no room below.

import { MoreHorizontal, type LucideIcon } from "lucide-react";
import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { Button } from "@/components/ui";
import { cn } from "@/lib/cn";

/** The space kept between a layer and its control, and the viewport's edge. */
const GAP = 4;
const EDGE = 8;

/**
 * Where a layer of the given height goes beside its control: below it, or
 * above it when it does not fit below and does above. Right edges align.
 */
export function placeLayer(anchor: DOMRect, height: number, viewport: { width: number; height: number }) {
  const below = anchor.bottom + GAP;
  const above = anchor.top - GAP - height;
  const top = below + height > viewport.height - EDGE && above >= EDGE ? above : below;
  return { top, right: viewport.width - anchor.right };
}

/**
 * A layer on the body, at its anchor. It follows the anchor while the page
 * scrolls or resizes. A mousedown outside both, or Escape, calls onAway;
 * without it the layer stays until its owner removes it.
 */
export function AnchoredLayer({
  anchor,
  onAway,
  className,
  children,
  ...rest
}: {
  /** The control the layer stands beside; nothing is drawn until it is known. */
  anchor: HTMLElement | null;
  onAway?: () => void;
  className?: string;
  children: ReactNode;
  role?: string;
  "aria-label"?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  // Placed on the element itself, before the browser paints it, and again as
  // the page moves: the layer is hidden until then, never at the corner.
  useLayoutEffect(() => {
    const layer = ref.current;
    if (!anchor || !layer) return;
    const place = () => {
      const { top, right } = placeLayer(anchor.getBoundingClientRect(), layer.offsetHeight, {
        width: window.innerWidth,
        height: window.innerHeight,
      });
      Object.assign(layer.style, { top: `${top}px`, right: `${right}px`, visibility: "visible" });
    };
    place();
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node;
      if (!layer.contains(t) && !anchor.contains(t)) onAway?.();
    };
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onAway?.();
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    // Capture sees the scroll of any container, the card's table among them.
    window.addEventListener("scroll", place, true);
    window.addEventListener("resize", place);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
      window.removeEventListener("scroll", place, true);
      window.removeEventListener("resize", place);
    };
  }, [anchor, onAway]);
  return createPortal(
    <div ref={ref} {...rest} style={{ position: "fixed", top: 0, right: 0, visibility: "hidden" }} className={cn("z-50", className)}>
      {children}
    </div>,
    document.body,
  );
}

export type MenuAction = {
  label: string;
  icon: LucideIcon;
  onSelect: () => void;
  disabled?: boolean;
  /** Drawn in the failure colour, as Delete is. */
  danger?: boolean;
};

/**
 * A "..." button opening a menu of actions; choosing one closes it. Its
 * children are given the button's place, for a layer of their own there.
 */
export function RowMenu({
  label,
  actions,
  empty,
  children,
}: {
  label: string;
  actions: MenuAction[];
  /** What the menu says when it holds no action. */
  empty?: string;
  children?: (anchor: HTMLElement | null) => ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const [anchor, setAnchor] = useState<HTMLDivElement | null>(null);
  const close = () => setOpen(false);
  return (
    <div ref={setAnchor} className="relative">
      <Button variant="ghost" size="icon" aria-label={label} aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(!open)}>
        <MoreHorizontal />
      </Button>
      {open && (
        <AnchoredLayer anchor={anchor} onAway={close} role="menu" aria-label={label} className="w-44 rounded-md border bg-surface p-1 shadow-lg">
          {actions.length === 0 && empty && <div className="px-2 py-1.5 text-[12.5px] text-subtle">{empty}</div>}
          {actions.map((a) => {
            const Icon = a.icon;
            return (
              <button
                key={a.label}
                role="menuitem"
                disabled={a.disabled}
                onClick={() => {
                  setOpen(false);
                  a.onSelect();
                }}
                className={cn(
                  "flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-[13px] hover:bg-surface-2 disabled:opacity-50",
                  a.danger && "text-fail",
                )}
              >
                <Icon className="size-3.5" /> {a.label}
              </button>
            );
          })}
        </AnchoredLayer>
      )}
      {children?.(anchor)}
    </div>
  );
}
