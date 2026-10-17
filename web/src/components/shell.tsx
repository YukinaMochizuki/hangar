// The console's frame: the sidebar (project switcher, navigation, the member
// and the theme), the top bar with its breadcrumbs, and the page between.

import { Link, useNavigate, useRouterState } from "@tanstack/react-router";
import {
  Box,
  Check,
  ChevronsUpDown,
  LayoutGrid,
  LogOut,
  Menu,
  Monitor,
  Moon,
  Server,
  Sun,
  type LucideIcon,
} from "lucide-react";
import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { HangarMark } from "@/components/marks";
import { Notice } from "@/components/notice";
import { Skeleton } from "@/components/states";
import { errorMessage, signOut } from "@/lib/api";
import { cn } from "@/lib/cn";
import { useMachines } from "@/lib/machines";
import { usePrefs, type ThemePref } from "@/lib/prefs";
import { projectLabel, rememberProject, useProjects } from "@/lib/projects";
import { useMember } from "@/lib/session";

type NavItem = { label: string; icon: LucideIcon; to: string; count?: number };

// Below the lg breakpoint the sidebar is a drawer, opened from the top bar.
const MenuContext = createContext<{ open: boolean; setOpen: (o: boolean) => void }>({
  open: false,
  setOpen: () => {},
});

const ProjectContext = createContext<string>("");

/** Sets the project the console is in; Shell does it for every page. */
export const ProjectProvider = ProjectContext.Provider;

/** The project the console is in. */
export function useProjectName(): string {
  return useContext(ProjectContext);
}

/** Closes a popover on a click outside it or on Escape. */
function useDismiss(open: boolean, close: () => void) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) close();
    };
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && close();
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open, close]);
  return ref;
}

function ProjectSwitcher({ project }: { project: string }) {
  const { projects, loading, error, tenantName } = useProjects();
  const [open, setOpen] = useState(false);
  const ref = useDismiss(open, () => setOpen(false));
  const navigate = useNavigate();
  const current = projects.find((p) => p.metadata?.name === project);
  const tenants = [...new Set(projects.map((p) => p.spec?.tenant ?? ""))];

  return (
    <div ref={ref} className="relative mx-3 mb-2">
      <button
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="flex w-full items-center gap-2.5 rounded-md border bg-background px-2.5 py-2 text-left hover:border-border-strong"
      >
        <span className="flex size-6 shrink-0 items-center justify-center rounded bg-accent-soft font-mono text-[11px] font-semibold uppercase text-accent">
          {projectLabel(current, project)[0]}
        </span>
        <span className="min-w-0 flex-1">
          {loading ? (
            <Skeleton className="h-3 w-24" />
          ) : (
            <span className="block truncate text-[13px] font-medium leading-tight">{projectLabel(current, project)}</span>
          )}
          <span className="block truncate font-mono text-[11px] leading-tight text-subtle">{project}</span>
        </span>
        <ChevronsUpDown className="size-3.5 shrink-0 text-subtle" />
      </button>
      {open && (
        <div
          role="listbox"
          aria-label="Projects"
          className="absolute inset-x-0 top-full z-50 mt-1 max-h-80 overflow-y-auto rounded-md border bg-surface py-1 shadow-lg"
        >
          {error && <div className="px-3 py-2 text-[12px] text-fail">{errorMessage(error)}</div>}
          {tenants.map((t) => (
            <div key={t}>
              <div className="px-3 pb-1 pt-2 text-[11px] font-medium uppercase tracking-wider text-subtle">{tenantName(t)}</div>
              {projects
                .filter((p) => (p.spec?.tenant ?? "") === t)
                .map((p) => {
                  const name = p.metadata?.name ?? "";
                  const selected = name === project;
                  return (
                    <button
                      key={name}
                      role="option"
                      aria-selected={selected}
                      onClick={() => {
                        setOpen(false);
                        void navigate({ to: "/projects/$project", params: { project: name } });
                      }}
                      className="flex w-full items-center gap-2 px-3 py-1.5 text-left hover:bg-surface-2"
                    >
                      <span className="min-w-0 flex-1">
                        <span className="block truncate text-[13px]">{projectLabel(p)}</span>
                        <span className="block truncate font-mono text-[11px] text-subtle">{name}</span>
                      </span>
                      {selected && <Check className="size-3.5 text-accent" />}
                    </button>
                  );
                })}
            </div>
          ))}
        </div>
      )}
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

function Sidebar({ project }: { project: string }) {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const { open, setOpen } = useContext(MenuContext);
  const containers = useMachines(project, "Container");
  const vms = useMachines(project, "VirtualMachine");
  useEffect(() => setOpen(false), [path, setOpen]);

  const base = `/projects/${project}`;
  // Only pages that exist are listed; a kind joins when its page does.
  const nav: { group?: string; items: NavItem[] }[] = [
    { items: [{ label: "Overview", icon: LayoutGrid, to: base }] },
    {
      group: "Compute",
      items: [
        { label: "Containers", icon: Box, to: `${base}/containers`, count: containers.loading || containers.error ? undefined : containers.items.length },
        { label: "Virtual machines", icon: Server, to: `${base}/vms`, count: vms.loading || vms.error ? undefined : vms.items.length },
      ],
    },
  ];
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
      <ProjectSwitcher project={project} />
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

/** The top bar: the menu button on small screens and the breadcrumbs. */
export function Topbar({ crumbs }: { crumbs: ReactNode[] }) {
  const { setOpen } = useContext(MenuContext);
  const project = useProjectName();
  const { projects, tenantName } = useProjects();
  const p = projects.find((x) => x.metadata?.name === project);
  const all = [...(p ? [tenantName(p.spec?.tenant ?? "")] : []), projectLabel(p, project), ...crumbs];
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

export function Shell({ project, children }: { project: string; children: ReactNode }) {
  const [open, setOpen] = useState(false);
  useEffect(() => rememberProject(project), [project]);
  return (
    <ProjectContext.Provider value={project}>
      <MenuContext.Provider value={{ open, setOpen }}>
        <div className="flex min-h-screen">
          <Sidebar project={project} />
          {open && <div className="fixed inset-0 z-30 bg-black/40 lg:hidden" onClick={() => setOpen(false)} />}
          <div className="min-w-0 flex-1">{children}</div>
        </div>
      </MenuContext.Provider>
    </ProjectContext.Provider>
  );
}
