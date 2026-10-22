// Who holds a role at one scope, a project or a tenant: the people bound
// there, with their roles changed, added and removed through MemberService
// (docs/decisions/tenancy-and-iam.md). A project's page adds one fact about
// who holds a role through its tenant. A person who is not a platform member
// yet is listed with that alone, never with whether they were invited.

import { Code, ConnectError } from "@connectrpc/connect";
import { Plus, UserMinus } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Dialog } from "@/components/dialog";
import { CommandLine, FormField, Input, Select } from "@/components/form";
import { RowMenu } from "@/components/menu";
import { Notice } from "@/components/notice";
import { Link } from "@tanstack/react-router";
import { ListCount, ListFrame, NameFilter, PageHead } from "@/components/page";
import { Topbar, useProjectName, useTenantName } from "@/components/shell";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Badge, Button, Card } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
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
  // Bindings name an email in lower case and ASCII, as the API stores it.
  // One that is not ASCII is refused, not lower-cased: that could fold it
  // into someone else's address (U+212A KELVIN SIGN becomes k). The field
  // is type text: a browser converts an email field's domain to ASCII
  // (IDNA) before onChange, so this check would never see it.
  const ascii = /^[ -~]*$/.test(email.trim());
  const address = ascii ? email.trim().toLowerCase() : email.trim();
  const valid = ascii && /^[^\s@]+@[^\s@]+$/.test(address);
  return (
    <Dialog open={open} onClose={close} title={`Add a member to ${where}`}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          add.mutate({ email: address, role }, { onSuccess: close });
        }}
      >
        <FormField label="Email" hint={ascii ? undefined : "ASCII email addresses only."}>
          <Input type="text" inputMode="email" autoComplete="email" value={email} onChange={(e) => setEmail(e.target.value)} autoFocus spellCheck={false} placeholder="name@example.com" className="font-sans" />
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

/** What the caller may do to the members of a scope: change and add (apply), remove (delete). */
type May = { apply: boolean; remove: boolean; settled: boolean };

/** A person's row: who they are with their username under it, their role, and their menu; a refused role change shows in a row under it. */
function MemberLine({ scope, where, row, you, may }: { scope: Scope; where: string; row: MemberRow; you: boolean; may: May }) {
  const setRole = useSetMemberRole(scope);
  const [removing, setRemoving] = useState(false);
  const options = roles[scope.level];
  const current = row.roles.length === 1 ? row.roles[0] : "";
  return (
    <>
      <tr className={cn(tbodyRow, setRole.error && "border-b-0")}>
        <td className="max-w-0 px-4 py-2.5">
          <span className="flex items-center gap-3">
            <span className="flex size-7 shrink-0 items-center justify-center rounded-full bg-surface-2 text-[12px] font-medium uppercase text-muted">
              {row.email[0]}
            </span>
            <span className="min-w-0">
              <span className="flex items-center gap-2">
                <span className="truncate font-medium">{row.email}</span>
                {you && <Badge className="shrink-0">You</Badge>}
              </span>
              {row.platformMember ? (
                <span className="block truncate font-mono text-[11.5px] text-subtle">{row.username}</span>
              ) : (
                <span className="block text-[11.5px] text-subtle">Not signed up yet</span>
              )}
            </span>
          </span>
        </td>
        <td className="whitespace-nowrap px-4 py-2.5">
          {may.apply ? (
            <Select
              aria-label={`Role of ${row.email}`}
              value={current}
              disabled={setRole.isPending}
              onChange={(e) => setRole.mutate({ email: row.email, role: e.target.value })}
              className="h-8 w-auto bg-surface font-sans"
            >
              {current === "" && <option value="">{row.roles.map(roleLabel).join(" + ")}</option>}
              {options.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.label}
                </option>
              ))}
            </Select>
          ) : (
            <span className="text-muted">{row.roles.map(roleLabel).join(" + ")}</span>
          )}
        </td>
        <td className="px-2 py-2.5">
          {may.remove && <RowMenu label="Actions" actions={[{ label: "Remove", icon: UserMinus, danger: true, onSelect: () => setRemoving(true) }]} />}
          <RemoveDialog scope={scope} where={where} row={row} open={removing} onClose={() => setRemoving(false)} />
        </td>
      </tr>
      {setRole.error && (
        <tr className="border-b last:border-0">
          <td colSpan={3} className="px-4 pb-2.5">
            <Notice tone="error" title="Changing the role failed" className="ml-10">
              <span className="flex items-center justify-between gap-2">
                {errorMessage(setRole.error)}
                <Button size="sm" variant="ghost" onClick={() => setRole.reset()}>
                  Dismiss
                </Button>
              </span>
            </Notice>
          </td>
        </tr>
      )}
    </>
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

// MemberService authorizes each call as the RoleBinding call it makes.
const apply = "iam.rolebindings.apply";
const remove = "iam.rolebindings.delete";

/** What the caller may do to the members at the scope, asked in one call. */
function useMay(scope: Scope): May {
  const { can, settled } = useCan(scope, [apply, remove]);
  return { apply: can(apply), remove: can(remove), settled };
}

/**
 * The page of the people bound at one scope, filtered by email or username,
 * with their roles changed, added and removed where the caller may, and
 * what the page adds under the list.
 */
function MembersPage({
  scope,
  where,
  description,
  newProps = {},
  children,
}: {
  scope: Scope;
  where: string;
  description: string;
  newProps?: NewProps;
  children?: ReactNode;
}) {
  const may = useMay(scope);
  const { rows, loading, error, refetch } = useMembers(scope);
  const who = useMember();
  const [query, setQuery] = useState("");
  const denied = error instanceof ConnectError && error.code === Code.PermissionDenied;
  // ?new=1 opens Add member only once the list says the caller may read the members.
  const [adding, setAdding] = useCreating(newProps, may.apply && !loading && !denied, may.settled && !loading);
  const q = query.trim().toLowerCase();
  const shown = rows.filter((r) => !q || r.email.includes(q) || r.username.toLowerCase().includes(q));
  return (
    <>
      <Topbar crumbs={["Members"]} />
      <PageHead
        title="Members"
        description={description}
        action={
          may.apply &&
          !denied && (
            <Button variant="primary" onClick={() => setAdding(true)}>
              <Plus /> Add member
            </Button>
          )
        }
      />
      <div className="px-4 pb-10 md:px-8">
        {denied ? (
          <Card>
            <p className="px-4 py-6 text-[13px] text-muted">This account's roles do not include listing the members here.</p>
          </Card>
        ) : (
          <ListFrame state={{ loading, error, refetch: () => void refetch(), count: rows.length }} noun="members" empty={{ title: "No members" }}>
            <NameFilter value={query} onChange={setQuery} placeholder="Email or username" label="Filter by email or username" />
            <Card className="overflow-x-auto">
              <table className="w-full text-[13px]">
                <thead>
                  <tr className={theadRow}>
                    <Th>Person</Th>
                    <Th className="w-px">Role</Th>
                    <Th className="w-10 px-0" />
                  </tr>
                </thead>
                <tbody>
                  {shown.map((r) => (
                    <MemberLine key={r.email} scope={scope} where={where} row={r} you={r.email === who.id.toLowerCase()} may={may} />
                  ))}
                  {shown.length === 0 && <NoMatch noun="member" colSpan={3} />}
                </tbody>
              </table>
            </Card>
            <ListCount shown={shown.length} total={rows.length} />
          </ListFrame>
        )}
        {children}
      </div>
      {may.apply && <AddMemberDialog scope={scope} where={where} open={adding} onClose={() => setAdding(false)} />}
    </>
  );
}

export function ProjectMembers(props: NewProps) {
  const project = useProjectName();
  const { projects, tenantName } = useProjects();
  const p = projects.find((x) => x.metadata?.name === project);
  const tenant = p?.spec?.tenant ?? "";
  return (
    <MembersPage
      scope={{ level: "project", name: project }}
      where={projectLabel(p, project)}
      description="People with a role in this project"
      newProps={props}
    >
      {tenant && <ThroughTheTenant tenant={tenant} label={tenantName(tenant)} />}
    </MembersPage>
  );
}

export function TenantMembers() {
  const tenant = useTenantName();
  const { tenantName } = useProjects();
  return (
    <MembersPage
      scope={{ level: "tenant", name: tenant }}
      where={tenantName(tenant)}
      description="Roles here hold in every project"
    />
  );
}
