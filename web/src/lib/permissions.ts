// What the caller may do at a scope, from AccountService.TestPermissions
// (docs/decisions/tenancy-and-iam.md). The console asks it only to decide
// which controls to show: the API still decides every call, and a control
// shown to someone it then refuses shows the refusal, as any error.

import { useQuery, useTransport, createQueryOptions } from "@connectrpc/connect-query";
import { skipToken, useQueries } from "@tanstack/react-query";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { toBindingScope, type Scope } from "@/lib/members";

/**
 * Which of the permissions the caller holds at the scope, asked in one call.
 * Until the answer has come, or when it failed, nothing is held: a control
 * appears once it is known to work. `known` says whether the answer came, so
 * a page states a refusal only when there was one.
 */
export function useCan(scope: Scope | undefined, permissions: string[]) {
  const q = useQuery(
    AccountService.method.testPermissions,
    scope?.name ? { scope: toBindingScope(scope), permissions } : skipToken,
    { staleTime: 30_000, retry: false },
  );
  const held = new Set(q.data?.permissions ?? []);
  return { can: (p: string) => held.has(p), known: q.isSuccess, error: q.error };
}

/**
 * Of the tenants named, those where the caller holds the permission, asked
 * once per tenant. `known` is true only once every answer came, and `error`
 * is the first that failed, so a page states a refusal only when there was one.
 */
export function useTenantsWhere(tenants: string[], permission: string) {
  const transport = useTransport();
  const answers = useQueries({
    queries: tenants.map((t) => ({
      ...createQueryOptions(AccountService.method.testPermissions, { scope: toBindingScope({ level: "tenant", name: t }), permissions: [permission] }, { transport }),
      staleTime: 30_000,
      retry: false,
    })),
  });
  return {
    tenants: tenants.filter((_, i) => answers[i]?.data?.permissions.includes(permission)),
    known: answers.every((a) => a.isSuccess),
    error: answers.find((a) => a.isError)?.error ?? null,
  };
}
