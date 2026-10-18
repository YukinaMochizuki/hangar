// A project's reserved public IPv6 addresses, one address, and reserving,
// binding, moving and unbinding one (docs/decisions/networking.md).

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
  Led,
  ListFrame,
  NameFilter,
  NoMatch,
  PageHead,
  tbodyRow,
  Th,
  theadRow,
} from "@/components/resource";
import { Topbar, useProjectName } from "@/components/shell";
import { Button, Card, CardHeader, CopyButton, Field } from "@/components/ui";
import { checkName, type KnownNames } from "@/lib/create";
import { date, machinePath, type MachineKind } from "@/lib/machines";
import { isStale, permission, useApplyAt, useChange, useCreate, useReload } from "@/lib/network-actions";
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
        <Led light={s.light} />
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
  const [creating, setCreating] = useState(false);
  const rows = addrs.items.filter((a) => !query || nameOf(a).includes(query));
  const { can: may } = useCan({ level: "project", name: project }, [permission("address", "apply")]);
  const newButton = may(permission("address", "apply")) ? (
    <Button variant="primary" onClick={() => setCreating(true)}>
      <Plus /> Reserve address
    </Button>
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
      <BindDialog
        key={String(creating)}
        open={creating}
        onClose={() => setCreating(false)}
        known={{ loading: addrs.loading, error: addrs.error, names: addrs.items.map(nameOf) }}
      />
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
                  <Led light={s.light} />
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
                        <Led light={s.light} />
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
              <Equivalent
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

/** The target an interface choice stands for, as the API takes it. */
function targetFrom(choice: string) {
  if (!choice) return undefined;
  const [kind, machine, iface] = choice.split("/") as [MachineKind, string, string];
  if (!machine) return undefined;
  return create(AddressTargetSchema, {
    object: { case: kind === "Container" ? "container" : "virtualMachine", value: machine },
    interface: iface || "eth0",
  });
}

// The interface list's entry for a target typed by hand.
const typed = "typed";

/**
 * Reserve a new address, or bind or move one. Binding and moving apply at
 * the version the dialog opened from: the interface chosen depends on where
 * the address was then, so a newer address is not overwritten. The API then
 * refuses it, and the dialog says so and can start again from the address
 * as it is.
 */
function BindDialog({ open, onClose, address, known }: { open: boolean; onClose: () => void; address?: Address; known?: KnownNames }) {
  const project = useProjectName();
  const navigate = useNavigate();
  const att = useAttachments(project);
  // The address as the dialog opened on it; the list refreshing does not move it.
  const [base, setBase] = useState(address);
  const t = targetOf(base?.spec?.target);
  const [name, setName] = useState(address ? nameOf(address) : "");
  const current = t ? `${t.kind}/${t.name}/${base?.spec?.target?.interface || "eth0"}` : "";
  const [choice, setChoice] = useState(current);
  const [own, setOwn] = useState({ kind: "Container" as MachineKind, machine: "", iface: "eth0" });
  const reserve = useCreate("address", project);
  const applyAt = useApplyAt("address");
  const reload = useReload("address", project, name);
  const submit = address ? applyAt : reserve;
  const stale = !!address && isStale(applyAt.error);
  const target = targetFrom(choice === typed ? `${own.kind}/${own.machine.trim()}/${own.iface.trim()}` : choice);

  let obj: Address;
  if (base) {
    // Only the target changes; the rest is the address as the dialog opened on it.
    obj = clone(AddressSchema, base);
    obj.spec ??= create(AddressSpecSchema);
    obj.spec.target = target;
  } else {
    obj = create(AddressSchema, { metadata: { name }, spec: { target } });
  }
  const problems: string[] = [];
  if (!address) {
    const check = checkName(name, "address", known ?? { loading: false, error: null, names: [] });
    if (!check.ok) problems.push(check.reason);
  } else if (!target) problems.push("No interface is chosen.");
  if (choice === typed && !own.machine.trim()) problems.push("The machine's name is empty.");
  const close = () => {
    reserve.reset();
    applyAt.reset();
    onClose();
  };
  const send = () => {
    if (address) applyAt.mutate(obj, { onSuccess: close });
    else reserve.mutate(obj, { onSuccess: () => void navigate({ to: "/projects/$project/addresses/$name", params: { project, name } }) });
  };
  const startAgain = () =>
    reload.mutate(undefined, {
      onSuccess: (fresh) => {
        setBase(fresh);
        const ft = targetOf(fresh.spec?.target);
        setChoice(ft ? `${ft.kind}/${ft.name}/${fresh.spec?.target?.interface || "eth0"}` : "");
        applyAt.reset();
      },
    });
  const listedChoices = att.items.map((x) => `${x.kind}/${x.name}/${x.iface}`);
  const verb = !address ? "Reserve" : t ? "Move" : "Bind";
  return (
    <Dialog open={open} onClose={close} title={address ? `${verb} ${nameOf(address)}` : "Reserve address"}>
      {!address && (
        <FormField label="Name">
          <input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} spellCheck={false} className={inputClass} />
        </FormField>
      )}
      <FormField
        label="Interface"
        hint={
          address && t
            ? "The interface it leaves draws a new automatic address. Neither machine restarts; the move waits until the old interface lets go."
            : "Bound, the address replaces the interface's automatic one. No machine restarts."
        }
      >
        <select aria-label="Interface" value={choice} onChange={(e) => setChoice(e.target.value)} className={inputClass}>
          <option value="">{address ? "No interface" : "No interface: reserved, unbound"}</option>
          {current && !listedChoices.includes(current) && (
            <option value={current}>
              {t?.name} · {base?.spec?.target?.interface || "eth0"} ({t && kindNoun(t.kind)})
            </option>
          )}
          {att.items.map((x) => (
            <option key={`${x.kind}/${x.name}/${x.iface}`} value={`${x.kind}/${x.name}/${x.iface}`}>
              {x.name} · {x.iface} ({kindNoun(x.kind)})
            </option>
          ))}
          <option value={typed}>A machine by name</option>
        </select>
      </FormField>
      {choice === typed && (
        <div className="grid grid-cols-[1fr_1.4fr_72px] gap-2">
          <FormField label="Kind">
            <select value={own.kind} onChange={(e) => setOwn({ ...own, kind: e.target.value as MachineKind })} className={inputClass}>
              <option value="Container">Container</option>
              <option value="VirtualMachine">Virtual machine</option>
            </select>
          </FormField>
          <FormField label="Machine">
            <input value={own.machine} onChange={(e) => setOwn({ ...own, machine: e.target.value.toLowerCase() })} spellCheck={false} className={inputClass} />
          </FormField>
          <FormField label="On">
            <input value={own.iface} onChange={(e) => setOwn({ ...own, iface: e.target.value })} spellCheck={false} className={inputClass} />
          </FormField>
          <span className="col-span-3 -mt-1 text-[11.5px] text-subtle">
            A machine that does not exist yet is waited for: the address binds once it is made.
          </span>
        </div>
      )}
      <FormEquivalent yaml={addressYaml(obj)} cli={[`hangar apply ${address ? "" : "--create-only "}-p ${project} -f ${name || "address"}.yaml`]} />
      {stale && (
        <StaleNotice title="The address changed after this dialog opened." reloading={reload.isPending} error={reload.error} onReload={startAgain} />
      )}
      <FormFooter
        problems={problems}
        blocked={stale}
        error={stale ? null : submit.error}
        failed={`${address ? (t ? "Moving" : "Binding") : "Reserving"} the address failed`}
        pending={submit.isPending}
        pendingLabel={`${address ? (t ? "Moving" : "Binding") : "Reserving"}…`}
        label={
          <>
            {address ? <Link2 /> : <Plus />} {verb}
          </>
        }
        onCancel={close}
        onSubmit={send}
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
