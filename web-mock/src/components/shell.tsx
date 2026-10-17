import { Link, useRouterState } from "@tanstack/react-router";
import {
  BookOpen,
  Box,
  ChevronsUpDown,
  Disc3,
  Globe,
  HardDrive,
  KeyRound,
  LayoutGrid,
  Menu,
  Monitor,
  Moon,
  Network,
  Search,
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
import { Kbd } from "@/components/ui";
import { cn } from "@/lib/cn";
import { accents, usePrefs, type ThemePref } from "@/lib/prefs";
import { currentProject, tenant } from "@/mock/data";

type NavItem = { label: string; icon: LucideIcon; to?: string; count?: number };

const nav: { group?: string; items: NavItem[] }[] = [
  { items: [{ label: "Overview", icon: LayoutGrid, to: "/" }] },
  {
    group: "Compute",
    items: [
      { label: "Containers", icon: Box, to: "/containers", count: 4 },
      { label: "Virtual machines", icon: Server, to: "/vms", count: 1 },
    ],
  },
  {
    group: "Network",
    items: [
      { label: "Networks", icon: Network },
      { label: "Security groups", icon: ShieldCheck },
      { label: "Addresses", icon: Globe },
      { label: "Public endpoints", icon: Waypoints },
    ],
  },
  {
    group: "Storage",
    items: [
      { label: "Disks", icon: HardDrive },
      { label: "Images", icon: Disc3 },
    ],
  },
  {
    group: "Access",
    items: [
      { label: "Secrets", icon: KeyRound },
      { label: "Members", icon: Users },
      { label: "Service tokens", icon: Terminal },
    ],
  },
];

// Below the lg breakpoint the sidebar is a drawer, opened from the top bar.
const MenuContext = createContext<{ open: boolean; setOpen: (o: boolean) => void }>({
  open: false,
  setOpen: () => {},
});

function Sidebar() {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const active = (to?: string) => !!to && (to === "/" ? path === "/" : path.startsWith(to));
  const { open, setOpen } = useContext(MenuContext);
  useEffect(() => setOpen(false), [path, setOpen]);
  return (
    <aside
      className={cn(
        "z-40 h-screen w-60 shrink-0 flex-col border-r bg-surface",
        "fixed inset-y-0 left-0 lg:sticky lg:top-0 lg:flex",
        open ? "flex shadow-2xl" : "hidden",
      )}
    >
      <div className="flex h-14 items-center gap-2 px-4">
        <HangarMark className="size-5 text-foreground" />
        <span className="text-[15px] font-semibold tracking-[-0.02em]">Hangar</span>
      </div>
      <button className="mx-3 mb-2 flex items-center gap-2.5 rounded-md border bg-background px-2.5 py-2 text-left hover:border-border-strong">
        <span className="flex size-6 items-center justify-center rounded bg-accent-soft font-mono text-[11px] font-semibold text-accent">
          {currentProject.displayName[0]}
        </span>
        <span className="min-w-0 flex-1">
          <span className="block truncate text-[13px] font-medium leading-tight">{currentProject.displayName}</span>
          <span className="block truncate font-mono text-[11px] leading-tight text-subtle">{currentProject.name}</span>
        </span>
        <ChevronsUpDown className="size-3.5 text-subtle" />
      </button>
      <nav className="flex-1 overflow-y-auto px-3 py-2">
        {nav.map((section, i) => (
          <div key={i} className="mb-3">
            {section.group && (
              <div className="px-2 pb-1 pt-2 text-[11px] font-medium uppercase tracking-wider text-subtle">
                {section.group}
              </div>
            )}
            {section.items.map((item) => {
              const Icon = item.icon;
              const cls = cn(
                "flex h-8 items-center gap-2.5 rounded-md px-2 text-[13px]",
                active(item.to)
                  ? "bg-surface-2 font-medium text-foreground"
                  : "text-muted hover:bg-surface-2 hover:text-foreground",
              );
              const body = (
                <>
                  <Icon className={cn("size-4", active(item.to) && "text-accent")} strokeWidth={1.75} />
                  <span className="flex-1">{item.label}</span>
                  {item.count !== undefined && (
                    <span className="font-mono text-[11px] text-subtle tabular">{item.count}</span>
                  )}
                </>
              );
              return item.to ? (
                <Link key={item.label} to={item.to} className={cls}>
                  {body}
                </Link>
              ) : (
                <span key={item.label} className={cls}>
                  {body}
                </span>
              );
            })}
          </div>
        ))}
      </nav>
      <div className="flex items-center gap-2.5 border-t px-4 py-3">
        <span className="flex size-7 items-center justify-center rounded-full bg-surface-2 text-[12px] font-medium">
          {tenant.displayName[0]}
        </span>
        <span className="min-w-0 flex-1">
          <span className="block truncate text-[13px] leading-tight">{tenant.displayName}</span>
          <span className="block truncate font-mono text-[11px] leading-tight text-subtle">{tenant.name}</span>
        </span>
        <BookOpen className="size-4 text-subtle" strokeWidth={1.75} />
      </div>
    </aside>
  );
}

export function Topbar({ crumbs }: { crumbs: ReactNode[] }) {
  const { setOpen } = useContext(MenuContext);
  return (
    <header className="sticky top-0 z-10 flex h-14 items-center gap-3 border-b bg-background/80 px-4 backdrop-blur md:gap-4 md:px-6">
      <button
        aria-label="Menu"
        onClick={() => setOpen(true)}
        className="-ml-1 rounded-md p-1.5 text-muted hover:bg-surface-2 lg:hidden"
      >
        <Menu className="size-4" />
      </button>
      <div className="flex min-w-0 flex-1 items-center gap-1.5 text-[13px]">
        {crumbs.map((c, i) => (
          <span key={i} className={cn("min-w-0 items-center gap-1.5", i < crumbs.length - 1 ? "hidden md:flex" : "flex")}>
            {i > 0 && <span className="text-subtle">/</span>}
            <span className={cn("truncate", i === crumbs.length - 1 ? "font-medium" : "text-muted")}>{c}</span>
          </span>
        ))}
      </div>
      <button className="hidden h-8 w-72 items-center gap-2 rounded-md border bg-surface px-2.5 text-[13px] text-subtle hover:border-border-strong md:flex">
        <Search className="size-3.5" />
        <span className="flex-1 text-left">Search or jump to…</span>
        <Kbd>⌘</Kbd>
        <Kbd>K</Kbd>
      </button>
      <button aria-label="Search" className="rounded-md p-1.5 text-muted hover:bg-surface-2 md:hidden">
        <Search className="size-4" />
      </button>
    </header>
  );
}

const themeOptions: { id: ThemePref; icon: LucideIcon }[] = [
  { id: "system", icon: Monitor },
  { id: "light", icon: Sun },
  { id: "dark", icon: Moon },
];

/** Switches theme and accent while the design is being chosen. */
function PocToolbar() {
  const { theme, setTheme, accent, setAccent } = usePrefs();
  return (
    <div className="fixed bottom-4 right-4 z-50 flex items-center gap-3 rounded-full border bg-surface/95 py-1.5 pl-3 pr-1.5 text-[11px] text-muted shadow-lg backdrop-blur">
      <span className="hidden font-medium sm:inline">Design PoC · mock data</span>
      <div className="flex gap-1">
        {accents.map((a) => (
          <button
            key={a.id}
            title={a.label}
            onClick={() => setAccent(a.id)}
            data-accent={a.id}
            className={cn(
              "size-5 rounded-full bg-accent ring-offset-2 ring-offset-surface",
              accent === a.id && "ring-2 ring-foreground/60",
            )}
          />
        ))}
      </div>
      <div className="flex rounded-full bg-surface-2 p-0.5">
        {themeOptions.map((t) => {
          const Icon = t.icon;
          return (
            <button
              key={t.id}
              title={t.id}
              onClick={() => setTheme(t.id)}
              className={cn(
                "flex size-6 items-center justify-center rounded-full",
                theme === t.id ? "bg-surface text-foreground shadow-sm" : "text-subtle",
              )}
            >
              <Icon className="size-3.5" />
            </button>
          );
        })}
      </div>
    </div>
  );
}

export function Shell({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <MenuContext.Provider value={{ open, setOpen }}>
      <div className="flex min-h-screen">
        <Sidebar />
        {open && <div className="fixed inset-0 z-30 bg-black/40 lg:hidden" onClick={() => setOpen(false)} />}
        <div className="min-w-0 flex-1">{children}</div>
        <PocToolbar />
      </div>
    </MenuContext.Provider>
  );
}
