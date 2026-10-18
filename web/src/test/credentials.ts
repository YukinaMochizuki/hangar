// What the Secrets and Service tokens pages' tests share: an in-memory API
// holding secrets, service tokens, the role bindings naming them and the
// containers naming secrets, issuing obviously fake token secrets, and
// answering which permissions the caller holds. Also a reader of
// everything the query client keeps, to assert that no value or secret
// survives a dialog.

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import type { QueryClient } from "@tanstack/react-query";
import { ContainerSchema, ContainerService, type Container } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { RoleBindingSchema, RoleBindingService, type RoleBinding } from "@hangar/api/hangar/iam/v1alpha1/role_binding_pb";
import {
  ServiceTokenIssuerService,
  type CreateServiceTokenRequest,
  type RotateServiceTokenRequest,
} from "@hangar/api/hangar/iam/v1alpha1/service_token_issuer_pb";
import { ServiceTokenSchema, ServiceTokenService, type DeleteServiceTokenRequest, type ServiceToken } from "@hangar/api/hangar/iam/v1alpha1/service_token_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { SecretSchema, SecretService, SecretType, type ApplySecretRequest, type DeleteSecretRequest, type Secret } from "@hangar/api/hangar/secrets/v1alpha1/secret_pb";
import { fakeApi } from "@/test/harness";

/** project is the project every test's objects are in. */
export const project = "demo";

const day = 86400_000;
const at = (offset: number) => timestampFromDate(new Date(Date.now() + offset));

/** secret is a Secret of the project as the API answers it: no values. */
export function secret(name: string, o: Partial<{ registries: string[]; ready: [ConditionStatus, string, string] | null }> = {}): Secret {
  const ready = o.ready === undefined ? ([ConditionStatus.TRUE, "Sealed", ""] as const) : o.ready;
  return create(SecretSchema, {
    metadata: { name, project, uid: `uid-${name}`, createTime: at(-3 * day) },
    spec: { type: SecretType.REGISTRY_AUTH },
    status: {
      keys: [".dockerconfigjson"],
      registries: o.registries ?? ["registry.example.com"],
      contentHmac: "hmac-sha256:EXAMPLEHMAC",
      conditions: ready ? [{ type: "Ready", status: ready[0], reason: ready[1], message: ready[2] }] : [],
    },
  });
}

/** token is a ServiceToken of the project; expires is days from now, and no prefix means no secret. */
export function token(name: string, o: Partial<{ prefix: string; expires: number; used: number; description: string }> = {}): ServiceToken {
  const prefix = o.prefix ?? `hgr_${name.slice(0, 3)}PREFIX`;
  return create(ServiceTokenSchema, {
    metadata: { name, project, uid: `uid-${name}`, createTime: at(-30 * day) },
    spec: { description: o.description ?? "" },
    status: {
      secretPrefix: prefix,
      issueTime: prefix ? at(-10 * day) : undefined,
      expireTime: prefix ? at((o.expires ?? 300) * day) : undefined,
      lastUseTime: o.used !== undefined ? at(-o.used * day) : undefined,
    },
  });
}

/** binding grants a role to a subject at a project. */
export function binding(subject: { serviceToken: string } | { user: string }, role: string, at = project): RoleBinding {
  const s = "serviceToken" in subject ? { case: "serviceToken" as const, value: subject.serviceToken } : { case: "user" as const, value: subject.user };
  return create(RoleBindingSchema, {
    metadata: { name: `rb-${s.value.replace(/[^a-z0-9]/g, "-")}-${role}` },
    spec: { subject: { subject: s }, role, scope: { target: { case: "project", value: at } } },
  });
}

/** container is a Container of the project naming pull secrets. */
export function container(name: string, pullSecrets: string[]): Container {
  return create(ContainerSchema, { metadata: { name, project }, spec: { image: `registry.example.com/${name}:1`, imagePullSecrets: pullSecrets } });
}

/** The fake secrets the issuer answers: hgr_ and 43 more characters, obviously not real. */
export const issued = ["hgr_EXAMPLEfirstEXAMPLEfirstEXAMPLEfirst00000", "hgr_EXAMPLEsecondEXAMPLEsecondEXAMPLEsecond0"];

export type Fake = {
  secrets?: () => Promise<{ items: Secret[] }>;
  tokens?: () => Promise<{ items: ServiceToken[] }>;
  bindings?: () => Promise<{ items: RoleBinding[] }>;
  containers?: () => Promise<{ items: Container[] }>;
  /** What TestPermissions answers the caller holds; unset, every permission asked. */
  perms?: () => Promise<string[]>;
  applySecret?: (req: ApplySecretRequest) => void;
  deleteSecret?: (req: DeleteSecretRequest) => void;
  createToken?: (req: CreateServiceTokenRequest) => void;
  rotateToken?: (req: RotateServiceTokenRequest) => void;
  deleteToken?: (req: DeleteServiceTokenRequest) => void;
};

/**
 * credentialsApi is an in-memory API for the credentials pages. A
 * create-only apply, or a create, of a name its list holds is refused with
 * AlreadyExists, as the API does; each issue answers the next of issued.
 */
export function credentialsApi(f: Fake) {
  let n = 0;
  const names = async (list?: () => Promise<{ items: { metadata?: { name: string } }[] }>) =>
    (list ? (await list()).items : []).map((i) => i.metadata?.name ?? "");
  return fakeApi((r) => {
    r.service(SecretService, {
      listSecrets: () => (f.secrets ? f.secrets() : { items: [] }),
      applySecret: async (req) => {
        if (req.createOnly && (await names(f.secrets)).includes(req.secret?.metadata?.name ?? "")) {
          throw new ConnectError(`secret ${req.secret?.metadata?.name} already exists`, Code.AlreadyExists);
        }
        f.applySecret?.(req);
        // As the API answers: the values left out.
        return { secret: create(SecretSchema, { metadata: req.secret?.metadata, spec: { type: req.secret?.spec?.type } }), created: true };
      },
      deleteSecret: (req) => {
        f.deleteSecret?.(req);
        return {};
      },
    });
    r.service(ServiceTokenService, {
      listServiceTokens: () => (f.tokens ? f.tokens() : { items: [] }),
      deleteServiceToken: (req) => {
        f.deleteToken?.(req);
        return {};
      },
    });
    r.service(ServiceTokenIssuerService, {
      createServiceToken: async (req) => {
        if ((await names(f.tokens)).includes(req.name)) throw new ConnectError(`servicetoken ${req.name} already exists`, Code.AlreadyExists);
        f.createToken?.(req);
        return { serviceToken: token(req.name), secret: issued[n++ % issued.length] };
      },
      rotateServiceToken: (req) => {
        f.rotateToken?.(req);
        return { serviceToken: token(req.name), secret: issued[n++ % issued.length] };
      },
    });
    r.service(RoleBindingService, { listRoleBindings: () => (f.bindings ? f.bindings() : { items: [] }) });
    r.service(ContainerService, { listContainers: () => (f.containers ? f.containers() : { items: [] }) });
    r.service(AccountService, {
      testPermissions: async (req) => {
        const held = f.perms ? await f.perms() : req.permissions;
        return { permissions: req.permissions.filter((p) => held.includes(p)) };
      },
    });
  });
}

/**
 * text is every string, number and byte field reachable from v through own
 * properties, joined: a value hidden on a boxed string, in a Map or a Set,
 * or behind a cycle is reached too. Bytes are decoded as text.
 */
function text(v: unknown): string {
  const out: string[] = [];
  const seen = new Set<unknown>();
  const walk = (x: unknown) => {
    if (x === null || x === undefined || typeof x === "function" || typeof x === "symbol") return;
    if (typeof x !== "object") return void out.push(String(x));
    if (seen.has(x)) return;
    seen.add(x);
    if (x instanceof Uint8Array) return void out.push(new TextDecoder().decode(x));
    if (x instanceof String || x instanceof Number || x instanceof Error) out.push(String(x));
    if (x instanceof Map) {
      for (const [k, val] of x) {
        walk(k);
        walk(val);
      }
    }
    if (x instanceof Set) for (const val of x) walk(val);
    for (const k of Object.keys(x)) walk((x as Record<string, unknown>)[k]);
  };
  walk(v);
  return out.join("\n");
}

/**
 * cacheText is everything the query client keeps that can be read as data,
 * as text: every query's key, data and error, and every mutation's options,
 * variables, result, context and error, through every own property. Byte
 * fields are decoded, so a value sent as bytes shows as text. What a
 * function closes over cannot be read this way; the tests replay a kept
 * mutation to show it sends nothing.
 */
export function cacheText(client: QueryClient): string {
  const queries = client.getQueryCache().getAll().map((q) => [q.queryKey, q.state.data, q.state.error]);
  const mutations = client
    .getMutationCache()
    .getAll()
    .map((m) => [m.options, m.state.variables, m.state.data, m.state.context, m.state.error]);
  return text([queries, mutations]);
}
