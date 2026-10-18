// The command palette: what it finds and offers where the console is, read
// from the pages' own list hooks, with the actions TestPermissions allows.
// docs/decisions/console-design.md says what it does.

import { useNavigate } from "@tanstack/react-router";
import { ArrowLeftRight, Box, Building2, Disc3, FolderKanban, Globe, HardDrive, KeyRound, Network, Plus, Server, ShieldCheck, Terminal, User, Waypoints } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { NewProjectDialog, ProjectPicker, createProject } from "@/components/project-picker";
import { projectPages, tenantPages, type PageSection } from "@/components/pages-nav";
import { PaletteView, type Entry, type Section, type SourceState } from "@/components/palette-view";
import type { Place } from "@/components/shell";
import { phaseLabel } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { perms as credentialPerms, useSecrets, useServiceTokens, useTokenRoles } from "@/lib/credentials";
import { useMachines, type ListOpts } from "@/lib/machines";
import { roleLabel, useMembers, type Scope } from "@/lib/members";
import { nameOf, useAddresses, useNetworks, usePublicEndpoints, useSecurityGroups } from "@/lib/network";
import { permission } from "@/lib/network-actions";
import { matchSections } from "@/lib/palette";
import { machinePerms, useCan, useTenantsWhere } from "@/lib/permissions";
import { projectLabel, useProjects } from "@/lib/projects";
import { quantityGiB } from "@/lib/quantity";
import { useDisks, useImages } from "@/lib/storage";

/** What each "New …" action needs at the project, as TestPermissions names it. */
export const actionPerms = {
  container: machinePerms.container,
  vm: machinePerms.vm,
  disk: "storage.disks.apply",
  network: permission("network", "apply"),
  securityGroup: permission("securityGroup", "apply"),
  address: permission("address", "apply"),
  publicEndpoint: permission("publicEndpoint", "apply"),
  secret: credentialPerms.secretApply,
  token: credentialPerms.tokenApply,
  member: "iam.rolebindings.apply",
} as const;

/** Everything the palette searches where the console is. */
type Model = {
  /** Shown with an empty query too: the actions and the pages. */
  always: Section[];
  /** Shown once something is typed. */
  searched: Section[];
  sources: SourceState[];
  noMatch: string;
  scopeNote: string;
};

/** A list as the palette states it: held says it has items from an earlier answer to show. */
type Listed = { loading: boolean; error: unknown; held: boolean };

// The palette reads each list as the cache holds it: a list a page has
// fetched is not fetched again on opening, and goes on being asked for on its
// own interval.
const cached: ListOpts = { refetchOnMount: false };

/**
 * The state of a list that has not answered, or nothing once it has. A list
 * whose refetch failed shows the items it holds, with nothing said.
 */
function sourceOf(key: string, noun: string, l: Listed): SourceState[] {
  if (l.error && !l.held) return [{ key, noun, state: "failed", error: errorMessage(l.error) }];
  if (l.loading) return [{ key, noun, state: "loading" }];
  return [];
}

const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? "" : "s"}`;

function gib(q: string | undefined): string | undefined {
  const n = quantityGiB(q);
  return Number.isFinite(n) && n > 0 ? `${n} GiB` : undefined;
}

/** What closing the palette leads to: nothing, or the picker or the new-project form it hands over to. */
type Then = "picker" | "newProject" | undefined;

type Hands = {
  /** Closes the palette, then does what was chosen. */
  done: (after?: () => void) => void;
  handOver: (t: Then) => void;
};

function pageEntries(sections: PageSection[], go: (to: string) => void): Section {
  return {
    key: "pages",
    title: "Pages",
    entries: sections.flatMap((s) => s.items).map((p) => ({ id: `page:${p.to}`, icon: p.icon, title: p.label, run: () => go(p.to) })),
  };
}

/** Projects and tenants, as the picker lists them; and the state of the list. */
function usePlaces(place: Place, hands: Hands) {
  const navigate = useNavigate();
  const { projects, loading, error, tenantName, tenantNames } = useProjects(cached);
  const projectSection: Section = {
    key: "projects",
    title: "Projects",
    entries: projects.map((p) => {
      const name = p.metadata?.name ?? "";
      const here = place.kind === "project" && place.name === name;
      return {
        id: `project:${name}`,
        icon: FolderKanban,
        title: projectLabel(p, name),
        id2: name,
        detail: `${tenantName(p.spec?.tenant ?? "")}${here ? " · current" : ""}`,
        run: () => hands.done(() => void navigate({ to: "/projects/$project", params: { project: name } })),
      };
    }),
  };
  const tenantSection: Section = {
    key: "tenants",
    title: "Tenants",
    entries: tenantNames.map((t) => ({
      id: `tenant:${t}`,
      icon: Building2,
      title: tenantName(t),
      id2: t,
      detail: place.kind === "tenant" && place.name === t ? "current" : undefined,
      run: () => hands.done(() => void navigate({ to: "/tenants/$tenant", params: { tenant: t } })),
    })),
  };
  return { sections: [projectSection, tenantSection], source: sourceOf("projects", "projects", { loading, error, held: projects.length > 0 }), tenantNames, projects };
}

/** The people bound at the scope, by username or email, each opening the scope's Members. */
function useMemberSection(scope: Scope, open: () => void): { section: Section; source: SourceState[] } {
  const m = useMembers(scope, cached);
  return {
    section: {
      key: "members",
      title: "Members",
      entries: m.rows.map((r) => ({
        id: `member:${r.email}`,
        icon: User,
        title: r.username || r.email,
        id2: r.username ? r.email : undefined,
        detail: r.roles.map(roleLabel).join(", "),
        run: open,
      })),
    },
    source: sourceOf("members", "members", { ...m, held: m.rows.length > 0 }),
  };
}

/** New project, where the caller may create one in some tenant it sees, and Switch project. */
function useCommonActions(tenantNames: string[], hands: Hands): Entry[] {
  const { tenants } = useTenantsWhere(tenantNames, createProject);
  return [
    ...(tenants.length ? [{ id: "new:project", icon: Plus, title: "New project", run: () => hands.handOver("newProject") }] : []),
    { id: "switch", icon: ArrowLeftRight, title: "Switch project…", run: () => hands.handOver("picker") },
  ];
}

function useProjectModel(project: string, hands: Hands): Model {
  const navigate = useNavigate();
  const place: Place = { kind: "project", name: project };
  const machines = useMachines(project, undefined, cached);
  const disks = useDisks(project, cached);
  const images = useImages(cached);
  const networks = useNetworks(project, cached);
  const groups = useSecurityGroups(project, cached);
  const addresses = useAddresses(project, cached);
  const endpoints = usePublicEndpoints(project, cached);
  const secrets = useSecrets(project, cached);
  const tokens = useServiceTokens(project, cached);
  // Only shown beside a token: nothing until the bindings come, and the last answer after a failed refetch.
  const tokenRoles = useTokenRoles(project, cached);
  const places = usePlaces(place, hands);
  const go = (to: string) => hands.done(() => void navigate({ to }));
  const members = useMemberSection({ level: "project", name: project }, () => go(`/projects/${project}/members`));
  const { can } = useCan({ level: "project", name: project }, Object.values(actionPerms));
  const common = useCommonActions(places.tenantNames, hands);

  const p = { project };
  const newIn = (to: string) => () => hands.done(() => void navigate({ to, search: { new: 1 } }));
  const gated: [string, string, () => void][] = [
    [actionPerms.container, "New container", () => hands.done(() => void navigate({ to: "/projects/$project/new", params: p, search: { kind: "container" } }))],
    [actionPerms.vm, "New virtual machine", () => hands.done(() => void navigate({ to: "/projects/$project/new", params: p, search: { kind: "vm" } }))],
    [actionPerms.disk, "New disk", newIn(`/projects/${project}/disks`)],
    [actionPerms.network, "New network", newIn(`/projects/${project}/networks`)],
    [actionPerms.securityGroup, "New security group", newIn(`/projects/${project}/security-groups`)],
    [actionPerms.address, "Reserve address", newIn(`/projects/${project}/addresses`)],
    [actionPerms.publicEndpoint, "New public endpoint", newIn(`/projects/${project}/public-endpoints`)],
    [actionPerms.secret, "New secret", newIn(`/projects/${project}/secrets`)],
    [actionPerms.token, "New service token", newIn(`/projects/${project}/service-tokens`)],
    [actionPerms.member, "Add member", newIn(`/projects/${project}/members`)],
  ];
  const actions: Section = {
    key: "actions",
    title: "Actions",
    entries: [
      ...gated.filter(([perm]) => can(perm)).map(([, title, run]) => ({ id: `new:${title}`, icon: Plus, title, run })),
      ...common,
    ],
  };

  const obj = (key: string, title: string, icon: Entry["icon"], items: { name: string; detail?: string; to: string }[]): Section => ({
    key,
    title,
    entries: items.map((o) => ({ id: `${key}:${o.name}`, icon, title: o.name, detail: o.detail, run: () => go(o.to) })),
  });
  const base = `/projects/${project}`;
  const searched: Section[] = [
    obj(
      "containers",
      "Containers",
      Box,
      machines.items.filter((m) => m.kind === "Container").map((m) => ({ name: m.name, detail: phaseLabel(m.phase), to: `${base}/containers/${m.name}` })),
    ),
    obj(
      "vms",
      "Virtual machines",
      Server,
      machines.items.filter((m) => m.kind === "VirtualMachine").map((m) => ({ name: m.name, detail: phaseLabel(m.phase), to: `${base}/vms/${m.name}` })),
    ),
    obj(
      "disks",
      "Disks",
      HardDrive,
      disks.items.map((d) => ({ name: nameOf(d), detail: gib(d.status?.size || d.spec?.size), to: `${base}/disks/${nameOf(d)}` })),
    ),
    obj("images", "Images", Disc3, images.items.map((i) => ({ name: nameOf(i), to: `${base}/images/${nameOf(i)}` }))),
    obj("networks", "Networks", Network, networks.items.map((n) => ({ name: nameOf(n), detail: n.spec?.ipv4Cidr || undefined, to: `${base}/networks/${nameOf(n)}` }))),
    obj(
      "security-groups",
      "Security groups",
      ShieldCheck,
      groups.items.map((g) => ({
        name: nameOf(g),
        detail: plural((g.spec?.ingress.length ?? 0) + (g.spec?.egress.length ?? 0), "rule"),
        to: `${base}/security-groups/${nameOf(g)}`,
      })),
    ),
    obj("addresses", "Addresses", Globe, addresses.items.map((a) => ({ name: nameOf(a), detail: a.status?.ipv6 || undefined, to: `${base}/addresses/${nameOf(a)}` }))),
    obj(
      "public-endpoints",
      "Public endpoints",
      Waypoints,
      endpoints.items.map((e) => ({ name: nameOf(e), detail: e.status?.hostname || undefined, to: `${base}/public-endpoints/${nameOf(e)}` })),
    ),
    obj("secrets", "Secrets", KeyRound, secrets.items.map((s) => ({ name: nameOf(s), detail: plural(s.status?.keys.length ?? 0, "key"), to: `${base}/secrets/${nameOf(s)}` }))),
    obj(
      "service-tokens",
      "Service tokens",
      Terminal,
      tokens.items.map((t) => ({
        name: nameOf(t),
        detail: (tokenRoles.roles[nameOf(t)] ?? []).map(roleLabel).join(", ") || undefined,
        to: `${base}/service-tokens/${nameOf(t)}`,
      })),
    ),
    members.section,
    ...places.sections,
  ];
  const label = projectLabel(
    places.projects.find((x) => x.metadata?.name === project),
    project,
  );
  return {
    always: [actions, pageEntries(projectPages(project), go)],
    searched,
    sources: [
      ...sourceOf("machines", "machines", { ...machines, held: machines.items.length > 0 }),
      ...sourceOf("disks", "disks", { ...disks, held: disks.items.length > 0 }),
      ...sourceOf("images", "images", { ...images, held: images.items.length > 0 }),
      ...sourceOf("networks", "networks", { ...networks, held: networks.items.length > 0 }),
      ...sourceOf("security-groups", "security groups", { ...groups, held: groups.items.length > 0 }),
      ...sourceOf("addresses", "addresses", { ...addresses, held: addresses.items.length > 0 }),
      ...sourceOf("public-endpoints", "public endpoints", { ...endpoints, held: endpoints.items.length > 0 }),
      ...sourceOf("secrets", "secrets", { ...secrets, held: secrets.loaded }),
      ...sourceOf("service-tokens", "service tokens", { ...tokens, held: tokens.loaded }),
      ...members.source,
      ...places.source,
    ],
    noMatch: `${label}, its members, the projects and tenants, or the pages`,
    scopeNote: `Objects of ${label}, projects, tenants and pages`,
  };
}

function useTenantModel(tenant: string, hands: Hands): Model {
  const navigate = useNavigate();
  const places = usePlaces({ kind: "tenant", name: tenant }, hands);
  const go = (to: string) => hands.done(() => void navigate({ to }));
  const members = useMemberSection({ level: "tenant", name: tenant }, () => go(`/tenants/${tenant}/members`));
  const common = useCommonActions(places.tenantNames, hands);
  const { tenantName } = useProjects(cached);
  return {
    always: [{ key: "actions", title: "Actions", entries: common }, pageEntries(tenantPages(tenant), go)],
    searched: [members.section, ...places.sections],
    sources: [...members.source, ...places.source],
    noMatch: `${tenantName(tenant)}'s members, the projects and tenants, or the pages`,
    scopeNote: `Members of ${tenantName(tenant)}, projects, tenants and pages`,
  };
}

/** The palette's search: the query, the active entry, and what choosing does. */
function Search({ model, onClose }: { model: Model; onClose: () => void }) {
  const [query, setQuery] = useState("");
  const sections = matchSections(query, model.always, model.searched);
  const flat = sections.flatMap((s) => s.entries);
  const [picked, setPicked] = useState<string>();
  const active = flat.find((e) => e.id === picked)?.id ?? flat[0]?.id;
  return (
    <PaletteView
      onClose={onClose}
      query={query}
      onQuery={(q) => {
        setQuery(q);
        setPicked(undefined);
      }}
      sections={sections}
      sources={model.sources}
      active={active}
      onActive={setPicked}
      onMove={(d) => {
        if (!flat.length) return;
        const i = flat.findIndex((e) => e.id === active);
        setPicked(flat[(i + d + flat.length) % flat.length]!.id);
      }}
      onChoose={(id) => flat.find((e) => e.id === id)?.run()}
      noMatch={model.noMatch}
      scopeNote={model.scopeNote}
    />
  );
}

function ProjectSearch({ project, hands, onClose }: { project: string; hands: Hands; onClose: () => void }) {
  return <Search model={useProjectModel(project, hands)} onClose={onClose} />;
}

function TenantSearch({ tenant, hands, onClose }: { tenant: string; hands: Hands; onClose: () => void }) {
  return <Search model={useTenantModel(tenant, hands)} onClose={onClose} />;
}

/**
 * The palette, open while mounted; onClose unmounts it. Choosing an entry
 * closes it first and then opens what was chosen. Switch project and New
 * project hand over to the picker or the form, which close it in turn.
 * Focus returns to where it was when it opened.
 */
export function CommandPalette({ place, onClose }: { place: Place; onClose: () => void }) {
  const [then, setThen] = useState<Then>();
  const opener = useRef<Element | null>(typeof document === "undefined" ? null : document.activeElement);
  const pending = useRef<(() => void) | undefined>(undefined);
  const { projects } = useProjects(cached);

  // Focus goes back first, then what was chosen opens, so a dialog it opens takes focus from there.
  useEffect(
    () => () => {
      const el = opener.current;
      if (el instanceof HTMLElement && el.isConnected) el.focus();
      const after = pending.current;
      pending.current = undefined;
      after?.();
    },
    [],
  );

  const hands: Hands = {
    done: (after) => {
      pending.current = after;
      onClose();
    },
    handOver: setThen,
  };

  if (then === "picker") return <ProjectPicker place={place} open onClose={onClose} />;
  if (then === "newProject") {
    const tenant = place.kind === "tenant" ? place.name : (projects.find((p) => p.metadata?.name === place.name)?.spec?.tenant ?? "");
    return <NewProjectDialog open tenant={tenant} onClose={onClose} />;
  }
  return place.kind === "project" ? (
    <ProjectSearch project={place.name} hands={hands} onClose={onClose} />
  ) : (
    <TenantSearch tenant={place.name} hands={hands} onClose={onClose} />
  );
}
