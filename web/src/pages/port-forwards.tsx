// A project's port forwards and one port forward: a port drawn at random on
// the platform's shared IPv4 address, forwarded to a port of one interface
// of a machine. Creating one is pages/create-port-forward.tsx. The data is
// the placeholder behind lib/port-forwards.ts until the API kind exists.

import { Link } from "@tanstack/react-router";
import { Plus, ShieldAlert } from "lucide-react";
import { useState } from "react";
import { ConditionsCard, EquivalentCard, LabelsField } from "@/components/object";
import { HeadActions, ObjectRowMenu, type Can } from "@/components/object-actions";
import { DetailBody, DetailFrame, DetailHead, ListFrame, NameFilter, PageHead } from "@/components/page";
import {
  AddressCell,
  ForwardStateCell,
  ForwardTargetText,
  InboundBrief,
  InboundNotice,
  useInbound,
  usePortForwardActions,
} from "@/components/port-forwards";
import { CommandLine } from "@/components/form";
import { Topbar, useProjectName } from "@/components/shell";
import { Skeleton } from "@/components/states";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Badge, buttonVariants, Card, CardHeader, CopyButton, Field, Led } from "@/components/ui";
import {
  examplesOf,
  portForwardPerms,
  portForwardYaml,
  portLabel,
  publicAddress,
  stateOf,
  usePortForwardCan,
  usePortForwardQuota,
  usePortForwards,
  type PortForward,
} from "@/lib/port-forwards";

const day = (d?: Date) => d?.toISOString().slice(0, 10);

/** A forward's row menu: the actions of its page. */
function ForwardRowMenu({ pf, can }: { pf: PortForward; can: Can }) {
  return <ObjectRowMenu name={pf.metadata.name} set={usePortForwardActions(pf, can)} />;
}

/** The state cell, with what the target's groups do to the forward's traffic under it. */
function StateWithInbound({ pf }: { pf: PortForward }) {
  const project = useProjectName();
  const inbound = useInbound(project, pf.spec.target, pf.spec.protocol, pf.spec.targetPort);
  return (
    <>
      <ForwardStateCell pf={pf} />
      <InboundBrief inbound={inbound} />
    </>
  );
}

/** On a phone, what the groups do to the forward's traffic, under its address. */
function PhoneInbound({ pf }: { pf: PortForward }) {
  const project = useProjectName();
  return <InboundBrief inbound={useInbound(project, pf.spec.target, pf.spec.protocol, pf.spec.targetPort)} />;
}

export function PortForwardList() {
  const project = useProjectName();
  const forwards = usePortForwards(project);
  const quota = usePortForwardQuota(project);
  const [query, setQuery] = useState("");
  const rows = forwards.items.filter((f) => !query || f.metadata.name.includes(query));
  const { can } = usePortForwardCan(project);
  const newButton = can(portForwardPerms.apply) ? (
    <Link to="/projects/$project/new/port-forward" params={{ project }} className={buttonVariants({ variant: "primary" })}>
      <Plus /> New port forward
    </Link>
  ) : undefined;
  const full = quota.limit !== undefined && quota.used >= quota.limit;
  return (
    <>
      <Topbar crumbs={["Port forwards"]} />
      <PageHead
        title="Port forwards"
        description="Ports of the platform's shared IPv4 address, each drawn at random and kept until deleted, forwarded to a port of one interface. The interface's security groups still decide what gets in."
        action={newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ ...forwards, count: forwards.items.length }}
          noun="port forwards"
          empty={{
            title: "No port forwards",
            text: (
              <>
                A port forward opens one TCP or UDP port of a machine to the internet over IPv4, on a port drawn at random on the platform's shared
                address. A website over HTTPS is served by a public endpoint, at a hostname; and every interface has a public IPv6 address of its own.
              </>
            ),
            action: newButton,
          }}
        >
          <NameFilter value={query} onChange={setQuery} />
          <Card className="md:hidden">
            <ul className="divide-y">
              {rows.map((f) => (
                <li key={f.metadata.name} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                  <Link to="/projects/$project/port-forwards/$name" params={{ project, name: f.metadata.name }} className="flex min-w-0 flex-1 items-center gap-3">
                    <span className="min-w-0 flex-1">
                      <span className="block truncate font-medium">{f.metadata.name}</span>
                      <span className="block truncate font-mono text-[11.5px] text-muted">{publicAddress(f) ?? "port not drawn yet"}</span>
                      <span className="block truncate font-mono text-[11.5px] text-subtle">
                        → {f.spec.target.name} · {f.spec.target.iface} · {portLabel(f.spec.targetPort, f.spec.protocol)}
                      </span>
                      <PhoneInbound pf={f} />
                    </span>
                    <span className="shrink-0 self-start text-right">
                      <ForwardStateCell pf={f} />
                    </span>
                  </Link>
                  <ForwardRowMenu pf={f} can={can} />
                </li>
              ))}
              {rows.length === 0 && <NoMatch noun="port forward" />}
            </ul>
          </Card>
          <Card className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[860px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[18%]">Name</Th>
                  <Th>Public address</Th>
                  <Th>Forwards to</Th>
                  <Th>State</Th>
                  <Th>Created</Th>
                  <Th className="w-10 px-0" />
                </tr>
              </thead>
              <tbody>
                {rows.map((f) => (
                  <tr key={f.metadata.name} className={tbodyRow}>
                    <td className="max-w-0 px-4 py-2.5">
                      <Link to="/projects/$project/port-forwards/$name" params={{ project, name: f.metadata.name }} className="block truncate font-medium group-hover:text-accent">
                        {f.metadata.name}
                      </Link>
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <AddressCell pf={f} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <ForwardTargetText t={f.spec.target} protocol={f.spec.protocol} port={f.spec.targetPort} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <StateWithInbound pf={f} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-muted">{day(f.metadata.createTime)}</td>
                    <td className="px-2 py-2.5">
                      <ForwardRowMenu pf={f} can={can} />
                    </td>
                  </tr>
                ))}
                {rows.length === 0 && <NoMatch noun="port forward" colSpan={6} />}
              </tbody>
            </table>
          </Card>
          <p className="mt-3 text-[12px] text-subtle">
            {rows.length} of {forwards.items.length}
            {quota.limit !== undefined && (
              <span className={full ? "text-pending" : undefined}>
                {" "}
                · {full ? `as many as the plan allows, ${quota.limit}` : `the plan allows ${quota.limit}`}
              </span>
            )}
          </p>
        </ListFrame>
      </div>
    </>
  );
}

export function PortForwardDetail({ name }: { name: string }) {
  const project = useProjectName();
  const forwards = usePortForwards(project);
  const { can } = usePortForwardCan(project);
  const pf = forwards.items.find((x) => x.metadata.name === name);
  return (
    <DetailFrame crumbs={["Port forwards", name]} state={forwards} found={!!pf} noun="port forward" name={name}>
      {pf && <Loaded pf={pf} can={can} />}
    </DetailFrame>
  );
}

/**
 * The Connect card: the address with its copy button, and commands that
 * reach it, under what the target's groups do to its traffic; while the
 * port is not drawn, that it is not.
 */
function ConnectCard({ pf }: { pf: PortForward }) {
  const project = useProjectName();
  const { target, protocol, targetPort } = pf.spec;
  const inbound = useInbound(project, target, protocol, targetPort);
  const addr = publicAddress(pf);
  return (
    <Card>
      <CardHeader title="Connect" />
      <div className="px-4 py-4">
        <div className="text-[11.5px] text-subtle">Public address</div>
        {addr ? (
          <div className="mt-1 flex items-center gap-1.5">
            <span className="break-all font-mono text-[20px] font-medium tracking-tight">{addr}</span>
            <CopyButton text={addr} className="shrink-0" />
          </div>
        ) : (
          <div className="mt-1.5 flex items-center gap-3">
            <Led tone="pending" pulse />
            <span className="text-[14px] font-medium">Drawing a public port</span>
            <Skeleton className="h-4 w-40" />
          </div>
        )}
        <p className="mt-1.5 text-[12.5px] text-muted">
          {addr ? "Forwards to " : "It will forward to "}
          <ForwardTargetText t={target} protocol={protocol} port={targetPort} />. The port is drawn at random on the platform's shared IPv4 address and
          kept until the forward is deleted; retargeting keeps it.
        </p>
      </div>
      {inbound && (
        <div className="border-t px-4 py-3">
          <InboundNotice t={target} protocol={protocol} port={targetPort} inbound={inbound} />
        </div>
      )}
      {addr && (
        <>
          <div className="border-t px-4 pb-4 pt-3">
            {examplesOf(pf).map((e) => (
              <div key={e.label}>
                <div className="text-[11.5px] text-subtle">{e.label}</div>
                <CommandLine command={e.command} className="mt-1.5" />
              </div>
            ))}
            {protocol === "TCP" && targetPort === 22 && (
              <p className="mt-2 text-[11.5px] text-subtle">
                <span className="font-mono">user</span> stands for the login the machine takes, such as its image's default user.
              </p>
            )}
          </div>
        </>
      )}
    </Card>
  );
}

/** The page of a forward that has loaded, its actions made once for its head and its labels. */
function Loaded({ pf, can }: { pf: PortForward; can: Can }) {
  const project = useProjectName();
  const name = pf.metadata.name;
  const set = usePortForwardActions(pf, can);
  const s = stateOf(pf);
  const addr = publicAddress(pf);
  const { target, protocol, targetPort } = pf.spec;
  const inbound = useInbound(project, target, protocol, targetPort);
  return (
    <>
      <DetailHead
        name={name}
        kind="Port forward"
        facts={
          <>
            <span className="inline-flex items-center gap-2">
              <Led tone={s.light} pulse={s.pulse} />
              {s.label}
            </span>
            {addr && <span className="font-mono text-[12px]">{addr}</span>}
            <span className="font-mono text-[12px]">
              → {target.name} · {target.iface} · {portLabel(targetPort, protocol)}
            </span>
            {inbound?.known && inbound.verdict === "none" && (
              <span className="inline-flex items-center gap-1.5 text-pending">
                <ShieldAlert className="size-3.5" /> No security group lets it in
              </span>
            )}
          </>
        }
        actions={set.actions.length > 0 && <HeadActions set={set} />}
      />
      <DetailBody
        main={
          <>
            <ConnectCard pf={pf} />
            <Card>
              <CardHeader title="Summary" />
              <dl className="divide-y px-4">
                <Field label="Public address" mono>
                  {addr ? (
                    <span className="flex items-center gap-1">
                      {addr}
                      <CopyButton text={addr} />
                    </span>
                  ) : (
                    <span className="font-sans text-subtle">not drawn yet</span>
                  )}
                </Field>
                <Field label="Target">
                  <ForwardTargetText t={target} protocol={protocol} port={targetPort} />
                </Field>
                <Field label="Protocol">{protocol}</Field>
                <Field label="Target port" mono>
                  {targetPort}
                </Field>
                <Field label="Security groups">
                  {inbound?.known ? (
                    <span className="flex flex-wrap gap-1">
                      {inbound.groups.map((g) =>
                        g === "default" ? (
                          <Badge key={g}>default</Badge>
                        ) : (
                          <Link key={g} to="/projects/$project/security-groups/$name" params={{ project, name: g }}>
                            <Badge className="hover:border-accent hover:text-accent">{g}</Badge>
                          </Link>
                        ),
                      )}
                    </span>
                  ) : (
                    <span className="text-subtle">known once the {target.kind === "Container" ? "container" : "virtual machine"} is made</span>
                  )}
                </Field>
                <Field label="State">
                  <span className="inline-flex items-center gap-2">
                    <Led tone={s.light} pulse={s.pulse} />
                    {s.label}
                    {s.reason && <span className="font-mono text-[12px] text-muted">{s.reason}</span>}
                  </span>
                  {s.message && <span className="block text-[12.5px] text-muted">{s.message}</span>}
                </Field>
                <Field label="Held by" mono>
                  {pf.status.machine || <span className="font-sans text-subtle">no machine</span>}
                </Field>
                <LabelsField labels={pf.metadata.labels} onEdit={set.editLabels?.onSelect} />
                <Field label="Created">{pf.metadata.createTime?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
                <Field label="UID" mono>
                  <span className="text-muted">{pf.metadata.uid}</span>
                </Field>
              </dl>
            </Card>
            <ConditionsCard conditions={pf.status.conditions} />
          </>
        }
        side={
          <EquivalentCard
            cli={[`hangar get portforward ${name} -p ${project}`, `hangar apply -p ${project} -f ${name}.yaml`, `hangar delete portforward ${name} -p ${project}`]}
            yaml={portForwardYaml(pf)}
          />
        }
      />
      {set.dialogs}
    </>
  );
}
