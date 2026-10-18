// The console's frame: the sidebar (project switcher, navigation, the member
// and the theme), the top bar with its breadcrumbs, and the page between.

import { Link, useRouterState } from "@tanstack/react-router";
import {
  Box,
  ChevronsUpDown,
  Disc3,
  Globe,
  HardDrive,
  KeyRound,
  LayoutGrid,
  LogOut,
  Menu,
  Monitor,
  Moon,
  Network,
  Server,
  ShieldCheck,
  Sun,
  Terminal,
  Users,
  Waypoints,
  type LucideIcon,
} from "lucide-react";
import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { HangarMark } from "@/components/marks";
import { ProjectPicker } from "@/components/project-picker";
import { Notice } from "@/components/notice";
import { Skeleton } from "@/components/states";
import { errorMessage, signOut } from "@/lib/api";
import { cn } from "@/lib/cn";
import { useMachines } from "@/lib/machines";
import { usePrefs, type ThemePref } from "@/lib/prefs";
import { projectLabel, rememberProject, useProjects } from "@/lib/projects";
import { useMember } from "@/lib/session";
import { useDisks } from "@/lib/storage";

type NavItem = { label: string; icon: LucideIcon; to: string; count?: number };

// Below the lg breakpoint the sidebar is a drawer, opened from the top bar.
const MenuContext = createContext<{ open: boolean; setOpen: (o: boolean) => void }>({
  open: false,
  setOpen: () => {},
});

/** Where the console is: in a project, or in a tenant's own pages. */
export type Place = { kind: "project"; name: string } | { kind: "tenant"; name: string };

const PlaceContext = createContext<Place>({ kind: "project", name: "" });

/** Sets where the console is; Shell does it for every page. */
export const PlaceProvider = PlaceContext.Provider;

/** The project the console is in, or "" on a tenant's pages. */
export function useProjectName(): string {
  const place = useContext(PlaceContext);
  return place.kind === "project" ? place.name : "";
}

/** The tenant whose pages the console is on, or "" in a project. */
export function useTenantName(): string {
  const place = useContext(PlaceContext);
  return place.kind === "tenant" ? place.name : "";
}

// The switcher names where the console is; it opens the picker, which lists
// every tenant and project the member sees.
function ProjectSwitcher({ place }: { place: Place }) {
  const { projects, loading, tenantName } = useProjects();
  const [open, setOpen] = useState(false);
  const project = place.kind === "project" ? place.name : "";
  const current = projects.find((p) => p.metadata?.name === project);
  const label = place.kind === "tenant" ? tenantName(place.name) : projectLabel(current, project);

  return (
    <div className="mx-3 mb-2">
      <button
        aria-haspopup="dialog"
        onClick={() => setOpen(true)}
        className="flex w-full items-center gap-2.5 rounded-md border bg-background px-2.5 py-2 text-left hover:border-border-strong"
      >
        <span className="flex size-6 shrink-0 items-center justify-center rounded bg-accent-soft font-mono text-[11px] font-semibold uppercase text-accent">
          {label[0]}
        </span>
        <span className="min-w-0 flex-1">
          {loading ? (
            <Skeleton className="h-3 w-24" />
          ) : (
            <span className="block truncate text-[13px] font-medium leading-tight">{label}</span>
          )}
          <span className="block truncate font-mono text-[11px] leading-tight text-subtle">
            {place.kind === "tenant" ? `Tenant · ${place.name}` : project}
          </span>
        </span>
        <ChevronsUpDown className="size-3.5 shrink-0 text-subtle" />
      </button>
      <ProjectPicker place={place} open={open} onClose={() => setOpen(false)} />
    </div>
  );
}

const themeOptions: { id: ThemePref; icon: LucideIcon; label: string }[] = [
  { id: "system", icon: Monitor, label: "System theme" },
  { id: "light", icon: Sun, label: "Light theme" },
  { id: "dark", icon: Moon, label: "Dark theme" },
];

function ThemeSwitch() {
  const { theme, setTheme } = usePrefs();
  return (
    <div role="radiogroup" aria-label="Theme" className="flex rounded-full bg-surface-2 p-0.5">
      {themeOptions.map((t) => {
        const Icon = t.icon;
        return (
          <button
            key={t.id}
            role="radio"
            aria-checked={theme === t.id}
            title={t.label}
            aria-label={t.label}
            onClick={() => setTheme(t.id)}
            className={cn(
              "flex size-6 items-center justify-center rounded-full",
              theme === t.id ? "bg-surface text-foreground shadow-sm" : "text-subtle hover:text-foreground",
            )}
          >
            <Icon className="size-3.5" />
          </button>
        );
      })}
    </div>
  );
}

function MemberFooter() {
  const who = useMember();
  const [error, setError] = useState<unknown>(null);
  const name = who.user?.spec?.displayName || who.user?.metadata?.name || who.id;
  return (
    <div className="border-t px-4 py-3">
      {error !== null && <Notice tone="error" title={errorMessage(error)} className="mb-2" />}
      <div className="flex items-center gap-2.5">
        <span className="flex size-7 shrink-0 items-center justify-center rounded-full bg-surface-2 text-[12px] font-medium uppercase">
          {name[0]}
        </span>
        <span className="min-w-0 flex-1">
          <span className="block truncate text-[13px] leading-tight">{name}</span>
          <span className="block truncate font-mono text-[11px] leading-tight text-subtle">{who.id}</span>
        </span>
        <button
          aria-label="Sign out"
          title="Sign out"
          onClick={async () => {
            try {
              await signOut();
              window.location.assign("/");
            } catch (e) {
              setError(e);
            }
          }}
          className="rounded-md p-1.5 text-subtle hover:bg-surface-2 hover:text-foreground"
        >
          <LogOut className="size-4" strokeWidth={1.75} />
        </button>
      </div>
      <div className="mt-3 flex items-center justify-between">
        <span className="text-[11.5px] text-subtle">Theme</span>
        <ThemeSwitch />
      </div>
    </div>
  );
}

function ProjectSidebar({ project }: { project: string }) {
  const containers = useMachines(project, "Container");
  const vms = useMachines(project, "VirtualMachine");
  const disks = useDisks(project);
  const base = `/projects/${project}`;
  // Only pages that exist are listed; a kind joins when its page does.
  const nav: NavSection[] = [
    { items: [{ label: "Overview", icon: LayoutGrid, to: base }] },
    {
      group: "Compute",
      items: [
        { label: "Containers", icon: Box, to: `${base}/containers`, count: containers.loading || containers.error ? undefined : containers.items.length },
        { label: "Virtual machines", icon: Server, to: `${base}/vms`, count: vms.loading || vms.error ? undefined : vms.items.length },
      ],
    },
    {
      group: "Storage",
      items: [
        { label: "Disks", icon: HardDrive, to: `${base}/disks`, count: disks.loading || disks.error ? undefined : disks.items.length },
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
  return <SidebarFrame place={{ kind: "project", name: project }} base={base} nav={nav} />;
}

/** A tenant's own pages: what it holds and who is in it. */
function TenantSidebar({ tenant }: { tenant: string }) {
  const base = `/tenants/${tenant}`;
  const nav: NavSection[] = [
    { items: [{ label: "Overview", icon: LayoutGrid, to: base }] },
    { group: "Access", items: [{ label: "Members", icon: Users, to: `${base}/members` }] },
  ];
  return <SidebarFrame place={{ kind: "tenant", name: tenant }} base={base} nav={nav} />;
}

type NavSection = { group?: string; items: NavItem[] };

function SidebarFrame({ place, base, nav }: { place: Place; base: string; nav: NavSection[] }) {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const { open, setOpen } = useContext(MenuContext);
  useEffect(() => setOpen(false), [path, setOpen]);
  const active = (to: string) => (to === base ? path === base || path === `${base}/` : path.startsWith(to));

  return (
    <aside
      className={cn(
        "z-40 h-screen w-60 shrink-0 flex-col border-r bg-surface",
        "fixed inset-y-0 left-0 lg:sticky lg:top-0 lg:flex",
        open ? "flex shadow-2xl" : "hidden",
      )}
    >
      <Link to="/" className="flex h-14 items-center gap-2 px-4">
        <HangarMark className="size-5 text-foreground" />
        <span className="text-[15px] font-semibold tracking-[-0.02em]">Hangar</span>
      </Link>
      <ProjectSwitcher place={place} />
      <nav className="flex-1 overflow-y-auto px-3 py-2">
        {nav.map((section, i) => (
          <div key={i} className="mb-3">
            {section.group && (
              <div className="px-2 pb-1 pt-2 text-[11px] font-medium uppercase tracking-wider text-subtle">{section.group}</div>
            )}
            {section.items.map((item) => {
              const Icon = item.icon;
              const on = active(item.to);
              const cls = cn(
                "flex h-8 items-center gap-2.5 rounded-md px-2 text-[13px]",
                on ? "bg-surface-2 font-medium text-foreground" : "text-muted hover:bg-surface-2 hover:text-foreground",
              );
              return (
                <Link key={item.label} to={item.to} className={cls} aria-current={on ? "page" : undefined}>
                  <Icon className={cn("size-4", on && "text-accent")} strokeWidth={1.75} />
                  <span className="flex-1">{item.label}</span>
                  {item.count !== undefined && <span className="font-mono text-[11px] text-subtle tabular">{item.count}</span>}
                </Link>
              );
            })}
          </div>
        ))}
      </nav>
      <MemberFooter />
    </aside>
  );
}

const crumbLink = "truncate text-muted hover:text-foreground";

/**
 * The top bar: the menu button on small screens and the breadcrumbs. The
 * tenant's crumb opens the tenant's pages, the project's its overview.
 */
export function Topbar({ crumbs }: { crumbs: ReactNode[] }) {
  const { setOpen } = useContext(MenuContext);
  const project = useProjectName();
  const tenantPage = useTenantName();
  const { projects, tenantName } = useProjects();
  const p = projects.find((x) => x.metadata?.name === project);
  const tenant = tenantPage || p?.spec?.tenant || "";
  const all: ReactNode[] = [
    ...(tenant
      ? [
          <Link key="t" to="/tenants/$tenant" params={{ tenant }} className={crumbLink}>
            {tenantName(tenant)}
          </Link>,
        ]
      : []),
    ...(project
      ? [
          <Link key="p" to="/projects/$project" params={{ project }} className={crumbLink}>
            {projectLabel(p, project)}
          </Link>,
        ]
      : []),
    ...crumbs,
  ];
  return (
    <header className="sticky top-0 z-10 flex h-14 items-center gap-3 border-b bg-background/80 px-4 backdrop-blur md:gap-4 md:px-6">
      <button aria-label="Menu" onClick={() => setOpen(true)} className="-ml-1 rounded-md p-1.5 text-muted hover:bg-surface-2 lg:hidden">
        <Menu className="size-4" />
      </button>
      <div className="flex min-w-0 flex-1 items-center gap-1.5 text-[13px]">
        {all.map((c, i) => (
          <span key={i} className={cn("min-w-0 items-center gap-1.5", i < all.length - 1 ? "hidden md:flex" : "flex")}>
            {i > 0 && <span className={cn("text-subtle", i === all.length - 1 && "hidden md:inline")}>/</span>}
            <span className={cn("truncate", i === all.length - 1 ? "font-medium" : "text-muted")}>{c}</span>
          </span>
        ))}
      </div>
    </header>
  );
}

export function Shell({ place, children }: { place: Place; children: ReactNode }) {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    if (place.kind === "project") rememberProject(place.name);
  }, [place]);
  return (
    <PlaceContext.Provider value={place}>
      <MenuContext.Provider value={{ open, setOpen }}>
        <div className="flex min-h-screen">
          {place.kind === "project" ? <ProjectSidebar project={place.name} /> : <TenantSidebar tenant={place.name} />}
          {open && <div className="fixed inset-0 z-30 bg-black/40 lg:hidden" onClick={() => setOpen(false)} />}
          <div className="min-w-0 flex-1">{children}</div>
        </div>
      </MenuContext.Provider>
    </PlaceContext.Provider>
  );
}
