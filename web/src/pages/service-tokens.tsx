// A project's service tokens: agents' credentials. A token's secret is
// returned once, by ServiceTokenIssuerService, when it creates or rotates the
// token (docs/decisions/authentication.md). The console shows it in the one
// dialog that asked for it, held in that dialog's state alone; closing the
// dialog drops it. Pages show the secret's first characters, which the
// status gives, to tell which secret someone holds.
//
// Only owners write tokens: editors read them (tenancy-and-iam.md), so for
// them these pages have no controls.

import { Link, useNavigate } from "@tanstack/react-router";
import type { ServiceToken } from "@hangar/api/hangar/iam/v1alpha1/service_token_pb";
import { Plus, RefreshCw, Trash2 } from "lucide-react";
import { useState } from "react";
import { CommandLine, FormLabel, inputClass, StaleBanner } from "@/components/credentials";
import { Dialog } from "@/components/dialog";
import { Notice } from "@/components/notice";
import { DetailSkeleton, ListFrame, PageHead } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, Skeleton, SkeletonRows } from "@/components/states";
import { tbodyRow, Th, theadRow } from "@/components/table";
import { EquivalentCard } from "@/components/object";
import { Badge, Button, Card, CardHeader, CopyButton, Field, Led } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { checkName } from "@/lib/create";
import {
  commands,
  day,
  durations,
  perms,
  relative,
  tokenPrincipal,
  tokenState,
  useDeleteToken,
  useIssueToken,
  useServiceTokens,
  useTokenRoles,
} from "@/lib/credentials";
import { roleLabel, roles } from "@/lib/members";
import { useCan } from "@/lib/permissions";
import { useCreating, type NewProps } from "@/lib/new-param";

function TokenState({ t }: { t: ServiceToken }) {
  const s = tokenState(t);
  return (
    <span className="inline-flex items-center gap-2">
      <Led tone={s.tone} />
      {s.label}
    </span>
  );
}

/** The secret's first characters, which tell which secret someone holds. */
function Prefix({ t }: { t: ServiceToken }) {
  if (!t.status?.secretPrefix) return <span className="text-subtle">none</span>;
  return <span className="font-mono text-[12px]">{t.status.secretPrefix}…</span>;
}

function Expires({ t }: { t: ServiceToken }) {
  if (!t.status?.expireTime) return <span className="text-subtle">–</span>;
  const s = tokenState(t);
  return (
    <span className={cn("tabular", s.tone === "pending" && "text-pending", s.tone === "fail" && "text-fail")}>
      {day(t.status.expireTime)}
      <span className="block text-[11.5px] text-subtle">{relative(t.status.expireTime)}</span>
    </span>
  );
}

function LastUsed({ t }: { t: ServiceToken }) {
  if (!t.status?.lastUseTime) return <span className="text-subtle">never</span>;
  return <span className="text-muted">{relative(t.status.lastUseTime)}</span>;
}

/** What the page knows of the roles the project's tokens hold: they may still load, or have failed to. */
type Held = { roles: Record<string, string[]>; loading: boolean; error: unknown };

function Roles({ held, name }: { held: Held; name: string }) {
  if (held.loading) return <Skeleton className="h-3 w-14" />;
  if (held.error) return <span className="text-subtle">not known</span>;
  const names = held.roles[name];
  if (!names?.length) return <span className="text-subtle">none</span>;
  return (
    <span className="flex flex-wrap gap-1.5">
      {names.map((r) => (
        <span key={r} className="rounded bg-surface-2 px-1.5 py-0.5 text-[12px]" title={r}>
          {roleLabel(r)}
        </span>
      ))}
    </span>
  );
}

function DurationSelect({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <FormLabel title="Secret works for">
      <select value={value} onChange={(e) => onChange(e.target.value)} className={cn(inputClass, "w-40 font-sans")}>
        {durations.map((d) => (
          <option key={d.id} value={d.id}>
            {d.label}
          </option>
        ))}
      </select>
    </FormLabel>
  );
}

/**
 * SecretOnce shows a secret the issuer just returned. It is the only place
 * the secret is held, in the state of the dialog that asked for it. Its
 * Dialog is locked: only its Close button drops the secret, and a close
 * the browser makes on its own opens the element again (components/dialog.tsx).
 */
function SecretOnce({ title, secret, onClose }: { title: string; secret: string; onClose: () => void }) {
  return (
    <Dialog open onClose={onClose} title={title} locked>
      <p>This secret is shown once. Hangar keeps only its hash, so no page, command or answer of the API shows it again.</p>
      <div className="mt-4 flex items-center gap-2 rounded-md border border-border-strong bg-surface-2 py-2 pl-3 pr-1.5">
        <code aria-label="Secret" className="min-w-0 flex-1 break-all font-mono text-[12.5px] text-foreground">
          {secret}
        </code>
        <CopyButton text={secret} className="shrink-0" />
      </div>
      <p className="mt-3 text-[12px]">An agent sends it as HANGAR_TOKEN, or as Authorization: Bearer.</p>
      <div className="mt-5 flex justify-end">
        <Button variant="primary" onClick={onClose}>
          Close
        </Button>
      </div>
    </Dialog>
  );
}

/**
 * NewTokenDialog creates a token with its first secret, and a binding for a
 * role when one is chosen, in one CreateServiceToken, which refuses a name
 * that exists. The secret it returns is shown once, in SecretOnce.
 */
function NewTokenDialog({ open, onClose, project, names }: { open: boolean; onClose: () => void; project: string; names: { loading: boolean; error: unknown; names: string[] } }) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [role, setRole] = useState("");
  const [duration, setDuration] = useState("");
  const [secret, setSecret] = useState<string | null>(null);
  const issue = useIssueToken(project, setSecret);
  const nameCheck = checkName(name, "service token", names);
  const problem = (name !== "" || names.loading || !!names.error) && !nameCheck.ok ? nameCheck.reason : undefined;
  const close = () => {
    issue.reset();
    setSecret(null);
    onClose();
  };
  if (!open) return null;
  if (secret !== null) return <SecretOnce title={`Token ${name} created`} secret={secret} onClose={close} />;
  return (
    <Dialog open onClose={close} title="New service token">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (nameCheck.ok) issue.mutate({ name, description, role, duration });
        }}
      >
        <div className="text-foreground">
          <FormLabel title="Name" hint={`It authenticates as serviceToken:${project}/${nameCheck.ok ? name : "NAME"}.`}>
            <input value={name} onChange={(e) => setName(e.target.value)} spellCheck={false} placeholder="ci" className={inputClass} />
          </FormLabel>
          <FormLabel title="Description">
            <input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Deploys from the main branch" className={cn(inputClass, "font-sans")} />
          </FormLabel>
          <fieldset className="mt-4">
            <legend className="text-[12.5px] text-foreground">Role in this project</legend>
            <div className="mt-1.5 divide-y rounded-md border">
              {[{ id: "", label: "None", holds: "The token holds only what bindings made later grant." }, ...roles.project].map((r) => (
                <label key={r.id} className="flex cursor-pointer gap-2.5 px-3 py-2 hover:bg-surface-2/60">
                  <input type="radio" name="role" value={r.id} checked={role === r.id} onChange={() => setRole(r.id)} className="mt-0.5 accent-[var(--accent)]" />
                  <span className="min-w-0">
                    <span className="flex items-baseline gap-2 text-[13px] text-foreground">
                      {r.label}
                      {r.id && <span className="font-mono text-[11px] text-subtle">{r.id}</span>}
                    </span>
                    <span className="block text-[12px] text-muted">{r.holds}</span>
                  </span>
                </label>
              ))}
            </div>
          </fieldset>
          <DurationSelect value={duration} onChange={setDuration} />
        </div>
        <CommandLine command={commands.createToken(project, nameCheck.ok ? name : "", { role, duration, description })} />
        {problem && <Notice tone="warning" title={problem} className="mt-3" />}
        {issue.error && (
          <Notice tone="error" title="Creating the token failed" className="mt-3">
            {errorMessage(issue.error)}
          </Notice>
        )}
        <div className="mt-5 flex justify-end gap-2">
          <Button type="button" onClick={close}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={!nameCheck.ok || issue.isPending}>
            <Plus /> {issue.isPending ? "Creating…" : "Create token"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

/** RotateDialog issues a token a new secret; the one it had stops working at once. */
function RotateDialog({ t, open, onClose }: { t: ServiceToken; open: boolean; onClose: () => void }) {
  const name = t.metadata?.name ?? "";
  const project = t.metadata?.project ?? "";
  const [duration, setDuration] = useState("");
  const [secret, setSecret] = useState<string | null>(null);
  const issue = useIssueToken(project, setSecret);
  const close = () => {
    issue.reset();
    setSecret(null);
    onClose();
  };
  if (!open) return null;
  if (secret !== null) return <SecretOnce title={`New secret of ${name}`} secret={secret} onClose={close} />;
  return (
    <Dialog open onClose={close} title={`Rotate ${name}`}>
      {t.status?.secretPrefix ? (
        <p>
          The secret starting <span className="font-mono text-foreground">{t.status.secretPrefix}</span> stops working the moment the new one is stored.
        </p>
      ) : (
        <p>{name} has no secret yet; this issues its first.</p>
      )}
      <div className="mt-4 text-foreground">
        <DurationSelect value={duration} onChange={setDuration} />
      </div>
      <CommandLine command={commands.rotateToken(project, name, duration)} />
      {issue.error && (
        <Notice tone="error" title="Rotating failed" className="mt-3">
          {errorMessage(issue.error)}
        </Notice>
      )}
      <div className="mt-5 flex justify-end gap-2">
        <Button onClick={close}>Cancel</Button>
        <Button variant="primary" disabled={issue.isPending} onClick={() => issue.mutate({ name, duration, rotate: true })}>
          <RefreshCw /> {issue.isPending ? "Rotating…" : "Rotate secret"}
        </Button>
      </div>
    </Dialog>
  );
}

function DeleteTokenDialog({ t, held, open, onClose, after }: { t: ServiceToken; held: string[] | undefined; open: boolean; onClose: () => void; after?: () => void }) {
  const [typed, setTyped] = useState("");
  const name = t.metadata?.name ?? "";
  const project = t.metadata?.project ?? "";
  const del = useDeleteToken(project, name);
  const close = () => {
    del.reset();
    setTyped("");
    onClose();
  };
  return (
    <Dialog open={open} onClose={close} title={`Delete token ${name}`}>
      <p>
        Its secret stops working at once
        {held === undefined
          ? ", and every binding naming it is deleted with it."
          : held.length
            ? `, and the bindings that grant it ${held.map(roleLabel).join(" and ")} are deleted with it.`
            : "."}
      </p>
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
      <CommandLine command={commands.deleteToken(project, name)} />
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

/** ServiceTokenList is the project's tokens, with New token for owners. */
export function ServiceTokenList(props: NewProps) {
  const project = useProjectName();
  const { items, loading, loaded, error, refetch } = useServiceTokens(project);
  const held = useTokenRoles(project);
  const createPerm = useCan({ level: "project", name: project }, [perms.tokenApply]);
  const canCreate = createPerm.can(perms.tokenApply);
  const [creating, setCreating] = useCreating(props, canCreate, createPerm.settled);
  const newButton = (
    <Button variant="primary" onClick={() => setCreating(true)}>
      <Plus /> New token
    </Button>
  );
  return (
    <>
      <Topbar crumbs={["Service tokens"]} />
      {loaded && error ? <StaleBanner what="service tokens" error={error} onRetry={refetch} /> : null}
      <PageHead
        title="Service tokens"
        description="Credentials agents use. A token holds the roles bound to it here; its secret is shown once, when issued."
        action={canCreate && newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ loading, error: loaded ? undefined : error, refetch, count: items.length }}
          noun="service tokens"
          empty={{
            title: "No service tokens",
            text: "An agent authenticates with a token's secret, and holds what its roles grant.",
            action: canCreate ? newButton : undefined,
          }}
        >
          <Card className="md:hidden">
            <ul className="divide-y">
              {items.map((t) => {
                const name = t.metadata?.name ?? "";
                return (
                  <li key={name} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                    <Link to="/projects/$project/service-tokens/$name" params={{ project, name }} className="min-w-0 flex-1">
                      <span className="block truncate font-medium">{name}</span>
                      <span className="block truncate text-[11.5px] text-subtle">
                        {held.loading || held.error ? "" : (held.roles[name] ?? []).map(roleLabel).join(", ") || "No role"}
                        {t.status?.expireTime ? `${held.loading || held.error ? "" : " · "}expires ${day(t.status.expireTime)}` : ""}
                      </span>
                    </Link>
                    <span className="shrink-0 text-right">
                      <TokenState t={t} />
                    </span>
                  </li>
                );
              })}
            </ul>
          </Card>
          <Card className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[860px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[26%]">Name</Th>
                  <Th>State</Th>
                  <Th>Role</Th>
                  <Th>Secret</Th>
                  <Th>Expires</Th>
                  <Th>Last used</Th>
                </tr>
              </thead>
              <tbody>
                {items.map((t) => {
                  const name = t.metadata?.name ?? "";
                  return (
                    <tr key={name} className={tbodyRow}>
                      <td className="max-w-0 px-4 py-2.5">
                        <Link to="/projects/$project/service-tokens/$name" params={{ project, name }} className="block">
                          <span className="block truncate font-medium group-hover:text-accent">{name}</span>
                        </Link>
                        <span className="block truncate text-[11.5px] text-subtle">{t.spec?.description || "No description"}</span>
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5">
                        <TokenState t={t} />
                      </td>
                      <td className="px-4 py-2.5">
                        <Roles held={held} name={name} />
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5">
                        <Prefix t={t} />
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5">
                        <Expires t={t} />
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5">
                        <LastUsed t={t} />
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </Card>
        </ListFrame>
      </div>
      {/* Kept while open, so an answer that changes in between never drops a secret it shows. */}
      {(canCreate || creating) && (
        <NewTokenDialog
          key={String(creating)}
          open={creating}
          onClose={() => setCreating(false)}
          project={project}
          names={{ loading, error, names: items.map((t) => t.metadata?.name ?? "") }}
        />
      )}
    </>
  );
}

/**
 * ServiceTokenDetail is one token's page, with Rotate and Delete for owners.
 *
 * The rotate dialog, and the secret it shows, stand apart from what the
 * page draws: it holds the token as it was when opened, and keeps its place
 * whether the list is loading, failed to load again, or no longer holds
 * the token, so nothing but its own Close drops a secret it shows. A list
 * that fails to load again leaves the page showing its last answer.
 */
export function ServiceTokenDetail({ name }: { name: string }) {
  const project = useProjectName();
  const { items, loading, loaded, error, refetch } = useServiceTokens(project);
  const held = useTokenRoles(project);
  const navigate = useNavigate();
  const [rotating, setRotating] = useState<ServiceToken | null>(null);
  const [deleting, setDeleting] = useState(false);
  const { can } = useCan({ level: "project", name: project }, [perms.tokenApply, perms.tokenDelete]);
  const t = items.find((x) => x.metadata?.name === name);
  const crumbs = [
    <Link key="l" to="/projects/$project/service-tokens" params={{ project }}>
      Service tokens
    </Link>,
    name,
  ];
  // Fourth child of either fragment below, so it is the same element whichever is drawn.
  const rotate = rotating && <RotateDialog t={rotating} open onClose={() => setRotating(null)} />;
  if (!t) {
    return (
      <>
        <Topbar crumbs={crumbs} />
        {loading ? (
          <DetailSkeleton />
        ) : error ? (
          <div className="px-4 py-6 md:px-8">
            <Card>
              <ErrorNotice error={error} title="The service token did not load" onRetry={refetch} />
            </Card>
          </div>
        ) : (
          <EmptyState title="No such service token">
            This project has no service token named <span className="font-mono">{name}</span>.
          </EmptyState>
        )}
        {null}
        {rotate}
      </>
    );
  }
  const tokenRoles = held.loading || held.error ? undefined : (held.roles[name] ?? []);
  return (
    <>
      <Topbar crumbs={crumbs} />
      <div>
        {loaded && error ? <StaleBanner what="service tokens" error={error} onRetry={refetch} /> : null}
        <div className="border-b px-4 pb-5 pt-6 md:px-8">
          <div className="flex flex-col gap-4 md:flex-row md:items-start md:justify-between md:gap-6">
            <div className="min-w-0">
              <div className="flex items-center gap-2.5">
                <h1 className="truncate text-[22px] font-semibold tracking-tight">{name}</h1>
                <Badge>Service token</Badge>
              </div>
              <div className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-1 text-[13px] text-muted">
                <TokenState t={t} />
                {t.status?.expireTime && <span>Expires {day(t.status.expireTime)}</span>}
                <span>{t.status?.lastUseTime ? `Used ${relative(t.status.lastUseTime)}` : "Never used"}</span>
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              {can(perms.tokenApply) && (
                <Button onClick={() => setRotating(t)} disabled={!!t.metadata?.deleteTime}>
                  <RefreshCw /> Rotate
                </Button>
              )}
              {can(perms.tokenDelete) && (
                <Button variant="danger" onClick={() => setDeleting(true)} disabled={!!t.metadata?.deleteTime}>
                  <Trash2 /> Delete
                </Button>
              )}
            </div>
          </div>
        </div>
        <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
          <div className="min-w-0 space-y-5">
            <Card>
              <CardHeader title="Summary" />
              <dl className="divide-y px-4">
                <Field label="Description">{t.spec?.description || <span className="text-subtle">None</span>}</Field>
                <Field label="Principal" mono>
                  {tokenPrincipal(t)}
                </Field>
                <Field label="Secret">
                  <span>
                    <Prefix t={t} />
                    {t.status?.secretPrefix && <span className="text-muted">, its first characters</span>}
                  </span>
                </Field>
                <Field label="Issued">{t.status?.issueTime ? `${day(t.status.issueTime)} UTC` : <span className="text-subtle">Never</span>}</Field>
                <Field label="Expires">
                  <Expires t={t} />
                </Field>
                <Field label="Last used">
                  <LastUsed t={t} />
                </Field>
                <Field label="Created">{day(t.metadata?.createTime)} UTC</Field>
                <Field label="UID" mono>
                  <span className="text-muted">{t.metadata?.uid}</span>
                </Field>
              </dl>
            </Card>
            <Card>
              <CardHeader title="Roles in this project" />
              {held.loading ? (
                <SkeletonRows rows={1} />
              ) : held.error ? (
                <ErrorNotice error={held.error} title="The role bindings did not load" />
              ) : !tokenRoles?.length ? (
                <p className="px-4 py-4 text-[13px] text-muted">No binding names this token, so it holds nothing.</p>
              ) : (
                <ul className="divide-y">
                  {tokenRoles.map((r) => (
                    <li key={r} className="flex items-baseline gap-3 px-4 py-2.5 text-[13px]">
                      <span className="w-16 shrink-0 font-medium">{roleLabel(r)}</span>
                      <span className="font-mono text-[12px] text-muted">{r}</span>
                      <span className="ml-auto text-[12px] text-subtle">deleted with the token</span>
                    </li>
                  ))}
                </ul>
              )}
            </Card>
          </div>
          <div className="space-y-5">
            <EquivalentCard
              cli={[
                commands.getToken(project, name),
                ...(can(perms.tokenApply) ? [commands.rotateToken(project, name)] : []),
                ...(can(perms.tokenDelete) ? [commands.deleteToken(project, name)] : []),
              ]}
            />
          </div>
        </div>
      </div>
      {can(perms.tokenDelete) ? (
        <DeleteTokenDialog
          t={t}
          held={tokenRoles}
          open={deleting}
          onClose={() => setDeleting(false)}
          after={() => void navigate({ to: "/projects/$project/service-tokens", params: { project } })}
        />
      ) : null}
      {rotate}
    </>
  );
}
