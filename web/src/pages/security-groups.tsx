// A project's security groups, one group, and making or editing one
// (docs/decisions/networking.md). Every network also has the built-in
// group "default", which is no object: it is listed, but has no page.

import { clone, create } from "@bufbuild/protobuf";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  EgressRuleSchema,
  IngressRuleSchema,
  SecurityGroupProtocol,
  SecurityGroupSchema,
  SecurityGroupSpecSchema,
  type EgressRule,
  type IngressRule,
  type SecurityGroup,
} from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { Pencil, Plus, Trash2, X } from "lucide-react";
import { Fragment, useState } from "react";
import { Dialog } from "@/components/dialog";
import {
  ConditionsCard,
  DeleteDialog,
  DetailBody,
  DetailFrame,
  DetailHead,
  Equivalent,
  FormEquivalent,
  FormField,
  FormFooter,
  inputClass,
  StaleNotice,
  Labels,
  ListFrame,
  MachinesNotice,
  MemberCount,
  NameFilter,
  NoMatch,
  PageHead,
  tbodyRow,
  Th,
  theadRow,
  usageBlock,
} from "@/components/resource";
import { Topbar, useProjectName } from "@/components/shell";
import { Badge, Button, Card, CardHeader, Field } from "@/components/ui";
import { cn } from "@/lib/cn";
import { checkName, type KnownNames } from "@/lib/create";
import { isStale, permission, useApplyAt, useCreate, useReload } from "@/lib/network-actions";
import { useCan } from "@/lib/permissions";
import { date } from "@/lib/machines";
import { useCreating, type NewProps, type OnCreated } from "@/lib/new-param";
import {
  nameOf,
  peerLabel,
  portsLabel,
  protocolLabel,
  securityGroupYaml,
  useAttachments,
  useNetworks,
  useSecurityGroups,
} from "@/lib/network";
import { AttachmentsCard } from "@/pages/networks";

type Rule = IngressRule | EgressRule;
const peerOf = (r: Rule) => (r.$typeName === "hangar.network.v1alpha1.IngressRule" ? r.from : r.to);

function egressText(g: SecurityGroup): string {
  const n = g.spec?.egress.length ?? 0;
  return n === 0 ? "open" : `${n} ${n === 1 ? "rule" : "rules"}`;
}

/** A network as a group names it, stated as missing when the project has none of the name. */
function NetworkName({ net, exists }: { net: string; exists: boolean }) {
  return exists ? <>{net}</> : <span className="text-pending">network {net} does not exist</span>;
}

export function SecurityGroupList(props: NewProps) {
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
  const { can: may, settled } = useCan({ level: "project", name: project }, [permission("securityGroup", "apply")]);
  const [creating, setCreating, created] = useCreating(props, may(permission("securityGroup", "apply")), settled);
  const newButton = may(permission("securityGroup", "apply")) ? (
    <Button variant="primary" onClick={() => setCreating(true)}>
      <Plus /> New security group
    </Button>
  ) : undefined;
  return (
    <>
      <Topbar crumbs={["Security groups"]} />
      <PageHead
        title="Security groups"
        description="Allow rules for the interfaces that name a group. Ingress is denied unless a rule allows it; egress is open unless a group of the interface has egress rules."
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
                    </li>
                  )}
                  {rows
                    .filter((g) => (g.spec?.network ?? "") === net)
                    .map((g) => (
                      <li key={nameOf(g)}>
                        <Link
                          to="/projects/$project/security-groups/$name"
                          params={{ project, name: nameOf(g) }}
                          className="flex items-center gap-3 px-4 py-2.5 text-[13px]"
                        >
                          <span className="min-w-0 flex-1">
                            <span className="block truncate font-medium">{nameOf(g)}</span>
                            <span className="block text-[11.5px] text-subtle">
                              <NetworkName net={net} exists={listed.includes(net)} /> · {g.spec?.ingress.length ?? 0} ingress · egress {egressText(g)}
                            </span>
                          </span>
                          <span className="shrink-0 text-muted">{members(net, nameOf(g), "member")}</span>
                        </Link>
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
                  <Th>Ingress</Th>
                  <Th>Egress</Th>
                  <Th>Members</Th>
                  <Th>Created</Th>
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
                        </tr>
                      ))}
                  </Fragment>
                ))}
                {!showDefault && rows.length === 0 && <NoMatch noun="security group" colSpan={6} />}
              </tbody>
            </table>
          </Card>
        </ListFrame>
      </div>
      <GroupDialog
        key={String(creating)}
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={created}
        networks={networks}
        known={{ loading: groups.loading, error: groups.error, names: groups.items.map(nameOf) }}
      />
    </>
  );
}

function RulesTable({ rules, peer, empty }: { rules: Rule[]; peer: "From" | "To"; empty: string }) {
  if (rules.length === 0) return <div className="px-4 py-3 text-[13px] text-muted">{empty}</div>;
  return (
    <table className="w-full text-[13px]">
      <thead>
        <tr className="border-b text-left text-[11.5px] text-subtle">
          <Th className="w-24">Protocol</Th>
          <Th className="w-32">Ports</Th>
          <Th>{peer}</Th>
        </tr>
      </thead>
      <tbody>
        {rules.map((r, i) => {
          const p = peerLabel(peerOf(r));
          return (
            <tr key={i} className="border-b last:border-0">
              <td className="px-4 py-2.5">{protocolLabel[r.protocol]}</td>
              <td className="px-4 py-2.5 font-mono text-[12.5px]">{portsLabel(r.protocol, r.ports)}</td>
              <td className="px-4 py-2.5">
                {p.group ? (
                  <span className="flex items-center gap-2">
                    <Badge>group</Badge>
                    <span className="font-mono text-[12.5px]">{p.text}</span>
                  </span>
                ) : (
                  <span className="font-mono text-[12.5px]">{p.text}</span>
                )}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

export function SecurityGroupDetail({ name }: { name: string }) {
  const project = useProjectName();
  const groups = useSecurityGroups(project);
  const nets = useNetworks(project);
  const att = useAttachments(project);
  const [deleting, setDeleting] = useState(false);
  const [editing, setEditing] = useState(false);
  const navigate = useNavigate();
  const { can } = useCan({ level: "project", name: project }, [permission("securityGroup", "apply"), permission("securityGroup", "delete")]);
  const mayApply = can(permission("securityGroup", "apply"));
  const mayDelete = can(permission("securityGroup", "delete"));
  const g = groups.items.find((x) => nameOf(x) === name);
  const net = g?.spec?.network ?? "";
  const members = att.items.filter((a) => a.network === net && a.securityGroups.includes(name));
  return (
    <DetailFrame crumbs={["Security groups", name]} state={groups} found={!!g} noun="security group" name={name}>
      {g && (
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
                <span className="tabular">{g.spec?.ingress.length ?? 0} ingress</span>
                <span className="tabular">egress {egressText(g)}</span>
                <MemberCount state={att} n={members.length} word="member" />
              </>
            }
            actions={
              mayApply || mayDelete ? (
                <>
                  {mayApply && (
                    <Button onClick={() => setEditing(true)}>
                      <Pencil /> Edit rules
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
                <Card className="overflow-hidden">
                  <CardHeader title="Ingress" />
                  <RulesTable rules={g.spec?.ingress ?? []} peer="From" empty="No ingress rules: this group lets nothing in." />
                </Card>
                <Card className="overflow-hidden">
                  <CardHeader title="Egress" />
                  <RulesTable
                    rules={g.spec?.egress ?? []}
                    peer="To"
                    empty="No egress rules: this group restricts nothing its members send."
                  />
                  {(g.spec?.egress.length ?? 0) > 0 && (
                    <div className="border-t px-4 py-2.5 text-[12px] text-muted">
                      A member sends only what some egress rule of its groups allows.
                    </div>
                  )}
                </Card>
                <AttachmentsCard title="Members" items={members} state={att} empty="No interface names this group." />
                <Card>
                  <CardHeader title="Summary" />
                  <dl className="divide-y px-4">
                    <Field label="Network">{net}</Field>
                    <Field label="Labels">
                      <Labels labels={g.metadata?.labels ?? {}} />
                    </Field>
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
              <Equivalent
                cli={[`hangar get sg ${name} -p ${project}`, `hangar apply -p ${project} -f ${name}.yaml`, `hangar delete sg ${name} -p ${project}`]}
                yaml={securityGroupYaml(g)}
              />
            }
          />
          <GroupDialog
            key={String(editing)}
            open={editing}
            onClose={() => setEditing(false)}
            networks={nets.items.map(nameOf)}
            known={{ loading: false, error: null, names: [] }}
            group={g}
          />
          <DeleteDialog
            kind="securityGroup"
            name={name}
            open={deleting}
            onClose={() => setDeleting(false)}
            after={() => void navigate({ to: "/projects/$project/security-groups", params: { project } })}
            consequence="Rules of other groups that name it then allow nothing."
            blocked={usageBlock(
              att,
              members.length > 0 ? `${members.length} ${members.length === 1 ? "interface is" : "interfaces are"} in this group.` : undefined,
            )}
          />
        </>
      )}
    </DetailFrame>
  );
}

type Draft = { protocol: SecurityGroupProtocol; ports: string; peer: string };

const protocols = [SecurityGroupProtocol.TCP, SecurityGroupProtocol.UDP, SecurityGroupProtocol.ICMP, SecurityGroupProtocol.ALL];

function draftOf(r: Rule): Draft {
  return { protocol: r.protocol, ports: r.ports, peer: peerLabel(peerOf(r)).text };
}

/** Only TCP and UDP take ports. */
const takesPorts = (p: SecurityGroupProtocol) => p === SecurityGroupProtocol.TCP || p === SecurityGroupProtocol.UDP;
const portsOf = (r: Draft) => (takesPorts(r.protocol) ? r.ports.trim() : "");

/** A peer as written: a range has a "/", anything else names a group. */
function peerFrom(written: string) {
  const text = written.trim();
  return text.includes("/") ? { peer: { case: "cidr" as const, value: text } } : { peer: { case: "group" as const, value: text } };
}

const ingressOf = (rules: Draft[]) => rules.map((r) => create(IngressRuleSchema, { protocol: r.protocol, ports: portsOf(r), from: peerFrom(r.peer) }));
const egressOf = (rules: Draft[]) => rules.map((r) => create(EgressRuleSchema, { protocol: r.protocol, ports: portsOf(r), to: peerFrom(r.peer) }));

function RulesEditor({ title, peer, rules, onChange }: { title: string; peer: string; rules: Draft[]; onChange: (r: Draft[]) => void }) {
  const set = (i: number, d: Partial<Draft>) => onChange(rules.map((r, j) => (j === i ? { ...r, ...d } : r)));
  const select = "h-9 rounded-md border border-border-strong bg-background px-1.5 text-[12.5px] text-foreground";
  return (
    <div className="mt-4">
      <div className="flex items-center justify-between">
        <span className="text-[12.5px] font-medium text-foreground">{title}</span>
        <Button size="sm" variant="ghost" onClick={() => onChange([...rules, { protocol: SecurityGroupProtocol.TCP, ports: "", peer: "" }])}>
          <Plus /> Rule
        </Button>
      </div>
      {rules.length > 0 && (
        <div className="mt-1 grid grid-cols-[72px_80px_1fr_28px] gap-1.5 text-[11px] text-subtle">
          <span>Protocol</span>
          <span>Ports</span>
          <span>{peer}</span>
        </div>
      )}
      {rules.map((r, i) => {
        const ports = takesPorts(r.protocol);
        return (
          <div key={i} className="mt-1.5 grid grid-cols-[72px_80px_1fr_28px] items-center gap-1.5">
            <select aria-label="Protocol" value={r.protocol} onChange={(e) => set(i, { protocol: Number(e.target.value) })} className={select}>
              {protocols.map((p) => (
                <option key={p} value={p}>
                  {protocolLabel[p]}
                </option>
              ))}
            </select>
            <input
              aria-label="Ports"
              value={ports ? r.ports : ""}
              disabled={!ports}
              placeholder={ports ? "all" : "–"}
              onChange={(e) => set(i, { ports: e.target.value })}
              className={cn(inputClass, "px-2 disabled:opacity-50")}
            />
            <input
              aria-label={peer}
              value={r.peer}
              placeholder="::/0 or a group"
              onChange={(e) => set(i, { peer: e.target.value })}
              className={cn(inputClass, "px-2")}
            />
            <button
              aria-label="Remove rule"
              onClick={() => onChange(rules.filter((_, j) => j !== i))}
              className="flex size-7 items-center justify-center rounded text-subtle hover:bg-surface-2 hover:text-foreground"
            >
              <X className="size-3.5" />
            </button>
          </div>
        );
      })}
      {rules.length === 0 && <div className="mt-1 text-[12px] text-subtle">None</div>}
    </div>
  );
}

/**
 * A new group, or a group's rules changed. A change is applied at the
 * version the dialog opened from: the rules shown are what it replaces, so a
 * newer group is not overwritten. The API then refuses it, and the dialog
 * says so and can start again from the group as it is.
 */
function GroupDialog({
  open,
  onClose,
  onCreated,
  networks,
  known,
  group,
}: {
  open: boolean;
  onClose: () => void;
  networks: string[];
  known: KnownNames;
} & ({ group: SecurityGroup; onCreated?: never } | { group?: undefined; onCreated: OnCreated })) {
  const project = useProjectName();
  const navigate = useNavigate();
  // The group as the dialog opened on it; the list refreshing does not move it.
  const [base, setBase] = useState(group);
  const [name, setName] = useState(group ? nameOf(group) : "");
  const [network, setNetwork] = useState(group?.spec?.network ?? (networks.includes("default") ? "default" : (networks[0] ?? "")));
  const [ingress, setIngress] = useState<Draft[]>(group?.spec?.ingress.map(draftOf) ?? [{ protocol: SecurityGroupProtocol.TCP, ports: "443", peer: "::/0" }]);
  const [egress, setEgress] = useState<Draft[]>(group?.spec?.egress.map(draftOf) ?? []);
  const createGroup = useCreate("securityGroup", project);
  const applyAt = useApplyAt("securityGroup");
  const reload = useReload("securityGroup", project, name);
  const submit = group ? applyAt : createGroup;
  const stale = !!group && isStale(applyAt.error);

  let obj: SecurityGroup;
  if (base) {
    // Only the rules change; the rest is the group as the dialog opened on it.
    obj = clone(SecurityGroupSchema, base);
    obj.spec ??= create(SecurityGroupSpecSchema);
    obj.spec.ingress = ingressOf(ingress);
    obj.spec.egress = egressOf(egress);
  } else {
    obj = create(SecurityGroupSchema, { metadata: { name }, spec: { network, ingress: ingressOf(ingress), egress: egressOf(egress) } });
  }
  const problems: string[] = [];
  if (!group) {
    const check = checkName(name, "security group", known);
    if (!check.ok) problems.push(check.reason);
    if (!network) problems.push("No network is chosen.");
  }
  if ([...ingress, ...egress].some((r) => !r.peer.trim())) problems.push("A rule has no peer.");
  const close = () => {
    createGroup.reset();
    applyAt.reset();
    onClose();
  };
  const send = () => {
    if (group) applyAt.mutate(obj, { onSuccess: close });
    else createGroup.mutate(obj, { onSuccess: () => onCreated?.(() => void navigate({ to: "/projects/$project/security-groups/$name", params: { project, name } })) });
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
  return (
    <Dialog open={open} onClose={close} title={group ? `Edit rules of ${nameOf(group)}` : "New security group"}>
      {!group && (
        <>
          <FormField label="Name">
            <input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} spellCheck={false} className={inputClass} />
          </FormField>
          <FormField label="Network" hint="Only interfaces on this network can join the group. It cannot be changed later.">
            <select value={network} onChange={(e) => setNetwork(e.target.value)} className={inputClass}>
              {networks.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </FormField>
        </>
      )}
      <RulesEditor title="Ingress" peer="From" rules={ingress} onChange={setIngress} />
      <RulesEditor title="Egress" peer="To" rules={egress} onChange={setEgress} />
      <FormEquivalent
        yaml={securityGroupYaml(obj)}
        cli={[`hangar apply ${group ? "" : "--create-only "}-p ${project} -f ${name || "group"}.yaml`]}
      />
      {stale && (
        <StaleNotice title="The rules changed after this dialog opened." reloading={reload.isPending} error={reload.error} onReload={startAgain} />
      )}
      <FormFooter
        problems={problems}
        blocked={stale}
        error={stale ? null : submit.error}
        failed={group ? "Applying the rules failed" : "Creating the security group failed"}
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
        onCancel={close}
        onSubmit={send}
      />
    </Dialog>
  );
}
