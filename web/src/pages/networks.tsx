// A project's networks and one network (docs/decisions/networking.md);
// pages/create-network.tsx makes one.

import { Link, useNavigate } from "@tanstack/react-router";
import type { Network } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { DeleteDialog, MachinesNotice, type MachinesState, MemberCount, usageBlock } from "@/components/resource";
import { ConditionsCard, EquivalentCard, Labels } from "@/components/object";
import { DetailBody, DetailFrame, DetailHead, ListFrame, NameFilter, PageHead } from "@/components/page";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Topbar, useProjectName } from "@/components/shell";
import { ErrorNotice, SkeletonRows } from "@/components/states";
import { Badge, Button, buttonVariants, Card, CardHeader, CopyButton, Field, Led } from "@/components/ui";
import { permission } from "@/lib/network-actions";
import { useCan } from "@/lib/permissions";
import { date, machinePath } from "@/lib/machines";
import {
  conditionOf,
  gatewayOf,
  lightOf,
  nameOf,
  networkYaml,
  useAttachments,
  useNetworks,
  useSecurityGroups,
  type Attachment,
} from "@/lib/network";
import { bare } from "@/pages/machine-list";
import { NetworkFileSharesCard } from "@/components/file-shares";
import { shareNetworks, sharePerms, useFileShares } from "@/lib/file-shares";

function ReadyCell({ n }: { n: Network }) {
  const ready = conditionOf(n.status?.conditions, "Ready");
  return (
    <span className="inline-flex items-center gap-2">
      <Led tone={lightOf(ready)} />
      {ready?.status === 1 ? "Ready" : "Pending"}
    </span>
  );
}

export function NetworkList() {
  const project = useProjectName();
  const nets = useNetworks(project);
  const att = useAttachments(project);
  const [query, setQuery] = useState("");
  const rows = nets.items.filter((n) => !query || nameOf(n).includes(query));
  const machinesOn = (n: string) => new Set(att.items.filter((a) => a.network === n).map((a) => `${a.kind}/${a.name}`)).size;
  const { can: may } = useCan({ level: "project", name: project }, [permission("network", "apply")]);
  const newButton = may(permission("network", "apply")) ? (
    <Link to="/projects/$project/new/network" params={{ project }} className={buttonVariants({ variant: "primary" })}>
      <Plus /> New network
    </Link>
  ) : undefined;
  return (
    <>
      <Topbar crumbs={["Networks"]} />
      <PageHead
        title="Networks"
        description="Private networks, each with its own IPv4 range. A machine names its networks in its interfaces."
        action={newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ ...nets, count: nets.items.length }}
          noun="networks"
          empty={{ title: "No networks", text: "The project's default network is made again shortly after it is deleted.", action: newButton }}
        >
          <NameFilter value={query} onChange={setQuery} />
          <MachinesNotice state={att} />
          <Card className="md:hidden">
            <ul className="divide-y">
              {rows.map((n) => (
                <li key={nameOf(n)}>
                  <Link to="/projects/$project/networks/$name" params={{ project, name: nameOf(n) }} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                    <span className="min-w-0 flex-1">
                      <span className="block truncate font-medium">{nameOf(n)}</span>
                      <span className="block truncate font-mono text-[11.5px] text-subtle">{n.spec?.ipv4Cidr}</span>
                    </span>
                    <span className="shrink-0 text-muted">
                      <MemberCount state={att} n={machinesOn(nameOf(n))} word="machine" />
                    </span>
                    <ReadyCell n={n} />
                  </Link>
                </li>
              ))}
              {rows.length === 0 && <NoMatch noun="network" />}
            </ul>
          </Card>
          <Card className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[720px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[24%]">Name</Th>
                  <Th>State</Th>
                  <Th>IPv4 range</Th>
                  <Th>Gateway</Th>
                  <Th>Machines</Th>
                  <Th>Created</Th>
                </tr>
              </thead>
              <tbody>
                {rows.map((n) => (
                  <tr key={nameOf(n)} className={tbodyRow}>
                    <td className="max-w-0 px-4 py-2.5">
                      <Link to="/projects/$project/networks/$name" params={{ project, name: nameOf(n) }} className="block truncate font-medium group-hover:text-accent">
                        {nameOf(n)}
                      </Link>
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <ReadyCell n={n} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px]">{n.spec?.ipv4Cidr}</td>
                    <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px] text-muted">{gatewayOf(n.spec?.ipv4Cidr ?? "")}</td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <MemberCount state={att} n={machinesOn(nameOf(n))} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-muted">{date(n.metadata?.createTime)?.toISOString().slice(0, 10)}</td>
                  </tr>
                ))}
                {rows.length === 0 && <NoMatch noun="network" colSpan={6} />}
              </tbody>
            </table>
          </Card>
          <p className="mt-3 text-[12px] text-subtle">
            {rows.length} of {nets.items.length}
          </p>
        </ListFrame>
      </div>
    </>
  );
}

/** The machine interfaces on a network, or naming a group. */
export function AttachmentsCard({ title, items, state, empty }: { title: string; items: Attachment[]; state: MachinesState; empty: string }) {
  const project = useProjectName();
  const loaded = !state.loading && !state.error;
  return (
    <Card>
      <CardHeader title={<>{title} {loaded && <span className="ml-1 font-mono text-[11.5px] font-normal text-subtle">{items.length}</span>}</>} />
      {state.loading ? (
        <SkeletonRows rows={2} />
      ) : state.error ? (
        <ErrorNotice error={state.error} title="The project's machines did not load" onRetry={state.refetch} />
      ) : items.length === 0 ? (
        <div className="px-4 py-3 text-[13px] text-muted">{empty}</div>
      ) : (
        <>
          <ul className="divide-y md:hidden">
            {items.map((a) => (
              <li key={`${a.kind}/${a.name}/${a.iface}`} className="px-4 py-2.5 text-[13px]">
                <div className="flex items-baseline justify-between gap-3">
                  <Link to={machinePath(a.kind)} params={{ project, name: a.name }} className="truncate font-medium text-accent hover:underline">
                    {a.name}
                  </Link>
                  <span className="font-mono text-[12px] text-muted">{a.iface}</span>
                </div>
                <div className="mt-1 font-mono text-[12px]">{a.ipv4 ? bare(a.ipv4) : "–"}</div>
                {a.ipv6 && <div className="break-all font-mono text-[12px] text-subtle">{bare(a.ipv6)}</div>}
              </li>
            ))}
          </ul>
          <div className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[640px] text-[13px]">
              <thead>
                <tr className="border-b text-left text-[11.5px] text-subtle">
                  <Th>Machine</Th>
                  <Th>Interface</Th>
                  <Th>IPv4</Th>
                  <Th>Public IPv6</Th>
                  <Th>Security groups</Th>
                </tr>
              </thead>
              <tbody>
                {items.map((a) => (
                  <tr key={`${a.kind}/${a.name}/${a.iface}`} className="border-b last:border-0">
                    <td className="px-4 py-2.5">
                      <Link to={machinePath(a.kind)} params={{ project, name: a.name }} className="font-medium text-accent hover:underline">
                        {a.name}
                      </Link>
                      <span className="ml-2 text-[11.5px] text-subtle">{a.kind === "Container" ? "container" : "VM"}</span>
                    </td>
                    <td className="px-4 py-2.5 font-mono text-[12.5px]">{a.iface}</td>
                    <td className="px-4 py-2.5 font-mono text-[12.5px]">{a.ipv4 ? bare(a.ipv4) : <span className="text-subtle">–</span>}</td>
                    <td className="px-4 py-2.5 font-mono text-[12.5px]">{a.ipv6 ? bare(a.ipv6) : <span className="text-subtle">–</span>}</td>
                    <td className="px-4 py-2.5">
                      <span className="flex flex-wrap gap-1">
                        {a.securityGroups.map((g) => (
                          <Badge key={g}>{g}</Badge>
                        ))}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </Card>
  );
}

export function NetworkDetail({ name }: { name: string }) {
  const project = useProjectName();
  const nets = useNetworks(project);
  const att = useAttachments(project);
  const groups = useSecurityGroups(project);
  const [deleting, setDeleting] = useState(false);
  const navigate = useNavigate();
  // One answer for the page: its Delete, and its File shares card's New file share.
  const { can: may } = useCan({ level: "project", name: project }, [permission("network", "delete"), sharePerms.apply]);
  const n = nets.items.find((x) => nameOf(x) === name);
  const on = att.items.filter((a) => a.network === name);
  const machines = new Set(on.map((a) => `${a.kind}/${a.name}`)).size;
  // The API refuses to delete a network a file share is on, as one a machine is on.
  const shares = useFileShares(project);
  const sharesOn = shares.items.filter((s) => shareNetworks(s).includes(name)).length;
  const cidr = n?.spec?.ipv4Cidr ?? "";
  const ready = conditionOf(n?.status?.conditions, "Ready");
  return (
    <DetailFrame
      crumbs={["Networks", name]}
      state={nets}
      found={!!n}
      noun="network"
      name={name}
    >
      {n && (
        <>
          <DetailHead
            name={name}
            kind="Network"
            facts={
              <>
                <span className="inline-flex items-center gap-2">
                  <Led tone={lightOf(ready)} />
                  {ready?.status === 1 ? "Ready" : "Pending"}
                </span>
                <span className="font-mono text-[12px]">{cidr}</span>
                <MemberCount state={att} n={machines} word="machine" />
              </>
            }
            actions={
              may(permission("network", "delete")) ? (
                <Button variant="danger" onClick={() => setDeleting(true)}>
                  <Trash2 /> Delete
                </Button>
              ) : undefined
            }
          />
          <DetailBody
            main={
              <>
                <Card>
                  <CardHeader title="Summary" />
                  <dl className="divide-y px-4">
                    <Field label="IPv4 range" mono>
                      {cidr}
                    </Field>
                    <Field label="Gateway" mono>
                      {gatewayOf(cidr)} <span className="font-sans text-[12px] text-subtle">· resolver on port 53</span>
                    </Field>
                    <Field label="Internal DNS" mono>
                      <span className="flex items-center gap-1">
                        &lt;name&gt;.{project}.hangar.internal
                        <CopyButton text={`${project}.hangar.internal`} />
                      </span>
                    </Field>
                    <Field label="Transit link" mono>
                      {n.status?.transitIpv4 || <span className="font-sans text-subtle">not assigned yet</span>}
                    </Field>
                    <Field label="Labels">
                      <Labels labels={n.metadata?.labels ?? {}} />
                    </Field>
                    <Field label="Created">{date(n.metadata?.createTime)?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
                    <Field label="UID" mono>
                      <span className="text-muted">{n.metadata?.uid}</span>
                    </Field>
                  </dl>
                </Card>
                <AttachmentsCard title="Interfaces" items={on} state={att} empty="No machine has an interface on this network." />
                <NetworkFileSharesCard network={name} canApply={may(sharePerms.apply)} />
                <Card>
                  <CardHeader title="Security groups" />
                  <ul className="divide-y">
                    <li className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                      <span className="w-32 shrink-0 font-medium">default</span>
                      <span className="text-muted">Built in. Lets in its own members and lets everything out.</span>
                    </li>
                    {groups.items
                      .filter((g) => g.spec?.network === name)
                      .map((g) => (
                        <li key={nameOf(g)} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                          <Link
                            to="/projects/$project/security-groups/$name"
                            params={{ project, name: nameOf(g) }}
                            className="w-32 shrink-0 truncate font-medium text-accent hover:underline"
                          >
                            {nameOf(g)}
                          </Link>
                          <span className="text-muted tabular">
                            {g.spec?.ingress.length ?? 0} ingress · {g.spec?.egress.length ? `${g.spec.egress.length} egress` : "egress open"}
                          </span>
                        </li>
                      ))}
                  </ul>
                </Card>
                <ConditionsCard conditions={n.status?.conditions ?? []} />
              </>
            }
            side={
              <EquivalentCard
                cli={[`hangar get network ${name} -p ${project}`, `hangar delete network ${name} -p ${project}`]}
                yaml={networkYaml(n)}
              />
            }
          />
          <DeleteDialog
            kind="network"
            name={name}
            open={deleting}
            onClose={() => setDeleting(false)}
            after={() => void navigate({ to: "/projects/$project/networks", params: { project } })}
            consequence={
              name === "default"
                ? "The project's default network is made again, with the range 10.0.0.0/24, shortly after it goes."
                : "Security groups of the network stay until deleted."
            }
            blocked={
              usageBlock(att, machines > 0 ? `${machines} ${machines === 1 ? "machine is" : "machines are"} on this network.` : undefined) ??
              (shares.loading
                ? "The project's file shares are still loading."
                : shares.error != null
                  ? "The project's file shares did not load, so whether one is on this network is not known."
                  : sharesOn > 0
                    ? `${sharesOn} ${sharesOn === 1 ? "file share is" : "file shares are"} on this network.`
                    : undefined)
            }
          />
        </>
      )}
    </DetailFrame>
  );
}
