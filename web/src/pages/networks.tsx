// A project's networks and one network (docs/decisions/networking.md);
// pages/create-network.tsx makes one.

import { Link, useNavigate } from "@tanstack/react-router";
import type { Network } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { DeleteDialog, MachinesNotice, type MachinesState, MemberCount, usageBlock } from "@/components/resource";
import { ConditionsCard, EquivalentCard, LabelsField } from "@/components/object";
import { editLabelsAction, HeadActions, ObjectRowMenu, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { DetailBody, DetailFrame, DetailHead, ListFrame, NameFilter, PageHead } from "@/components/page";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Topbar, useProjectName } from "@/components/shell";
import { ErrorNotice, SkeletonRows } from "@/components/states";
import { Badge, buttonVariants, Card, CardHeader, CopyButton, Field, Led } from "@/components/ui";
import { permission, useNetworkCan } from "@/lib/network-actions";
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

/**
 * A network's actions, on its page and in its row's menu: Edit labels and
 * Delete, each shown once TestPermissions holds its permission; `after`
 * follows a delete the API took.
 */
export function useNetworkActions(n: Network, can: Can, after?: () => void): ActionSet {
  const name = nameOf(n);
  const project = n.metadata?.project ?? "";
  const [open, setOpen] = useState<"delete" | "labels" | null>(null);
  const att = useAttachments(project);
  // The API refuses to delete a network a file share is on, as one a machine is on.
  const shares = useFileShares(project);
  const machines = new Set(att.items.filter((a) => a.network === name).map((a) => `${a.kind}/${a.name}`)).size;
  const sharesOn = shares.items.filter((s) => shareNetworks(s).includes(name)).length;
  const actions: ObjectAction[] = [];
  if (can(permission("network", "delete"))) actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: () => setOpen("delete") });
  const close = () => setOpen(null);
  return {
    actions,
    editLabels: editLabelsAction(can(permission("network", "apply")), !!n.metadata?.deleteTime, () => setOpen("labels")),
    deleting: !!n.metadata?.deleteTime,
    dialogs: (
      <>
        {open === "labels" && <EditLabelsDialog kind="network" object={n} onClose={close} />}
        {open === "delete" && (
          <DeleteDialog
            kind="network"
            name={name}
            open
            onClose={close}
            after={after}
            consequence={
              name === "default"
                ? "It is made again as 10.0.0.0/24 shortly after."
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
        )}
      </>
    ),
  };
}

/** A network's row menu: the actions of its page. */
function NetworkRowMenu({ n, can }: { n: Network; can: Can }) {
  return <ObjectRowMenu name={nameOf(n)} set={useNetworkActions(n, can)} />;
}

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
  const { can: may } = useNetworkCan("network", project);
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
        description="Private IPv4 networks"
        action={newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ ...nets, count: nets.items.length }}
          noun="networks"
          empty={{ title: "No networks", action: newButton }}
        >
          <NameFilter value={query} onChange={setQuery} />
          <MachinesNotice state={att} />
          <Card className="md:hidden">
            <ul className="divide-y">
              {rows.map((n) => (
                <li key={nameOf(n)} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                  <Link to="/projects/$project/networks/$name" params={{ project, name: nameOf(n) }} className="flex min-w-0 flex-1 items-center gap-3">
                    <span className="min-w-0 flex-1">
                      <span className="block truncate font-medium">{nameOf(n)}</span>
                      <span className="block truncate font-mono text-[11.5px] text-subtle">{n.spec?.ipv4Cidr}</span>
                    </span>
                    <span className="shrink-0 text-muted">
                      <MemberCount state={att} n={machinesOn(nameOf(n))} word="machine" />
                    </span>
                    <ReadyCell n={n} />
                  </Link>
                  <NetworkRowMenu n={n} can={may} />
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
                  <Th className="w-10 px-0" />
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
                    <td className="px-2 py-2.5">
                      <NetworkRowMenu n={n} can={may} />
                    </td>
                  </tr>
                ))}
                {rows.length === 0 && <NoMatch noun="network" colSpan={7} />}
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
  // Asked with the list, so the page's controls come with it, and once.
  const { can } = useNetworkCan("network", project);
  const n = nets.items.find((x) => nameOf(x) === name);
  return (
    <DetailFrame crumbs={["Networks", name]} state={nets} found={!!n} noun="network" name={name}>
      {n && <Loaded n={n} can={can} />}
    </DetailFrame>
  );
}

/** The page of a network that has loaded, its actions made once for its head and its labels. */
function Loaded({ n, can }: { n: Network; can: Can }) {
  const name = nameOf(n);
  const project = n.metadata?.project ?? "";
  const att = useAttachments(project);
  const groups = useSecurityGroups(project);
  const navigate = useNavigate();
  const set = useNetworkActions(n, can, () => void navigate({ to: "/projects/$project/networks", params: { project } }));
  // The File shares card's New file share, from the answer the page's actions use.
  const may = can;
  const on = att.items.filter((a) => a.network === name);
  const machines = new Set(on.map((a) => `${a.kind}/${a.name}`)).size;
  const cidr = n.spec?.ipv4Cidr ?? "";
  const ready = conditionOf(n.status?.conditions, "Ready");
  return (
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
        actions={set.actions.length > 0 && <HeadActions set={set} />}
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
                <LabelsField labels={n.metadata?.labels ?? {}} onEdit={set.editLabels?.onSelect} />
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
                  <span className="text-muted">Built in · its own members in, everything out</span>
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
      {set.dialogs}
    </>
  );
}
