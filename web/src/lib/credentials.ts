// Secrets and service tokens as the console reads and changes them.
//
// A secret's values are write-only (docs/decisions/secrets.md): the console
// sends them once, in a create-only ApplySecret, and never reads them back.
// Pages show what the status says without them (keys, registries) and of
// the spec only its type; a value field an answer held anyway is dropped
// as the answer arrives, before the query cache keeps it. The values go to
// the call through a ref the form fills as it submits, which the call
// empties as it reads it and the form empties as it closes; the mutation
// holds the name alone and is dropped once the form is gone, so nothing
// the query client keeps reaches the values, and no replay can send them.
//
// A service token's secret is returned once, by ServiceTokenIssuerService,
// when it creates or rotates the token (docs/decisions/authentication.md).
// It goes straight to the dialog that asked for it: the mutation's result
// holds none, and nothing else keeps it.

import { clone, create } from "@bufbuild/protobuf";
import { DurationSchema, type Timestamp } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createQueryOptions, useQuery, useTransport } from "@connectrpc/connect-query";
import { useMutation, useQuery as useTanstackQuery, useQueryClient, type QueryFunctionContext } from "@tanstack/react-query";
import type { RefObject } from "react";
import { ContainerService, type Container } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { ServiceTokenIssuerService } from "@hangar/api/hangar/iam/v1alpha1/service_token_issuer_pb";
import { ServiceTokenService, type ServiceToken } from "@hangar/api/hangar/iam/v1alpha1/service_token_pb";
import { RoleBindingService, type RoleBinding } from "@hangar/api/hangar/iam/v1alpha1/role_binding_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import {
  ListSecretsResponseSchema,
  SecretSchema,
  SecretService,
  SecretSpecSchema,
  SecretType,
  type ListSecretsResponse,
  type Secret,
} from "@hangar/api/hangar/secrets/v1alpha1/secret_pb";
import { createError } from "@/lib/create";
import { date, refetchInterval, type ListOpts } from "@/lib/machines";
import type { Listing } from "@/lib/storage";

/** The permissions the pages' controls need, as the authz tables name them. */
export const perms = {
  secretApply: "secrets.secrets.apply",
  secretDelete: "secrets.secrets.delete",
  tokenApply: "iam.servicetokens.apply",
  tokenDelete: "iam.servicetokens.delete",
} as const;

/** The key a registry-auth secret holds its Docker config under. */
export const dockerConfigKey = ".dockerconfigjson";

export const secretTypes: Record<number, { label: string }> = {
  [SecretType.REGISTRY_AUTH]: { label: "Registry login" },
};

export function secretTypeLabel(s: Secret): string {
  return secretTypes[s.spec?.type ?? 0]?.label ?? "Unknown";
}

/**
 * withoutValues is a secret as pages may hold it: its spec's type alone.
 * The API answers no values; this holds even if one did.
 */
export function withoutValues(s: Secret): Secret {
  const out = clone(SecretSchema, s);
  if (out.spec) out.spec = create(SecretSpecSchema, { type: s.spec?.type });
  return out;
}

/** secretFault is why the apiserver cannot open a secret: its Ready condition, when False. */
export function secretFault(s: Secret): { reason: string; message: string } | undefined {
  const c = s.status?.conditions.find((x) => x.type === "Ready");
  return c?.status === ConditionStatus.FALSE ? { reason: c.reason, message: c.message } : undefined;
}

/** The principal a token authenticates as, which RoleBindings name it by. */
export function tokenPrincipal(t: ServiceToken): string {
  return `serviceToken:${t.metadata?.project}/${t.metadata?.name}`;
}

export type TokenTone = "ok" | "pending" | "fail" | "off";

/** expiringWithin is how close to its end a secret is said to be expiring. */
const expiringWithin = 14 * 86400_000;

/** tokenState says whether a token's secret works, from its status alone. */
export function tokenState(t: ServiceToken, now = Date.now()): { label: string; tone: TokenTone } {
  if (t.metadata?.deleteTime) return { label: "Deleting", tone: "off" };
  if (!t.status?.secretPrefix) return { label: "No secret", tone: "off" };
  const end = date(t.status.expireTime)?.getTime();
  if (end !== undefined && end <= now) return { label: "Expired", tone: "fail" };
  if (end !== undefined && end - now <= expiringWithin) return { label: "Expiring", tone: "pending" };
  return { label: "Active", tone: "ok" };
}

/** day writes a timestamp as its UTC date, "2026-10-18". */
export function day(ts: Timestamp | undefined): string {
  return date(ts)?.toISOString().slice(0, 10) ?? "";
}

/** In how long, or how long ago, a time is: "in 9d", "3d ago". */
export function relative(ts: Timestamp | undefined, now = Date.now()): string {
  const d = date(ts);
  if (!d) return "";
  const s = Math.round((d.getTime() - now) / 1000);
  const a = Math.abs(s);
  const n = a < 3600 ? `${Math.max(1, Math.floor(a / 60))}m` : a < 86400 ? `${Math.floor(a / 3600)}h` : `${Math.floor(a / 86400)}d`;
  return s >= 0 ? `in ${n}` : `${n} ago`;
}

/**
 * The lengths a secret may be issued for, within the issuer's bounds (an
 * hour to three years), as the CLI's --duration writes them. "" is unset,
 * which the issuer takes as a year.
 */
export const durations = [
  { id: "720h", label: "30 days" },
  { id: "2160h", label: "90 days" },
  { id: "", label: "1 year" },
  { id: "26280h", label: "3 years" },
] as const;

function toDuration(id: string) {
  const hours = Number.parseInt(id, 10);
  return Number.isFinite(hours) ? create(DurationSchema, { seconds: BigInt(hours * 3600) }) : undefined;
}

/**
 * The CLI lines beside each action. None ever holds a value or a secret:
 * only names and options. A form passes "" for a name it would not send,
 * which reads NAME, so no line offers a name the form refuses.
 */
export const commands = {
  createSecret: (project: string, name: string) =>
    `hangar create secret registry ${name || "NAME"} --create-only --from-docker-config FILE -p ${project}`,
  getSecret: (project: string, name: string) => `hangar get secret ${name} -p ${project}`,
  deleteSecret: (project: string, name: string) => `hangar delete secret ${name} -p ${project}`,
  createToken: (project: string, name: string, o: { role?: string; duration?: string; description?: string }) =>
    [
      `hangar token create ${name || "NAME"}`,
      o.role ? ` --role ${o.role}` : "",
      o.duration ? ` --duration ${o.duration}` : "",
      o.description ? ` --description ${shellQuote(o.description)}` : "",
      ` -p ${project}`,
    ].join(""),
  rotateToken: (project: string, name: string, duration?: string) =>
    `hangar token rotate ${name}${duration ? ` --duration ${duration}` : ""} -p ${project}`,
  getToken: (project: string, name: string) => `hangar token get ${name} -p ${project}`,
  deleteToken: (project: string, name: string) => `hangar token delete ${name} -p ${project}`,
};

function shellQuote(s: string): string {
  return /^[A-Za-z0-9_./:-]+$/.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`;
}

/** What the New secret form holds: a registry login, or a Docker config as docker login writes it. */
export type SecretValues = { mode: "login"; registry: string; username: string; password: string } | { mode: "config"; config: string };

/** dockerConfig is the .dockerconfigjson the values make. A pasted config is sent as given. */
export function dockerConfig(v: SecretValues): Uint8Array {
  const text =
    v.mode === "config" ? v.config : JSON.stringify({ auths: { [v.registry.trim()]: { username: v.username, password: v.password } } });
  return new TextEncoder().encode(text);
}

// The API's largest page: a project holds far fewer of these than this.
const pageSize = 1000;

const byName = (a: { metadata?: { name: string } }, b: { metadata?: { name: string } }) =>
  (a.metadata?.name ?? "").localeCompare(b.metadata?.name ?? "");

/**
 * A list as a page reads it, and whether it has loaded at least once: a
 * later failure leaves the last answer, which a page keeps showing.
 */
export type Loaded<T> = Listing<T> & { loaded: boolean };

/** withoutValuesAll is a ListSecrets answer with every secret's value fields dropped. */
export function withoutValuesAll(res: ListSecretsResponse): ListSecretsResponse {
  const out = clone(ListSecretsResponseSchema, res);
  out.items = out.items.map(withoutValues);
  return out;
}

/**
 * useSecrets lists the project's secrets, each with its type and status
 * alone: value fields are dropped in the query function, so the query
 * cache never holds one.
 */
export function useSecrets(project: string, opts: ListOpts = {}): Loaded<Secret> {
  const transport = useTransport();
  const base = createQueryOptions(SecretService.method.listSecrets, { project, pageSize }, { transport });
  const q = useTanstackQuery({
    ...base,
    queryFn: async (ctx: QueryFunctionContext<typeof base.queryKey>) => withoutValuesAll(await base.queryFn(ctx)),
    refetchInterval,
    ...opts,
  });
  return {
    items: [...(q.data?.items ?? [])].sort(byName),
    loading: q.isPending,
    loaded: q.data !== undefined,
    error: q.error,
    refetch: () => void q.refetch(),
  };
}

/** Which containers name each secret in spec.imagePullSecrets, by secret. */
export type SecretUse = Record<string, string[]>;

/** useSecretUse finds, from the project's containers, which of them name each secret. */
export function useSecretUse(project: string): { use: SecretUse; loading: boolean; error: unknown } {
  const q = useQuery(ContainerService.method.listContainers, { project, pageSize }, { refetchInterval });
  return { use: secretUse(q.data?.items ?? []), loading: q.isPending, error: q.error };
}

/** secretUse is, by secret, the containers whose spec.imagePullSecrets name it. */
export function secretUse(containers: Container[]): SecretUse {
  const use: SecretUse = {};
  for (const c of containers) {
    for (const s of c.spec?.imagePullSecrets ?? []) (use[s] ??= []).push(c.metadata?.name ?? "");
  }
  for (const names of Object.values(use)) names.sort();
  return use;
}

/** useServiceTokens lists the project's service tokens. */
export function useServiceTokens(project: string, opts: ListOpts = {}): Loaded<ServiceToken> {
  const q = useQuery(ServiceTokenService.method.listServiceTokens, { project, pageSize }, { refetchInterval, ...opts });
  return {
    items: [...(q.data?.items ?? [])].sort(byName),
    loading: q.isPending,
    loaded: q.data !== undefined,
    error: q.error,
    refetch: () => void q.refetch(),
  };
}

/**
 * useTokenRoles is the roles each of the project's tokens holds in it, by
 * token name: the RoleBindings in the project whose subject is the token.
 */
export function useTokenRoles(project: string, opts: ListOpts = {}): { roles: Record<string, string[]>; loading: boolean; error: unknown } {
  const q = useQuery(RoleBindingService.method.listRoleBindings, { pageSize }, { refetchInterval, ...opts });
  return { roles: tokenRoles(q.data?.items ?? [], project), loading: q.isPending, error: q.error };
}

/**
 * tokenRoles is, by token name, the roles bindings grant the project's
 * tokens in the project. Bindings for people, for another project's
 * tokens, or at another scope are not the page's.
 */
export function tokenRoles(bindings: RoleBinding[], project: string): Record<string, string[]> {
  const roles: Record<string, string[]> = {};
  const prefix = `${project}/`;
  for (const b of bindings) {
    const subject = b.spec?.subject?.subject;
    const scope = b.spec?.scope?.target;
    if (subject?.case !== "serviceToken" || !subject.value.startsWith(prefix)) continue;
    if (scope?.case !== "project" || scope.value !== project) continue;
    (roles[subject.value.slice(prefix.length)] ??= []).push(b.spec!.role);
  }
  for (const r of Object.values(roles)) r.sort();
  return roles;
}

/** The New secret form's values, between its submit and the call that sends them. */
export type ValuesRef = RefObject<SecretValues | null>;

/** errNoValues refuses a create whose values are gone: a replay after the form closed sends nothing. */
export const errNoValues = "The values are no longer held; nothing is sent.";

/**
 * useCreateSecret creates a registry-auth secret with a create-only apply,
 * which the API refuses for a name that exists (lib/create.ts). The values
 * are taken from valuesRef, which the call empties as it reads it; the
 * mutation's variables are the name alone, its result is nothing, and
 * with gcTime 0 it leaves the cache once its form is gone.
 */
export function useCreateSecret(project: string, valuesRef: ValuesRef) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    gcTime: 0,
    mutationFn: async (name: string) => {
      const v = valuesRef.current;
      valuesRef.current = null;
      if (!v) throw new Error(errNoValues);
      const secret = create(SecretSchema, {
        metadata: { project, name },
        spec: { type: SecretType.REGISTRY_AUTH, data: { [dockerConfigKey]: dockerConfig(v) } },
      });
      try {
        await createClient(SecretService, transport).applySecret({ secret, createOnly: true });
      } catch (err) {
        throw createError(err, "secret", name);
      }
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** useDeleteSecret deletes a secret. */
export function useDeleteSecret(project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      await createClient(SecretService, transport).deleteSecret({ project, name });
    },
    // Not awaited: the page that deleted the object goes on (to the list)
    // before the refetch finds it gone and says so.
    onSettled: () => void queryClient.invalidateQueries(),
  });
}

/** What a create or a rotate asks for; the secret is the issuer's answer, never an argument. */
export type IssueRequest = { name: string; description?: string; role?: string; duration?: string; rotate?: boolean };

/**
 * useIssueToken creates a token with its first secret (and a binding for a
 * role, if one is asked for), or rotates one. The secret the issuer
 * returns goes to onSecret, the dialog's own state, and the mutation's
 * result holds none, so the query client never keeps it.
 */
export function useIssueToken(project: string, onSecret: (secret: string) => void) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    // Gone from the cache once its dialog is, so nothing can replay an issue.
    gcTime: 0,
    mutationFn: async (r: IssueRequest): Promise<void> => {
      const c = createClient(ServiceTokenIssuerService, transport);
      const duration = toDuration(r.duration ?? "");
      if (r.rotate) {
        const res = await c.rotateServiceToken({ project, name: r.name, duration });
        onSecret(res.secret);
        return;
      }
      try {
        const res = await c.createServiceToken({ project, name: r.name, description: r.description ?? "", role: r.role ?? "", duration });
        onSecret(res.secret);
      } catch (err) {
        throw createError(err, "service token", r.name);
      }
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/** useDeleteToken deletes a token: its secret stops working at once, and its bindings go with it. */
export function useDeleteToken(project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      await createClient(ServiceTokenService, transport).deleteServiceToken({ project, name });
    },
    // Not awaited: the page that deleted the object goes on (to the list)
    // before the refetch finds it gone and says so.
    onSettled: () => void queryClient.invalidateQueries(),
  });
}
