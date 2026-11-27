// A line that falls back to shorter words when the longer ones do not fit.

import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";

/**
 * Whether el's text runs past its box: by whole pixels, or by the fraction
 * of one that scrollWidth rounds away while the browser still draws an
 * ellipsis.
 */
function overflows(el: HTMLElement): boolean {
  if (el.scrollWidth > el.clientWidth) return true;
  if (typeof document.createRange !== "function") return false;
  const range = document.createRange();
  range.selectNodeContents(el);
  if (typeof range.getBoundingClientRect !== "function") return false;
  return range.getBoundingClientRect().width > el.getBoundingClientRect().width + 0.01;
}

/**
 * The first of texts that fits the width it is given, measured: the longest
 * is drawn first, and the next while it overflows. The measure starts over
 * whenever its box's width differs from the one it measured at, and once
 * fonts finish loading, which change the text's width but not the box's.
 * The last is cut short when even it does not fit. render draws a text,
 * as the text itself unless given.
 */
export function FittingText({ texts, className, render }: { texts: string[]; className?: string; render?: (text: string) => ReactNode }) {
  const ref = useRef<HTMLSpanElement>(null);
  // The width of its box at the last measure, which a resize is compared with.
  const measuredAt = useRef(-1);
  // Which text is drawn, and how often the measure started over.
  const [fit, setFit] = useState({ i: 0, key: texts.join("\n"), restarts: 0 });
  const key = texts.join("\n");
  if (fit.key !== key) setFit({ i: 0, key, restarts: fit.restarts });
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    measuredAt.current = el.parentElement?.clientWidth ?? -1;
    if (fit.i < texts.length - 1 && overflows(el)) setFit((f) => ({ ...f, i: f.i + 1 }));
  }, [fit, texts.length]);
  useEffect(() => {
    const parent = ref.current?.parentElement;
    const restart = () => setFit((f) => ({ ...f, i: 0, restarts: f.restarts + 1 }));
    let ro: ResizeObserver | undefined;
    if (parent && typeof ResizeObserver !== "undefined") {
      ro = new ResizeObserver(() => {
        if (parent.clientWidth !== measuredAt.current) restart();
      });
      ro.observe(parent);
    }
    const fonts = typeof document !== "undefined" ? document.fonts : undefined;
    let live = true;
    fonts?.addEventListener?.("loadingdone", restart);
    void fonts?.ready?.then(() => live && restart());
    return () => {
      live = false;
      ro?.disconnect();
      fonts?.removeEventListener?.("loadingdone", restart);
    };
  }, []);
  const text = texts[Math.min(fit.i, texts.length - 1)];
  return (
    <span ref={ref} className={className}>
      {render ? render(text) : text}
    </span>
  );
}
