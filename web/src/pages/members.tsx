// Who holds a role at one scope, a project or a tenant: the people bound
// there, with their roles changed, added and removed through MemberService
// (docs/decisions/tenancy-and-iam.md). A project's page adds one fact about
// who holds a role through its tenant. A person who is not a platform member
// yet is listed with that alone, never with whether they were invited.

import { Code, ConnectError } from "@connectrpc/connect";
import { ChevronDown, MoreHorizontal, Plus, UserMinus } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { Dialog } from "@/components/dialog";
import { CommandLine, FormField, Input, Select } from "@/components/form";
import { Notice } from "@/components/notice";
import { Link } from "@tanstack/react-router";
import { Topbar, useProjectName, useTenantName } from "@/components/shell";
import { ErrorNotice, SkeletonRows } from "@/components/states";
import { Badge, Button, Card, CardHeader } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import {
  memberCommand,
  roleLabel,
  roles,
  useAddMember,
  useMembers,
  useRemoveMember,
  useSetMemberRole,
  type MemberRow,
  type Scope,
} from "@/lib/members";
import { projectLabel, useProjects } from "@/lib/projects";
import { useCan } from "@/lib/permissions";
import { useMember } from "@/lib/session";
import { useCreating, type NewProps } from "@/lib/new-param";

function AddMemberDialog({ scope, where, open, onClose }: { scope: Scope; where: string; open: boolean; onClose: () => void }) {
  const add = useAddMember(scope);
  const options = roles[scope.level];
  const [email, setEmail] = useState("");
  // The least that lets someone work: editor in a project, viewer in a tenant.
  const [role, setRole] = useState((options.find((r) => r.id.endsWith(".editor")) ?? options[options.length - 1]).id);
  const close = () => {
    add.reset();
    setEmail("");
    onClose();
  };
  // Bindings name an email in lower case, as the API stores it.
  const address = email.trim().toLowerCase();
  const valid = /^[^\s@]+@[^\s@]+$/.test(address);
  return (
    <Dialog open={open} onClose={close} title={`Add a member to ${where}`}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          add.mutate({ email: address, role }, { onSuccess: close });
        }}
      >
        <FormField label="Email">
          <Input type="email" value={email} onChange={(e) => setEmail(e.target.value)} autoFocus spellCheck={false} placeholder="name@example.com" className="font-sans" />
        </FormField>
        <fieldset className="mt-4">
          <legend className="text-[12.5px] text-foreground">Role</legend>
          <div className="mt-1.5 divide-y rounded-md border">
            {options.map((r) => (
              <label key={r.id} className="flex cursor-pointer gap-2.5 px-3 py-2.5 hover:bg-surface-2/60">
                <input type="radio" name="role" value={r.id} checked={role === r.id} onChange={() => setRole(r.id)} className="mt-0.5 accent-[var(--accent)]" />
                <span className="min-w-0">
                  <span className="flex items-baseline gap-2 text-[13px] text-foreground">
                    {r.label}
                    <span className="font-mono text-[11px] text-subtle">{r.id}</span>
                  </span>
                  <span className="block text-[12px] text-muted">{r.holds}</span>
                </span>
              </label>
            ))}
          </div>
        </fieldset>
        <CommandLine command={memberCommand(scope, "add", address, role)} />
        {add.error && (
          <Notice tone="error" title="Adding failed" className="mt-3">
            {errorMessage(add.error)}
          </Notice>
        )}
        <div className="mt-4 flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={close}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={!valid || add.isPending}>
            {add.isPending ? "Adding…" : "Add member"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function RemoveDialog({ scope, where, row, open, onClose }: { scope: Scope; where: string; row: MemberRow; open: boolean; onClose: () => void }) {
  const remove = useRemoveMember(scope);
  const close = () => {
    remove.reset();
    onClose();
  };
  const held = row.roles.map(roleLabel).join(" and ");
  return (
    <Dialog open={open} onClose={close} title={`Remove ${row.email}`}>
      <p>
        Their {held} role in {where} ends.
        {scope.level === "project" ? " A role they hold in the tenant stays." : " Roles they hold in its projects stay."}
      </p>
      <CommandLine command={memberCommand(scope, "remove", row.email)} />
      {remove.error && (
        <Notice tone="error" title="Removing failed" className="mt-3">
          {errorMessage(remove.error)}
        </Notice>
      )}
      <div className="mt-4 flex justify-end gap-2">
        <Button variant="ghost" onClick={close}>
          Cancel
        </Button>
        <Button variant="danger" disabled={remove.isPending} onClick={() => remove.mutate({ email: row.email }, { onSuccess: close })}>
          <UserMinus /> {remove.isPending ? "Removing…" : "Remove member"}
        </Button>
      </div>
    </Dialog>
  );
}

function RowMenu({ onRemove }: { onRemove: () => void }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const away = (e: MouseEvent) => !ref.current?.contains(e.target as Node) && setOpen(false);
    document.addEventListener("mousedown", away);
    return () => document.removeEventListener("mousedown", away);
  }, [open]);
  return (
    <div ref={ref} className="relative">
      <button aria-label="Actions" onClick={() => setOpen(!open)} className="rounded-md p-1.5 text-subtle hover:bg-surface-2 hover:text-foreground">
        <MoreHorizontal className="size-4" />
      </button>
      {open && (
        <div role="menu" className="absolute right-0 z-20 mt-1 w-40 rounded-md border bg-surface p-1 shadow-lg">
          <button
            role="menuitem"
            onClick={() => {
              setOpen(false);
              onRemove();
            }}
            className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-[13px] text-fail hover:bg-surface-2"
          >
            <UserMinus className="size-3.5" /> Remove
          </button>
        </div>
      )}
    </div>
  );
}

/** What the caller may do to the members of a scope: change and add (apply), remove (delete). */
type May = { apply: boolean; remove: boolean; settled: boolean };

function MemberLine({ scope, where, row, you, may }: { scope: Scope; where: string; row: MemberRow; you: boolean; may: May }) {
  const setRole = useSetMemberRole(scope);
  const [removing, setRemoving] = useState(false);
  const options = roles[scope.level];
  const current = row.roles.length === 1 ? row.roles[0] : "";
  return (
    <li className="px-4 py-2.5">
      <div className="flex items-center gap-3">
        <span className="flex size-7 shrink-0 items-center justify-center rounded-full bg-surface-2 text-[12px] font-medium uppercase text-muted">
          {row.email[0]}
        </span>
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-2">
            <span className="truncate text-[13px] font-medium">{row.email}</span>
            {you && <Badge>You</Badge>}
          </span>
          {row.platformMember ? (
            <span className="block truncate font-mono text-[11.5px] text-subtle">{row.username}</span>
          ) : (
            <span className="block text-[11.5px] text-subtle">Not a platform member yet; the role takes effect once they are.</span>
          )}
        </span>
        {may.apply ? (
          <>
            <span className="relative shrink-0">
              <Select
                aria-label={`Role of ${row.email}`}
                value={current}
                disabled={setRole.isPending}
                onChange={(e) => setRole.mutate({ email: row.email, role: e.target.value })}
                className="h-8 w-auto appearance-none bg-surface pl-2.5 pr-7 font-sans hover:bg-surface-2 disabled:opacity-50"
              >
                {current === "" && <option value="">{row.roles.map(roleLabel).join(" + ")}</option>}
                {options.map((r) => (
                  <option key={r.id} value={r.id}>
                    {r.label}
                  </option>
                ))}
              </Select>
              <ChevronDown className="pointer-events-none absolute right-2 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
            </span>
          </>
        ) : (
          <span className="shrink-0 text-[13px] text-muted">{row.roles.map(roleLabel).join(" + ")}</span>
        )}
        {may.remove && <RowMenu onRemove={() => setRemoving(true)} />}
      </div>
      {setRole.error && (
        <Notice tone="error" title="Changing the role failed" className="ml-10 mt-2">
          <span className="flex items-center justify-between gap-2">
            {errorMessage(setRole.error)}
            <Button size="sm" variant="ghost" onClick={() => setRole.reset()}>
              Dismiss
            </Button>
          </span>
        </Notice>
      )}
      <RemoveDialog scope={scope} where={where} row={row} open={removing} onClose={() => setRemoving(false)} />
    </li>
  );
}

/** The people bound at one scope, with their roles changed, added and removed where the caller may. */
function MemberList({ scope, where, may, newProps = {} }: { scope: Scope; where: string; may: May; newProps?: NewProps }) {
  const { rows, loading, error, refetch } = useMembers(scope);
  const who = useMember();
  const denied = error instanceof ConnectError && error.code === Code.PermissionDenied;
  // ?new=1 opens Add member only once the list says the caller may read the members.
  const [adding, setAdding] = useCreating(newProps, may.apply && !loading && !denied, may.settled && !loading);
  return (
    <Card>
      <CardHeader
        title={
          <span className="flex items-baseline gap-2">
            People
            {!loading && !error && <span className="font-mono text-[11.5px] font-normal text-subtle">{rows.length}</span>}
          </span>
        }
        action={
          may.apply &&
          !denied && (
            <Button size="sm" onClick={() => setAdding(true)}>
              <Plus /> Add member
            </Button>
          )
        }
      />
      {loading ? (
        <SkeletonRows rows={3} />
      ) : denied ? (
        <p className="px-4 py-6 text-[13px] text-muted">This account's roles do not include listing the members here.</p>
      ) : error ? (
        <ErrorNotice error={error} title="The members did not load" onRetry={() => void refetch()} />
      ) : rows.length === 0 ? (
        <p className="px-4 py-6 text-[13px] text-muted">No one holds a role here.</p>
      ) : (
        <ul className="divide-y">
          {rows.map((r) => (
            <MemberLine key={r.email} scope={scope} where={where} row={r} you={r.email === who.id.toLowerCase()} may={may} />
          ))}
        </ul>
      )}
      {may.apply && <AddMemberDialog scope={scope} where={where} open={adding} onClose={() => setAdding(false)} />}
    </Card>
  );
}

/**
 * Who reaches the project through its tenant, as one fact under the list:
 * how many, when the caller may list the tenant's members, and that they do
 * when not.
 */
function ThroughTheTenant({ tenant, label }: { tenant: string; label: string }) {
  const { rows, loading, error } = useMembers({ level: "tenant", name: tenant });
  if (loading) return null;
  if (error) return <p className="mt-3 text-[12.5px] text-muted">Roles held in the tenant {label} also hold here.</p>;
  if (rows.length === 0) return null;
  return (
    <p className="mt-3 text-[12.5px] text-muted">
      {rows.length === 1 ? "1 person holds" : `${rows.length} people hold`} a role here through the tenant{" "}
      <Link to="/tenants/$tenant/members" params={{ tenant }} className="text-accent hover:underline">
        {label}
      </Link>
      .
    </p>
  );
}

function PageHead({ text }: { text: string }) {
  return (
    <div className="px-4 pb-4 pt-6 md:px-8">
      <h1 className="text-[22px] font-semibold tracking-tight">Members</h1>
      <p className="mt-1 text-[13px] text-muted">{text}</p>
    </div>
  );
}

// MemberService authorizes each call as the RoleBinding call it makes.
const apply = "iam.rolebindings.apply";
const remove = "iam.rolebindings.delete";

/** What the caller may do to the members at the scope, asked in one call. */
function useMay(scope: Scope): May {
  const { can, settled } = useCan(scope, [apply, remove]);
  return { apply: can(apply), remove: can(remove), settled };
}

export function ProjectMembers(props: NewProps) {
  const project = useProjectName();
  const { projects, tenantName } = useProjects();
  const p = projects.find((x) => x.metadata?.name === project);
  const tenant = p?.spec?.tenant ?? "";
  const scope: Scope = { level: "project", name: project };
  const may = useMay(scope);
  return (
    <>
      <Topbar crumbs={["Members"]} />
      <PageHead text="The people who hold a role in this project." />
      <div className="max-w-4xl px-4 pb-10 md:px-8">
        <MemberList scope={scope} where={projectLabel(p, project)} may={may} newProps={props} />
        {tenant && <ThroughTheTenant tenant={tenant} label={tenantName(tenant)} />}
      </div>
    </>
  );
}

export function TenantMembers() {
  const tenant = useTenantName();
  const { tenantName } = useProjects();
  const scope: Scope = { level: "tenant", name: tenant };
  const may = useMay(scope);
  return (
    <>
      <Topbar crumbs={["Members"]} />
      <PageHead text="The people who hold a role in the tenant. A tenant role holds in every project of it." />
      <div className="max-w-4xl px-4 pb-10 md:px-8">
        <MemberList scope={scope} where={tenantName(tenant)} may={may} />
      </div>
    </>
  );
}
