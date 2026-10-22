// The pieces the port forward pages and a machine's page share: a forward's
// state, where it sends, whether its target's security groups let its
// traffic in, its actions, and the Port forwards card of a machine's page.

import { Link } from "@tanstack/react-router";
import { Crosshair, Plus, ShieldCheck, Trash2 } from "lucide-react";
import { Notice } from "@/components/notice";
import { editLabelsAction, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { useProjectName } from "@/components/shell";
import { ErrorNotice, SkeletonRows } from "@/components/states";
import { buttonVariants, Card, CardHeader, CopyButton, Led } from "@/components/ui";
import { machinePath, type MachineView } from "@/lib/machines";
import { kindNoun } from "@/lib/network";
import {
  inboundOf,
  portForwardPerms,
  portLabel,
  publicAddress,
  stateOf,
  useForwardTargets,
  usePortForwardCan,
  usePortForwards,
  type ForwardProtocol,
  type ForwardTarget,
  type Inbound,
  type PortForward,
} from "@/lib/port-forwards";
import { ruleSentence } from "@/lib/security-group-rules";

/**
 * A forward's actions, on its page and in its row's menu: Retarget, Edit
 * labels and Delete, each shown once the caller may.
 */
// Screens only: the dialogs these open come with the API.
export function usePortForwardActions(pf: PortForward, can: Can): ActionSet {
  const deleting = !!pf.metadata.deleteTime;
  const actions: ObjectAction[] = [];
  const none = () => {};
  if (can(portForwardPerms.apply)) actions.push({ label: "Retarget", icon: Crosshair, onSelect: none });
  if (can(portForwardPerms.delete)) actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: none });
  return { actions, editLabels: editLabelsAction(can(portForwardPerms.apply), deleting, none), deleting, dialogs: null };
}

/** A forward's state, with its reason under it. */
export function ForwardStateCell({ pf }: { pf: PortForward }) {
  const s = stateOf(pf);
  return (
    <>
      <span className="inline-flex items-center gap-2">
        <Led tone={s.light} pulse={s.pulse} />
        {s.label}
      </span>
      {s.reason && <span className="block font-mono text-[11.5px] text-subtle">{s.reason}</span>}
    </>
  );
}

/** Where a forward sends: its machine, linked, the interface, and the port. */
export function ForwardTargetText({ t, protocol, port }: { t: ForwardTarget; protocol: ForwardProtocol; port: number }) {
  const project = useProjectName();
  return (
    <span>
      <Link to={machinePath(t.kind)} params={{ project, name: t.name }} className="font-medium text-accent hover:underline">
        {t.name}
      </Link>
      <span className="font-mono text-[12px] text-muted">
        {" "}
        · {t.iface} · {portLabel(port, protocol)}
      </span>
      <span className="ml-2 whitespace-nowrap break-normal text-[11.5px] text-subtle">{t.kind === "Container" ? "container" : "VM"}</span>
    </span>
  );
}

/** The public address in the monospace face with its copy button, or that the port is not drawn yet. */
export function AddressCell({ pf }: { pf: PortForward }) {
  const addr = publicAddress(pf);
  if (!addr) return <span className="font-sans text-[12.5px] text-subtle">port not drawn yet</span>;
  return (
    <span className="inline-flex items-center gap-1 font-mono text-[12px]">
      {addr}
      <CopyButton text={addr} />
    </span>
  );
}

/** Whether the forward's traffic gets in, from the project's interfaces and groups. */
export function useInbound(project: string, t: ForwardTarget, protocol: ForwardProtocol, port: number): Inbound | undefined {
  const { attachments, groups } = useForwardTargets(project);
  if (attachments.loading || groups.loading || attachments.error || groups.error) return undefined;
  return inboundOf(t, protocol, port, attachments.items, groups.items);
}

const what = (protocol: ForwardProtocol, port: number) => `${protocol} port ${port}`;

/** A group's name, linked to its page; the built-in default is no object and has none. */
function GroupLink({ name }: { name: string }) {
  const project = useProjectName();
  if (name === "default") return <span className="font-mono text-[12px] text-foreground">default</span>;
  return (
    <Link to="/projects/$project/security-groups/$name" params={{ project, name }} className="font-mono text-[12px] text-accent hover:underline">
      {name}
    </Link>
  );
}

const joined = (names: string[]) =>
  names.map((g, i) => (
    <span key={g}>
      {i > 0 && (i === names.length - 1 ? " and " : ", ")}
      <GroupLink name={g} />
    </span>
  ));

/**
 * What the target's security groups do to the forward's traffic: a line when
 * a rule lets it in from any IPv4 address, a notice when only some ranges
 * are let in, and a warning when none is, with the groups to edit.
 */
export function InboundNotice({ t, protocol, port, inbound }: { t: ForwardTarget; protocol: ForwardProtocol; port: number; inbound: Inbound | undefined }) {
  const project = useProjectName();
  if (!inbound) return null;
  const iface = `${t.name} · ${t.iface}`;
  if (!inbound.known) {
    return (
      <Notice tone="info" title={`${iface} does not exist yet`}>
        Its security groups are known once the {kindNoun(t.kind)} is made; until one of them lets in {what(protocol, port)}, the forward reaches nothing.
      </Notice>
    );
  }
  if (inbound.verdict === "open") {
    const a = inbound.admitting.find((x) => x.from === "0.0.0.0/0")!;
    return (
      <p className="flex items-start gap-2 text-[12.5px] text-muted">
        <ShieldCheck className="mt-px size-4 shrink-0 text-ok" strokeWidth={1.9} />
        <span>
          <GroupLink name={a.group} /> {ruleSentence(a.rule, "in").replace(/^Lets/, "lets")}.
        </span>
      </p>
    );
  }
  const editable = inbound.groups.filter((g) => g !== "default");
  const edits = editable.length > 0 && (
    <span className="mt-2 flex flex-wrap gap-2">
      {editable.map((g) => (
        <Link key={g} to="/projects/$project/security-groups/$name/edit" params={{ project, name: g }} className={buttonVariants({ size: "sm" })}>
          <ShieldCheck /> Edit rules of {g}
        </Link>
      ))}
    </span>
  );
  if (inbound.verdict === "some") {
    const from = [...new Set(inbound.admitting.map((a) => a.from))];
    return (
      <Notice tone="info" title={`Let in from ${from.join(", ")} only`}>
        {joined([...new Set(inbound.admitting.map((a) => a.group))])} {inbound.admitting.length === 1 ? "lets" : "let"} in {what(protocol, port)} from{" "}
        {from.join(", ")}; from any other address, the forward reaches nothing.
        {edits}
      </Notice>
    );
  }
  return (
    <Notice tone="warning" title={`No security group of ${iface} lets in ${what(protocol, port)}`}>
      Inbound traffic is denied unless a rule of one of the interface's groups lets it in, so the forward is in place and nothing reaches the{" "}
      {kindNoun(t.kind)}. {iface} is in {joined(inbound.groups)}.{edits}
    </Notice>
  );
}

/** A list row's word on the groups: said only when they let nothing in, or only some ranges. */
export function InboundBrief({ inbound }: { inbound: Inbound | undefined }) {
  if (!inbound?.known || inbound.verdict === "open") return null;
  return inbound.verdict === "none" ? (
    <span className="block text-[11.5px] text-pending">No security group lets it in</span>
  ) : (
    <span className="block text-[11.5px] text-subtle">From {[...new Set(inbound.admitting.map((a) => a.from))].join(", ")} only</span>
  );
}

function MachineForwardRow({ pf, project }: { pf: PortForward; project: string }) {
  const { target, protocol, targetPort } = pf.spec;
  const inbound = useInbound(project, target, protocol, targetPort);
  const s = stateOf(pf);
  return (
    <li className="px-4 py-2.5 text-[13px]">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
        <Link to="/projects/$project/port-forwards/$name" params={{ project, name: pf.metadata.name }} className="min-w-0 font-medium text-accent hover:underline">
          {pf.metadata.name}
        </Link>
        <AddressCell pf={pf} />
        <span className="font-mono text-[12px] text-muted">
          → {target.iface} · {portLabel(targetPort, protocol)}
        </span>
        <span className="ml-auto inline-flex items-center gap-2 text-[12.5px]">
          <Led tone={s.light} pulse={s.pulse} />
          {s.label}
        </span>
      </div>
      <InboundBrief inbound={inbound} />
    </li>
  );
}

/** A machine's page's Port forwards card: the forwards that send to it, and New port forward for it. */
export function MachinePortForwards({ m }: { m: MachineView }) {
  const forwards = usePortForwards(m.project);
  const { can } = usePortForwardCan(m.project);
  const mine = forwards.items.filter((f) => f.spec.target.kind === m.kind && f.spec.target.name === m.name);
  const iface = m.interfaces[0]?.name ?? "eth0";
  const action = can(portForwardPerms.apply) ? (
    <Link
      to="/projects/$project/new/port-forward"
      params={{ project: m.project }}
      search={{ target: `${m.kind}/${m.name}/${iface}` }}
      className={buttonVariants({ size: "sm" })}
    >
      <Plus /> New port forward
    </Link>
  ) : undefined;
  return (
    <Card>
      <CardHeader
        title={
          <>
            Port forwards {mine.length > 0 && <span className="ml-1 font-mono text-[11.5px] font-normal text-subtle">{mine.length}</span>}
          </>
        }
        action={action}
      />
      {forwards.loading ? (
        <SkeletonRows rows={2} />
      ) : forwards.error ? (
        <ErrorNotice error={forwards.error} title="The port forwards did not load" onRetry={forwards.refetch} />
      ) : mine.length === 0 ? (
        <div className="px-4 py-3 text-[13px] text-muted">
          No port forwards. A port forward opens one port of this {kindNoun(m.kind)} to the internet, on a port of the platform's shared IPv4 address.
        </div>
      ) : (
        <ul className="divide-y">
          {mine.map((pf) => (
            <MachineForwardRow key={pf.metadata.name} pf={pf} project={m.project} />
          ))}
        </ul>
      )}
    </Card>
  );
}
