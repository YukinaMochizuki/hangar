// A project's public endpoints, one endpoint, and changing who may use it
// (docs/decisions/public-endpoints.md); pages/create-public-endpoint.tsx
// makes one. An endpoint a container's public port made is the
// container's: the API sets its spec back and makes it again, so it offers
// no edit and no delete here.

import { clone, create } from "@bufbuild/protobuf";
import type { CustomDomain } from "@hangar/api/hangar/network/v1alpha1/custom_domain_pb";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  PublicEndpointAccess,
  PublicEndpointSchema,
  PublicEndpointSpecSchema,
  type PublicEndpoint,
} from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { ArrowUpRight, AtSign, Globe, Lock, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { Dialog } from "@/components/dialog";
import { DeleteDialog, FormFooter, StaleNotice } from "@/components/resource";
import { Notice } from "@/components/notice";
import { ErrorNotice, Skeleton } from "@/components/states";
import { FormEquivalent } from "@/components/form";
import { HostnameRows } from "@/components/hostnames";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { ConditionsCard, EquivalentCard, LabelsField } from "@/components/object";
import { editLabelsAction, HeadActions, ObjectRowMenu, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { DetailBody, DetailFrame, DetailHead, ListCount, ListFrame, NameFilter, PageHead } from "@/components/page";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Topbar, useProjectName } from "@/components/shell";
import { Button, buttonVariants, Card, CardHeader, CopyButton, Field, Led } from "@/components/ui";
import { cn } from "@/lib/cn";
import { domainFor, domainOf, hostnameOf, isVerified, rowProblems, rowsOf, useProjectDomains, type HostnameRow } from "@/lib/custom-domains";
import { date, machinePath } from "@/lib/machines";
import { accessLabel, conditionOf, endpointYaml, lightOf, nameOf, ownerOf, targetOf, usePublicEndpoints, type ListState } from "@/lib/network";
import { isStale, permission, useChange, useNetworkCan } from "@/lib/network-actions";
import { useApplyObjectAt, useReadObject } from "@/lib/object-kinds";

/**
 * An endpoint's actions, on its page and in its row's menu: Make public or
 * private, Edit labels and Delete, each shown once TestPermissions holds
 * its permission, and none for an endpoint a container's port made, which
 * is the container's. `after` follows a delete the API took.
 */
export function useEndpointActions(e: PublicEndpoint, can: Can, after?: () => void): ActionSet {
  const name = nameOf(e);
  const [open, setOpen] = useState<"access" | "hostnames" | "delete" | "labels" | null>(null);
  const owned = !!ownerOf(e);
  const mayApply = !owned && can(permission("publicEndpoint", "apply"));
  const isPublic = e.spec?.access === PublicEndpointAccess.PUBLIC;
  const actions: ObjectAction[] = [];
  if (mayApply) actions.push({ label: isPublic ? "Make private" : "Make public", icon: isPublic ? Lock : Globe, onSelect: () => setOpen("access") });
  // Only a public endpoint serves at custom hostnames (docs/decisions/custom-domains.md).
  if (mayApply && isPublic) actions.push({ label: "Edit hostnames", icon: AtSign, onSelect: () => setOpen("hostnames") });
  if (!owned && can(permission("publicEndpoint", "delete")))
    actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: () => setOpen("delete") });
  const close = () => setOpen(null);
  return {
    actions,
    editLabels: editLabelsAction(mayApply, !!e.metadata?.deleteTime, () => setOpen("labels")),
    deleting: !!e.metadata?.deleteTime,
    dialogs: (
      <>
        {open === "access" && <AccessDialog open onClose={close} endpoint={e} />}
        {open === "hostnames" && <HostnamesDialog onClose={close} endpoint={e} />}
        {open === "labels" && <EditLabelsDialog kind="publicEndpoint" object={e} onClose={close} />}
        {open === "delete" && (
          <DeleteDialog
            kind="publicEndpoint"
            name={name}
            open
            onClose={close}
            after={after}
            consequence={`Its connections close. Its hostname stays reserved for an endpoint named ${name} in this project.`}
          />
        )}
      </>
    ),
  };
}

/** An endpoint's row menu: the actions of its page. */
function EndpointRowMenu({ e, can }: { e: PublicEndpoint; can: Can }) {
  return <ObjectRowMenu name={nameOf(e)} set={useEndpointActions(e, can)} />;
}

function AccessBadge({ access }: { access: PublicEndpointAccess }) {
  const pub = access === PublicEndpointAccess.PUBLIC;
  const Icon = pub ? Globe : Lock;
  return (
    <span className="inline-flex items-center gap-1.5 text-[12.5px]">
      <Icon className="size-3.5 text-subtle" strokeWidth={1.75} />
      {accessLabel(access)}
    </span>
  );
}

function ReadyCell({ e }: { e: PublicEndpoint }) {
  const c = conditionOf(e.status?.conditions, "Ready");
  return (
    <>
      <span className="inline-flex items-center gap-2">
        <Led tone={lightOf(c)} />
        {c?.status === 1 ? "Ready" : "Not ready"}
      </span>
      {c?.status !== 1 && c?.reason && <span className="block font-mono text-[11.5px] text-subtle">{c.reason}</span>}
    </>
  );
}

function UrlLink({ e, className }: { e: PublicEndpoint; className?: string }) {
  if (!e.status?.url) return <span className="text-subtle">no URL yet</span>;
  return (
    <a href={e.status.url} target="_blank" rel="noreferrer" className={cn("inline-flex min-w-0 items-center gap-1 text-accent hover:underline", className)}>
      <span className="truncate font-mono text-[12.5px]">{e.status.url.replace(/^https:\/\//, "")}</span>
      <ArrowUpRight className="size-3 shrink-0" />
    </a>
  );
}

function TargetText({ e }: { e: PublicEndpoint }) {
  const project = useProjectName();
  const t = targetOf(e.spec?.target);
  if (!t) return <span className="text-subtle">none</span>;
  return (
    <span className="whitespace-nowrap">
      <Link to={machinePath(t.kind)} params={{ project, name: t.name }} className="font-medium text-accent hover:underline">
        {t.name}
      </Link>
      <span className="font-mono text-[12px] text-muted">:{e.spec?.port}</span>
      <span className="ml-2 text-[11.5px] text-subtle">{t.kind === "Container" ? "container" : "VM"}</span>
    </span>
  );
}

export function PublicEndpointList() {
  const project = useProjectName();
  const eps = usePublicEndpoints(project);
  const [query, setQuery] = useState("");
  const rows = eps.items.filter((e) => !query || nameOf(e).includes(query));
  const { can: may } = useNetworkCan("publicEndpoint", project);
  const newButton = may(permission("publicEndpoint", "apply")) ? (
    <Link to="/projects/$project/new/public-endpoint" params={{ project }} className={buttonVariants({ variant: "primary" })}>
      <Plus /> New public endpoint
    </Link>
  ) : undefined;
  return (
    <>
      <Topbar crumbs={["Public endpoints"]} />
      <PageHead
        title="Public endpoints"
        description="A machine's port, over HTTPS"
        action={newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ ...eps, count: eps.items.length }}
          noun="public endpoints"
          empty={{ title: "No public endpoints", text: "A public endpoint serves a machine's HTTP port over HTTPS, at a hostname.", action: newButton }}
        >
          <NameFilter value={query} onChange={setQuery} />
          <Card className="md:hidden">
            <ul className="divide-y">
              {rows.map((e) => (
                <li key={nameOf(e)} className="px-4 py-2.5 text-[13px]">
                  <div className="flex items-center gap-3">
                    <Link to="/projects/$project/public-endpoints/$name" params={{ project, name: nameOf(e) }} className="min-w-0 flex-1 truncate font-medium">
                      {nameOf(e)}
                    </Link>
                    <AccessBadge access={e.spec?.access ?? 0} />
                    <span className="shrink-0 text-right">
                      <ReadyCell e={e} />
                    </span>
                    <EndpointRowMenu e={e} can={may} />
                  </div>
                  <div className="mt-1 flex min-w-0">
                    <UrlLink e={e} className="text-[12px]" />
                  </div>
                </li>
              ))}
              {rows.length === 0 && <NoMatch noun="public endpoint" />}
            </ul>
          </Card>
          <Card className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[880px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[16%]">Name</Th>
                  <Th>URL</Th>
                  <Th>State</Th>
                  <Th>Access</Th>
                  <Th>Reaches</Th>
                  <Th>Created</Th>
                  <Th className="w-10 px-0" />
                </tr>
              </thead>
              <tbody>
                {rows.map((e) => (
                  <tr key={nameOf(e)} className={tbodyRow}>
                    <td className="max-w-0 px-4 py-2.5">
                      <Link to="/projects/$project/public-endpoints/$name" params={{ project, name: nameOf(e) }} className="block truncate font-medium group-hover:text-accent">
                        {nameOf(e)}
                      </Link>
                      {ownerOf(e) && <span className="block truncate text-[11.5px] text-subtle">port of {ownerOf(e)}</span>}
                    </td>
                    <td className="max-w-[300px] px-4 py-2.5">
                      <UrlLink e={e} className="max-w-full" />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <ReadyCell e={e} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <AccessBadge access={e.spec?.access ?? 0} />
                    </td>
                    <td className="px-4 py-2.5">
                      <TargetText e={e} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-muted">{date(e.metadata?.createTime)?.toISOString().slice(0, 10)}</td>
                    <td className="px-2 py-2.5">
                      <EndpointRowMenu e={e} can={may} />
                    </td>
                  </tr>
                ))}
                {rows.length === 0 && <NoMatch noun="public endpoint" colSpan={7} />}
              </tbody>
            </table>
          </Card>
          <ListCount shown={rows.length} total={eps.items.length} />
        </ListFrame>
      </div>
    </>
  );
}

export function PublicEndpointDetail({ name }: { name: string }) {
  const project = useProjectName();
  const eps = usePublicEndpoints(project);
  // Asked with the list, so the page's controls come with it, and once.
  const { can } = useNetworkCan("publicEndpoint", project);
  const e = eps.items.find((x) => nameOf(x) === name);
  return (
    <DetailFrame crumbs={["Public endpoints", name]} state={eps} found={!!e} noun="public endpoint" name={name}>
      {e && <Loaded e={e} can={can} />}
    </DetailFrame>
  );
}

/** The page of an endpoint that has loaded, its actions made once for its head and its labels. */
function Loaded({ e, can }: { e: PublicEndpoint; can: Can }) {
  const name = nameOf(e);
  const project = e.metadata?.project ?? "";
  const navigate = useNavigate();
  const set = useEndpointActions(e, can, () => void navigate({ to: "/projects/$project/public-endpoints", params: { project } }));
  const owner = ownerOf(e);
  const ready = conditionOf(e.status?.conditions, "Ready");
  return (
    <>
      <DetailHead
        name={name}
        kind="Public endpoint"
        facts={
          <>
            <span className="inline-flex items-center gap-2">
              <Led tone={lightOf(ready)} />
              {ready?.status === 1 ? "Ready" : "Not ready"}
            </span>
            <AccessBadge access={e.spec?.access ?? 0} />
            <UrlLink e={e} />
          </>
        }
        actions={set.actions.length > 0 && <HeadActions set={set} />}
      />
      <DetailBody
        main={
          <>
            <Card>
              <CardHeader title="Summary" />
              <dl className="divide-y px-4">
                <Field label="URL">
                  {e.status?.url ? (
                    <span className="flex items-center gap-1">
                      <UrlLink e={e} />
                      <CopyButton text={e.status.url} />
                    </span>
                  ) : (
                    <span className="text-subtle">no URL yet</span>
                  )}
                </Field>
                <Field label="Reaches">
                  <TargetText e={e} />
                  <span className="block text-[12px] text-subtle">plain HTTP, on eth0</span>
                </Field>
                <Field label="Access">
                  <AccessBadge access={e.spec?.access ?? 0} />
                </Field>
                {owner && (
                  <Field label="Made by">
                    <Link to="/projects/$project/containers/$name" params={{ project, name: owner }} className="text-accent hover:underline">
                      {owner}
                    </Link>
                    <span className="block text-[12px] text-muted">Goes with the container's port.</span>
                  </Field>
                )}
                <Field label="Machine" mono>
                  {e.status?.machine || <span className="font-sans text-subtle">none</span>}
                </Field>
                <Field label="Host" mono>
                  {e.status?.node || <span className="font-sans text-subtle">not placed</span>}
                </Field>
                <LabelsField labels={e.metadata?.labels ?? {}} onEdit={set.editLabels?.onSelect} />
                <Field label="Created">{date(e.metadata?.createTime)?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
                <Field label="UID" mono>
                  <span className="text-muted">{e.metadata?.uid}</span>
                </Field>
              </dl>
            </Card>
            <HostnamesCard e={e} edit={set.actions.find((a) => a.label === "Edit hostnames")?.onSelect} />
            {ready && ready.status !== 1 && ready.message && (
              <Card>
                <CardHeader title="Not ready" />
                <div className="px-4 py-3 text-[13px]">
                  <span className="font-mono text-[12px] text-muted">{ready.reason}</span>
                  <p className="mt-0.5">{ready.message}</p>
                </div>
              </Card>
            )}
            <ConditionsCard conditions={e.status?.conditions ?? []} />
          </>
        }
        side={
          <EquivalentCard
            cli={[
              `hangar get ep ${name} -p ${project}`,
              ...(owner ? [] : [`hangar apply -p ${project} -f ${name}.yaml`, `hangar delete ep ${name} -p ${project}`]),
            ]}
            yaml={endpointYaml(e)}
          />
        }
      />
      {set.dialogs}
    </>
  );
}

function AccessDialog({ open, onClose, endpoint }: { open: boolean; onClose: () => void; endpoint: PublicEndpoint }) {
  const project = useProjectName();
  const change = useChange("publicEndpoint", project, nameOf(endpoint));
  const toPublic = endpoint.spec?.access !== PublicEndpointAccess.PUBLIC;
  const want = toPublic ? PublicEndpointAccess.PUBLIC : PublicEndpointAccess.PRIVATE;
  const obj = create(PublicEndpointSchema, {
    metadata: { name: nameOf(endpoint), labels: endpoint.metadata?.labels ?? {} },
    spec: { target: endpoint.spec?.target, port: endpoint.spec?.port, access: want },
  });
  const close = () => {
    change.reset();
    onClose();
  };
  const verb = toPublic ? "Make public" : "Make private";
  // The API refuses a private endpoint custom hostnames (docs/decisions/custom-domains.md).
  const hostnames = endpoint.spec?.hostnames ?? [];
  const held = !toPublic && hostnames.length > 0;
  return (
    <Dialog open={open} onClose={close} title={toPublic ? `Make ${nameOf(endpoint)} public` : `Make ${nameOf(endpoint)} private`}>
      <p>
        {toPublic
          ? "Anyone reaches it, with no sign-in. Open connections close."
          : "Sign-in is required. Open connections close."}
      </p>
      <FormEquivalent yaml={endpointYaml(obj)} cli={[`hangar apply -p ${project} -f ${nameOf(endpoint)}.yaml`]} />
      {held && (
        <Notice tone="warning" className="mt-3" title={`${nameOf(endpoint)} serves at ${hostnames.length === 1 ? "a custom hostname" : `${hostnames.length} custom hostnames`}: ${hostnames.join(", ")}.`}>
          Only a public endpoint serves at custom hostnames.
        </Notice>
      )}
      <FormFooter
        problems={[]}
        blocked={held}
        error={change.error}
        failed={toPublic ? "Making the endpoint public failed" : "Making the endpoint private failed"}
        pending={change.isPending}
        pendingLabel="Applying…"
        label={
          <>
            {toPublic ? <Globe /> : <Lock />} {verb}
          </>
        }
        onCancel={close}
        onSubmit={() =>
          change.mutate(
            (o) => {
              o.spec ??= create(PublicEndpointSpecSchema);
              o.spec.access = want;
            },
            { onSuccess: close },
          )
        }
      />
    </Dialog>
  );
}

/** A URL as a link, without its scheme. */
function HostLink({ url }: { url: string }) {
  return (
    <a href={url} target="_blank" rel="noreferrer" className="inline-flex min-w-0 items-center gap-1 text-accent hover:underline">
      <span className="truncate font-mono text-[12.5px]">{url.replace(/^https:\/\//, "")}</span>
      <ArrowUpRight className="size-3 shrink-0" />
    </a>
  );
}

/**
 * Where the endpoint serves: its own hostname, then each of spec.hostnames,
 * with its URL when the status lists it, else why it waits, as the
 * Hostnames condition says.
 */
function HostnamesCard({ e, edit }: { e: PublicEndpoint; edit?: () => void }) {
  const domains = useProjectDomains(e.metadata?.project ?? "");
  const hostnames = e.spec?.hostnames ?? [];
  const cond = conditionOf(e.status?.conditions, "Hostnames");
  const isPublic = e.spec?.access === PublicEndpointAccess.PUBLIC;
  return (
    <Card>
      <CardHeader
        title={
          <span className="flex items-baseline gap-2">
            Hostnames
            <span className="font-mono text-[11.5px] font-normal text-subtle">{1 + hostnames.length}</span>
          </span>
        }
        action={
          edit && (
            <Button size="sm" variant="ghost" onClick={edit} className="-mr-2">
              <AtSign /> Edit hostnames
            </Button>
          )
        }
      />
      <ul className="divide-y">
        <li className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
          <Led tone={e.status?.url ? "ok" : "pending"} />
          <span className="flex min-w-0 flex-1 items-center gap-1">
            {e.status?.url ? (
              <>
                <HostLink url={e.status.url} />
                <CopyButton text={e.status.url} />
              </>
            ) : (
              <span className="text-subtle">no URL yet</span>
            )}
          </span>
          <span className="shrink-0 text-[12px] text-subtle">its own</span>
        </li>
        {hostnames.map((h) => {
          const url = e.status?.hostnameUrls.find((u) => u === `https://${h}`);
          const d = domainFor(h, domains.items);
          return (
            <li key={h} className="flex items-start gap-3 px-4 py-2.5 text-[13px]">
              <Led tone={url ? "ok" : "pending"} className="mt-1.5" />
              <span className="min-w-0 flex-1">
                {url ? (
                  <span className="flex min-w-0 items-center gap-1">
                    <HostLink url={url} />
                    <CopyButton text={url} />
                  </span>
                ) : (
                  <>
                    <span className="block truncate font-mono text-[12.5px]">{h}</span>
                    <span className="block text-[12px] text-muted">
                      {cond?.reason === "DomainNotVerified" || (d && !isVerified(d))
                        ? `Waits for ${d ? domainOf(d) : "its custom domain"} to be verified.`
                        : "Not serving yet."}
                    </span>
                  </>
                )}
              </span>
              <span className="shrink-0 pt-0.5 text-[12px] text-subtle">{url ? "serving" : "waiting"}</span>
            </li>
          );
        })}
      </ul>
      {hostnames.length === 0 && (
        <p className="border-t px-4 py-2.5 text-[12.5px] text-muted">
          {ownerOf(e) || isPublic ? "No custom hostname." : "Public endpoints only."}
        </p>
      )}
    </Card>
  );
}

/** What applying the rows does to spec.hostnames, as "Adds a · Removes b"; "" for no change. */
function hostnameChange(before: string[], after: string[]): string {
  const added = after.filter((h) => !before.includes(h));
  const removed = before.filter((h) => !after.includes(h));
  return [...(added.length ? [`Adds ${added.join(", ")}`] : []), ...(removed.length ? [`Removes ${removed.join(", ")}`] : [])].join(" · ");
}

/**
 * Edit hostnames: the endpoint's custom hostnames as the create page's rows.
 * It applies the endpoint as it opened on with spec.hostnames alone
 * changed, at that resource version, as Edit labels does: a newer endpoint
 * is never written over, and Reload starts the rows again from it. The rows
 * wait for the project's custom domains, which say where each hostname's
 * domain ends.
 */
function HostnamesDialog({ onClose, endpoint }: { onClose: () => void; endpoint: PublicEndpoint }) {
  const domains = useProjectDomains(endpoint.metadata?.project ?? "");
  return (
    <Dialog open onClose={onClose} title={`Edit hostnames of ${nameOf(endpoint)}`} wide>
      {domains.loading ? (
        <div aria-busy="true" aria-label="Loading" className="space-y-2 py-2">
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-full" />
        </div>
      ) : domains.error ? (
        <ErrorNotice error={domains.error} title="The project's custom domains did not load" onRetry={domains.refetch} className="px-0" />
      ) : (
        <HostnamesForm endpoint={endpoint} domains={domains} onClose={onClose} />
      )}
    </Dialog>
  );
}

function HostnamesForm({ endpoint, domains, onClose }: { endpoint: PublicEndpoint; domains: ListState<CustomDomain>; onClose: () => void }) {
  const [base, setBase] = useState(endpoint);
  const project = base.metadata?.project ?? "";
  const name = nameOf(base);
  const before = base.spec?.hostnames ?? [];
  const [rows, setRows] = useState<HostnameRow[]>(() => rowsOf(before, domains.items));
  const apply = useApplyObjectAt("publicEndpoint");
  const reload = useReadObject("publicEndpoint", project, name);
  const stale = isStale(apply.error);
  const after = rows.map(hostnameOf);
  const change = hostnameChange(before, after);
  const obj = clone(PublicEndpointSchema, base);
  if (obj.spec) obj.spec.hostnames = after;
  const startAgain = () =>
    reload.mutate(undefined, {
      onSuccess: (fresh) => {
        setBase(fresh);
        setRows(rowsOf(fresh.spec?.hostnames ?? [], domains.items));
        apply.reset();
      },
    });
  return (
    <>
      <p className="mb-4">Each a custom domain of the tenant or one label below one. Connections at a hostname taken out close.</p>
      <HostnameRows rows={rows} onChange={setRows} domains={domains} />
      <div className="mt-4 text-[12.5px] text-foreground">{change || "No change"}</div>
      <FormEquivalent yaml={endpointYaml(obj)} cli={[`hangar apply -p ${project} -f ${name}.yaml`]} />
      {stale && <StaleNotice title={`${name} changed since this dialog opened.`} reloading={reload.isPending} error={reload.error} onReload={startAgain} />}
      <FormFooter
        problems={rowProblems(rows)}
        blocked={!change || stale}
        error={stale ? null : apply.error}
        failed="Changing the hostnames failed"
        pending={apply.isPending}
        pendingLabel="Applying…"
        label={
          <>
            <AtSign /> Apply
          </>
        }
        onCancel={onClose}
        onSubmit={() => apply.mutate(obj, { onSuccess: onClose })}
      />
    </>
  );
}
