// One file share: the networks it is on and its address on each, the
// machines that use it and from which network, the machines that could and
// those that cannot and why, and how a machine mounts it.

import { Link, useNavigate } from "@tanstack/react-router";
import { Box, HardDrive, Network as NetworkIcon, Plus, Server, Trash2 } from "lucide-react";
import { Fragment, useState } from "react";
import {
  CopyBlock,
  DeleteShareDialog,
  EditNetworksDialog,
  GrowShareDialog,
  kindWord,
  MachineChip,
  machinesWithNetworks,
  Mono,
  NetworkLink,
  notServed,
  OrNames,
  rowsShown,
  ShareState,
  ShowAll,
  UseBadge,
  UseReasons,
  useDeclarations,
  useSharePerms,
  type MachineRef,
} from "@/components/file-shares";
import { CappedNames } from "@/components/machine-names";
import { PageBanner } from "@/components/notice";
import { EquivalentCard } from "@/components/object";
import { DetailBody, DetailFrame, DetailHead } from "@/components/page";
import { useProjectName } from "@/components/shell";
import { Th } from "@/components/table";
import { ErrorNotice, SkeletonRows } from "@/components/states";
import { Badge, Button, Card, CardHeader, CopyButton, Field } from "@/components/ui";
import {
  addressOn,
  shareGiB,
  shareNetworks,
  shareUses,
  shareYaml,
  squashOf,
  squashWords,
  useFileShares,
  vmMountCommands,
  vmSnippet,
  volumeSnippet,
} from "@/lib/file-shares";
import { date, machinePath } from "@/lib/machines";
import { count, nameOf, useAttachments, useNetworks } from "@/lib/network";
import { formatNumber, quantityGiB } from "@/lib/quantity";

/** What stands for a VM's machine ID in the share's mount commands, which no one VM's are. */
const machineIdPlaceholder = "MACHINE_ID";

export function FileShareDetail({ name }: { name: string }) {
  const project = useProjectName();
  const navigate = useNavigate();
  const shares = useFileShares(project);
  const nets = useNetworks(project);
  const att = useAttachments(project);
  const decls = useDeclarations(project);
  const { canApply, canDelete } = useSharePerms(project);
  const [growing, setGrowing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [editing, setEditing] = useState<string[] | null>(null);
  const [allUsers, setAllUsers] = useState(false);
  const [allOthers, setAllOthers] = useState(false);
  const s = shares.items.find((x) => nameOf(x) === name);
  const crumbs = [
    <Link key="list" to="/projects/$project/file-shares" params={{ project }} className="hover:text-foreground">
      File shares
    </Link>,
    name,
  ];
  const networks = s ? shareNetworks(s) : [];
  const uses = s ? shareUses(s, decls.items) : [];
  const cidrOf = (n: string) => nets.items.find((x) => nameOf(x) === n)?.spec?.ipv4Cidr ?? "";
  const machines = machinesWithNetworks(att.items);
  const onShare = machines.filter((m) => m.networks.some((n) => networks.includes(n.network)));
  const notUsing = onShare.filter((m) => !uses.some((u) => u.kind === m.kind && u.name === m.name));
  const outside = machines.filter((m) => !m.networks.some((n) => networks.includes(n.network)));
  // The networks the share is not on, each with the project's machines on it that cannot use the share.
  const outsideBy = new Map<string, MachineRef[]>();
  for (const m of outside) for (const n of m.networks) outsideBy.set(n.network, [...(outsideBy.get(n.network) ?? []), { kind: m.kind, name: m.name, ipv4: n.ipv4 }]);
  const usersShown = allUsers ? uses : uses.slice(0, rowsShown);
  const othersShown = allOthers ? notUsing : notUsing.slice(0, rowsShown);
  const growingTo = s && quantityGiB(s.spec?.size) > quantityGiB(s.status?.size) ? s.spec?.size : undefined;
  const deletingNow = !!s?.metadata?.deleteTime;
  const served = s ? networks.filter((n) => addressOn(s, n)) : [];
  return (
    <DetailFrame crumbs={crumbs} state={shares} found={!!s} noun="file share" name={name}>
      {s && (
        <>
          {deletingNow && (
            <PageBanner
              tone="warning"
              title={
                <>
                  <Mono>{name}</Mono> is being deleted
                </>
              }
            >
              It goes once its data is released.
            </PageBanner>
          )}
          <DetailHead
            name={name}
            kind="File share"
            facts={
              <>
                <ShareState s={s} />
                <span>
                  On{" "}
                  {networks.map((n, i) => (
                    <span key={n}>
                      {i > 0 && <span className="mr-1 text-subtle">,</span>}
                      <NetworkLink name={n} />
                    </span>
                  ))}
                </span>
                <span className="font-mono text-[12px] tabular">{formatNumber(shareGiB(s))} GiB</span>
              </>
            }
            actions={
              (canApply || canDelete) && (
                <>
                  {canApply && (
                    <Button onClick={() => setEditing(networks)} disabled={deletingNow}>
                      <NetworkIcon /> Edit networks
                    </Button>
                  )}
                  {canApply && (
                    <Button onClick={() => setGrowing(true)} disabled={deletingNow}>
                      <HardDrive /> Grow
                    </Button>
                  )}
                  {canDelete && (
                    <Button variant="danger" onClick={() => setDeleting(true)} disabled={deletingNow}>
                      <Trash2 /> Delete
                    </Button>
                  )}
                </>
              )
            }
          />
          <DetailBody
            main={
              <>
                <Card>
                  <CardHeader title="Summary" />
                  <dl className="divide-y px-4">
                    <Field label="Networks">
                      <span className="block">
                        {/* Columns as wide as their longest entry, so a network's whole name always fits. */}
                        <span className="grid w-fit grid-cols-[repeat(3,max-content)] items-center gap-x-4 gap-y-1">
                          {networks.map((n) => (
                            <Fragment key={n}>
                              <NetworkLink name={n} />
                              <span className="font-mono text-[12px] text-muted">{cidrOf(n)}</span>
                              {addressOn(s, n) ? (
                                <span className="inline-flex items-center gap-1 font-mono text-[12.5px]">
                                  {addressOn(s, n)}
                                  <CopyButton text={addressOn(s, n)!} />
                                </span>
                              ) : (
                                <span className="text-[12.5px] text-subtle">{notServed}</span>
                              )}
                            </Fragment>
                          ))}
                        </span>
                        <span className="mt-1 block break-normal text-[12px] text-muted">
                          Machines with an interface on <OrNames names={networks} /> can use it, each from the address on its own network.
                        </span>
                      </span>
                    </Field>
                    <Field label="Size">
                      <span className="font-mono">
                        {formatNumber(shareGiB(s))} GiB
                        {growingTo && <span className="ml-2 font-sans text-[12px] text-muted">growing to {formatNumber(quantityGiB(growingTo))} GiB</span>}
                      </span>
                    </Field>
                    <Field label="Access">
                      <span>
                        Squash {squashWords[squashOf(s)].label}
                        {s.spec?.anonUid !== undefined && (
                          <span className="ml-2 font-mono text-[12px] text-muted">
                            uid {s.spec.anonUid} · gid {s.spec.anonGid ?? "–"}
                          </span>
                        )}
                        <span className="mt-0.5 block text-[12px] text-muted">{squashWords[squashOf(s)].text}</span>
                      </span>
                    </Field>
                    <Field label="Phase">
                      <ShareState s={s} />
                    </Field>
                    <Field label="Node" mono>
                      {s.status?.node || <span className="font-sans text-subtle">Not placed yet</span>}
                    </Field>
                    <Field label="Created">{date(s.metadata?.createTime)?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
                    <Field label="UID" mono>
                      <span className="text-muted">{s.metadata?.uid}</span>
                    </Field>
                  </dl>
                </Card>

                <Card>
                  <CardHeader
                    title={
                      <>
                        Used by{" "}
                        {!decls.loading && !decls.error && <span className="ml-1 font-mono text-[11.5px] font-normal text-subtle">{uses.length}</span>}
                      </>
                    }
                  />
                  {decls.error != null && (
                    // status.users alone may list some, but not the declarations not listed there yet.
                    <ErrorNotice error={decls.error} title="The project's machines did not load" onRetry={decls.refetch} />
                  )}
                  {decls.loading && uses.length === 0 ? (
                    <SkeletonRows rows={2} />
                  ) : decls.error != null && uses.length === 0 ? null : uses.length === 0 ? (
                    <div className="px-4 py-3 text-[13px] text-muted">No machine declares this share.</div>
                  ) : (
                    <div className="overflow-x-auto">
                      <table className="w-full text-[13px]">
                        <thead>
                          <tr className="border-b text-left text-[11.5px] text-subtle">
                            <Th>Machine</Th>
                            <Th>Kind</Th>
                            <Th>From</Th>
                            <Th>Access</Th>
                            <Th>Path</Th>
                          </tr>
                        </thead>
                        <tbody>
                          {usersShown.map((u) => (
                            <tr key={`${u.kind}/${u.name}`} className="border-b align-top last:border-0">
                              <td className="px-4 py-2.5">
                                <Link to={machinePath(u.kind)} params={{ project, name: u.name }} className="font-mono text-[12.5px] font-medium text-accent hover:underline">
                                  {u.name}
                                </Link>
                                <UseBadge u={u} />
                              </td>
                              <td className="px-4 py-2.5 text-muted">{u.kind === "Container" ? "Container" : "Virtual machine"}</td>
                              <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px]">
                                {u.network || "–"}{" "}
                                <span className={u.address ? "text-muted" : "font-sans text-subtle"}>· {u.address ?? notServed.toLowerCase()}</span>
                              </td>
                              <td className="px-4 py-2.5">{u.readOnly ? <Badge>read-only</Badge> : <span className="text-muted">read-write</span>}</td>
                              <td className="px-4 py-2.5 font-mono text-[12.5px]">
                                {u.path || <span className="font-sans text-[12.5px] text-subtle">mounted by the VM itself</span>}
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  )}
                  {uses.length > rowsShown && <ShowAll all={allUsers} n={uses.length} onToggle={() => setAllUsers(!allUsers)} />}
                  <UseReasons uses={uses} />
                </Card>

                <Card>
                  <CardHeader
                    title={
                      <>
                        Other machines
                        {!att.loading && !att.error && (
                          <span className="ml-1 font-mono text-[11.5px] font-normal text-subtle">{notUsing.length + outside.length}</span>
                        )}
                      </>
                    }
                  />
                  {att.loading ? (
                    <SkeletonRows rows={2} />
                  ) : att.error ? (
                    <ErrorNotice error={att.error} title="The project's machines did not load" onRetry={att.refetch} />
                  ) : (
                    <>
                      <div className="px-4 pt-3 text-[12.5px] text-muted">
                        {notUsing.length === 0 ? (
                          <>
                            Every machine on <OrNames names={networks} /> uses <Mono>{name}</Mono>.
                          </>
                        ) : (
                          <>
                            On <OrNames names={networks} />, so able to use <Mono>{name}</Mono> once they declare it:
                          </>
                        )}
                      </div>
                      {notUsing.length > 0 && (
                        <ul className="mt-2 divide-y border-t">
                          {othersShown.map((m) => {
                            const via = m.networks.find((n) => networks.includes(n.network))!;
                            const address = addressOn(s, via.network);
                            return (
                              <li
                                key={`${m.kind}/${m.name}`}
                                className="grid grid-cols-[132px_64px_150px_minmax(0,1fr)_auto] items-center gap-3 px-4 py-2.5 text-[13px]"
                              >
                                <span className="min-w-0">
                                  <MachineChip m={m} />
                                </span>
                                <span className="text-[12px] text-subtle">{kindWord(m.kind)}</span>
                                <span className="font-mono text-[12px] text-muted">
                                  {via.network} · {via.ipv4}
                                </span>
                                <span className="truncate text-[12px] text-subtle">
                                  {address ? (
                                    <>
                                      would mount from <span className="font-mono">{address}</span>
                                    </>
                                  ) : (
                                    <>address {notServed.toLowerCase()}</>
                                  )}
                                </span>
                                <span className="flex items-center gap-1">
                                  <span className="w-[112px] text-right font-mono text-[11.5px] text-subtle">{m.kind === "Container" ? "spec.volumes" : "spec.fileShares"}</span>
                                  <CopyButton text={m.kind === "Container" ? volumeSnippet(name) : vmSnippet(name)} />
                                </span>
                              </li>
                            );
                          })}
                        </ul>
                      )}
                      {notUsing.length > rowsShown && <ShowAll all={allOthers} n={notUsing.length} onToggle={() => setAllOthers(!allOthers)} />}
                      <div className="mt-2 border-t px-4 py-3 text-[12.5px] text-muted">
                        {outside.length === 0 ? (
                          <>
                            Every machine in this project is on <OrNames names={networks} />.
                          </>
                        ) : (
                          <>
                            <div>
                              {count(outside.length, "machine")} in this project {outside.length === 1 ? "has" : "have"} no interface on <OrNames names={networks} />{" "}
                              and cannot use <Mono>{name}</Mono>:
                            </div>
                            <ul className="mt-2.5 space-y-2">
                              {[...outsideBy.entries()].map(([net, ms]) => (
                                <li key={net} className="grid min-h-7 grid-cols-[minmax(0,1fr)_auto] items-center gap-4">
                                  <span className="min-w-0">
                                    On <span className="font-mono text-foreground">{net}</span>, {count(ms.length, "machine")}:{" "}
                                    <CappedNames
                                      machines={ms}
                                      title={
                                        <>
                                          On <Mono>{net}</Mono>, not able to use <Mono>{name}</Mono>
                                        </>
                                      }
                                    />
                                  </span>
                                  {canApply && !deletingNow && (
                                    <Button size="sm" onClick={() => setEditing([...networks, net])}>
                                      <Plus /> Add network <Mono>{net}</Mono> to this share
                                    </Button>
                                  )}
                                </li>
                              ))}
                            </ul>
                          </>
                        )}
                      </div>
                    </>
                  )}
                </Card>

                <Card>
                  <CardHeader title="Mount" />
                  <div className="space-y-4 px-4 py-3.5">
                    <div className="space-y-2.5">
                      <div className="flex items-center gap-1.5 text-[12px] text-muted">
                        <Server className="size-3.5" strokeWidth={1.75} /> A virtual machine mounts the share itself, from the address on its own network,
                        such as with:
                      </div>
                      {served.length === 0 && <p className="text-[12.5px] text-subtle">{notServed}: the share has no address on any network.</p>}
                      {served.map((n) => (
                        <CopyBlock
                          key={n}
                          label={
                            <>
                              From <span className="font-mono text-foreground">{n}</span>:
                            </>
                          }
                          text={vmMountCommands(name, addressOn(s, n), false, machineIdPlaceholder)}
                          prompt
                        />
                      ))}
                      {served.length > 0 && (
                        <p className="text-[12px] text-subtle">
                          Each VM is an NFS client of its own: <Mono>{machineIdPlaceholder}</Mono> is its machine's ID, which the VM's page fills in.
                        </p>
                      )}
                    </div>
                    <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
                      <CopyBlock label="A virtual machine declares it in spec.fileShares:" text={vmSnippet(name)} />
                      <CopyBlock
                        label={
                          <span className="flex items-center gap-1.5">
                            <Box className="size-3.5" strokeWidth={1.75} /> A container declares a volume, mounted at its target:
                          </span>
                        }
                        text={volumeSnippet(name)}
                      />
                    </div>
                    <p className="text-[12px] text-subtle">
                      A container's mount is from the address on its own network, always nosuid,nodev, and the image's files at the target are not copied
                      onto the share. A container declaring a share that is on no node yet is held back until it is.
                    </p>
                  </div>
                </Card>
              </>
            }
            side={
              <EquivalentCard
                cli={[
                  `hangar get fileshare ${name} -p ${project}`,
                  ...(canApply ? [`hangar apply -f ${name}.yaml -p ${project}`] : []),
                  ...(canDelete ? [`hangar delete fileshare ${name} -p ${project}`] : []),
                ]}
                yaml={shareYaml(s)}
              />
            }
          />
          {editing && (
            <EditNetworksDialog
              key={editing.join(",")}
              s={s}
              decls={decls.items}
              declsState={decls}
              attachments={att.items}
              onClose={() => setEditing(null)}
              initial={editing}
            />
          )}
          <GrowShareDialog
            key={String(growing)}
            s={s}
            uses={uses}
            usesKnown={!decls.loading && !decls.error}
            open={growing}
            onClose={() => setGrowing(false)}
          />
          <DeleteShareDialog
            s={s}
            uses={uses}
            usesKnown={!decls.loading && !decls.error}
            open={deleting}
            onClose={() => setDeleting(false)}
            after={() => void navigate({ to: "/projects/$project/file-shares", params: { project } })}
          />
        </>
      )}
    </DetailFrame>
  );
}
