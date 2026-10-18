// The router's Link as an anchor to the path it would open, for tests that
// render pages without a router. It imports nothing that imports the
// router, so a vi.mock factory of the router can load it.

import type { ReactNode } from "react";

/** FakeLink renders an anchor whose href is `to` with its params filled in. */
export function FakeLink({ to, params, children, className }: { to: string; params?: Record<string, string>; children?: ReactNode; className?: string }) {
  const href = Object.entries(params ?? {}).reduce((p, [k, v]) => p.replace(`$${k}`, v), to);
  return (
    <a href={href} className={className}>
      {children}
    </a>
  );
}
