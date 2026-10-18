// A project's secrets: what each holds a login for, never the login. Values
// are write-only (docs/decisions/secrets.md): the create sends them, and
// nothing reads them back, so these pages show names, keys, registries,
// types and times alone, whatever the API answers.

import { Link, useNavigate } from "@tanstack/react-router";
import type { Secret } from "@hangar/api/hangar/secrets/v1alpha1/secret_pb";
import { KeyRound, Plus, Trash2 } from "lucide-react";
import { useRef, useState } from "react";
import { CommandLine, FormLabel, inputClass, StaleBanner } from "@/components/credentials";
import { Dialog } from "@/components/dialog";
import { Notice, PageBanner } from "@/components/notice";
import { DetailSkeleton, ListFrame, PageHead } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, Skeleton } from "@/components/states";
import { tbodyRow, Th, theadRow } from "@/components/table";
import { ConditionsCard, EquivalentCard } from "@/components/storage";
import { Badge, Button, Card, CardHeader, Field, Led } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { checkName } from "@/lib/create";
import {
  commands,
  day,
  dockerConfigKey,
  perms,
  secretFault,
  secretTypeLabel,
  useCreateSecret,
  useDeleteSecret,
  useSecretUse,
  useSecrets,
  type SecretValues,
} from "@/lib/credentials";
import { useCan } from "@/lib/permissions";
import { useCreating, type NewProps, type OnCreated } from "@/lib/new-param";

function SecretState({ s }: { s: Secret }) {
  const fault = secretFault(s);
  const ready = s.status?.conditions.find((c) => c.type === "Ready");
  const [tone, label] = s.metadata?.deleteTime
    ? (["off", "Deleting"] as const)
    : fault
      ? (["fail", "Cannot be opened"] as const)
      : ready
        ? (["ok", "Ready"] as const)
        : (["pending", "Not checked yet"] as const);
  return (
    <>
      <span className="inline-flex items-center gap-2">
        <Led tone={tone} />
        {label}
      </span>
      {fault && <span className="block text-[11.5px] text-fail">{fault.reason}</span>}
    </>
  );
}

function Registries({ s }: { s: Secret }) {
  const r = s.status?.registries ?? [];
  if (r.length === 0) return <span className="text-subtle">none</span>;
  return <span className="font-mono text-[12px]">{r.join(", ")}</span>;
}

/** What the page knows of the containers naming a secret: they may still load, or have failed to. */
type Use = { names: string[] | undefined; loading: boolean; error: unknown };

function UsedBy({ use: { names, loading, error }, project }: { use: Use; project: string }) {
  if (loading) return <Skeleton className="h-3 w-16" />;
  if (error) return <span className="text-subtle">not known: the containers did not load</span>;
  if (!names?.length) return <span className="text-subtle">none</span>;
  return (
    <span>
      {names.map((n, i) => (
        <span key={n}>
          {i > 0 && <span className="text-muted">, </span>}
          <Link to="/projects/$project/containers/$name" params={{ project, name: n }} className="font-mono text-[12.5px] text-accent hover:underline">
            {n}
          </Link>
        </span>
      ))}
    </span>
  );
}

/**
 * NewSecretDialog takes a registry login and sends it in one create-only
 * ApplySecret. The values live in this form's state, and in a ref from its
 * submit until the call reads it; closing the form empties the ref and
 * drops the state.
 */
function NewSecretDialog({ open, onClose, onCreated, project, names }: { open: boolean; onClose: () => void; onCreated: OnCreated; project: string; names: { loading: boolean; error: unknown; names: string[] } }) {
  const [name, setName] = useState("");
  const [mode, setMode] = useState<"login" | "config">("login");
  const [registry, setRegistry] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [config, setConfig] = useState("");
  const valuesRef = useRef<SecretValues | null>(null);
  const create = useCreateSecret(project, valuesRef);
  const navigate = useNavigate();
  const nameCheck = checkName(name, "secret", names);
  const filled = mode === "login" ? !!registry && !!username && !!password : !!config.trim();
  const problem = (name !== "" || names.loading || !!names.error) && !nameCheck.ok ? nameCheck.reason : undefined;
  const close = () => {
    valuesRef.current = null;
    create.reset();
    onClose();
  };
  return (
    <Dialog open={open} onClose={close} title="New secret">
      <form
        autoComplete="off"
        onSubmit={(e) => {
          e.preventDefault();
          if (!nameCheck.ok || !filled) return;
          valuesRef.current = mode === "login" ? { mode, registry, username, password } : { mode, config };
          create.mutate(
            name,
            {
              onSuccess: () => {
                valuesRef.current = null;
                onCreated(() => void navigate({ to: "/projects/$project/secrets/$name", params: { project, name } }));
              },
            },
          );
        }}
      >
        <div className="text-foreground">
          <FormLabel title="Name">
            <input value={name} onChange={(e) => setName(e.target.value)} spellCheck={false} placeholder="registry-login" className={inputClass} />
          </FormLabel>
          <FormLabel title="Type">
            <span className="mt-1.5 flex h-9 items-center rounded-md border bg-surface-2 px-2.5 text-[13px] text-muted">Registry login</span>
          </FormLabel>
          <div role="radiogroup" aria-label="Values from" className="mt-4 flex h-8 w-fit items-center rounded-md border bg-surface p-0.5">
            {(
              [
                ["login", "Registry and password"],
                ["config", "Docker config"],
              ] as const
            ).map(([id, label]) => (
              <button
                key={id}
                type="button"
                role="radio"
                aria-checked={mode === id}
                onClick={() => setMode(id)}
                className={cn("flex h-full items-center rounded px-2.5 text-[12.5px]", mode === id ? "bg-surface-2 font-medium text-foreground" : "text-muted hover:text-foreground")}
              >
                {label}
              </button>
            ))}
          </div>
          {mode === "login" ? (
            <>
              <FormLabel title="Registry" hint="Its host. Docker Hub is index.docker.io.">
                <input value={registry} onChange={(e) => setRegistry(e.target.value)} spellCheck={false} placeholder="registry.example.com" className={inputClass} />
              </FormLabel>
              <FormLabel title="Username">
                <input value={username} onChange={(e) => setUsername(e.target.value)} spellCheck={false} autoComplete="off" className={inputClass} />
              </FormLabel>
              <FormLabel title="Password or access token">
                {/* Not a password for this site: "off" asks the browser neither to suggest a generated one nor to offer saving it. */}
                <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="off" className={inputClass} />
              </FormLabel>
            </>
          ) : (
            <FormLabel title="Docker config" hint={`As docker login writes it, ~/.docker/config.json. It is stored as ${dockerConfigKey}.`}>
              <textarea
                value={config}
                onChange={(e) => setConfig(e.target.value)}
                spellCheck={false}
                rows={5}
                placeholder={'{"auths": {"registry.example.com": {"auth": "…"}}}'}
                className={cn(inputClass, "h-auto py-2 text-[12px]")}
              />
            </FormLabel>
          )}
        </div>
        <p className="mt-4 text-[12px]">The values are write-only: once created, no page and no answer of the API holds them.</p>
        <CommandLine command={commands.createSecret(project, nameCheck.ok ? name : "")} />
        {problem && <Notice tone="warning" title={problem} className="mt-3" />}
        {create.error && (
          <Notice tone="error" title="Creating the secret failed" className="mt-3">
            {errorMessage(create.error)}
          </Notice>
        )}
        <div className="mt-5 flex justify-end gap-2">
          <Button type="button" onClick={close}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={!nameCheck.ok || !filled || create.isPending}>
            <Plus /> {create.isPending ? "Creating…" : "Create secret"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function DeleteSecretDialog({ s, use, open, onClose, after }: { s: Secret; use: Use; open: boolean; onClose: () => void; after?: () => void }) {
  const [typed, setTyped] = useState("");
  const name = s.metadata?.name ?? "";
  const project = s.metadata?.project ?? "";
  const del = useDeleteSecret(project, name);
  const usedBy = use.names ?? [];
  const close = () => {
    del.reset();
    setTyped("");
    onClose();
  };
  return (
    <Dialog open={open} onClose={close} title={`Delete secret ${name}`}>
      {use.loading ? (
        <p>The project's containers are still loading, so which of them name it is not known yet.</p>
      ) : use.error ? (
        <p>The project's containers did not load, so which of them name it is not known.</p>
      ) : usedBy.length > 0 ? (
        <p>
          {usedBy.length === 1 ? "1 container names it" : `${usedBy.length} containers name it`}: {usedBy.join(", ")}. Running ones keep running; one that
          pulls its image again without it is Ready=False ImagePullSecretNotFound.
        </p>
      ) : (
        <p>No container names it.</p>
      )}
      <label className="mt-4 block">
        <span className="text-[12.5px] text-foreground">
          Name, to confirm: <span className="font-mono">{name}</span>
        </span>
        <input
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          autoFocus
          spellCheck={false}
          className="mt-1.5 h-9 w-full rounded-md border border-border-strong bg-background px-2.5 font-mono text-[13px] text-foreground outline-none focus:border-fail"
        />
      </label>
      <CommandLine command={commands.deleteSecret(project, name)} />
      {del.error && (
        <Notice tone="error" title="Deleting failed" className="mt-3">
          {errorMessage(del.error)}
        </Notice>
      )}
      <div className="mt-5 flex justify-end gap-2">
        <Button onClick={close}>Cancel</Button>
        <Button
          variant="danger"
          disabled={typed !== name || del.isPending}
          onClick={() =>
            del.mutate(undefined, {
              onSuccess: () => {
                close();
                after?.();
              },
            })
          }
        >
          <Trash2 /> {del.isPending ? "Deleting…" : "Delete"}
        </Button>
      </div>
    </Dialog>
  );
}

/** SecretList is the project's secrets, with New secret. */
export function SecretList(props: NewProps) {
  const project = useProjectName();
  const { items, loading, loaded, error, refetch } = useSecrets(project);
  const use = useSecretUse(project);
  const createPerm = useCan({ level: "project", name: project }, [perms.secretApply]);
  const canCreate = createPerm.can(perms.secretApply);
  const [creating, setCreating, created] = useCreating(props, canCreate, createPerm.settled);
  const newButton = (
    <Button variant="primary" onClick={() => setCreating(true)}>
      <Plus /> New secret
    </Button>
  );
  return (
    <>
      <Topbar crumbs={["Secrets"]} />
      {loaded && error ? <StaleBanner what="secrets" error={error} onRetry={refetch} /> : null}
      <PageHead
        title="Secrets"
        description="Registry logins the platform pulls this project's images with. Their values are write-only."
        action={canCreate && newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ loading, error: loaded ? undefined : error, refetch, count: items.length }}
          noun="secrets"
          empty={{ title: "No secrets", text: "A container names a secret to pull a private image with it.", action: canCreate ? newButton : undefined }}
        >
          <Card className="md:hidden">
            <ul className="divide-y">
              {items.map((s) => {
                const name = s.metadata?.name ?? "";
                return (
                  <li key={name} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                    <Link to="/projects/$project/secrets/$name" params={{ project, name }} className="min-w-0 flex-1">
                      <span className="block truncate font-medium">{name}</span>
                      <span className="block truncate font-mono text-[11.5px] text-subtle">{(s.status?.registries ?? []).join(", ")}</span>
                    </Link>
                    <span className="shrink-0 text-right">
                      <SecretState s={s} />
                    </span>
                  </li>
                );
              })}
            </ul>
          </Card>
          <Card className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[820px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[24%]">Name</Th>
                  <Th>State</Th>
                  <Th>Registries</Th>
                  <Th>Keys</Th>
                  <Th>Used by</Th>
                  <Th>Created</Th>
                </tr>
              </thead>
              <tbody>
                {items.map((s) => {
                  const name = s.metadata?.name ?? "";
                  return (
                    <tr key={name} className={tbodyRow}>
                      <td className="max-w-0 px-4 py-2.5">
                        <Link to="/projects/$project/secrets/$name" params={{ project, name }} className="block">
                          <span className="block truncate font-medium group-hover:text-accent">{name}</span>
                        </Link>
                        <span className="block truncate text-[11.5px] text-subtle">{secretTypeLabel(s)}</span>
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5">
                        <SecretState s={s} />
                      </td>
                      <td className="px-4 py-2.5">
                        <Registries s={s} />
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px] text-muted">{(s.status?.keys ?? []).join(", ")}</td>
                      <td className="px-4 py-2.5">
                        <UsedBy use={{ ...use, names: use.use[name] }} project={project} />
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 text-muted">{day(s.metadata?.createTime)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </Card>
        </ListFrame>
      </div>
      {(canCreate || creating) && (
        <NewSecretDialog
          key={String(creating)}
          open={creating}
          onClose={() => setCreating(false)}
          onCreated={created}
          project={project}
          names={{ loading, error, names: items.map((s) => s.metadata?.name ?? "") }}
        />
      )}
    </>
  );
}

/** SecretDetail is one secret's page: what it holds a login for, with Delete. */
export function SecretDetail({ name }: { name: string }) {
  const project = useProjectName();
  const { items, loading, loaded, error, refetch } = useSecrets(project);
  const secretUse = useSecretUse(project);
  const navigate = useNavigate();
  const [deleting, setDeleting] = useState(false);
  const canDelete = useCan({ level: "project", name: project }, [perms.secretDelete]).can(perms.secretDelete);
  const s = items.find((x) => x.metadata?.name === name);
  const crumbs = [
    <Link key="l" to="/projects/$project/secrets" params={{ project }}>
      Secrets
    </Link>,
    name,
  ];
  if (loading) {
    return (
      <>
        <Topbar crumbs={crumbs} />
        <DetailSkeleton />
      </>
    );
  }
  if (!s) {
    return (
      <>
        <Topbar crumbs={crumbs} />
        {error ? (
          <div className="px-4 py-6 md:px-8">
            <Card>
              <ErrorNotice error={error} title="The secret did not load" onRetry={refetch} />
            </Card>
          </div>
        ) : (
          <EmptyState title="No such secret">
            This project has no secret named <span className="font-mono">{name}</span>.
          </EmptyState>
        )}
      </>
    );
  }
  const fault = secretFault(s);
  const use: Use = { names: secretUse.use[name], loading: secretUse.loading, error: secretUse.error };
  const usedBy = use.names ?? [];
  return (
    <>
      <Topbar crumbs={crumbs} />
      {loaded && error ? <StaleBanner what="secrets" error={error} onRetry={refetch} /> : null}
      {fault && (
        <PageBanner tone="error" title={`${name} cannot be opened: ${fault.reason}`}>
          {fault.message}
        </PageBanner>
      )}
      <div className="border-b px-4 pb-5 pt-6 md:px-8">
        <div className="flex flex-col gap-4 md:flex-row md:items-start md:justify-between md:gap-6">
          <div className="min-w-0">
            <div className="flex items-center gap-2.5">
              <h1 className="truncate text-[22px] font-semibold tracking-tight">{name}</h1>
              <Badge>Secret</Badge>
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-1 text-[13px] text-muted">
              <span>
                <SecretState s={s} />
              </span>
              <span>{secretTypeLabel(s)}</span>
              {!use.loading && !use.error && (
                <span>
                  {usedBy.length === 0 ? "Named by no container" : usedBy.length === 1 ? "Named by 1 container" : `Named by ${usedBy.length} containers`}
                </span>
              )}
            </div>
          </div>
          {canDelete && (
            <Button variant="danger" onClick={() => setDeleting(true)} disabled={!!s.metadata?.deleteTime}>
              <Trash2 /> Delete
            </Button>
          )}
        </div>
      </div>
      <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
        <div className="min-w-0 space-y-5">
          <Card>
            <CardHeader title="Summary" />
            <dl className="divide-y px-4">
              <Field label="Type">{secretTypeLabel(s)}</Field>
              <Field label="Registries">
                <Registries s={s} />
              </Field>
              <Field label="Keys" mono>
                {(s.status?.keys ?? []).join(", ")}
              </Field>
              <Field label="Values">
                <span className="inline-flex items-center gap-1.5 text-muted">
                  <KeyRound className="size-3.5" /> Write-only; no answer of the API holds them.
                </span>
              </Field>
              <Field label="Used by">
                <UsedBy use={use} project={project} />
              </Field>
              <Field label="Created">{s.metadata?.createTime ? `${day(s.metadata.createTime)} UTC` : ""}</Field>
              <Field label="UID" mono>
                <span className="text-muted">{s.metadata?.uid}</span>
              </Field>
            </dl>
          </Card>
          <ConditionsCard conditions={s.status?.conditions ?? []} />
        </div>
        <div className="space-y-5">
          <EquivalentCard cli={[commands.getSecret(project, name), ...(canDelete ? [commands.deleteSecret(project, name)] : [])]} />
        </div>
      </div>
      {canDelete && (
        <DeleteSecretDialog
          s={s}
          use={use}
          open={deleting}
          onClose={() => setDeleting(false)}
          after={() => void navigate({ to: "/projects/$project/secrets", params: { project } })}
        />
      )}
    </>
  );
}
