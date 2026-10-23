// A project's secrets: what each holds a login for, never the login. Values
// are write-only (docs/decisions/secrets.md): the create sends them
// (pages/create-secret.tsx), and nothing reads them back, so these pages
// show names, keys, registries, types and times alone, whatever the API
// answers.

import { Link, useNavigate } from "@tanstack/react-router";
import type { Secret } from "@hangar/api/hangar/secrets/v1alpha1/secret_pb";
import { KeyRound, Plus, Trash2 } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { StaleBanner } from "@/components/credentials";
import { ConfirmDeleteDialog } from "@/components/delete-dialog";
import { Dialog } from "@/components/dialog";
import { FormEquivalent } from "@/components/form";
import { Notice, PageBanner } from "@/components/notice";
import { HeadActions, ObjectRowMenu, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { StaleNotice } from "@/components/resource";
import { SecretValuesFields, useSecretValuesForm } from "@/components/secret-values";
import { DetailSkeleton, ListFrame, PageHead } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, Skeleton } from "@/components/states";
import { tbodyRow, Th, theadRow } from "@/components/table";
import { ConditionsCard, EquivalentCard, LabelsField } from "@/components/object";
import { Badge, Button, buttonVariants, Card, CardHeader, Field, StateText } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import {
  commands,
  day,
  perms,
  secretFault,
  secretTypeLabel,
  useDeleteSecret,
  useReadSecret,
  useReplaceSecret,
  useSecretUse,
  useSecrets,
  type SecretValues,
} from "@/lib/credentials";
import { isStale } from "@/lib/network-actions";
import { useCan } from "@/lib/permissions";

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
  return <StateText tone={tone} label={label} reason={fault?.reason} fault />;
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

function DeleteSecretDialog({ s, use, open, onClose, after }: { s: Secret; use: Use; open: boolean; onClose: () => void; after?: () => void }) {
  const name = s.metadata?.name ?? "";
  const project = s.metadata?.project ?? "";
  const del = useDeleteSecret(project, name);
  const usedBy = use.names ?? [];
  return (
    <ConfirmDeleteDialog
      open={open}
      onClose={onClose}
      title={`Delete secret ${name}`}
      name={name}
      consequence={
        use.loading ? (
          "The project's containers are still loading, so which of them name it is not known yet."
        ) : use.error ? (
          "The project's containers did not load, so which of them name it is not known."
        ) : usedBy.length > 0 ? (
          <>
            {usedBy.length === 1 ? "1 container names it" : `${usedBy.length} containers name it`}: {usedBy.join(", ")}. Their next image pull fails.
          </>
        ) : (
          "No container names it."
        )
      }
      cli={commands.deleteSecret(project, name)}
      del={del}
      after={after}
    />
  );
}

/** Names, joined as a sentence lists them: "a", "a and b", "a, b and c". */
function listed(names: string[]): string {
  return names.length < 2 ? (names[0] ?? "") : `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

/**
 * ReplaceValueDialog takes a secret's whole value again, in New secret's
 * fields, and applies it over the secret at the version the dialog opened
 * on. Apply replaces spec.data whole, so the one key a registry login holds
 * is sent with it. The values live in the fields alone; the dialog is
 * mounted while open, so closing it drops them.
 */
function ReplaceValueDialog({ s, use, onClose }: { s: Secret; use: Use; onClose: () => void }) {
  // The secret as the dialog opened on it; the list refreshing does not move it.
  const [base, setBase] = useState(s);
  const name = base.metadata?.name ?? "";
  const project = base.metadata?.project ?? "";
  const form = useSecretValuesForm();
  const valuesRef = useRef<SecretValues | null>(null);
  const apply = useReplaceSecret(valuesRef);
  const reload = useReadSecret(project, name);
  useEffect(() => {
    const ref = valuesRef;
    return () => {
      ref.current = null;
    };
  }, []);
  const stale = isStale(apply.error);
  const send = () => {
    if (form.problems.length > 0 || stale || apply.isPending) return;
    valuesRef.current = form.values();
    apply.mutate(base, { onSuccess: onClose });
  };
  // The value typed stays; Reload moves the dialog onto the secret as it is now, and nothing is applied until Replace value again.
  const startAgain = () =>
    reload.mutate(undefined, {
      onSuccess: (fresh) => {
        setBase(fresh);
        apply.reset();
      },
    });
  const registries = base.status?.registries ?? [];
  const usedBy = use.names ?? [];
  return (
    <Dialog open onClose={onClose} title={`Replace the value of ${name}`} wide>
      <p>
        The old value{registries.length > 0 && <>, a login for {listed(registries)},</>} is gone once applied.
      </p>
      <p className="mt-2">
        {use.loading
          ? "The project's containers are still loading, so which of them name it is not known yet."
          : use.error
            ? "The project's containers did not load, so which of them name it is not known."
            : usedBy.length === 0
              ? "No container names it."
              : `${listed(usedBy)} ${usedBy.length === 1 ? "names" : "name"} it; the new value counts from the next image pull.`}
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          send();
        }}
      >
        <div className="mt-4 text-foreground">
          <SecretValuesFields form={form} />
        </div>
        <FormEquivalent cli={[commands.replaceSecret(project, name)]} />
        {/* The command builds the secret afresh (internal/cli/secret.go): no labels, no version to apply at. */}
        <p className="mt-1.5 text-[12px] text-subtle">
          The command sets no labels{Object.keys(base.metadata?.labels ?? {}).length > 0 ? `, dropping ${name}'s,` : ""} and overwrites any change since it was read.
        </p>
        {stale ? (
          <StaleNotice title={`${name} changed since this dialog opened.`} reloading={reload.isPending} error={reload.error} onReload={startAgain} />
        ) : (
          apply.error != null && (
            <Notice tone="error" title="Replacing the value failed" className="mt-3">
              {errorMessage(apply.error)}
            </Notice>
          )
        )}
        {form.problems.length > 0 && <div className="mt-3 text-[12px] text-subtle">{form.problems.join(" ")}</div>}
        <div className="mt-5 flex justify-end gap-2">
          <Button type="button" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={form.problems.length > 0 || stale || apply.isPending}>
            <KeyRound /> {apply.isPending ? "Replacing…" : "Replace value"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

/**
 * A secret's actions, on its page and in its row's menu: Replace value and
 * Delete, each shown once TestPermissions holds its permission; `after`
 * follows a delete the API took. Its labels are not edited apart: every
 * apply of a secret carries its value, which nothing reads back.
 */
export function useSecretActions(s: Secret, use: Use, can: Can, after?: () => void): ActionSet {
  const [open, setOpen] = useState<"replace" | "delete" | null>(null);
  const actions: ObjectAction[] = [];
  if (can(perms.secretApply)) actions.push({ label: "Replace value", icon: KeyRound, onSelect: () => setOpen("replace") });
  if (can(perms.secretDelete)) actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: () => setOpen("delete") });
  const close = () => setOpen(null);
  return {
    actions,
    deleting: !!s.metadata?.deleteTime,
    dialogs: (
      <>
        {open === "replace" && <ReplaceValueDialog s={s} use={use} onClose={close} />}
        {open === "delete" && <DeleteSecretDialog s={s} use={use} open onClose={close} after={after} />}
      </>
    ),
  };
}

/** A secret's row menu: the actions of its page. */
function SecretRowMenu({ s, use, can }: { s: Secret; use: Use; can: Can }) {
  return <ObjectRowMenu name={s.metadata?.name ?? ""} set={useSecretActions(s, use, can)} />;
}

/** SecretList is the project's secrets, with New secret. */
export function SecretList() {
  const project = useProjectName();
  const { items, loading, loaded, error, refetch } = useSecrets(project);
  const use = useSecretUse(project);
  const createPerm = useCan({ level: "project", name: project }, [perms.secretApply, perms.secretDelete]);
  const canCreate = createPerm.can(perms.secretApply);
  const newButton = (
    <Link to="/projects/$project/new/secret" params={{ project }} className={buttonVariants({ variant: "primary" })}>
      <Plus /> New secret
    </Link>
  );
  return (
    <>
      <Topbar crumbs={["Secrets"]} />
      {loaded && error ? <StaleBanner what="secrets" error={error} onRetry={refetch} /> : null}
      <PageHead
        title="Secrets"
        description="Write-only registry logins"
        action={canCreate && newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ loading, error: loaded ? undefined : error, refetch, count: items.length }}
          noun="secrets"
          empty={{ title: "No secrets", text: "A secret holds values the project's containers use, such as a registry login, out of their spec.", action: canCreate ? newButton : undefined }}
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
                    <SecretRowMenu s={s} use={{ ...use, names: use.use[name] }} can={createPerm.can} />
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
                  <Th className="w-10 px-0" />
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
                      <td className="px-2 py-2.5">
                        <SecretRowMenu s={s} use={{ ...use, names: use.use[name] }} can={createPerm.can} />
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </Card>
        </ListFrame>
      </div>
    </>
  );
}

/** SecretDetail is one secret's page: what it holds a login for, with Delete. */
export function SecretDetail({ name }: { name: string }) {
  const project = useProjectName();
  const { items, loading, loaded, error, refetch } = useSecrets(project);
  const secretUse = useSecretUse(project);
  // Asked with the list, so the page's controls come with it, and once.
  const { can } = useCan({ level: "project", name: project }, [perms.secretApply, perms.secretDelete]);
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
  return <Loaded s={s} crumbs={crumbs} use={{ names: secretUse.use[name], loading: secretUse.loading, error: secretUse.error }} stale={loaded ? error : null} refetch={refetch} can={can} />;
}

/** The page of a secret that has loaded, its actions made once for its head. */
function Loaded({ s, crumbs, use, stale, refetch, can }: { s: Secret; crumbs: ReactNode[]; use: Use; stale: unknown; refetch: () => void; can: Can }) {
  const name = s.metadata?.name ?? "";
  const project = s.metadata?.project ?? "";
  const navigate = useNavigate();
  const set = useSecretActions(s, use, can, () => void navigate({ to: "/projects/$project/secrets", params: { project } }));
  const fault = secretFault(s);
  const usedBy = use.names ?? [];
  return (
    <>
      <Topbar crumbs={crumbs} />
      {stale ? <StaleBanner what="secrets" error={stale} onRetry={refetch} /> : null}
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
          {set.actions.length > 0 && (
            <div className="flex flex-wrap items-center gap-2">
              <HeadActions set={set} />
            </div>
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
                  <KeyRound className="size-3.5" /> Write-only
                </span>
              </Field>
              <Field label="Used by">
                <UsedBy use={use} project={project} />
              </Field>
              <LabelsField labels={s.metadata?.labels ?? {}} note="Changed only with the value, by hangar apply." />
              <Field label="Created">{s.metadata?.createTime ? `${day(s.metadata.createTime)} UTC` : ""}</Field>
              <Field label="UID" mono>
                <span className="text-muted">{s.metadata?.uid}</span>
              </Field>
            </dl>
          </Card>
          <ConditionsCard conditions={s.status?.conditions ?? []} />
        </div>
        <div className="space-y-5">
          <EquivalentCard
            cli={[
              commands.getSecret(project, name),
              ...(can(perms.secretApply) ? [commands.replaceSecret(project, name)] : []),
              ...(can(perms.secretDelete) ? [commands.deleteSecret(project, name)] : []),
            ]}
          />
        </div>
      </div>
      {set.dialogs}
    </>
  );
}
