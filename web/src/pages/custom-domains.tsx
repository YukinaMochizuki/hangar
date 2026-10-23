// A tenant's custom domains and one domain's page
// (docs/decisions/custom-domains.md): whether it is verified and why not,
// the DNS records it needs, the projects that may serve under it and the
// endpoints that do. pages/create-custom-domain.tsx makes one.

import { clone } from "@bufbuild/protobuf";
import { Link, useNavigate } from "@tanstack/react-router";
import { CustomDomainSchema, type CustomDomain } from "@hangar/api/hangar/network/v1alpha1/custom_domain_pb";
import type { PublicEndpoint } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { ArrowUpRight, FolderCog, Plus, RefreshCw, Trash2 } from "lucide-react";
import { useState, type ReactNode } from "react";
import { ConfirmDeleteDialog } from "@/components/delete-dialog";
import { Dialog } from "@/components/dialog";
import { FormEquivalent } from "@/components/form";
import { Choice } from "@/components/form-page";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { ConditionsCard, EquivalentCard, LabelsField } from "@/components/object";
import { editLabelsAction, HeadActions, ObjectRowMenu, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { DetailBody, DetailFrame, DetailHead, ListCount, ListFrame, NameFilter, PageHead } from "@/components/page";
import { Notice } from "@/components/notice";
import { FormFooter, StaleNotice } from "@/components/resource";
import { Topbar, useTenantName } from "@/components/shell";
import { ErrorNotice, Skeleton } from "@/components/states";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Button, buttonVariants, Card, CardHeader, CopyButton, Field, Led, StateText } from "@/components/ui";
import { domainOf, domainPerms, domainState, useCheckDomain, useCustomDomains, useDeleteDomain, useDomainCan, usesOf, useTenantEndpoints, type DomainUse } from "@/lib/custom-domains";
import { errorMessage } from "@/lib/api";
import { date } from "@/lib/machines";
import { count, customDomainYaml, nameOf } from "@/lib/network";
import { isStale } from "@/lib/network-actions";
import { useApplyObjectAt, useReadObject } from "@/lib/object-kinds";
import { projectLabel, useProjects } from "@/lib/projects";

/** The hostnames under a domain, read from the tenant's endpoints: loading, failed, or known. */
type Uses = { loading: boolean; error: unknown; refetch: () => void; items: DomainUse[] };

/** Why what the API refuses while endpoints serve under the domain cannot be judged yet; undefined once it can. */
function usesUnknown(uses: Uses): string | undefined {
  if (uses.loading) return "The tenant's endpoints are still loading.";
  if (uses.error) return "The tenant's endpoints did not load, so whether any serves under the domain is not known.";
  return undefined;
}

/** The tenant's projects, by display name. */
function useTenantProjects(tenant: string) {
  const { projects, loading } = useProjects();
  return { loading, projects: projects.filter((p) => p.spec?.tenant === tenant) };
}

/** The projects a domain lets, in words: "All projects", or their display names. */
function ProjectsText({ d, tenant }: { d: CustomDomain; tenant: string }) {
  const { projects } = useTenantProjects(tenant);
  const names = d.spec?.projects ?? [];
  if (names.length === 0) return <span>All projects</span>;
  return <span>{names.map((n) => projectLabel(projects.find((p) => p.metadata?.name === n), n)).join(", ")}</span>;
}

function StateCell({ d }: { d: CustomDomain }) {
  const s = domainState(d);
  return <StateText tone={s.tone} label={s.label} className="whitespace-nowrap" />;
}

/**
 * A domain's actions, on its page and in its row's menu: Edit projects,
 * Edit labels and Delete, each shown once TestPermissions at the tenant
 * holds its permission. Delete is off, with the fact, while endpoints have
 * hostnames under the domain, which the API refuses, and while that is not
 * known.
 */
function useDomainActions(d: CustomDomain, uses: Uses, can: Can, after?: () => void): ActionSet {
  const name = nameOf(d);
  const [open, setOpen] = useState<"projects" | "labels" | "delete" | null>(null);
  const del = useDeleteDomain(name);
  const deleting = !!d.metadata?.deleteTime;
  const actions: ObjectAction[] = [];
  const mayApply = can(domainPerms.apply);
  if (mayApply) actions.push({ label: "Edit projects", icon: FolderCog, onSelect: () => setOpen("projects") });
  if (can(domainPerms.delete)) actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: () => setOpen("delete") });
  const close = () => setOpen(null);
  const endpoints = [...new Set(uses.items.map((u) => nameOf(u.endpoint)))];
  const inUse = endpoints.length > 0 ? `${count(endpoints.length, "endpoint")} ${endpoints.length === 1 ? "has" : "have"} hostnames under this domain: ${endpoints.join(", ")}.` : undefined;
  return {
    actions,
    editLabels: editLabelsAction(mayApply, deleting, () => setOpen("labels")),
    deleting,
    dialogs: (
      <>
        {open === "projects" && <EditProjectsDialog d={d} uses={uses} onClose={close} />}
        {open === "labels" && <EditLabelsDialog kind="customDomain" object={d} onClose={close} />}
        {open === "delete" && (
          <ConfirmDeleteDialog
            open
            onClose={close}
            title={`Delete custom domain ${domainOf(d)}`}
            name={name}
            consequence={
              <>
                Its certificates stop renewing, and another tenant may then claim <span className="font-mono text-foreground">{domainOf(d)}</span>.
              </>
            }
            cli={`hangar delete cdom ${name}`}
            blocked={usesUnknown(uses) ?? inUse}
            del={del}
            after={after}
          />
        )}
      </>
    ),
  };
}

function DomainRowMenu({ d, uses, can }: { d: CustomDomain; uses: Uses; can: Can }) {
  return <ObjectRowMenu name={domainOf(d)} set={useDomainActions(d, uses, can)} />;
}

export function CustomDomainList() {
  const tenant = useTenantName();
  const domains = useCustomDomains(tenant);
  const endpoints = useTenantEndpoints(tenant);
  const { can } = useDomainCan(tenant);
  const [query, setQuery] = useState("");
  const rows = domains.items.filter((d) => !query || domainOf(d).includes(query) || nameOf(d).includes(query));
  const usesOfRow = (d: CustomDomain): Uses => ({ ...endpoints, items: usesOf(d, domains.items, endpoints.items) });
  const endpointCount = (d: CustomDomain) => new Set(usesOfRow(d).items.map((u) => `${u.endpoint.metadata?.project}/${nameOf(u.endpoint)}`)).size;
  // The endpoints come from another list, so their count waits for it on its own.
  const countText = (d: CustomDomain, words: boolean): ReactNode =>
    endpoints.loading ? (
      <Skeleton className="inline-block h-3 w-6 align-middle" />
    ) : endpoints.error ? (
      <span className="font-sans text-subtle">{words ? "endpoints unknown" : "unknown"}</span>
    ) : !words ? (
      endpointCount(d)
    ) : endpointCount(d) === 0 ? (
      "no endpoint"
    ) : (
      count(endpointCount(d), "endpoint")
    );
  const newButton = can(domainPerms.apply) ? (
    <Link to="/tenants/$tenant/new/custom-domain" params={{ tenant }} className={buttonVariants({ variant: "primary" })}>
      <Plus /> New custom domain
    </Link>
  ) : undefined;
  return (
    <>
      <Topbar crumbs={["Domains"]} />
      <PageHead
        title="Domains"
        description="The tenant's own domains"
        action={newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ ...domains, count: domains.items.length }}
          noun="custom domains"
          empty={{
            title: "No custom domains",
            text: "A custom domain lets the tenant's public endpoints serve at hostnames of its own.",
            action: newButton,
          }}
        >
          <NameFilter value={query} onChange={setQuery} placeholder="Domain" label="Filter by domain" />
          <Card className="md:hidden">
            <ul className="divide-y">
              {rows.map((d) => (
                <li key={nameOf(d)} className="px-4 py-2.5 text-[13px]">
                  <div className="flex items-center gap-3">
                    <Link to="/tenants/$tenant/domains/$name" params={{ tenant, name: nameOf(d) }} className="min-w-0 flex-1 truncate font-mono font-medium">
                      {domainOf(d)}
                    </Link>
                    <DomainRowMenu d={d} uses={usesOfRow(d)} can={can} />
                  </div>
                  <div className="-mt-1 flex min-w-0 items-center gap-1.5 text-[12px] text-muted">
                    <span className="shrink-0 text-foreground">
                      <StateCell d={d} />
                    </span>
                    <span className="truncate">
                      · <ProjectsText d={d} tenant={tenant} /> · {countText(d, true)}
                    </span>
                  </div>
                </li>
              ))}
              {rows.length === 0 && <NoMatch noun="custom domain" />}
            </ul>
          </Card>
          <Card className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[820px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[26%]">Domain</Th>
                  <Th>Verification</Th>
                  <Th>Projects</Th>
                  <Th className="text-right">Endpoints</Th>
                  <Th>Created</Th>
                  <Th className="w-10 px-0" />
                </tr>
              </thead>
              <tbody>
                {rows.map((d) => (
                  <tr key={nameOf(d)} className={tbodyRow}>
                    <td className="max-w-0 px-4 py-2.5">
                      <Link to="/tenants/$tenant/domains/$name" params={{ tenant, name: nameOf(d) }} className="block truncate font-mono font-medium group-hover:text-accent">
                        {domainOf(d)}
                      </Link>
                      <span className="block truncate font-mono text-[11.5px] text-subtle">{nameOf(d)}</span>
                    </td>
                    <td className="px-4 py-2.5">
                      <StateCell d={d} />
                    </td>
                    <td className="max-w-[260px] truncate px-4 py-2.5">
                      <ProjectsText d={d} tenant={tenant} />
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular">{countText(d, false)}</td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-muted">{date(d.metadata?.createTime)?.toISOString().slice(0, 10)}</td>
                    <td className="px-2 py-2.5">
                      <DomainRowMenu d={d} uses={usesOfRow(d)} can={can} />
                    </td>
                  </tr>
                ))}
                {rows.length === 0 && <NoMatch noun="custom domain" colSpan={6} />}
              </tbody>
            </table>
          </Card>
          <ListCount shown={rows.length} total={domains.items.length} />
        </ListFrame>
      </div>
    </>
  );
}

export function CustomDomainDetail({ name }: { name: string }) {
  const tenant = useTenantName();
  const domains = useCustomDomains(tenant);
  const { can } = useDomainCan(tenant);
  const d = domains.items.find((x) => nameOf(x) === name);
  const crumbs = [
    <Link key="list" to="/tenants/$tenant/domains" params={{ tenant }} className="hover:text-foreground">
      Domains
    </Link>,
    d ? domainOf(d) : name,
  ];
  return (
    <DetailFrame crumbs={crumbs} state={domains} found={!!d} noun="custom domain" name={name} where="This tenant">
      {d && <Loaded d={d} all={domains.items} can={can} />}
    </DetailFrame>
  );
}

/** A hostname that wraps, when it must, after a dot rather than inside a label. */
function AtDots({ text }: { text: string }) {
  const parts = text.split(".");
  return (
    <>
      {parts.map((p, i) => (
        <span key={i}>
          {p}
          {i < parts.length - 1 && (
            <>
              .<wbr />
            </>
          )}
        </span>
      ))}
    </>
  );
}

/** A purpose as the API words it, as a sentence. */
const sentence = (s: string) => (s ? s[0].toUpperCase() + s.slice(1) + (s.endsWith(".") ? "" : ".") : "");

function Loaded({ d, all, can }: { d: CustomDomain; all: CustomDomain[]; can: Can }) {
  const tenant = useTenantName();
  const name = nameOf(d);
  const navigate = useNavigate();
  const endpoints = useTenantEndpoints(tenant);
  const uses: Uses = { ...endpoints, items: usesOf(d, all, endpoints.items) };
  const set = useDomainActions(d, uses, can, () => void navigate({ to: "/tenants/$tenant/domains", params: { tenant } }));
  const state = domainState(d);
  const editProjects = set.actions.find((a) => a.label === "Edit projects")?.onSelect;
  return (
    <>
      <DetailHead
        name={domainOf(d)}
        kind="Custom domain"
        facts={
          <>
            <StateText tone={state.tone} label={state.label} />
            <span className="font-mono text-[12.5px]">{name}</span>
            <span>
              <span className="text-subtle">Projects</span> <ProjectsText d={d} tenant={tenant} />
            </span>
          </>
        }
        actions={set.actions.length > 0 && <HeadActions set={set} />}
      />
      <DetailBody
        main={
          <>
            {state.tone !== "ok" && <WaitingCard d={d} can={can} />}
            <RecordsCard d={d} />
            <Card>
              <CardHeader
                title="Projects"
                action={
                  editProjects && (
                    <Button size="sm" variant="ghost" onClick={editProjects} className="-mr-2">
                      <FolderCog /> Edit projects
                    </Button>
                  )
                }
              />
              <AllowedProjects d={d} tenant={tenant} />
            </Card>
            <UsesCard uses={uses} />
            <ConditionsCard conditions={d.status?.conditions ?? []} />
          </>
        }
        side={
          <>
            <Card>
              <CardHeader title="Summary" />
              <dl className="divide-y px-4">
                <Field label="Domain" mono>
                  {domainOf(d)}
                </Field>
                <Field label="Name" mono>
                  {name}
                </Field>
                <Field label="Tenant" mono>
                  {d.spec?.tenant}
                </Field>
                <LabelsField labels={d.metadata?.labels ?? {}} onEdit={set.editLabels?.onSelect} />
                <Field label="Created">{date(d.metadata?.createTime)?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
                <Field label="UID" mono>
                  <span className="text-muted">{d.metadata?.uid}</span>
                </Field>
              </dl>
            </Card>
            <EquivalentCard
              cli={[
                `hangar get cdom ${name}`,
                ...(can(domainPerms.apply) ? [`hangar apply -f ${name}.yaml`, ...(checkable(d) ? [`hangar check customdomain ${name}`] : [])] : []),
                ...(can(domainPerms.delete) ? [`hangar delete cdom ${name}`] : []),
              ]}
              yaml={customDomainYaml(d)}
            />
          </>
        }
      />
      {set.dialogs}
    </>
  );
}

/**
 * Whether checking now can change the domain's state: it is waiting for its
 * record, or the last lookup failed. One another tenant holds stays so
 * whatever its DNS says, and a verified one stays verified.
 */
const checkable = (d: CustomDomain) => domainState(d).tone !== "ok" && domainState(d).reason !== "HeldByAnotherTenant";

/**
 * Why the domain is not verified, as its Verified condition says, with
 * Check now where looking again can change that and the caller may apply
 * the domain. The API answers with the domain as the check left it, and the
 * card shows that answer's message: what the lookup found.
 */
function WaitingCard({ d, can }: { d: CustomDomain; can: Can }) {
  const state = domainState(d);
  const check = useCheckDomain(nameOf(d));
  return (
    <Card>
      <CardHeader
        title={state.label}
        action={
          checkable(d) &&
          can(domainPerms.apply) && (
            <Button size="sm" onClick={() => check.mutate()} disabled={check.isPending} className="-mr-2">
              <RefreshCw /> {check.isPending ? "Checking…" : "Check now"}
            </Button>
          )
        }
      />
      <div className="px-4 py-3 text-[13px]">
        {state.reason && <span className="font-mono text-[12px] text-muted">{state.reason}</span>}
        {state.message && <p className="mt-0.5">{sentence(state.message)}</p>}
        <p className="mt-1.5 text-[12.5px] text-muted">
          {state.reason === "HeldByAnotherTenant"
            ? "Nothing serves under it while another tenant holds it."
            : "Checked every minute; endpoints under it serve once it is verified."}
        </p>
        {check.error != null && (
          <Notice tone="error" title="Checking the domain failed" className="mt-3">
            {errorMessage(check.error)}
          </Notice>
        )}
      </div>
    </Card>
  );
}

/** The records the domain's DNS needs, each name and value copyable. */
function RecordsCard({ d }: { d: CustomDomain }) {
  const records = d.status?.records ?? [];
  return (
    <Card className="overflow-hidden">
      <CardHeader title="DNS records" />
      {records.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-muted">Not given yet.</p>
      ) : (
        <>
          <p className="border-b px-4 py-2.5 text-[12.5px] text-muted">DNS only, no proxy (in Cloudflare, the grey cloud): behind a proxy no certificate is issued.</p>
          <ul className="divide-y md:hidden">
            {records.map((r) => (
              <li key={r.name + r.type} className="px-4 py-2.5 text-[13px]">
                <div className="flex items-center gap-1">
                  <span className="min-w-0 font-mono text-[12.5px]">
                    <AtDots text={r.name} />
                  </span>
                  <CopyButton text={r.name} className="shrink-0" />
                  <span className="shrink-0 font-mono text-[11.5px] text-subtle">{r.type}</span>
                </div>
                <div className="mt-0.5 flex items-center gap-1">
                  <span className="min-w-0 font-mono text-[12.5px] text-muted">
                    <AtDots text={r.value} />
                  </span>
                  <CopyButton text={r.value} className="shrink-0" />
                </div>
                <p className="mt-0.5 text-[12px] text-subtle">{sentence(r.purpose)}</p>
              </li>
            ))}
          </ul>
          <table className="hidden w-full text-[13px] md:table">
            <thead>
              <tr className={theadRow}>
                <Th>Name</Th>
                <Th>Type</Th>
                <Th>Value</Th>
              </tr>
            </thead>
            {records.map((r) => (
              <tbody key={r.name + r.type} className="border-b last:border-0">
                <tr className="align-top">
                  <td className="px-4 pt-2">
                    <span className="flex items-start gap-1">
                      <span className="min-w-0 pt-0.5 font-mono text-[12.5px]">
                        <AtDots text={r.name} />
                      </span>
                      <CopyButton text={r.name} className="shrink-0" />
                    </span>
                  </td>
                  <td className="px-4 pt-2.5 font-mono text-[12.5px] text-muted">{r.type}</td>
                  <td className="px-4 pt-2">
                    <span className="flex items-start gap-1">
                      <span className="min-w-0 pt-0.5 font-mono text-[12.5px]">
                        <AtDots text={r.value} />
                      </span>
                      <CopyButton text={r.value} className="shrink-0" />
                    </span>
                  </td>
                </tr>
                <tr>
                  <td colSpan={3} className="px-4 pb-2.5 pt-0.5 text-[12px] text-muted">
                    {sentence(r.purpose)}
                  </td>
                </tr>
              </tbody>
            ))}
          </table>
        </>
      )}
    </Card>
  );
}

function AllowedProjects({ d, tenant }: { d: CustomDomain; tenant: string }) {
  const { projects } = useTenantProjects(tenant);
  const names = d.spec?.projects ?? [];
  const all = names.length === 0;
  const shown = all ? projects.map((p) => p.metadata?.name ?? "") : names;
  return (
    <div className="px-4 py-3 text-[13px]">
      {all && <p className="mb-2 text-muted">All projects, including those made later.</p>}
      <ul className="flex flex-wrap gap-1.5">
        {shown.map((n) => (
          <li key={n}>
            <Link to="/projects/$project" params={{ project: n }} className="inline-flex items-baseline gap-1.5 rounded bg-surface-2 px-1.5 py-0.5 hover:text-accent">
              {projectLabel(projects.find((p) => p.metadata?.name === n), n)}
              <span className="font-mono text-[11px] text-subtle">{n}</span>
            </Link>
          </li>
        ))}
      </ul>
    </div>
  );
}

/** The endpoints with hostnames under the domain, each hostname serving or waiting. */
function UsesCard({ uses }: { uses: Uses }) {
  return (
    <Card>
      <CardHeader
        title={
          <span className="flex items-baseline gap-2">
            Endpoints
            {!uses.loading && !uses.error && (
              <span className="font-mono text-[11.5px] font-normal text-subtle">{new Set(uses.items.map((u) => endpointKey(u.endpoint))).size}</span>
            )}
          </span>
        }
      />
      {uses.loading ? (
        <div aria-busy="true" aria-label="Loading" className="px-4 py-3">
          <Skeleton className="h-3 w-48" />
        </div>
      ) : uses.error ? (
        <ErrorNotice error={uses.error} title="The tenant's endpoints did not load" onRetry={uses.refetch} />
      ) : uses.items.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-muted">No endpoints.</p>
      ) : (
        <ul className="divide-y">
          {uses.items.map((u) => (
            <li key={u.hostname} className="flex flex-wrap items-center gap-x-3 gap-y-0.5 px-4 py-2.5 text-[13px]">
              <Led tone={u.serving ? "ok" : "pending"} />
              <span className="min-w-0 flex-1">
                {u.serving ? (
                  <a href={`https://${u.hostname}`} target="_blank" rel="noreferrer" className="inline-flex max-w-full items-center gap-1 text-accent hover:underline">
                    <span className="truncate font-mono text-[12.5px]">{u.hostname}</span>
                    <ArrowUpRight className="size-3 shrink-0" />
                  </a>
                ) : (
                  <span className="font-mono text-[12.5px]">{u.hostname}</span>
                )}
              </span>
              <EndpointLink e={u.endpoint} />
              <span className="w-14 shrink-0 text-right text-[12px] text-subtle">{u.serving ? "serving" : "waiting"}</span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

const endpointKey = (e: PublicEndpoint) => `${e.metadata?.project}/${nameOf(e)}`;

function EndpointLink({ e }: { e: PublicEndpoint }): ReactNode {
  const project = e.metadata?.project ?? "";
  return (
    <span className="whitespace-nowrap text-[12.5px]">
      <Link to="/projects/$project/public-endpoints/$name" params={{ project, name: nameOf(e) }} className="font-medium text-accent hover:underline">
        {nameOf(e)}
      </Link>
      <span className="ml-1.5 font-mono text-[11.5px] text-subtle">{project}</span>
    </span>
  );
}

/**
 * Edit projects: all the tenant's projects, or those chosen. It applies the
 * domain as it opened on, with spec.projects alone changed, at that
 * resource version: a newer domain is never written over, and Reload
 * starts the dialog again from it. The API refuses to leave out a project
 * an endpoint of which has a hostname under the domain, so the dialog
 * states that and Apply is off.
 */
function EditProjectsDialog({ d, uses, onClose }: { d: CustomDomain; uses: Uses; onClose: () => void }) {
  const [base, setBase] = useState(d);
  const tenant = base.spec?.tenant ?? "";
  const { projects } = useTenantProjects(tenant);
  const before = base.spec?.projects ?? [];
  const [all, setAll] = useState(before.length === 0);
  const [chosen, setChosen] = useState<string[]>(before);
  const apply = useApplyObjectAt("customDomain");
  const reload = useReadObject("customDomain", "", nameOf(base));
  const stale = isStale(apply.error);
  const after = all ? [] : projects.map((p) => p.metadata?.name ?? "").filter((n) => chosen.includes(n));
  const label = (n: string) => projectLabel(projects.find((p) => p.metadata?.name === n), n);
  const lets = (list: string[], n: string) => list.length === 0 || list.includes(n);
  const names = projects.map((p) => p.metadata?.name ?? "");
  const added = names.filter((n) => !lets(before, n) && lets(after, n));
  const removed = names.filter((n) => lets(before, n) && !lets(after, n));
  const change = [
    ...(before.length > 0 && after.length === 0 ? ["Lets every project, and those made later"] : added.length ? [`Lets ${added.map(label).join(", ")}`] : []),
    ...(before.length === 0 && after.length > 0
      ? [`Stops letting ${removed.length ? `${removed.map(label).join(", ")} and ` : ""}projects made later`]
      : removed.length
        ? [`Stops letting ${removed.map(label).join(", ")}`]
        : []),
  ].join(" · ");
  const refused = uses.items.filter((u) => removed.includes(u.endpoint.metadata?.project ?? ""));
  const unknown = removed.length > 0 ? usesUnknown(uses) : undefined;
  const obj = clone(CustomDomainSchema, base);
  if (obj.spec) obj.spec.projects = after;
  const startAgain = () =>
    reload.mutate(undefined, {
      onSuccess: (fresh) => {
        setBase(fresh);
        const p = fresh.spec?.projects ?? [];
        setAll(p.length === 0);
        setChosen(p);
        apply.reset();
      },
    });
  const problems = all || after.length > 0 ? [] : ["No project is chosen."];
  return (
    <Dialog open onClose={onClose} title={`Edit projects of ${domainOf(d)}`} wide>
      <div role="radiogroup" aria-label="Projects" className="grid grid-cols-1 gap-2 sm:grid-cols-2">
        <Choice selected={all} onClick={() => setAll(true)} label="All projects" className="p-3">
          <div className="text-[13px] font-medium text-foreground">All projects</div>
          <div className="mt-0.5 text-[12px] leading-snug">Including projects made later.</div>
        </Choice>
        <Choice selected={!all} onClick={() => setAll(false)} label="Chosen projects" className="p-3">
          <div className="text-[13px] font-medium text-foreground">Chosen projects</div>
          <div className="mt-0.5 text-[12px] leading-snug">Only the projects chosen below.</div>
        </Choice>
      </div>
      {!all && <ProjectChecklist projects={names} label={label} chosen={chosen} onChange={setChosen} className="mt-3" />}
      <div className="mt-4 text-[12.5px] text-foreground">{change || "No change"}</div>
      <FormEquivalent yaml={customDomainYaml(obj)} cli={[`hangar apply -f ${nameOf(d)}.yaml`]} />
      {refused.length > 0 && (
        <Notice
          tone="warning"
          className="mt-3"
          title={`${refused.map((u) => `${nameOf(u.endpoint)} of ${label(u.endpoint.metadata?.project ?? "")} serves at ${u.hostname}`).join("; ")}, under this domain.`}
        >
          A project cannot be left out while an endpoint of it serves under the domain.
        </Notice>
      )}
      {unknown && <Notice tone="warning" className="mt-3" title={unknown} />}
      {stale && <StaleNotice title={`${nameOf(base)} changed since this dialog opened.`} reloading={reload.isPending} error={reload.error} onReload={startAgain} />}
      <FormFooter
        problems={problems}
        blocked={!change || refused.length > 0 || !!unknown || stale}
        error={stale ? null : apply.error}
        failed="Changing the projects failed"
        pending={apply.isPending}
        pendingLabel="Applying…"
        label={
          <>
            <FolderCog /> Apply
          </>
        }
        onCancel={onClose}
        onSubmit={() => apply.mutate(obj, { onSuccess: onClose })}
      />
    </Dialog>
  );
}

/** The tenant's projects as checkboxes, each with its display name and name. */
export function ProjectChecklist({
  projects,
  label,
  chosen,
  onChange,
  className,
}: {
  projects: string[];
  label: (name: string) => string;
  chosen: string[];
  onChange: (chosen: string[]) => void;
  className?: string;
}) {
  return (
    <fieldset className={className}>
      <legend className="sr-only">Projects</legend>
      <div className="divide-y rounded-md border">
        {projects.map((n) => (
          <label key={n} className="flex cursor-pointer items-center gap-2.5 px-3 py-2 hover:bg-surface-2/60">
            <input
              type="checkbox"
              checked={chosen.includes(n)}
              onChange={(e) => onChange(e.target.checked ? [...chosen, n] : chosen.filter((x) => x !== n))}
              className="accent-[var(--accent)]"
            />
            <span className="text-[13px] text-foreground">{label(n)}</span>
            <span className="font-mono text-[11.5px] text-subtle">{n}</span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}
