// The pieces the port forward pages and a machine's page share: a forward's
// actions and its Retarget dialog, its state, where it leads, whether its
// target's security groups let its traffic in, and the Port forwards card of
// a machine's page.

import { clone, create } from "@bufbuild/protobuf";
import { PortForwardSchema, PortForwardSpecSchema, PortForwardTargetSchema, type PortForward } from "@hangar/api/hangar/network/v1alpha1/port_forward_pb";
import { Link } from "@tanstack/react-router";
import { Crosshair, Plus, ShieldCheck, Trash2 } from "lucide-react";
import { useState } from "react";
import { Dialog } from "@/components/dialog";
import { FormEquivalent } from "@/components/form";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { MachineTargetText, SecurityGroupLink } from "@/components/links";
import { Notice } from "@/components/notice";
import { editLabelsAction, ObjectRowMenu, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { DeleteDialog, FormFooter, StaleNotice } from "@/components/resource";
import { useProjectName } from "@/components/shell";
import { ErrorNotice, SkeletonRows } from "@/components/states";
import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { buttonVariants, Card, CardHeader, CopyButton, phaseLabel, StateText } from "@/components/ui";
import { cn } from "@/lib/cn";
import { machinePath, useMachines, type MachineView } from "@/lib/machines";
import { nameOf, portForwardYaml, usePortForwards } from "@/lib/network";
import { isStale, permission, useApplyAt, useNetworkCan, useReload } from "@/lib/network-actions";
import {
  inboundOf,
  portLabel,
  publicAddress,
  specOf,
  stateOf,
  useForwardTargets,
  type ForwardProtocol,
  type ForwardTarget,
  type Inbound,
} from "@/lib/port-forwards";
import { ruleSentence } from "@/lib/security-group-rules";
import { InterfaceField, pickTarget, type InterfacePick } from "@/components/interface-field";

/** A pick of the interface field as a forward's target, as the API takes it; undefined for none. */
export function forwardTargetOfPick(p: InterfacePick) {
  const t = pickTarget(p);
  return t && create(PortForwardTargetSchema, { object: t.object, interface: t.interface });
}

const pickOfForward = (pf: PortForward) => {
  const t = specOf(pf).target;
  return t ? `${t.kind}/${t.name}/${t.iface}` : "";
};

/**
 * A forward's actions, on its page and in its row's menu: Retarget, Edit
 * labels and Delete, each shown once TestPermissions holds its permission;
 * `after` follows a delete the API took.
 */
export function usePortForwardActions(pf: PortForward, can: Can, after?: () => void): ActionSet {
  const name = nameOf(pf);
  const [open, setOpen] = useState<"retarget" | "delete" | "labels" | null>(null);
  const deleting = !!pf.metadata?.deleteTime;
  const mayApply = can(permission("portForward", "apply"));
  const actions: ObjectAction[] = [];
  if (mayApply) actions.push({ label: "Retarget", icon: Crosshair, onSelect: () => setOpen("retarget") });
  if (can(permission("portForward", "delete"))) actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: () => setOpen("delete") });
  const close = () => setOpen(null);
  const port = pf.status?.port;
  return {
    actions,
    editLabels: editLabelsAction(mayApply, deleting, () => setOpen("labels")),
    deleting,
    dialogs: (
      <>
        {open === "retarget" && <RetargetDialog onClose={close} forward={pf} />}
        {open === "labels" && <EditLabelsDialog kind="portForward" object={pf} onClose={close} />}
        {open === "delete" && (
          <DeleteDialog
            kind="portForward"
            name={name}
            open
            onClose={close}
            after={after}
            consequence={
              port
                ? `Port ${port} closes, and is free to be drawn again once the node that opened it has closed it.`
                : "The port forward is deleted."
            }
          />
        )}
      </>
    ),
  };
}

/** A forward's row menu: the actions of its page. */
export function ForwardRowMenu({ pf, can }: { pf: PortForward; can: Can }) {
  return <ObjectRowMenu name={nameOf(pf)} set={usePortForwardActions(pf, can)} />;
}

/**
 * Retarget a forward: another interface, at the version the dialog opened
 * on, so a newer forward is not written over; the API then refuses it, and
 * the dialog says so and can start again from the forward as it is. The
 * public port and the protocol stay.
 */
function RetargetDialog({ onClose, forward }: { onClose: () => void; forward: PortForward }) {
  const project = useProjectName();
  const name = nameOf(forward);
  const [base, setBase] = useState(forward);
  const [pick, setPick] = useState(pickOfForward(forward));
  const applyAt = useApplyAt("portForward");
  const reload = useReload("portForward", project, name);
  const stale = isStale(applyAt.error);
  const target = forwardTargetOfPick(pick);
  const was = specOf(base).target;
  const current = was ? `${was.kind}/${was.name}/${was.iface}` : "";

  const obj = clone(PortForwardSchema, base);
  obj.spec ??= create(PortForwardSpecSchema);
  obj.spec.target = target;
  const problems: string[] = [];
  if (!target) problems.push("No machine is chosen.");
  if (target && pick === current) problems.push("It leads there already.");
  const close = () => {
    applyAt.reset();
    onClose();
  };
  const startAgain = () =>
    reload.mutate(undefined, {
      onSuccess: (fresh) => {
        setBase(fresh);
        setPick(pickOfForward(fresh));
        applyAt.reset();
      },
    });
  const port = base.status?.port;
  return (
    <Dialog open onClose={close} title={`Retarget ${name}`}>
      <InterfaceField value={pick} onChange={setPick} current={current} hint={port ? `Port ${port} stays.` : undefined} />
      <FormEquivalent yaml={portForwardYaml(obj)} cli={[`hangar apply -p ${project} -f ${name}.yaml`]} />
      {stale && (
        <StaleNotice title="The port forward changed after this dialog opened." reloading={reload.isPending} error={reload.error} onReload={startAgain} />
      )}
      <FormFooter
        problems={problems}
        blocked={stale}
        error={stale ? null : applyAt.error}
        failed="Retargeting the port forward failed"
        pending={applyAt.isPending}
        pendingLabel="Retargeting…"
        label={
          <>
            <Crosshair /> Retarget
          </>
        }
        onCancel={close}
        onSubmit={() => applyAt.mutate(obj, { onSuccess: close })}
      />
    </Dialog>
  );
}

/** A forward's state, with its reason under it. */
export function ForwardStateCell({ pf }: { pf: PortForward }) {
  const s = stateOf(pf);
  return <StateText tone={s.light} pulse={s.pulse} label={s.label} reason={s.reason} />;
}

/** Where a forward leads: its machine, linked, the interface, and the port; or that it names none. */
export function ForwardTargetText({ t, protocol, port }: { t?: ForwardTarget; protocol: ForwardProtocol; port: number }) {
  return <MachineTargetText t={t} detail={t && ` · ${t.iface} · ${portLabel(port, protocol)}`} empty="no target" />;
}

/**
 * The target of a Ready forward whose machine is not running, with its
 * state; undefined otherwise, and while the project's machines load or
 * when they failed. Ready says the forward is in place, as an address is
 * Bound whatever its machine does, so the console says this beside it.
 */
export function useIdleTarget(pf: PortForward): { t: ForwardTarget; state: string } | undefined {
  const machines = useMachines(pf.metadata?.project ?? "");
  const t = specOf(pf).target;
  const ready = pf.status?.conditions.find((c) => c.type === "Ready")?.status === ConditionStatus.TRUE;
  if (!t || !ready || pf.metadata?.deleteTime || machines.loading || machines.error) return undefined;
  const m = machines.items.find((x) => x.kind === t.kind && x.name === t.name);
  if (!m || (m.phase === MachinePhase.RUNNING && !m.stopped && !m.deleting)) return undefined;
  return { t, state: m.deleting ? "Deleting" : m.stopped ? "Stopped" : phaseLabel(m.phase) };
}

/**
 * "Ready, but web is not running · Starting", muted, the machine linked;
 * nothing while it runs. plain leaves the link out, inside a row that is a
 * link itself.
 */
export function NotRunningNote({ pf, className, plain }: { pf: PortForward; className?: string; plain?: boolean }) {
  const project = useProjectName();
  const idle = useIdleTarget(pf);
  if (!idle) return null;
  return (
    <span className={cn("text-[11.5px] text-subtle", className)}>
      Ready, but{" "}
      {plain ? (
        <span className="text-muted">{idle.t.name}</span>
      ) : (
        <Link to={machinePath(idle.t.kind)} params={{ project, name: idle.t.name }} className="text-muted underline-offset-2 hover:text-accent hover:underline">
          {idle.t.name}
        </Link>
      )}{" "}
      is not running · {idle.state}
    </span>
  );
}

/** What stands in for the address before it is known: the port alone, or nothing yet. */
export const noAddress = (pf: PortForward) => (pf.status?.port ? `port ${pf.status.port}, no address yet` : "no port yet");

/** The public address in the monospace face with its copy button, or what is known of it so far. */
export function AddressCell({ pf }: { pf: PortForward }) {
  const addr = publicAddress(pf);
  if (!addr) return <span className="font-sans text-[12.5px] text-subtle">{noAddress(pf)}</span>;
  return (
    <span className="inline-flex items-center gap-1 font-mono text-[12px]">
      {addr}
      <CopyButton text={addr} />
    </span>
  );
}

/** Whether the forward's traffic gets in, from the project's interfaces and groups; undefined while they load or when they failed. */
export function useInbound(project: string, t: ForwardTarget | undefined, protocol: ForwardProtocol, port: number): Inbound | undefined {
  const { attachments, groups } = useForwardTargets(project);
  if (!t || attachments.loading || groups.loading || attachments.error || groups.error) return undefined;
  return inboundOf(t, protocol, port, attachments.items, groups.items);
}

/** useInbound for a forward as the API has it. */
export function useForwardInbound(pf: PortForward): Inbound | undefined {
  const { target, protocol, targetPort } = specOf(pf);
  return useInbound(pf.metadata?.project ?? "", target, protocol, targetPort);
}

const what = (protocol: ForwardProtocol, port: number) => `${protocol} port ${port}`;

const joined = (names: string[]) =>
  names.map((g, i) => (
    <span key={g}>
      {i > 0 && (i === names.length - 1 ? " and " : ", ")}
      <SecurityGroupLink name={g} />
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
        Nothing gets through until one of its security groups lets in {what(protocol, port)}.
      </Notice>
    );
  }
  if (inbound.verdict === "open") {
    const a = inbound.admitting.find((x) => x.from === "0.0.0.0/0")!;
    return (
      <p className="flex items-start gap-2 text-[12.5px] text-muted">
        <ShieldCheck className="mt-px size-4 shrink-0 text-ok" strokeWidth={1.9} />
        <span>
          <SecurityGroupLink name={a.group} /> {ruleSentence(a.rule, "in").replace(/^Lets/, "lets")}.
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
        {joined([...new Set(inbound.admitting.map((a) => a.group))])} {inbound.admitting.length === 1 ? "lets" : "let"} it in.
        {edits}
      </Notice>
    );
  }
  return (
    <Notice tone="warning" title={`No security group lets in ${what(protocol, port)}`}>
      {iface} is in {inbound.groups.length > 0 ? joined(inbound.groups) : "no group"}.{edits}
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
  const { target, protocol, targetPort } = specOf(pf);
  const inbound = useForwardInbound(pf);
  const s = stateOf(pf);
  return (
    <li className="px-4 py-2.5 text-[13px]">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
        <Link to="/projects/$project/port-forwards/$name" params={{ project, name: nameOf(pf) }} className="min-w-0 font-medium text-accent hover:underline">
          {nameOf(pf)}
        </Link>
        <AddressCell pf={pf} />
        <span className="font-mono text-[12px] text-muted">
          → {target?.iface ?? "eth0"} · {portLabel(targetPort, protocol)}
        </span>
        <StateText tone={s.light} pulse={s.pulse} label={s.label} className="ml-auto text-[12.5px]" />
      </div>
      <NotRunningNote pf={pf} className="block" />
      {!pf.metadata?.deleteTime && <InboundBrief inbound={inbound} />}
    </li>
  );
}

/** A machine's page's Port forwards card: the forwards that lead to it, and New port forward for it. */
export function MachinePortForwards({ m }: { m: MachineView }) {
  const forwards = usePortForwards(m.project);
  const { can } = useNetworkCan("portForward", m.project);
  const mine = forwards.items.filter((f) => {
    const t = specOf(f).target;
    return t?.kind === m.kind && t.name === m.name;
  });
  const iface = m.interfaces[0]?.name ?? "eth0";
  const action = can(permission("portForward", "apply")) ? (
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
          No port forwards.
        </div>
      ) : (
        <ul className="divide-y">
          {mine.map((pf) => (
            <MachineForwardRow key={nameOf(pf)} pf={pf} project={m.project} />
          ))}
        </ul>
      )}
    </Card>
  );
}
