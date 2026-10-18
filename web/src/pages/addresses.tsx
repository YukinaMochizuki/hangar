// A project's reserved public IPv6 addresses, one address, and binding,
// moving and unbinding one (docs/decisions/networking.md); reserving one is
// pages/create-address.tsx.

import { clone, create } from "@bufbuild/protobuf";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  AddressSchema,
  AddressSpecSchema,
  AddressTargetSchema,
  type Address,
} from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { Link2, Link2Off, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { Dialog } from "@/components/dialog";
import { DeleteDialog, FormFooter, StaleNotice } from "@/components/resource";
import { FormEquivalent, FormField, Input, Select } from "@/components/form";
import { ConditionsCard, EquivalentCard, Labels } from "@/components/object";
import { DetailBody, DetailFrame, DetailHead, ListFrame, NameFilter, PageHead } from "@/components/page";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Topbar, useProjectName } from "@/components/shell";
import { Button, buttonVariants, Card, CardHeader, CopyButton, Field, Led } from "@/components/ui";
import { date, machinePath, type MachineKind } from "@/lib/machines";
import { isStale, permission, useApplyAt, useChange, useReload } from "@/lib/network-actions";
import { useCan } from "@/lib/permissions";
import { addressYaml, conditionOf, kindNoun, nameOf, targetOf, useAddresses, useAttachments } from "@/lib/network";
import { bare } from "@/pages/machine-list";

/** Where an address stands, in the words of its Bound condition. */
function stateOf(a: Address): { label: string; light: "ok" | "pending" | "off"; reason?: string; message?: string } {
  const c = conditionOf(a.status?.conditions, "Bound");
  if (c?.status === 1) return { label: "Bound", light: "ok" };
  if (!targetOf(a.spec?.target)) return { label: "Unbound", light: "off" };
  return { label: "Waiting", light: "pending", reason: c?.reason, message: c?.message };
}

function StateCell({ a }: { a: Address }) {
  const s = stateOf(a);
  return (
    <>
      <span className="inline-flex items-center gap-2">
        <Led tone={s.light} />
        {s.label}
      </span>
      {s.reason && <span className="block font-mono text-[11.5px] text-subtle">{s.reason}</span>}
    </>
  );
}

function TargetText({ a }: { a: Address }) {
  const project = useProjectName();
  const t = targetOf(a.spec?.target);
  if (!t) return <span className="text-subtle">none</span>;
  const iface = a.spec?.target?.interface || "eth0";
  return (
    <span>
      <Link to={machinePath(t.kind)} params={{ project, name: t.name }} className="font-medium text-accent hover:underline">
        {t.name}
      </Link>
      <span className="font-mono text-[12px] text-muted"> · {iface}</span>
      <span className="ml-2 text-[11.5px] text-subtle">{t.kind === "Container" ? "container" : "VM"}</span>
    </span>
  );
}

export function AddressList() {
  const project = useProjectName();
  const addrs = useAddresses(project);
  const [query, setQuery] = useState("");
  const rows = addrs.items.filter((a) => !query || nameOf(a).includes(query));
  const { can: may } = useCan({ level: "project", name: project }, [permission("address", "apply")]);
  const newButton = may(permission("address", "apply")) ? (
    <Link to="/projects/$project/new/address" params={{ project }} className={buttonVariants({ variant: "primary" })}>
      <Plus /> Reserve address
    </Link>
  ) : undefined;
  return (
    <>
      <Topbar crumbs={["Addresses"]} />
      <PageHead
        title="Addresses"
        description="Public IPv6 addresses, each kept until deleted. Bound to an interface, one replaces its automatic address; moving it restarts no machine."
        action={newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ ...addrs, count: addrs.items.length }}
          noun="addresses"
          empty={{ title: "No addresses", text: "Every interface has an automatic public IPv6 address, given up when its machine goes.", action: newButton }}
        >
          <NameFilter value={query} onChange={setQuery} />
          <Card className="md:hidden">
            <ul className="divide-y">
              {rows.map((a) => (
                <li key={nameOf(a)}>
                  <Link to="/projects/$project/addresses/$name" params={{ project, name: nameOf(a) }} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                    <span className="min-w-0 flex-1">
                      <span className="block truncate font-medium">{nameOf(a)}</span>
                      <span className="block truncate font-mono text-[11.5px] text-subtle">{bare(a.status?.ipv6 ?? "") || "not drawn yet"}</span>
                    </span>
                    <span className="shrink-0 text-right">
                      <StateCell a={a} />
                    </span>
                  </Link>
                </li>
              ))}
              {rows.length === 0 && <NoMatch noun="address" />}
            </ul>
          </Card>
          <Card className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[820px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[18%]">Name</Th>
                  <Th>Address</Th>
                  <Th>State</Th>
                  <Th>Bound to</Th>
                  <Th>Created</Th>
                </tr>
              </thead>
              <tbody>
                {rows.map((a) => (
                  <tr key={nameOf(a)} className={tbodyRow}>
                    <td className="max-w-0 px-4 py-2.5">
                      <Link to="/projects/$project/addresses/$name" params={{ project, name: nameOf(a) }} className="block truncate font-medium group-hover:text-accent">
                        {nameOf(a)}
                      </Link>
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px]">
                      {a.status?.ipv6 ? bare(a.status.ipv6) : <span className="font-sans text-subtle">not drawn yet</span>}
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <StateCell a={a} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <TargetText a={a} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-muted">{date(a.metadata?.createTime)?.toISOString().slice(0, 10)}</td>
                  </tr>
                ))}
                {rows.length === 0 && <NoMatch noun="address" colSpan={5} />}
              </tbody>
            </table>
          </Card>
          <p className="mt-3 text-[12px] text-subtle">
            {rows.length} of {addrs.items.length}
          </p>
        </ListFrame>
      </div>
    </>
  );
}

export function AddressDetail({ name }: { name: string }) {
  const project = useProjectName();
  const addrs = useAddresses(project);
  const [deleting, setDeleting] = useState(false);
  const [binding, setBinding] = useState(false);
  const [unbinding, setUnbinding] = useState(false);
  const navigate = useNavigate();
  const { can } = useCan({ level: "project", name: project }, [permission("address", "apply"), permission("address", "delete")]);
  const mayApply = can(permission("address", "apply"));
  const mayDelete = can(permission("address", "delete"));
  const a = addrs.items.find((x) => nameOf(x) === name);
  const s = a ? stateOf(a) : undefined;
  const bound = a && targetOf(a.spec?.target);
  return (
    <DetailFrame crumbs={["Addresses", name]} state={addrs} found={!!a} noun="address" name={name}>
      {a && s && (
        <>
          <DetailHead
            name={name}
            kind="Address"
            facts={
              <>
                <span className="inline-flex items-center gap-2">
                  <Led tone={s.light} />
                  {s.label}
                </span>
                {a.status?.ipv6 && <span className="font-mono text-[12px]">{bare(a.status.ipv6)}</span>}
              </>
            }
            actions={
              mayApply || mayDelete ? (
                <>
                  {mayApply && (
                    <Button onClick={() => setBinding(true)}>
                      <Link2 /> {bound ? "Move" : "Bind"}
                    </Button>
                  )}
                  {mayApply && bound && (
                    <Button onClick={() => setUnbinding(true)}>
                      <Link2Off /> Unbind
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
                    <Field label="Address" mono>
                      {a.status?.ipv6 ? (
                        <span className="flex items-center gap-1">
                          {bare(a.status.ipv6)}
                          <CopyButton text={bare(a.status.ipv6)} />
                        </span>
                      ) : (
                        <span className="font-sans text-subtle">not drawn yet: no node has an IPv6 prefix</span>
                      )}
                    </Field>
                    <Field label="Target">
                      <TargetText a={a} />
                    </Field>
                    <Field label="State">
                      <span className="inline-flex items-center gap-2">
                        <Led tone={s.light} />
                        {s.label}
                        {s.reason && <span className="font-mono text-[12px] text-muted">{s.reason}</span>}
                      </span>
                      {s.message && <span className="block text-[12.5px] text-muted">{s.message}</span>}
                    </Field>
                    <Field label="Held by" mono>
                      {a.status?.machine || <span className="font-sans text-subtle">no machine</span>}
                    </Field>
                    <Field label="Labels">
                      <Labels labels={a.metadata?.labels ?? {}} />
                    </Field>
                    <Field label="Created">{date(a.metadata?.createTime)?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
                    <Field label="UID" mono>
                      <span className="text-muted">{a.metadata?.uid}</span>
                    </Field>
                  </dl>
                </Card>
                <ConditionsCard conditions={a.status?.conditions ?? []} />
              </>
            }
            side={
              <EquivalentCard
                cli={[`hangar get addr ${name} -p ${project}`, `hangar apply -p ${project} -f ${name}.yaml`, `hangar delete addr ${name} -p ${project}`]}
                yaml={addressYaml(a)}
              />
            }
          />
          <BindDialog key={`bind-${binding}`} open={binding} onClose={() => setBinding(false)} address={a} />
          <UnbindDialog key={`unbind-${unbinding}`} open={unbinding} onClose={() => setUnbinding(false)} address={a} />
          <DeleteDialog
            kind="address"
            name={name}
            open={deleting}
            onClose={() => setDeleting(false)}
            after={() => void navigate({ to: "/projects/$project/addresses", params: { project } })}
            consequence={
              bound
                ? "The address is released once its interface lets go of it; the interface then draws a new automatic address."
                : "The address is released."
            }
          />
        </>
      )}
    </DetailFrame>
  );
}

// The interface list's entry for a target typed by hand.
const typed = "typed";

/**
 * InterfacePick is an interface chosen for an address: a listed one as
 * kind/machine/interface, the typed one, or none (""); own is what is typed.
 */
export type InterfacePick = { choice: string; own: { kind: MachineKind; machine: string; iface: string } };

/** pickOf is a pick of the choice given, with nothing typed. */
export const pickOf = (choice: string): InterfacePick => ({ choice, own: { kind: "Container", machine: "", iface: "eth0" } });

/** pickTarget is the target a pick stands for, as the API takes it; undefined for none. */
export function pickTarget({ choice, own }: InterfacePick) {
  const c = choice === typed ? `${own.kind}/${own.machine.trim()}/${own.iface.trim()}` : choice;
  if (!c) return undefined;
  const [kind, machine, iface] = c.split("/") as [MachineKind, string, string];
  if (!machine) return undefined;
  return create(AddressTargetSchema, {
    object: { case: kind === "Container" ? "container" : "virtualMachine", value: machine },
    interface: iface || "eth0",
  });
}

/** pickProblem says why a pick cannot be sent, if it cannot. */
export const pickProblem = (p: InterfacePick) => (p.choice === typed && !p.own.machine.trim() ? "The machine's name is empty." : undefined);

/**
 * InterfaceField chooses an address's interface among the project's
 * machines' interfaces, or a machine typed by name. current is the
 * interface the address is bound to, listed even when no machine has it.
 */
export function InterfaceField({
  pick,
  onChange,
  none,
  hint,
  current,
}: {
  pick: InterfacePick;
  onChange: (p: InterfacePick) => void;
  /** The label of no interface. */
  none: string;
  hint: string;
  current?: { value: string; label: string };
}) {
  const project = useProjectName();
  const att = useAttachments(project);
  const listed = att.items.map((x) => `${x.kind}/${x.name}/${x.iface}`);
  const { own } = pick;
  return (
    <>
      <Select aria-label="Interface" value={pick.choice} onChange={(e) => onChange({ ...pick, choice: e.target.value })}>
        <option value="">{none}</option>
        {current && !listed.includes(current.value) && <option value={current.value}>{current.label}</option>}
        {att.items.map((x) => (
          <option key={`${x.kind}/${x.name}/${x.iface}`} value={`${x.kind}/${x.name}/${x.iface}`}>
            {x.name} · {x.iface} ({kindNoun(x.kind)})
          </option>
        ))}
        <option value={typed}>A machine by name</option>
      </Select>
      <span className="mt-1 block text-[11.5px] text-subtle">{hint}</span>
      {pick.choice === typed && (
        <div className="mt-4 grid grid-cols-[1fr_1.4fr_72px] gap-2">
          <FormField label="Kind" className="mt-0">
            <Select value={own.kind} onChange={(e) => onChange({ ...pick, own: { ...own, kind: e.target.value as MachineKind } })}>
              <option value="Container">Container</option>
              <option value="VirtualMachine">Virtual machine</option>
            </Select>
          </FormField>
          <FormField label="Machine" className="mt-0">
            <Input value={own.machine} onChange={(e) => onChange({ ...pick, own: { ...own, machine: e.target.value.toLowerCase() } })} spellCheck={false} />
          </FormField>
          <FormField label="On" className="mt-0">
            <Input value={own.iface} onChange={(e) => onChange({ ...pick, own: { ...own, iface: e.target.value } })} spellCheck={false} />
          </FormField>
          <span className="col-span-3 -mt-1 text-[11.5px] text-subtle">
            A machine that does not exist yet is waited for: the address binds once it is made.
          </span>
        </div>
      )}
    </>
  );
}

/**
 * Bind or move an address. Both apply at the version the dialog opened
 * from: the interface chosen depends on where the address was then, so a
 * newer address is not overwritten. The API then refuses it, and the dialog
 * says so and can start again from the address as it is.
 */
function BindDialog({ open, onClose, address }: { open: boolean; onClose: () => void; address: Address }) {
  const project = useProjectName();
  const name = nameOf(address);
  // The address as the dialog opened on it; the list refreshing does not move it.
  const [base, setBase] = useState(address);
  const t = targetOf(base.spec?.target);
  const current = t ? `${t.kind}/${t.name}/${base.spec?.target?.interface || "eth0"}` : "";
  const [pick, setPick] = useState(pickOf(current));
  const applyAt = useApplyAt("address");
  const reload = useReload("address", project, name);
  const stale = isStale(applyAt.error);
  const target = pickTarget(pick);

  // Only the target changes; the rest is the address as the dialog opened on it.
  const obj = clone(AddressSchema, base);
  obj.spec ??= create(AddressSpecSchema);
  obj.spec.target = target;
  const problems: string[] = [];
  if (!target) problems.push("No interface is chosen.");
  const typedProblem = pickProblem(pick);
  if (typedProblem) problems.push(typedProblem);
  const close = () => {
    applyAt.reset();
    onClose();
  };
  const startAgain = () =>
    reload.mutate(undefined, {
      onSuccess: (fresh) => {
        setBase(fresh);
        const ft = targetOf(fresh.spec?.target);
        setPick(pickOf(ft ? `${ft.kind}/${ft.name}/${fresh.spec?.target?.interface || "eth0"}` : ""));
        applyAt.reset();
      },
    });
  const verb = t ? "Move" : "Bind";
  return (
    <Dialog open={open} onClose={close} title={`${verb} ${name}`}>
      <div className="text-[12.5px] text-foreground">Interface</div>
      <div className="mt-1.5">
        <InterfaceField
          pick={pick}
          onChange={setPick}
          none="No interface"
          current={t ? { value: current, label: `${t.name} · ${base.spec?.target?.interface || "eth0"} (${kindNoun(t.kind)})` } : undefined}
          hint={
            t
              ? "The interface it leaves draws a new automatic address. Neither machine restarts; the move waits until the old interface lets go."
              : "Bound, the address replaces the interface's automatic one. No machine restarts."
          }
        />
      </div>
      <FormEquivalent yaml={addressYaml(obj)} cli={[`hangar apply -p ${project} -f ${name}.yaml`]} />
      {stale && (
        <StaleNotice title="The address changed after this dialog opened." reloading={reload.isPending} error={reload.error} onReload={startAgain} />
      )}
      <FormFooter
        problems={problems}
        blocked={stale}
        error={stale ? null : applyAt.error}
        failed={`${t ? "Moving" : "Binding"} the address failed`}
        pending={applyAt.isPending}
        pendingLabel={`${t ? "Moving" : "Binding"}…`}
        label={
          <>
            <Link2 /> {verb}
          </>
        }
        onCancel={close}
        onSubmit={() => applyAt.mutate(obj, { onSuccess: close })}
      />
    </Dialog>
  );
}

function UnbindDialog({ open, onClose, address }: { open: boolean; onClose: () => void; address: Address }) {
  const project = useProjectName();
  const change = useChange("address", project, nameOf(address));
  const t = targetOf(address.spec?.target);
  const obj = create(AddressSchema, { metadata: { name: nameOf(address), labels: address.metadata?.labels ?? {} }, spec: {} });
  const close = () => {
    change.reset();
    onClose();
  };
  return (
    <Dialog open={open} onClose={close} title={`Unbind ${nameOf(address)}`}>
      <p>
        {t ? `${t.name}'s ${address.spec?.target?.interface || "eth0"}` : "Its interface"} gives the address up and draws a new automatic
        one. The address stays reserved.
      </p>
      <FormEquivalent yaml={addressYaml(obj)} cli={[`hangar apply -p ${project} -f ${nameOf(address)}.yaml`]} />
      <FormFooter
        problems={[]}
        error={change.error}
        failed="Unbinding the address failed"
        pending={change.isPending}
        pendingLabel="Unbinding…"
        label={
          <>
            <Link2Off /> Unbind
          </>
        }
        onCancel={close}
        onSubmit={() =>
          change.mutate(
            (o) => {
              if (o.spec) o.spec.target = undefined;
            },
            { onSuccess: close },
          )
        }
      />
    </Dialog>
  );
}
