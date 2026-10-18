// A project's public endpoints, one endpoint, and making one or changing
// who may use it (docs/decisions/public-endpoints.md). An endpoint a
// container's public port made is the container's: the API sets its spec
// back and makes it again, so it offers no edit and no delete here.

import { create } from "@bufbuild/protobuf";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  PublicEndpointAccess,
  PublicEndpointSchema,
  PublicEndpointSpecSchema,
  type PublicEndpoint,
} from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { ArrowUpRight, Globe, Lock, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { Dialog } from "@/components/dialog";
import { DeleteDialog, FormFooter } from "@/components/resource";
import { FormEquivalent, FormField, Input, Select } from "@/components/form";
import { ConditionsCard, EquivalentCard, Labels } from "@/components/object";
import { DetailBody, DetailFrame, DetailHead, ListFrame, NameFilter, PageHead } from "@/components/page";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Topbar, useProjectName } from "@/components/shell";
import { Button, Card, CardHeader, CopyButton, Field, Led } from "@/components/ui";
import { cn } from "@/lib/cn";
import { checkName, type KnownNames } from "@/lib/create";
import { date, machinePath, useMachines, type MachineKind } from "@/lib/machines";
import { accessLabel, conditionOf, endpointYaml, kindNoun, lightOf, nameOf, ownerOf, targetOf, usePublicEndpoints } from "@/lib/network";
import { permission, useChange, useCreate } from "@/lib/network-actions";
import { useCan } from "@/lib/permissions";
import { useCreating, type NewProps, type OnCreated } from "@/lib/new-param";

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

export function PublicEndpointList(props: NewProps) {
  const project = useProjectName();
  const eps = usePublicEndpoints(project);
  const [query, setQuery] = useState("");
  const rows = eps.items.filter((e) => !query || nameOf(e).includes(query));
  const { can: may, settled } = useCan({ level: "project", name: project }, [permission("publicEndpoint", "apply")]);
  const [creating, setCreating, created] = useCreating(props, may(permission("publicEndpoint", "apply")), settled);
  const newButton = may(permission("publicEndpoint", "apply")) ? (
    <Button variant="primary" onClick={() => setCreating(true)}>
      <Plus /> New public endpoint
    </Button>
  ) : undefined;
  return (
    <>
      <Topbar crumbs={["Public endpoints"]} />
      <PageHead
        title="Public endpoints"
        description="One TCP port of a machine, served over HTTPS at a hostname of its own. Private endpoints take only those who may use the project's endpoints."
        action={newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ ...eps, count: eps.items.length }}
          noun="public endpoints"
          empty={{ title: "No public endpoints", text: "A container port marked public makes one; a virtual machine's are made here or with apply.", action: newButton }}
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
                  </tr>
                ))}
                {rows.length === 0 && <NoMatch noun="public endpoint" colSpan={6} />}
              </tbody>
            </table>
          </Card>
          <p className="mt-3 text-[12px] text-subtle">
            {rows.length} of {eps.items.length}
          </p>
        </ListFrame>
      </div>
      <EndpointDialog
        key={String(creating)}
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={created}
        known={{ loading: eps.loading, error: eps.error, names: eps.items.map(nameOf) }}
      />
    </>
  );
}

export function PublicEndpointDetail({ name }: { name: string }) {
  const project = useProjectName();
  const eps = usePublicEndpoints(project);
  const [deleting, setDeleting] = useState(false);
  const [changing, setChanging] = useState(false);
  const navigate = useNavigate();
  const { can } = useCan({ level: "project", name: project }, [permission("publicEndpoint", "apply"), permission("publicEndpoint", "delete")]);
  const mayApply = can(permission("publicEndpoint", "apply"));
  const mayDelete = can(permission("publicEndpoint", "delete"));
  const e = eps.items.find((x) => nameOf(x) === name);
  const owner = e && ownerOf(e);
  const ready = conditionOf(e?.status?.conditions, "Ready");
  return (
    <DetailFrame crumbs={["Public endpoints", name]} state={eps} found={!!e} noun="public endpoint" name={name}>
      {e && (
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
            actions={
              !owner && (mayApply || mayDelete) ? (
                <>
                  {mayApply && (
                    <Button onClick={() => setChanging(true)}>
                      {e.spec?.access === PublicEndpointAccess.PUBLIC ? <Lock /> : <Globe />}
                      {e.spec?.access === PublicEndpointAccess.PUBLIC ? "Make private" : "Make public"}
                    </Button>
                  )}
                  {mayDelete && (
                    <Button variant="danger" onClick={() => setDeleting(true)}>
                      <Trash2 /> Delete
                    </Button>
                  )}
                </>
              ) : undefined
            }
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
                      <span className="block text-[12px] text-subtle">plain HTTP on its first interface, eth0</span>
                    </Field>
                    <Field label="Access">
                      <AccessBadge access={e.spec?.access ?? 0} />
                      <span className="block text-[12px] text-muted">
                        {e.spec?.access === PublicEndpointAccess.PUBLIC
                          ? "Anyone on the internet, with no sign-in."
                          : "Only those who may use this project's endpoints, signed in through Hangar or holding a Hangar token."}
                      </span>
                    </Field>
                    {owner && (
                      <Field label="Made by">
                        <Link to="/projects/$project/containers/$name" params={{ project, name: owner }} className="text-accent hover:underline">
                          {owner}
                        </Link>
                        <span className="block text-[12px] text-muted">
                          A public port of the container. The endpoint follows that port and goes with it.
                        </span>
                      </Field>
                    )}
                    <Field label="Machine" mono>
                      {e.status?.machine || <span className="font-sans text-subtle">none</span>}
                    </Field>
                    <Field label="Host" mono>
                      {e.status?.node || <span className="font-sans text-subtle">not placed</span>}
                    </Field>
                    <Field label="Labels">
                      <Labels labels={e.metadata?.labels ?? {}} />
                    </Field>
                    <Field label="Created">{date(e.metadata?.createTime)?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
                    <Field label="UID" mono>
                      <span className="text-muted">{e.metadata?.uid}</span>
                    </Field>
                  </dl>
                </Card>
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
          {!owner && (
            <>
              <AccessDialog key={String(changing)} open={changing} onClose={() => setChanging(false)} endpoint={e} />
              <DeleteDialog
                kind="publicEndpoint"
                name={name}
                open={deleting}
                onClose={() => setDeleting(false)}
                after={() => void navigate({ to: "/projects/$project/public-endpoints", params: { project } })}
                consequence={`Its connections close. Its hostname stays claimed by this project: an endpoint named ${name} made here again takes it back, and no other ever does.`}
              />
            </>
          )}
        </>
      )}
    </DetailFrame>
  );
}

function EndpointDialog({ open, onClose, onCreated, known }: { open: boolean; onClose: () => void; onCreated: OnCreated; known: KnownNames }) {
  const project = useProjectName();
  const navigate = useNavigate();
  const submit = useCreate("publicEndpoint", project);
  const all = useMachines(project);
  const [name, setName] = useState("");
  const [choice, setChoice] = useState("");
  const [port, setPort] = useState("8080");
  const [access, setAccess] = useState(PublicEndpointAccess.PRIVATE);
  const first = all.items[0];
  const target = choice || (first ? `${first.kind}/${first.name}` : "");
  const [kind, machine] = target.split("/") as [MachineKind, string];
  const obj = create(PublicEndpointSchema, {
    metadata: { name },
    spec: {
      target: machine ? { object: { case: kind === "Container" ? "container" : "virtualMachine", value: machine } } : undefined,
      port: Number(port) || 0,
      access,
    },
  });
  const check = checkName(name, "public endpoint", known);
  const problems = [
    ...(check.ok ? [] : [check.reason]),
    ...(machine ? [] : ["No machine is chosen."]),
    ...(Number(port) >= 1 && Number(port) <= 65535 ? [] : ["The port is not between 1 and 65535."]),
  ];
  const close = () => {
    submit.reset();
    onClose();
  };
  return (
    <Dialog open={open} onClose={close} title="New public endpoint">
      <FormField label="Name" hint="The endpoint's hostname is made from its name and the project's, and is claimed for good.">
        <Input value={name} onChange={(ev) => setName(ev.target.value.toLowerCase())} spellCheck={false} />
      </FormField>
      <div className="mt-4 grid grid-cols-[1fr_96px] gap-2">
        <FormField label="Machine" className="mt-0">
          <Select value={target} onChange={(ev) => setChoice(ev.target.value)}>
            {all.items.map((m) => (
              <option key={`${m.kind}/${m.name}`} value={`${m.kind}/${m.name}`}>
                {m.name} ({kindNoun(m.kind)})
              </option>
            ))}
          </Select>
        </FormField>
        <FormField label="Port" className="mt-0">
          <Input value={port} onChange={(ev) => setPort(ev.target.value.replace(/\D/g, ""))} inputMode="numeric" />
        </FormField>
      </div>
      <span className="mt-1 block text-[11.5px] text-subtle">The TCP port the machine serves plain HTTP on, on eth0.</span>
      <AccessChoice value={access} onChange={setAccess} />
      <FormEquivalent yaml={endpointYaml(obj)} cli={[`hangar apply --create-only -p ${project} -f ${name || "endpoint"}.yaml`]} />
      <FormFooter
        problems={problems}
        error={submit.error}
        failed="Creating the public endpoint failed"
        pending={submit.isPending}
        pendingLabel="Creating…"
        label={
          <>
            <Plus /> Create public endpoint
          </>
        }
        onCancel={close}
        onSubmit={() =>
          submit.mutate(obj, {
            onSuccess: () => onCreated(() => void navigate({ to: "/projects/$project/public-endpoints/$name", params: { project, name } })),
          })
        }
      />
    </Dialog>
  );
}

function AccessChoice({ value, onChange }: { value: PublicEndpointAccess; onChange: (a: PublicEndpointAccess) => void }) {
  const options = [
    { id: PublicEndpointAccess.PRIVATE, icon: Lock, label: "Private", text: "Those who may use this project's endpoints, signed in." },
    { id: PublicEndpointAccess.PUBLIC, icon: Globe, label: "Public", text: "Anyone on the internet, with no sign-in." },
  ];
  return (
    <div role="radiogroup" aria-label="Access" className="mt-3">
      <span className="text-[12.5px] font-medium text-foreground">Access</span>
      <div className="mt-1.5 grid grid-cols-2 gap-2">
        {options.map((o) => {
          const Icon = o.icon;
          const on = value === o.id;
          return (
            <button
              key={o.id}
              role="radio"
              aria-checked={on}
              onClick={() => onChange(o.id)}
              className={cn(
                "rounded-md border px-3 py-2 text-left",
                on ? "border-accent bg-accent-soft" : "border-border-strong hover:bg-surface-2",
              )}
            >
              <span className="flex items-center gap-1.5 text-[12.5px] font-medium text-foreground">
                <Icon className={cn("size-3.5", on ? "text-accent" : "text-subtle")} /> {o.label}
              </span>
              <span className="mt-0.5 block text-[11.5px] leading-snug text-muted">{o.text}</span>
            </button>
          );
        })}
      </div>
    </div>
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
  return (
    <Dialog open={open} onClose={close} title={toPublic ? `Make ${nameOf(endpoint)} public` : `Make ${nameOf(endpoint)} private`}>
      <p>
        {toPublic
          ? "Anyone on the internet reaches it, with no sign-in. Its open connections close."
          : "Only those who may use this project's endpoints reach it. Its open connections close."}
      </p>
      <FormEquivalent yaml={endpointYaml(obj)} cli={[`hangar apply -p ${project} -f ${nameOf(endpoint)}.yaml`]} />
      <FormFooter
        problems={[]}
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

