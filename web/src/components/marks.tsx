// The Hangar mark: a fighter shelter (low and wide, as a hardened aircraft
// shelter is) with a swept tail fin standing inside, on a 24-unit grid. The
// shell is currentColor; only the fin takes the accent.

import { useId } from "react";

/** The mark as an SVG of its own; inside another SVG, pass `bare` for just its shapes on the 24-unit grid. */
export function HangarMark({ className, bare }: { className?: string; bare?: boolean }) {
  const mask = `m-${useId().replace(/:/g, "")}`;
  const shapes = (
    <>
      <defs>
        <mask id={mask}>
          <rect width="24" height="24" fill="white" />
          <path d="M5.2 20v-3c0-3 3-4.9 6.8-4.9s6.8 1.9 6.8 4.9V20Z" fill="black" />
        </mask>
      </defs>
      <path d="M1 20v-3.8C1 10.6 5.9 7.2 12 7.2s11 3.4 11 9V20Z" fill="currentColor" mask={`url(#${mask})`} />
      <path d="M9.6 20 12.2 14.1h2.3L13.9 20Z" fill="var(--accent)" />
    </>
  );
  if (bare) return shapes;
  return (
    <svg viewBox="0 0 24 24" className={className} aria-hidden>
      {shapes}
    </svg>
  );
}
