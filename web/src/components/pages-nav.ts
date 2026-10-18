// The console's pages for a project and for a tenant, in the sidebar's
// groups. The sidebar and the command palette both list them from here, so
// a page joins both when it is added.

import { Box, Disc3, Globe, HardDrive, KeyRound, LayoutGrid, Network, Server, ShieldCheck, Terminal, Users, Waypoints, type LucideIcon } from "lucide-react";

export type PageLink = { label: string; icon: LucideIcon; to: string };

export type PageSection = { group?: string; items: PageLink[] };

/** A project's pages; the first is its overview, at the project's own path. */
export function projectPages(project: string): PageSection[] {
  const base = `/projects/${project}`;
  return [
    { items: [{ label: "Overview", icon: LayoutGrid, to: base }] },
    {
      group: "Compute",
      items: [
        { label: "Containers", icon: Box, to: `${base}/containers` },
        { label: "Virtual machines", icon: Server, to: `${base}/vms` },
      ],
    },
    {
      group: "Storage",
      items: [
        { label: "Disks", icon: HardDrive, to: `${base}/disks` },
        { label: "Images", icon: Disc3, to: `${base}/images` },
      ],
    },
    {
      group: "Network",
      items: [
        { label: "Networks", icon: Network, to: `${base}/networks` },
        { label: "Security groups", icon: ShieldCheck, to: `${base}/security-groups` },
        { label: "Addresses", icon: Globe, to: `${base}/addresses` },
        { label: "Public endpoints", icon: Waypoints, to: `${base}/public-endpoints` },
      ],
    },
    {
      group: "Access",
      items: [
        { label: "Secrets", icon: KeyRound, to: `${base}/secrets` },
        { label: "Members", icon: Users, to: `${base}/members` },
        { label: "Service tokens", icon: Terminal, to: `${base}/service-tokens` },
      ],
    },
  ];
}

/** A tenant's own pages. */
export function tenantPages(tenant: string): PageSection[] {
  const base = `/tenants/${tenant}`;
  return [
    { items: [{ label: "Overview", icon: LayoutGrid, to: base }] },
    { group: "Access", items: [{ label: "Members", icon: Users, to: `${base}/members` }] },
  ];
}
