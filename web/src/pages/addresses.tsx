// A project's reserved public IPv6 addresses, one address, and binding,
// moving and unbinding one (docs/decisions/networking.md); reserving one is
// pages/create-address.tsx.

import { clone, create } from "@bufbuild/protobuf";
import { Link, useNavigate } from "@tanstack/react-router";
import { AddressSchema, AddressSpecSchema, type Address } from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { Link2, Link2Off, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { Dialog } from "@/components/dialog";
import { DeleteDialog, FormFooter, StaleNotice } from "@/components/resource";
import { FormEquivalent } from "@/components/form";
import { InterfaceField, pickTarget } from "@/components/interface-field";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { ConditionsCard, EquivalentCard, LabelsField } from "@/components/object";
import { editLabelsAction, HeadActions, ObjectRowMenu, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { DetailBody, DetailFrame, DetailHead, ListFrame, NameFilter, PageHead } from "@/components/page";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Topbar, useProjectName } from "@/components/shell";
import { buttonVariants, Card, CardHeader, CopyButton, Field, Led } from "@/components/ui";
import { date, machinePath } from "@/lib/machines";
import { isStale, permission, useApplyAt, useChange, useReload, useNetworkCan } from "@/lib/network-actions";
import { addressYaml, conditionOf, nameOf, targetOf, useAddresses } from "@/lib/network";
import { bare } from "@/pages/machine-list";

/**
 * An address's actions, on its page and in its row's menu: Bind or Move,
 * Unbind when bound, Edit labels and Delete, each shown once
 * TestPermissions holds its permission; `after` follows a delete the API
 * took.
 */
export function useAddressActions(a: Address, can: Can, after?: () => void): ActionSet {
  const name = nameOf(a);
  const [open, setOpen] = useState<"bind" | "unbind" | "delete" | "labels" | null>(null);
  const mayApply = can(permission("address", "apply"));
  const bound = targetOf(a.spec?.target);
  const actions: ObjectAction[] = [];
  if (mayApply) actions.push({ label: bound ? "Move" : "Bind", icon: Link2, onSelect: () => setOpen("bind") });
  if (mayApply && bound) actions.push({ label: "Unbind", icon: Link2Off, onSelect: () => setOpen("unbind") });
  if (can(permission("address", "delete"))) actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: () => setOpen("delete") });
  const close = () => setOpen(null);
  return {
    actions,
    editLabels: editLabelsAction(mayApply, !!a.metadata?.deleteTime, () => setOpen("labels")),
    deleting: !!a.metadata?.deleteTime,
    dialogs: (
      <>
        {open === "bind" && <BindDialog open onClose={close} address={a} />}
        {open === "unbind" && <UnbindDialog open onClose={close} address={a} />}
        {open === "labels" && <EditLabelsDialog kind="address" object={a} onClose={close} />}
        {open === "delete" && (
          <DeleteDialog
            kind="address"
            name={name}
            open
            onClose={close}
            after={after}
            consequence={
              bound
                ? "The address is released; its interface gets a new automatic one."
                : "The address is released."
            }
          />
        )}
      </>
    ),
  };
}

/** An address's row menu: the actions of its page. */
function AddressRowMenu({ a, can }: { a: Address; can: Can }) {
  return <ObjectRowMenu name={nameOf(a)} set={useAddressActions(a, can)} />;
}

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
  const { can: may } = useNetworkCan("address", project);
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
        description="Reserved public IPv6 addresses"
        action={newButton}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ ...addrs, count: addrs.items.length }}
          noun="addresses"
          empty={{ title: "No addresses", action: newButton }}
        >
          <NameFilter value={query} onChange={setQuery} />
          <Card className="md:hidden">
            <ul className="divide-y">
              {rows.map((a) => (
                <li key={nameOf(a)} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                  <Link to="/projects/$project/addresses/$name" params={{ project, name: nameOf(a) }} className="flex min-w-0 flex-1 items-center gap-3">
                    <span className="min-w-0 flex-1">
                      <span className="block truncate font-medium">{nameOf(a)}</span>
                      <span className="block truncate font-mono text-[11.5px] text-subtle">{bare(a.status?.ipv6 ?? "") || "not drawn yet"}</span>
                    </span>
                    <span className="shrink-0 text-right">
                      <StateCell a={a} />
                    </span>
                  </Link>
                  <AddressRowMenu a={a} can={may} />
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
                  <Th className="w-10 px-0" />
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
                    <td className="px-2 py-2.5">
                      <AddressRowMenu a={a} can={may} />
                    </td>
                  </tr>
                ))}
                {rows.length === 0 && <NoMatch noun="address" colSpan={6} />}
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
  // Asked with the list, so the page's controls come with it, and once.
  const { can } = useNetworkCan("address", project);
  const a = addrs.items.find((x) => nameOf(x) === name);
  return (
    <DetailFrame crumbs={["Addresses", name]} state={addrs} found={!!a} noun="address" name={name}>
      {a && <Loaded a={a} can={can} />}
    </DetailFrame>
  );
}

/** The page of an address that has loaded, its actions made once for its head and its labels. */
function Loaded({ a, can }: { a: Address; can: Can }) {
  const name = nameOf(a);
  const project = a.metadata?.project ?? "";
  const navigate = useNavigate();
  const set = useAddressActions(a, can, () => void navigate({ to: "/projects/$project/addresses", params: { project } }));
  const s = stateOf(a);
  return (
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
        actions={set.actions.length > 0 && <HeadActions set={set} />}
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
                <LabelsField labels={a.metadata?.labels ?? {}} onEdit={set.editLabels?.onSelect} />
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
      {set.dialogs}
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
  const [pick, setPick] = useState(current);
  const applyAt = useApplyAt("address");
  const reload = useReload("address", project, name);
  const stale = isStale(applyAt.error);
  const target = pickTarget(pick);

  // Only the target changes; the rest is the address as the dialog opened on it.
  const obj = clone(AddressSchema, base);
  obj.spec ??= create(AddressSpecSchema);
  obj.spec.target = target;
  const problems: string[] = [];
  if (!target) problems.push("No machine is chosen.");
  const close = () => {
    applyAt.reset();
    onClose();
  };
  const startAgain = () =>
    reload.mutate(undefined, {
      onSuccess: (fresh) => {
        setBase(fresh);
        const ft = targetOf(fresh.spec?.target);
        setPick(ft ? `${ft.kind}/${ft.name}/${fresh.spec?.target?.interface || "eth0"}` : "");
        applyAt.reset();
      },
    });
  const verb = t ? "Move" : "Bind";
  return (
    <Dialog open={open} onClose={close} title={`${verb} ${name}`}>
      <InterfaceField value={pick} onChange={setPick} current={current} hint="No machine restarts." />
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
        {t ? `${t.name}'s ${address.spec?.target?.interface || "eth0"}` : "Its interface"} gets a new automatic address. This one stays reserved.
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
