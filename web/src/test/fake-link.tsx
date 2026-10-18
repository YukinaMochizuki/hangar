// The router's Link as an anchor to the path it would open, for tests that
// render pages without a router. It imports nothing that imports the
// router, so a vi.mock factory of the router can load it.

import type { ReactNode } from "react";

/** FakeLink renders an anchor whose href is `to` with its params filled in, and its search. */
export function FakeLink({
  to,
  params,
  search,
  children,
  className,
}: {
  to: string;
  params?: Record<string, string>;
  search?: Record<string, string>;
  children?: ReactNode;
  className?: string;
}) {
  const path = Object.entries(params ?? {}).reduce((p, [k, v]) => p.replace(`$${k}`, v), to);
  const href = search ? `${path}?${new URLSearchParams(search)}` : path;
  return (
    <a href={href} className={className}>
      {children}
    </a>
  );
}
