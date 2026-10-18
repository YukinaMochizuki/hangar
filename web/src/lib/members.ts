// The people who hold roles in a project or its tenant, through
// MemberService. Each write is the API's own call, authorized and admitted as
// the RoleBinding it makes, so the API's rules (who may grant what, a tenant
// keeping an owner) decide, and its error is what the console shows.

import { create } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { skipToken, useMutation, useQueryClient } from "@tanstack/react-query";
import { MemberService, type Member } from "@hangar/api/hangar/iam/v1alpha1/member_pb";
import { BindingScopeSchema, type BindingScope } from "@hangar/api/hangar/iam/v1alpha1/role_binding_pb";
import type { ListOpts } from "@/lib/machines";

export type ScopeLevel = "project" | "tenant";

export type Scope = { level: ScopeLevel; name: string };

export type Role = { id: string; label: string; holds: string };

// The roles that bind at each level, and what each holds, as
// docs/decisions/tenancy-and-iam.md defines them.
export const roles: Record<ScopeLevel, Role[]> = {
  project: [
    { id: "project.owner", label: "Owner", holds: "Everything in the project, its members and service tokens included." },
    {
      id: "project.editor",
      label: "Editor",
      holds: "Changes what runs in the project, with logs, exec and ssh. Writes no members or service tokens.",
    },
    { id: "project.viewer", label: "Viewer", holds: "Reads the project." },
  ],
  tenant: [
    { id: "tenant.owner", label: "Owner", holds: "Everything in the tenant and every project in it." },
    { id: "tenant.viewer", label: "Viewer", holds: "Reads every project in the tenant, without logs, exec, ssh or endpoints." },
  ],
};

export function roleLabel(id: string): string {
  for (const level of Object.values(roles)) {
    const r = level.find((r) => r.id === id);
    if (r) return r.label;
  }
  return id;
}

export function toBindingScope(s: Scope): BindingScope {
  return create(BindingScopeSchema, { target: { case: s.level, value: s.name } });
}

/** The CLI command that does the same, for the copyable line beside an action. */
export function memberCommand(s: Scope, verb: "add" | "set-role" | "remove", email: string, role?: string): string {
  const scope = s.level === "tenant" ? ` --tenant ${s.name}` : ` --project ${s.name}`;
  return `hangar member ${verb} ${email || "EMAIL"}${role ? ` --role ${role}` : ""}${scope}`;
}

/** A scope's members, one row per person: a person holding two roles there shows both. */
export type MemberRow = { email: string; username: string; platformMember: boolean; roles: string[] };

export function byPerson(members: Member[]): MemberRow[] {
  const rows = new Map<string, MemberRow>();
  for (const m of members) {
    const row = rows.get(m.email) ?? { email: m.email, username: m.username, platformMember: m.platformMember, roles: [] };
    row.roles.push(m.role);
    rows.set(m.email, row);
  }
  return [...rows.values()];
}

export function useMembers(s: Scope | undefined, opts: ListOpts = {}) {
  const q = useQuery(
    MemberService.method.listMembers,
    s?.name ? { scope: toBindingScope(s) } : skipToken,
    { refetchInterval: 15_000, retry: false, ...opts },
  );
  return { rows: byPerson(q.data?.members ?? []), loading: q.isPending && !!s?.name, error: q.error, refetch: q.refetch };
}

function useMemberWrite<A>(call: (c: ReturnType<typeof createClient<typeof MemberService>>, a: A) => Promise<{ warnings?: string[] }>) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (a: A) => call(createClient(MemberService, transport), a),
    onSettled: () => queryClient.invalidateQueries(),
  });
}

export function useAddMember(s: Scope) {
  return useMemberWrite((c, a: { email: string; role: string }) => c.addMember({ scope: toBindingScope(s), ...a }));
}

export function useSetMemberRole(s: Scope) {
  return useMemberWrite((c, a: { email: string; role: string }) => c.setMemberRole({ scope: toBindingScope(s), ...a }));
}

export function useRemoveMember(s: Scope) {
  return useMemberWrite(async (c, a: { email: string }) => {
    await c.removeMember({ scope: toBindingScope(s), ...a });
    return {};
  });
}
