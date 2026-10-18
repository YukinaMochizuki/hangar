// Choosing where the console is: a dialog listing every tenant the member
// sees, each with its projects under it, filtered by name or ID. A tenant's
// row opens the tenant's own pages, a project's row the project. Creating a
// project starts here too, and on a tenant's overview.

import { useNavigate } from "@tanstack/react-router";
import { Check, Plus, Search } from "lucide-react";
import { useMemo, useState } from "react";
import { Dialog } from "@/components/dialog";
import { Notice } from "@/components/notice";
import { Skeleton } from "@/components/states";
import type { Place } from "@/components/shell";
import { Button, CopyButton } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { useTenantsWhere } from "@/lib/permissions";
import { cn } from "@/lib/cn";
import { projectLabel, randomSuffix, slug, useCreateProject, useNameFree, useProjects } from "@/lib/projects";

type Row = { kind: "tenant" | "project"; name: string; label: string; tenant: string };

/** What creating a project in a tenant takes. */
export const createProject = "tenancy.projects.apply";

/** New project, for whoever may create a project in some tenant they see. */
function NewProjectButton({ onClick }: { onClick: () => void }) {
  const { tenantNames } = useProjects();
  const { tenants } = useTenantsWhere(tenantNames, createProject);
  if (tenants.length === 0) return null;
  return (
    <Button size="sm" onClick={onClick}>
      <Plus /> New project
    </Button>
  );
}

export function ProjectPicker({ place, open, onClose }: { place: Place; open: boolean; onClose: () => void }) {
  const { projects, loading, error, tenantName, tenantNames } = useProjects();
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [creating, setCreating] = useState(false);

  const rows = useMemo(() => {
    const q = query.trim().toLowerCase();
    const hit = (r: Row) => !q || r.name.includes(q) || r.label.toLowerCase().includes(q);
    const out: Row[] = [];
    for (const t of tenantNames) {
      const tenantRow: Row = { kind: "tenant", name: t, label: tenantName(t), tenant: t };
      const own = projects
        .filter((p) => (p.spec?.tenant ?? "") === t)
        .map((p): Row => ({ kind: "project", name: p.metadata?.name ?? "", label: projectLabel(p), tenant: t }))
        .filter(hit);
      if (own.length || hit(tenantRow)) out.push(tenantRow, ...own);
    }
    return out;
  }, [projects, query, tenantName, tenantNames]);

  const go = (r: Row) => {
    onClose();
    if (r.kind === "tenant") void navigate({ to: "/tenants/$tenant", params: { tenant: r.name } });
    else void navigate({ to: "/projects/$project", params: { project: r.name } });
  };
  const here = (r: Row) => r.kind === place.kind && r.name === place.name;
  const defaultTenant = place.kind === "tenant" ? place.name : (projects.find((p) => p.metadata?.name === place.name)?.spec?.tenant ?? "");

  return (
    <>
      <Dialog
        open={open && !creating}
        onClose={() => {
          setQuery("");
          onClose();
        }}
        title="Projects and tenants"
        wide
        // The dialog draws its title row only while open, so the permission is asked only then.
        action={<NewProjectButton onClick={() => setCreating(true)} />}
      >
        <label className="mt-2 flex h-9 items-center gap-2 rounded-md border border-border-strong bg-background px-2.5 focus-within:border-accent">
          <Search className="size-3.5 text-subtle" />
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              const first = rows.find((r) => r.kind === "project") ?? rows[0];
              if (e.key === "Enter" && first) go(first);
            }}
            autoFocus
            placeholder="Name or ID"
            aria-label="Filter by name or ID"
            className="min-w-0 flex-1 bg-transparent text-[13px] text-foreground outline-none placeholder:text-subtle"
          />
        </label>
        {error && (
          <Notice tone="error" title="The projects did not load" className="mt-3">
            {errorMessage(error)}
          </Notice>
        )}
        <div className="mt-3 max-h-[min(420px,60vh)] overflow-y-auto rounded-md border">
          <table className="w-full text-[13px]">
            <thead className="sticky top-0 bg-surface-2 text-left text-[11.5px] text-subtle">
              <tr>
                <th className="px-3 py-1.5 font-medium">Name</th>
                <th className="px-3 py-1.5 font-medium">ID</th>
                <th className="w-8" />
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr
                  key={`${r.kind}/${r.name}`}
                  onClick={() => go(r)}
                  aria-current={here(r) ? "page" : undefined}
                  className={cn(
                    "cursor-pointer border-t hover:bg-surface-2/70",
                    r.kind === "tenant" && "bg-surface-2/40",
                    here(r) && "bg-accent-soft/50",
                  )}
                >
                  <td className={cn("px-3 py-2", r.kind === "project" && "pl-7")}>
                    <span className={cn("text-foreground", r.kind === "tenant" && "font-medium")}>{r.label}</span>
                    {r.kind === "tenant" && <span className="ml-2 text-[11.5px] text-subtle">Tenant</span>}
                  </td>
                  <td className="px-3 py-2 font-mono text-[12px] text-muted">{r.name}</td>
                  <td className="px-2">{here(r) && <Check className="size-3.5 text-accent" />}</td>
                </tr>
              ))}
              {loading && (
                <tr aria-busy="true" aria-label="Loading">
                  <td colSpan={3} className="px-3 py-3">
                    <Skeleton className="h-3 w-40" />
                  </td>
                </tr>
              )}
              {!loading && !error && rows.length === 0 && (
                <tr>
                  <td colSpan={3} className="px-3 py-8 text-center text-muted">
                    {query ? "Nothing matches." : "This account sees no project."}
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </Dialog>
      <NewProjectDialog
        open={creating}
        tenant={defaultTenant}
        onClose={() => setCreating(false)}
        onCreated={() => {
          setCreating(false);
          onClose();
        }}
      />
    </>
  );
}

/**
 * A new project in one of the member's tenants. The name is global and never
 * reused, so one is suggested from the display name, as `hangar create`
 * does, and checked to be free; it can be changed before creating.
 */
export function NewProjectDialog({ open, tenant, onClose, onCreated }: { open: boolean; tenant: string; onClose: () => void; onCreated?: () => void }) {
  // The form mounts with the dialog's content, so each opening starts empty.
  return (
    <Dialog open={open} onClose={onClose} title="New project">
      <NewProjectForm tenant={tenant} onClose={onClose} onCreated={onCreated} />
    </Dialog>
  );
}

/** A display name as one shell word, for the copyable command: inside single quotes nothing is expanded. */
function shellQuote(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

function NewProjectForm({ tenant, onClose, onCreated }: { tenant: string; onClose: () => void; onCreated?: () => void }) {
  const { tenantNames, tenantName, tenant: tenantOf } = useProjects();
  // Only tenants the caller may create a project in are offered.
  const allowed = useTenantsWhere(tenantNames, createProject);
  const [picked, setPicked] = useState<string>();
  // The tenant the form was opened for, until every answer is in; then it, if allowed, or the first allowed.
  const chosen = picked ?? (!allowed.known || allowed.tenants.includes(tenant) ? tenant : (allowed.tenants[0] ?? ""));
  const mayHere = allowed.known && allowed.tenants.includes(chosen);
  const [displayName, setDisplayName] = useState("");
  // Until the ID is typed in, it follows the display name, with one suffix for the whole form.
  const [typed, setTyped] = useState<string>();
  const [suffix] = useState(randomSuffix);
  const name = typed ?? (displayName.trim() ? `${slug(displayName) || "project"}-${suffix}` : "");
  const create = useCreateProject();
  const navigate = useNavigate();
  const check = useNameFree(name);

  const t = tenantOf(chosen);
  const limit = t?.status?.limits?.projects;
  const used = t?.status?.usage?.projects ?? 0;
  const full = limit !== undefined && used >= limit;
  const none = allowed.known && allowed.tenants.length === 0;
  const command = `hangar create project ${name || "NAME"} --tenant ${chosen || "TENANT"} --display-name ${shellQuote(displayName)}`;
  const field = "mt-1.5 h-9 w-full rounded-md border border-border-strong bg-background px-2.5 text-[13px] text-foreground outline-none focus:border-accent";
  const hint = !name
    ? "Made from the display name. An ID is global and never reused."
    : !check.valid
      ? "Lower-case letters, digits and dashes, starting and ending with a letter or digit."
      : check.error
        ? `Checking the ID failed: ${errorMessage(check.error)}`
        : check.free === false
          ? `${name} is taken or was used before.`
          : check.free === true
            ? `${name} is free. An ID is global and never reused.`
            : "Checking…";

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate(
          { name, tenant: chosen, displayName: displayName.trim() },
          {
            onSuccess: () => {
              onClose();
              onCreated?.();
              void navigate({ to: "/projects/$project", params: { project: name } });
            },
          },
        );
      }}
    >
      <label className="block">
        <span className="text-[12.5px] text-foreground">Tenant</span>
        <select value={chosen} onChange={(e) => setPicked(e.target.value)} disabled={!allowed.known} className={field}>
          {allowed.tenants.map((x) => (
            <option key={x} value={x}>
              {tenantName(x)} ({x})
            </option>
          ))}
        </select>
      </label>
      <label className="mt-4 block">
        <span className="text-[12.5px] text-foreground">Display name</span>
        <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} autoFocus placeholder="My demo" className={field} />
      </label>
      <label className="mt-4 block">
        <span className="text-[12.5px] text-foreground">ID</span>
        <input value={name} onChange={(e) => setTyped(e.target.value.toLowerCase())} spellCheck={false} className={cn(field, "font-mono")} />
        <span className="mt-1 block text-[11.5px] text-subtle">{hint}</span>
      </label>
      <div className="mt-4 flex items-center gap-2 rounded-md border bg-surface-2 px-2.5 py-1.5">
        <code className="min-w-0 flex-1 truncate font-mono text-[11.5px] text-muted">{command}</code>
        <CopyButton text={command} />
      </div>
      {none && <Notice tone="warning" title="This account's roles do not include creating projects in any tenant." className="mt-3" />}
      {allowed.error && (
        <Notice tone="error" title="The permission check did not answer" className="mt-3">
          {errorMessage(allowed.error)}
        </Notice>
      )}
      {full && <Notice tone="warning" title={`The tenant holds ${used} of ${limit} projects.`} className="mt-3" />}
      {create.error && (
        <Notice tone="error" title="Creating failed" className="mt-3">
          {errorMessage(create.error)}
        </Notice>
      )}
      <div className="mt-4 flex justify-end gap-2">
        <Button type="button" variant="ghost" onClick={onClose}>
          Cancel
        </Button>
        <Button
          type="submit"
          variant="primary"
          disabled={!mayHere || !displayName.trim() || !check.valid || check.free !== true || full || create.isPending}
        >
          {create.isPending ? "Creating…" : "Create project"}
        </Button>
      </div>
    </form>
  );
}
