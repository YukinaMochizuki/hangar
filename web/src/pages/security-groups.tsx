// A project's security groups, one group, and the pages that make one or
// edit its rules (docs/decisions/networking.md). Every network also has the
// built-in group "default", which is no object: it is listed, but has no
// page.

import { clone, create } from "@bufbuild/protobuf";
import { Link, useNavigate } from "@tanstack/react-router";
import { SecurityGroupProtocol, SecurityGroupSchema, SecurityGroupSpecSchema, type SecurityGroup } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { Pencil, Plus, ShieldCheck, Trash2, X } from "lucide-react";
import { Fragment, useState } from "react";
import { DeleteDialog, MachinesNotice, MemberCount, StaleNotice, usageBlock } from "@/components/resource";
import { Input, Select } from "@/components/form";
import { CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { Notice } from "@/components/notice";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { ConditionsCard, EquivalentCard, LabelsField } from "@/components/object";
import { editLabelsAction, HeadActions, ObjectRowMenu, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { DetailBody, DetailFrame, DetailHead, ListFrame, NameFilter, PageHead } from "@/components/page";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState } from "@/components/states";
import { Badge, Button, buttonVariants, Card, CardHeader, Field } from "@/components/ui";
import { checkName } from "@/lib/create";
import { isStale, permission, useApplyAt, useCreate, useReload, useNetworkCan } from "@/lib/network-actions";
import { useCan } from "@/lib/permissions";
import { date } from "@/lib/machines";
import { count, nameOf, portsLabel, protocolLabel, securityGroupYaml, useAttachments, useNetworks, useSecurityGroups } from "@/lib/network";
import {
  draftOf,
  draftProblems,
  egressOf,
  ingressOf,
  newlyRestricted,
  presets,
  protocols,
  ruleSentence,
  sentence,
  takesPorts,
  type Direction,
  type Draft,
  type PeerKind,
  type Rule,
} from "@/lib/security-group-rules";
import { AttachmentsCard } from "@/pages/networks";

function egressText(g: SecurityGroup): string {
  const n = g.spec?.egress.length ?? 0;
  return n === 0 ? "open" : `${n} ${n === 1 ? "rule" : "rules"}`;
}

/**
 * A group's actions, on its page and in its row's menu: Edit rules, Edit
 * labels and Delete, each shown once TestPermissions holds its permission;
 * `after` follows a delete the API took.
 */
export function useSecurityGroupActions(g: SecurityGroup, can: Can, after?: () => void): ActionSet {
  const name = nameOf(g);
  const project = g.metadata?.project ?? "";
  const navigate = useNavigate();
  const [open, setOpen] = useState<"delete" | "labels" | null>(null);
  const att = useAttachments(project);
  const net = g.spec?.network ?? "";
  const members = att.items.filter((a) => a.network === net && a.securityGroups.includes(name)).length;
  const actions: ObjectAction[] = [];
  if (can(permission("securityGroup", "apply"))) {
    const link = { to: "/projects/$project/security-groups/$name/edit", params: { project, name } };
    actions.push({ label: "Edit rules", icon: Pencil, link, onSelect: () => void navigate(link) });
  }
  if (can(permission("securityGroup", "delete"))) actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: () => setOpen("delete") });
  const close = () => setOpen(null);
  return {
    actions,
    editLabels: editLabelsAction(can(permission("securityGroup", "apply")), !!g.metadata?.deleteTime, () => setOpen("labels")),
    deleting: !!g.metadata?.deleteTime,
    dialogs: (
      <>
        {open === "labels" && <EditLabelsDialog kind="securityGroup" object={g} onClose={close} />}
        {open === "delete" && (
          <DeleteDialog
            kind="securityGroup"
            name={name}
            open
            onClose={close}
            after={after}
            consequence="Rules of other groups that name it then allow nothing."
            blocked={usageBlock(att, members > 0 ? `${members} ${members === 1 ? "interface is" : "interfaces are"} in this group.` : undefined)}
          />
        )}
      </>
    ),
  };
}

/** A group's row menu: the actions of its page. */
function SecurityGroupRowMenu({ g, can }: { g: SecurityGroup; can: Can }) {
  return <ObjectRowMenu name={nameOf(g)} set={useSecurityGroupActions(g, can)} />;
}

/** A network as a group names it, stated as missing when the project has none of the name. */
function NetworkName({ net, exists }: { net: string; exists: boolean }) {
  return exists ? <>{net}</> : <span className="text-pending">network {net} does not exist</span>;
}

export function SecurityGroupList() {
  const project = useProjectName();
  const groups = useSecurityGroups(project);
  const nets = useNetworks(project);
  const att = useAttachments(project);
  const [query, setQuery] = useState("");
  const membersOf = (network: string, group: string) => att.items.filter((a) => a.network === network && a.securityGroups.includes(group)).length;
  const listed = nets.items.map(nameOf);
  // Rows go under the network each group names, whether the project has it or not.
  const networks = [...new Set([...listed, ...groups.items.map((g) => g.spec?.network ?? "")])].sort();
  const rows = groups.items.filter((g) => !query || nameOf(g).includes(query));
  const showDefault = !query || "default".includes(query);
  const members = (net: string, group: string, word?: string) => <MemberCount state={att} n={membersOf(net, group)} word={word} />;
  const { can: may } = useNetworkCan("securityGroup", project);
  const newButton = may(permission("securityGroup", "apply")) ? (
    <Link to="/projects/$project/new/security-group" params={{ project }} className={buttonVariants({ variant: "primary" })}>
      <Plus /> New security group
    </Link>
  ) : undefined;
  return (
    <>
      <Topbar crumbs={["Security groups"]} />
      <PageHead
        title="Security groups"
        description="Allow rules for the interfaces that name a group. Inbound is denied unless a rule lets it in; outbound is open unless a group of the interface has outbound rules."
        action={newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ loading: groups.loading || nets.loading, error: groups.error ?? nets.error, refetch: groups.refetch, count: groups.items.length + listed.length }}
          noun="security groups"
          empty={{ title: "No security groups", text: "An interface naming no group is in its network's built-in group default alone.", action: newButton }}
        >
          <NameFilter value={query} onChange={setQuery} />
          <MachinesNotice state={att} />
          <Card className="md:hidden">
            <ul className="divide-y">
              {networks.map((net) => (
                <Fragment key={net}>
                  {showDefault && listed.includes(net) && (
                    <li className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                      <span className="min-w-0 flex-1">
                        <span className="flex items-center gap-2 font-medium">
                          default <Badge>built in</Badge>
                        </span>
                        <span className="block text-[11.5px] text-subtle">{net} · lets in its own members, lets everything out</span>
                      </span>
                      <span className="shrink-0 text-muted">{members(net, "default", "member")}</span>
                      <span className="block size-8 shrink-0" />
                    </li>
                  )}
                  {rows
                    .filter((g) => (g.spec?.network ?? "") === net)
                    .map((g) => (
                      <li key={nameOf(g)} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                        <Link
                          to="/projects/$project/security-groups/$name"
                          params={{ project, name: nameOf(g) }}
                          className="flex min-w-0 flex-1 items-center gap-3"
                        >
                          <span className="min-w-0 flex-1">
                            <span className="block truncate font-medium">{nameOf(g)}</span>
                            <span className="block text-[11.5px] text-subtle">
                              <NetworkName net={net} exists={listed.includes(net)} /> · {count(g.spec?.ingress.length ?? 0, "inbound rule")} · outbound {egressText(g)}
                            </span>
                          </span>
                          <span className="shrink-0 text-muted">{members(net, nameOf(g), "member")}</span>
                        </Link>
                        <SecurityGroupRowMenu g={g} can={may} />
                      </li>
                    ))}
                </Fragment>
              ))}
              {!showDefault && rows.length === 0 && <NoMatch noun="security group" />}
            </ul>
          </Card>
          <Card className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[720px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[24%]">Name</Th>
                  <Th>Network</Th>
                  <Th>Inbound</Th>
                  <Th>Outbound</Th>
                  <Th>Members</Th>
                  <Th>Created</Th>
                  <Th className="w-10 px-0" />
                </tr>
              </thead>
              <tbody>
                {networks.map((net) => (
                  <Fragment key={net}>
                    {showDefault && listed.includes(net) && (
                      <tr className="border-b last:border-0">
                        <td className="px-4 py-2.5">
                          <span className="flex items-center gap-2 font-medium">
                            default <Badge>built in</Badge>
                          </span>
                        </td>
                        <td className="px-4 py-2.5">{net}</td>
                        <td className="px-4 py-2.5 text-muted">its own members</td>
                        <td className="px-4 py-2.5 text-muted">open</td>
                        <td className="px-4 py-2.5 tabular">{members(net, "default")}</td>
                        <td className="px-4 py-2.5 text-subtle">–</td>
                        <td />
                      </tr>
                    )}
                    {rows
                      .filter((g) => (g.spec?.network ?? "") === net)
                      .map((g) => (
                        <tr key={nameOf(g)} className={tbodyRow}>
                          <td className="max-w-0 px-4 py-2.5">
                            <Link
                              to="/projects/$project/security-groups/$name"
                              params={{ project, name: nameOf(g) }}
                              className="block truncate font-medium group-hover:text-accent"
                            >
                              {nameOf(g)}
                            </Link>
                          </td>
                          <td className="px-4 py-2.5">
                            <NetworkName net={net} exists={listed.includes(net)} />
                          </td>
                          <td className="px-4 py-2.5 tabular">
                            {g.spec?.ingress.length ?? 0} {g.spec?.ingress.length === 1 ? "rule" : "rules"}
                          </td>
                          <td className="px-4 py-2.5 tabular">{egressText(g)}</td>
                          <td className="px-4 py-2.5 tabular">{members(net, nameOf(g))}</td>
                          <td className="whitespace-nowrap px-4 py-2.5 text-muted">{date(g.metadata?.createTime)?.toISOString().slice(0, 10)}</td>
                          <td className="px-2 py-2.5">
                            <SecurityGroupRowMenu g={g} can={may} />
                          </td>
                        </tr>
                      ))}
                  </Fragment>
                ))}
                {!showDefault && rows.length === 0 && <NoMatch noun="security group" colSpan={7} />}
              </tbody>
            </table>
          </Card>
        </ListFrame>
      </div>
    </>
  );
}

/** A group's rules of one direction, each as the sentence of what it lets through. */
function RulesView({ title, dir, rules, empty, foot }: { title: string; dir: Direction; rules: Rule[]; empty: string; foot?: string }) {
  return (
    <Card className="overflow-hidden">
      <CardHeader
        title={
          <span className="flex items-baseline gap-2">
            {title}
            <span className="font-mono text-[11.5px] font-normal text-subtle">{rules.length}</span>
          </span>
        }
      />
      {rules.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-muted">{empty}</p>
      ) : (
        <ul className="divide-y">
          {rules.map((r, i) => (
            <li key={i} className="flex items-baseline gap-4 px-4 py-2.5 text-[13px]">
              <span className="w-10 shrink-0 font-mono text-[12px] text-muted">{protocolLabel[r.protocol]}</span>
              <span className="w-24 shrink-0 truncate font-mono text-[12px] text-muted">{portsLabel(r.protocol, r.ports)}</span>
              <span className="min-w-0">{ruleSentence(r, dir)}.</span>
            </li>
          ))}
        </ul>
      )}
      {foot && <div className="border-t px-4 py-2.5 text-[12px] text-muted">{foot}</div>}
    </Card>
  );
}

export function SecurityGroupDetail({ name }: { name: string }) {
  const project = useProjectName();
  const groups = useSecurityGroups(project);
  // Asked with the list, so the page's controls come with it, and once.
  const { can } = useNetworkCan("securityGroup", project);
  const g = groups.items.find((x) => nameOf(x) === name);
  return (
    <DetailFrame crumbs={["Security groups", name]} state={groups} found={!!g} noun="security group" name={name}>
      {g && <Loaded g={g} can={can} />}
    </DetailFrame>
  );
}

/** The page of a group that has loaded, its actions made once for its head and its labels. */
function Loaded({ g, can }: { g: SecurityGroup; can: Can }) {
  const name = nameOf(g);
  const project = g.metadata?.project ?? "";
  const nets = useNetworks(project);
  const att = useAttachments(project);
  const navigate = useNavigate();
  const set = useSecurityGroupActions(g, can, () => void navigate({ to: "/projects/$project/security-groups", params: { project } }));
  const net = g.spec?.network ?? "";
  const members = att.items.filter((a) => a.network === net && a.securityGroups.includes(name));
  return (
    <>
      <DetailHead
        name={name}
        kind="Security group"
        facts={
          <>
            <span>
              on{" "}
              {nets.loading || nets.error || nets.items.some((n) => nameOf(n) === net) ? (
                <Link to="/projects/$project/networks/$name" params={{ project, name: net }} className="text-accent hover:underline">
                  {net}
                </Link>
              ) : (
                <NetworkName net={net} exists={false} />
              )}
            </span>
            <span className="tabular">{count(g.spec?.ingress.length ?? 0, "inbound rule")}</span>
            <span className="tabular">outbound {egressText(g)}</span>
            <MemberCount state={att} n={members.length} word="member" />
          </>
        }
        actions={set.actions.length > 0 && <HeadActions set={set} />}
      />
      <DetailBody
        main={
          <>
            <RulesView title="Inbound" dir="in" rules={g.spec?.ingress ?? []} empty="No inbound rules: this group lets nothing in." />
            <RulesView
              title="Outbound"
              dir="out"
              rules={g.spec?.egress ?? []}
              empty="No outbound rules: this group restricts nothing its members send."
              foot={(g.spec?.egress.length ?? 0) > 0 ? "A member sends only what some outbound rule of its groups lets out." : undefined}
            />
            <AttachmentsCard title="Members" items={members} state={att} empty="No interface names this group." />
            <Card>
              <CardHeader title="Summary" />
              <dl className="divide-y px-4">
                <Field label="Network">{net}</Field>
                <LabelsField labels={g.metadata?.labels ?? {}} onEdit={set.editLabels?.onSelect} />
                <Field label="Created">{date(g.metadata?.createTime)?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
                <Field label="UID" mono>
                  <span className="text-muted">{g.metadata?.uid}</span>
                </Field>
              </dl>
            </Card>
            <ConditionsCard conditions={g.status?.conditions ?? []} />
          </>
        }
        side={
          <EquivalentCard
            cli={[`hangar get sg ${name} -p ${project}`, `hangar apply -p ${project} -f ${name}.yaml`, `hangar delete sg ${name} -p ${project}`]}
            yaml={securityGroupYaml(g)}
          />
        }
      />
      {set.dialogs}
    </>
  );
}

const peerKinds: { kind: PeerKind; label: string }[] = [
  { kind: "any4", label: "Any IPv4" },
  { kind: "any6", label: "Any IPv6" },
  { kind: "cidr", label: "Range" },
  { kind: "group", label: "Group" },
];

const blankRule: Draft = { protocol: SecurityGroupProtocol.TCP, ports: "", peer: "any4", value: "" };

/** One direction's rules as a table being edited: protocol, ports, the peer by kind, and the sentence of what each lets through. */
function RulesEditor({
  title,
  dir,
  rules,
  onChange,
  groups,
  self,
  empty,
}: {
  title: string;
  dir: Direction;
  rules: Draft[];
  onChange: (r: Draft[]) => void;
  /** The groups of the network a rule can name: "default" and the project's. */
  groups: string[];
  /** The group being edited, which a rule may name before it exists. */
  self: string;
  empty: string;
}) {
  const set = (i: number, d: Partial<Draft>) => onChange(rules.map((r, j) => (j === i ? { ...r, ...d } : r)));
  const choosable = [...new Set([self, ...groups].filter(Boolean))];
  return (
    <Card className="overflow-hidden">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b px-4 py-2.5">
        <span className="text-[13px] font-medium">{title}</span>
        <span className="flex flex-wrap items-center gap-1.5">
          {dir === "in" &&
            presets(self).map((p) => (
              <button
                key={p.label}
                type="button"
                disabled={p.draft.peer === "group" && !self}
                onClick={() => onChange([...rules, p.draft])}
                className="flex h-6 items-center gap-1 rounded border px-2 text-[11.5px] text-muted hover:border-border-strong hover:text-foreground disabled:opacity-50"
              >
                <Plus className="size-3" /> {p.label}
              </button>
            ))}
          <Button type="button" size="sm" onClick={() => onChange([...rules, blankRule])}>
            <Plus /> Rule
          </Button>
        </span>
      </div>
      {rules.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-muted">{empty}</p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[640px] table-fixed text-[13px]">
            <thead>
              <tr className={theadRow}>
                <Th className="w-[112px]">Protocol</Th>
                <Th className="w-[120px] px-2">Ports</Th>
                <Th className="px-2">{dir === "in" ? "From" : "To"}</Th>
                <Th className="hidden w-[26%] px-2 lg:table-cell">What it does</Th>
                <Th className="w-10 px-0" />
              </tr>
            </thead>
            <tbody>
              {rules.map((r, i) => {
                const ports = takesPorts(r.protocol);
                return (
                  <tr key={i} className="border-b align-top last:border-0">
                    <td className="px-4 py-2">
                      <Select
                        aria-label="Protocol"
                        value={r.protocol}
                        onChange={(e) => set(i, { protocol: Number(e.target.value) })}
                        className="h-8 font-sans text-[12.5px]"
                      >
                        {protocols.map((p) => (
                          <option key={p} value={p}>
                            {protocolLabel[p]}
                          </option>
                        ))}
                      </Select>
                    </td>
                    <td className="px-2 py-2">
                      <Input
                        aria-label="Ports"
                        value={ports ? r.ports : ""}
                        disabled={!ports}
                        placeholder={ports ? "every port" : "–"}
                        onChange={(e) => set(i, { ports: e.target.value })}
                        className="h-8 px-2 text-[12.5px] disabled:opacity-50"
                      />
                    </td>
                    <td className="px-2 py-2">
                      <div className="flex gap-1.5">
                        <Select
                          aria-label={dir === "in" ? "From" : "To"}
                          value={r.peer}
                          onChange={(e) => {
                            const peer = e.target.value as PeerKind;
                            set(i, { peer, value: peer === "group" ? (choosable[0] ?? "") : "" });
                          }}
                          className="h-8 w-auto shrink-0 font-sans text-[12.5px]"
                        >
                          {peerKinds.map((k) => (
                            <option key={k.kind} value={k.kind}>
                              {k.label}
                            </option>
                          ))}
                        </Select>
                        {r.peer === "cidr" && (
                          <Input
                            aria-label="Range"
                            value={r.value}
                            placeholder="10.0.0.0/24"
                            onChange={(e) => set(i, { value: e.target.value })}
                            className="h-8 min-w-0 px-2 text-[12.5px]"
                          />
                        )}
                        {r.peer === "group" && (
                          <Select aria-label="Group" value={r.value} onChange={(e) => set(i, { value: e.target.value })} className="h-8 min-w-0 flex-1 text-[12.5px]">
                            {!choosable.includes(r.value) && <option value={r.value}>{r.value || "–"}</option>}
                            {choosable.map((g) => (
                              <option key={g} value={g}>
                                {g}
                              </option>
                            ))}
                          </Select>
                        )}
                      </div>
                    </td>
                    <td className="hidden px-2 py-2.5 text-[12.5px] text-muted lg:table-cell">{sentence(r, dir)}.</td>
                    <td className="py-2 pr-2">
                      <button
                        type="button"
                        aria-label="Remove rule"
                        onClick={() => onChange(rules.filter((_, j) => j !== i))}
                        className="flex size-8 items-center justify-center rounded text-subtle hover:bg-surface-2 hover:text-foreground"
                      >
                        <X className="size-3.5" />
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

/** How a group's rules work, beside its editor (docs/decisions/networking.md). */
function HowGroupsWork() {
  return (
    <div className="border-b px-4 py-3">
      <div className="flex items-center gap-2 text-[13px] font-medium">
        <ShieldCheck className="size-4 text-subtle" /> How a group works
      </div>
      <ul className="mt-2 space-y-1.5 text-[12.5px] text-muted">
        <li>
          <span className="text-foreground">Inbound</span> is denied unless a rule of one of the interface's groups lets it in. Replies to what an
          interface sent always come back.
        </li>
        <li>
          <span className="text-foreground">Outbound</span> is allowed to everywhere while none of the interface's groups has outbound rules. Once one
          does, the interface sends only what some rule lets out.
        </li>
      </ul>
    </div>
  );
}

/**
 * The page that makes a security group, or edits a group's rules. An edit is
 * applied at the version the page opened on: the rules shown are what it
 * replaces, so a newer group is not overwritten. The API then refuses it, and
 * the page says so and can start again from the group as it is.
 */
function SecurityGroupForm({ group }: { group?: SecurityGroup }) {
  const project = useProjectName();
  const navigate = useNavigate();
  const groups = useSecurityGroups(project);
  const nets = useNetworks(project);
  const att = useAttachments(project);
  // The group as the page opened on it; the list refreshing does not move it.
  const [base, setBase] = useState(group);
  const [name, setName] = useState(group ? nameOf(group) : "");
  const networks = nets.items.map(nameOf);
  const [chosenNetwork, setNetwork] = useState(group?.spec?.network ?? "");
  const network = chosenNetwork || (networks.includes("default") ? "default" : (networks[0] ?? ""));
  const [ingress, setIngress] = useState<Draft[]>(group?.spec?.ingress.map(draftOf) ?? []);
  const [egress, setEgress] = useState<Draft[]>(group?.spec?.egress.map(draftOf) ?? []);
  const createGroup = useCreate("securityGroup", project);
  const applyAt = useApplyAt("securityGroup");
  const reload = useReload("securityGroup", project, name);
  const submit = group ? applyAt : createGroup;
  const stale = !!group && isStale(applyAt.error);

  let obj: SecurityGroup;
  if (base) {
    // Only the rules change; the rest is the group as the page opened on it.
    obj = clone(SecurityGroupSchema, base);
    obj.spec ??= create(SecurityGroupSpecSchema);
    obj.spec.ingress = ingressOf(ingress);
    obj.spec.egress = egressOf(egress);
  } else {
    obj = create(SecurityGroupSchema, { metadata: { name }, spec: { network, ingress: ingressOf(ingress), egress: egressOf(egress) } });
  }
  const problems: string[] = [];
  if (!group) {
    const check = checkName(name, "security group", { loading: groups.loading, error: groups.error, names: groups.items.map(nameOf) });
    if (!check.ok) problems.push(check.reason);
    if (!nets.loading && !network) problems.push("The project has no network.");
  }
  problems.push(...draftProblems([...ingress, ...egress]));

  // The interfaces this edit's first outbound rule restricts, when the group lets everything out now.
  const firstEgress = (group?.spec?.egress.length ?? 0) === 0 && egress.length > 0;
  const restricted = firstEgress && !att.loading && !att.error ? newlyRestricted(name, network, att.items, groups.items) : [];
  const peersHere = ["default", ...groups.items.filter((g) => g.spec?.network === network).map(nameOf)];

  const open = (to: string) => void navigate({ to: "/projects/$project/security-groups/$name", params: { project, name: to }, replace: true });
  const send = () => {
    if (problems.length > 0 || stale) return;
    if (group) applyAt.mutate(obj, { onSuccess: () => open(name) });
    else createGroup.mutate(obj, { onSuccess: () => open(name) });
  };
  const startAgain = () =>
    reload.mutate(undefined, {
      onSuccess: (fresh) => {
        setBase(fresh);
        setIngress(fresh.spec?.ingress.map(draftOf) ?? []);
        setEgress(fresh.spec?.egress.map(draftOf) ?? []);
        applyAt.reset();
      },
    });
  const where = group ? <>On network {network}.</> : undefined;
  const n = group ? 0 : 2;
  const editors = (
    <>
      <FormSection n={n + 1} title="Inbound rules" hint="What may come into a member">
        <RulesEditor title="Inbound" dir="in" rules={ingress} onChange={setIngress} groups={peersHere} self={name} empty="No rule: this group lets nothing in." />
      </FormSection>
      <FormSection n={n + 2} title="Outbound rules" hint="What a member may send">
        <RulesEditor
          title="Outbound"
          dir="out"
          rules={egress}
          onChange={setEgress}
          groups={peersHere}
          self={name}
          empty="No rule: everything may go out, unless another group of an interface has outbound rules."
        />
      </FormSection>
    </>
  );
  return (
    <FormPage
      crumbs={group ? undefined : ["Security groups", "New security group"]}
      title={group ? `Edit rules of ${nameOf(group)}` : "New security group"}
      description={where ?? "Allow rules for the interfaces that name the group, on one network."}
      onSubmit={send}
      side={
        <>
          <Card>
            <HowGroupsWork />
            <FormSummary
              summary={
                <span className="text-[13px] text-muted">
                  <span className="font-mono text-foreground">{name || "–"}</span> on {network || "–"}: {count(ingress.length, "inbound rule")},{" "}
                  {egress.length === 0 ? "outbound open" : count(egress.length, "outbound rule")}
                </span>
              }
              problems={problems}
              blocked={stale}
              notices={
                <>
                  {restricted.length > 0 && (
                    <Notice tone="warning" title={`Outbound rules restrict ${count(restricted.length, "interface")}`}>
                      {restricted.map((a) => a.name).join(", ")} on network {network} then send only what an outbound rule of their groups lets out.
                    </Notice>
                  )}
                  {stale && (
                    <StaleNotice title="The rules changed after this page opened." reloading={reload.isPending} error={reload.error} onReload={startAgain} />
                  )}
                </>
              }
              error={stale ? null : submit.error}
              failed={group ? "Applying the rules failed" : "Creating the security group failed"}
              cancelTo={group ? `/projects/${project}/security-groups/${nameOf(group)}` : `/projects/${project}/security-groups`}
              pending={submit.isPending}
              pendingLabel={group ? "Applying…" : "Creating…"}
              label={
                group ? (
                  "Apply rules"
                ) : (
                  <>
                    <Plus /> Create security group
                  </>
                )
              }
            />
          </Card>
          <FormManifest yaml={securityGroupYaml(obj)} cli={[`hangar apply ${group ? "" : "--create-only "}-p ${project} -f ${name || "group"}.yaml`]} />
        </>
      }
    >
      {!group && (
        <>
          <FormSection n={1} title="Network" hint="Only its interfaces can join the group; it cannot be changed later">
            <div className="max-w-xl">
              <Select aria-label="Network" value={network} onChange={(e) => setNetwork(e.target.value)} disabled={nets.loading}>
                {networks.map((x) => (
                  <option key={x} value={x}>
                    {x}
                  </option>
                ))}
              </Select>
            </div>
          </FormSection>
          <FormSection n={2} title="Group name" hint="Unique among this project's security groups">
            <div className="max-w-xl">
              <Input aria-label="Group name" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} placeholder="web" spellCheck={false} />
            </div>
          </FormSection>
        </>
      )}
      {editors}
    </FormPage>
  );
}

/** The caller's permission to write security groups, stated on a page that needs it once the answer says no. */
function useMayApply(project: string) {
  const p = permission("securityGroup", "apply");
  const { can, known } = useCan({ level: "project", name: project }, [p]);
  return { may: can(p), refused: known && !can(p) };
}

/** The page that makes a security group. */
export function NewSecurityGroup() {
  const project = useProjectName();
  const p = permission("securityGroup", "apply");
  const perm = useCan({ level: "project", name: project }, [p]);
  return (
    <CreateGate crumbs={["Security groups", "New security group"]} title="New security group" perm={perm} may={perm.can(p)} refusal="creating security groups">
      <SecurityGroupForm />
    </CreateGate>
  );
}

/** The page that edits a group's rules. */
export function EditSecurityGroup({ name }: { name: string }) {
  const project = useProjectName();
  const groups = useSecurityGroups(project);
  const { may, refused } = useMayApply(project);
  const g = groups.items.find((x) => nameOf(x) === name);
  return (
    <DetailFrame crumbs={["Security groups", name, "Edit rules"]} state={groups} found={!!g} noun="security group" name={name}>
      {g && may ? (
        // Keyed by the group's identity so a group made again under the name starts a new edit.
        <SecurityGroupForm key={g.metadata?.uid} group={g} />
      ) : refused ? (
        <EmptyState title="No editing">This account's roles do not include changing security groups in this project.</EmptyState>
      ) : null}
    </DetailFrame>
  );
}
