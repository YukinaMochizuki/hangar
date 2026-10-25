// A new container or virtual machine, and the same form opened on an
// existing one to edit it. No quota is shown here: the form warns only when
// this creation, or this change, would take the project past a limit, on the
// size that does it and above the button (docs/decisions/console-design.md).

import { create } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { ContainerSchema, ContainerService, type Container } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import {
  VirtualMachineSchema,
  VirtualMachineService,
  type VirtualMachine,
} from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { NetworkService } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { SecurityGroupService } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { ImageService } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { customImageFloorGiB, customImageState, useCustomImages } from "@/lib/custom-images";
import { imageFloorGiB } from "@/lib/storage";
import { Box, Check, Globe, HardDrive, Lock, RotateCw, Server, SlidersHorizontal, Undo2, X } from "lucide-react";
import { DiskLink } from "@/components/links";
import { networksAfter, VolumePicker, volumeProblems, type ShareAdds, type VolumeRow } from "@/components/volume-picker";
import { addNetworks, removeNetworks, sharePerms, shareName, shareUses, shareYaml, useFileShares, withNetworks } from "@/lib/file-shares";
import { useDeclarations } from "@/components/file-shares";
import { useCan } from "@/lib/permissions";
import { useState, type CSSProperties, type ReactNode } from "react";
import { Input, Select } from "@/components/form";
import { advancedSpec, ContainerAdvanced, noAdvanced, type Advanced } from "@/components/container-advanced";
import { Choice, FormManifest, FormPage, FormSection as Section, FormSummary } from "@/components/form-page";
import { gibText, SizeSliders } from "@/components/size-sliders";
import { MachinePortForwards } from "@/components/machine-port-forwards";
import { noVmAdvanced, sshKeysOf, VmAdvancedFields, vmAdvancedProblems, type VmAdvanced } from "@/components/vm-advanced";
import { dataDiskProblems, VmDisks } from "@/components/vm-disks";
import { PortForwardSchema, PortForwardService } from "@hangar/api/hangar/network/v1alpha1/port_forward_pb";
import { nameOf, portForwardYaml, usePortForwards } from "@/lib/network";
import { forwardPlan, forwardRowsOf, protocolEnum, usePortForwardQuota, type ForwardRow } from "@/lib/port-forwards";
import { Notice } from "@/components/notice";
import { StaleNotice } from "@/components/resource";
import { useProjectName } from "@/components/shell";
import { ErrorNotice, Skeleton } from "@/components/states";
import { Button } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { checkName, createError } from "@/lib/create";
import { createdKey, markJustCreated } from "@/lib/just-created";
import {
  advancedOf,
  changesOf,
  containerDefaults,
  editedObject,
  interfacesOf,
  networkFieldsOf,
  quotaDelta,
  vmDefaults,
  vmShareRowsOf,
  vmSharesOf,
  volumeRowsOf,
  volumesOf,
  type Change,
  type Effect,
  type ExtraInterface,
  type FormSpec,
  type MachineObject,
} from "@/lib/machine-edit";
import { manifestOf, toYaml } from "@/lib/manifest";
import { useMachines } from "@/lib/machines";
import {
  clampCPU,
  maxMiBPerCPU,
  minMiBPerCPU,
  memoryQuantity,
  memoryStops,
  minDiskGi,
  shapeProblem,
  sizeOf,
  sliderLimits,
  useMachineSizes,
  withinRules,
  type Size,
} from "@/lib/machine-sizes";
import { isStale } from "@/lib/network-actions";
import { useOverQuota } from "@/lib/over-quota";
import { formatNumber, quantityCPU, quantityGiB, wholeGiB } from "@/lib/quantity";

export type CreateKind = "container" | "vm";

/**
 * The shares a create changed before it failed: each stated, with Undo,
 * which takes the added networks off again unless a machine mounts the
 * share from one of them by now.
 */
function SharesChanged({ project, applied, onUndone }: { project: string; applied: { share: string; adds: string[] }[]; onUndone: (share: string) => void }) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const decls = useDeclarations(project);
  const undo = useMutation({
    mutationFn: async ({ share, adds }: { share: string; adds: string[] }) => {
      await removeNetworks(transport, project, share, adds, (s) => shareUses(s, decls.items).map((u) => u.network));
      return share;
    },
    onSuccess: (share) => onUndone(share),
    onSettled: () => queryClient.invalidateQueries(),
  });
  return (
    <Notice tone="warning" title={applied.length === 1 ? <>File share <span className="font-mono">{applied[0]!.share}</span> was changed</> : `${applied.length} file shares were changed`}>
      {applied.map((a) => (
        <div key={a.share} className="mt-1 flex items-center justify-between gap-2">
          <span>
            <span className="font-mono">{a.adds.join(", ")}</span> was added to <span className="font-mono">{a.share}</span>.
          </span>
          <Button
            type="button"
            size="sm"
            disabled={undo.isPending || decls.loading || !!decls.error}
            onClick={() => undo.mutate(a)}
          >
            <Undo2 /> {undo.isPending && undo.variables?.share === a.share ? "Undoing…" : "Undo"}
          </Button>
        </div>
      ))}
      {decls.error != null && <div className="mt-1">The project's machines did not load, so which machines mount from these networks is not known.</div>}
      {undo.error != null && <div className="mt-1 text-fail">{errorMessage(undo.error)}</div>}
    </Notice>
  );
}

/** What the form needs to edit an object rather than create one. */
export type MachineEditing = {
  /** The object as the page opened on it; an edit is applied at its resource version. */
  base: MachineObject;
  /** The disk the container's machine holds in whole GiB, once placed: an edit asks for no less. */
  placedDiskGi?: number;
  /** Writes the edited object as it is, at the version it carries. */
  write: (o: MachineObject) => Promise<unknown>;
  /** Reads the object again, for the page to open the form on it anew. */
  reload: { pending: boolean; error: unknown; run: () => void };
};

const effectTitle = (e: Effect, noun: string) => (e === "rebuild" ? "Rebuilds the root disk" : e === "restart" ? `Restarts the ${noun}` : "Restarts nothing");
const effectIcon = { rebuild: HardDrive, restart: RotateCw, nothing: Check } as const;

/** The right column's list of what applying does, grouped by effect, or that nothing differs. */
function WhatApplyingDoes({ changes, noun }: { changes: Change[]; noun: string }) {
  if (changes.length === 0) {
    return <div className="px-4 py-3 text-[12.5px] text-muted">No change.</div>;
  }
  return (
    <div className="space-y-3 px-4 py-3">
      {(["rebuild", "restart", "nothing"] as const).map((e) => {
        const these = changes.filter((c) => c.effect === e);
        if (these.length === 0) return null;
        const Icon = effectIcon[e];
        return (
          <div key={e}>
            <div className="flex items-center gap-2 text-[12.5px] font-medium">
              <Icon className={cn("size-3.5 shrink-0", e === "nothing" ? "text-subtle" : "text-pending")} strokeWidth={2} />
              {effectTitle(e, noun)}
            </div>
            <ul className="mt-1 space-y-0.5 pl-5.5 text-[12px]">
              {these.map((c) => (
                <li key={c.field} className="flex gap-2">
                  <span className="shrink-0 text-muted">{c.label}</span>
                  <span className="min-w-0 break-words font-mono text-[11.5px]">{c.detail}</span>
                </li>
              ))}
            </ul>
          </div>
        );
      })}
    </div>
  );
}

/** A locked field of an edit: what it holds, and why it does not change. */
function Locked({ children, why }: { children: ReactNode; why: string }) {
  return (
    <div className="flex max-w-xl items-start gap-3 rounded-lg border border-dashed bg-surface px-4 py-3">
      <Lock className="mt-0.5 size-3.5 shrink-0 text-subtle" />
      <div className="min-w-0">
        <div className="text-[13px]">{children}</div>
        <div className="mt-0.5 text-[12px] text-muted">{why}</div>
      </div>
    </div>
  );
}

export function CreateMachine({ kind, size }: { kind: CreateKind; size?: string }) {
  return <MachineForm kind={kind} size={size} />;
}

/**
 * The new-machine form, or with `edit` the same form opened on an existing
 * object: every field prefilled from its spec, its kind, name and a VM's
 * image locked, and only what the person changes written back over it.
 */
export function MachineForm({ kind: initialKind, size: initialSize, edit }: { kind: CreateKind; size?: string; edit?: MachineEditing }) {
  const project = useProjectName();
  const navigate = useNavigate();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const baseC = edit?.base.$typeName === ContainerSchema.typeName ? (edit.base as Container) : undefined;
  const baseV = edit && !baseC ? (edit.base as VirtualMachine) : undefined;
  const baseRoot = baseV?.spec?.rootDisk?.source;
  // What the fields held when the form opened: an edit writes back only those changed since.
  const [initial] = useState(() => ({
    image: baseC?.spec?.image ?? "",
    net: networkFieldsOf((baseC ?? baseV)?.spec?.interfaces ?? []),
    volumes: baseC ? volumeRowsOf(baseC) : [],
    advanced: baseC ? advancedOf(baseC) : noAdvanced,
    vmShares: baseV ? vmShareRowsOf(baseV) : [],
    dataDisks: baseV ? (baseV.spec?.disks ?? []).map((d) => d.disk) : [],
    vmAdvanced: baseV
      ? {
          sshKeys: (baseV.spec?.sshAuthorizedKeys ?? []).join("\n"),
          userData: baseV.spec?.userData ?? "",
          serial: baseV.spec?.platform?.serialNumber ?? "",
          tpm: baseV.spec?.tpm?.enabled ?? false,
        }
      : noVmAdvanced,
    // The stored disk as the field shows it, which may not be whole GiB, such as 10G.
    diskText: baseC ? formatNumber(quantityGiB(baseC.spec?.resources?.disk || containerDefaults.disk)) : undefined,
    diskWholeGi: baseC ? wholeGiB(baseC.spec?.resources?.disk || containerDefaults.disk) : undefined,
  }));
  const [kind, setKind] = useState<CreateKind>(initialKind);
  const [image, setImage] = useState(initial.image);
  const [vmImage, setVmImage] = useState(baseRoot?.case === "fromImage" ? (baseRoot.value.image?.name ?? "") : "");
  // Whether vmImage names one of the project's custom images rather than a platform image.
  const [vmImageCustom, setVmImageCustom] = useState(baseRoot?.case === "fromImage" && !!baseRoot.value.image?.project);
  const [size, setSize] = useState(initialSize);
  // A shape of the person's own, as the sliders set it, once Custom is
  // chosen; null while a size is.
  const [custom, setCustom] = useState<{ cpu: number; memMiB: number } | null>(null);
  const [network, setNetwork] = useState(initial.net.network);
  const [groups, setGroups] = useState<string[]>(initial.net.groups);
  // Interfaces after the first: an edited machine's own, and each added for a file share on its network, in the network's default group.
  const [extraNets, setExtraNets] = useState<ExtraInterface[]>(initial.net.extra);
  const [justAdded, setJustAdded] = useState<{ network: string; share: string } | null>(null);
  // A container's volumes, the row whose source list is open, and the
  // networks to add to shares when it is created.
  const [volumes, setVolumes] = useState<VolumeRow[]>(initial.volumes);
  const [pickerOpen, setPickerOpen] = useState<number | null>(null);
  const [shareAdds, setShareAdds] = useState<ShareAdds>({});
  // The shares this page has changed and not undone: a create that failed
  // after them leaves them changed, so they are stated, skipped on a retry,
  // and can be undone.
  const [applied, setApplied] = useState<{ share: string; adds: string[] }[]>([]);
  const [name, setName] = useState(edit?.base.metadata?.name ?? "");
  // The disk as typed; null until the person edits it, while it follows
  // the minimum. An edit starts on the disk the container has, which no
  // size moves: a disk that grew never shrinks again.
  const [diskText, setDiskText] = useState<string | null>(initial.diskText ?? null);
  // A container's process, folded until opened.
  const [advanced, setAdvanced] = useState<Advanced>(initial.advanced);
  const [advancedOpen, setAdvancedOpen] = useState(false);
  // A VM's file shares, data disks, and its cloud-init and hardware fields, folded until opened.
  const [vmShares, setVmShares] = useState<VolumeRow[]>(initial.vmShares);
  const [dataDisks, setDataDisks] = useState<string[]>(initial.dataDisks);
  const [vmAdvanced, setVmAdvanced] = useState<VmAdvanced>(initial.vmAdvanced);
  const [vmAdvancedOpen, setVmAdvancedOpen] = useState(false);
  // The port forward rows as edited; null until then, while they are the forwards the machine has.
  const [forwardEdits, setForwardEdits] = useState<ForwardRow[] | null>(null);
  const proc = advancedSpec(advanced);

  const images = useQuery(ImageService.method.listImages, { pageSize: 1000 }, { enabled: kind === "vm" });
  const customImages = useCustomImages(project);
  const readyCustom = customImages.items.filter((c) => customImageState(c).tone === "ok");
  const networks = useQuery(NetworkService.method.listNetworks, { project, pageSize: 1000 });
  const securityGroups = useQuery(SecurityGroupService.method.listSecurityGroups, { project, pageSize: 1000 });
  const existing = useMachines(project, kind === "container" ? "Container" : "VirtualMachine");
  const overQuota = useOverQuota(project);
  const machineSizes = useMachineSizes();
  // The sizes offered: those a machine may have under the shape rules.
  const offered = machineSizes.sizes.filter(withinRules);
  const shares = useFileShares(project);
  const forwards = usePortForwards(project);
  const fwdQuota = usePortForwardQuota(project);
  const sharePerm = useCan({ level: "project", name: project }, [sharePerms.apply]);
  const mayApplyShares = sharePerm.known ? sharePerm.can(sharePerms.apply) : undefined;

  const chosenImage = vmImage || images.data?.items[0]?.metadata?.name || "";
  const chosenCustom = !!vmImage && vmImageCustom;
  const chooseImage = (name: string, custom: boolean) => {
    setVmImage(name);
    setVmImageCustom(custom);
  };

  // An edited machine's own size: the listed size it matches, or Current,
  // with its quantities as written, so it is kept unless another is chosen.
  const res = baseC?.spec?.resources ?? baseV?.spec?.resources;
  const defaults = baseC ? containerDefaults : vmDefaults;
  const currentMatch = edit ? sizeOf({ cpu: quantityCPU(res?.cpu || defaults.cpu), memoryGi: quantityGiB(res?.memory || defaults.memory) }, offered) : undefined;
  const current: Size | undefined = edit && {
    name: currentMatch?.name ?? "",
    label: currentMatch?.label ?? "Current",
    order: -1,
    cpu: quantityCPU(res?.cpu || defaults.cpu),
    memoryGi: quantityGiB(res?.memory || defaults.memory),
    diskGi: 0,
    resources: { cpu: res?.cpu ?? "", memory: res?.memory ?? "", disk: "" },
  };
  const chosen = offered.find((z) => z.name === size);
  // The size asked for, if the platform lists it, or else its first; an
  // edit's own until another is chosen.
  const picked: Size | undefined = edit ? (chosen && chosen.name !== currentMatch?.name ? chosen : current) : (chosen ?? offered[0]);
  const isCurrent = (z: Size) => !!edit && z.name === currentMatch?.name;
  // With no size to pick, a new machine's shape is the person's own.
  const noSizes = !machineSizes.loading && !machineSizes.error && offered.length === 0 && !current;
  const customFields = custom ?? (noSizes ? { cpu: 1, memMiB: 1024 } : null);
  const customCPU = customFields?.cpu ?? NaN;
  const customMiB = customFields?.memMiB ?? NaN;
  const customProblem = customFields ? shapeProblem(customCPU, customMiB) : undefined;
  const customSize: Size | undefined = customFields
    ? {
        name: "",
        label: "Custom",
        order: -2,
        cpu: customCPU,
        memoryGi: customMiB / 1024,
        // The disk follows the size chosen before, as the sliders start from it.
        diskGi: picked?.diskGi ?? 0,
        resources: { cpu: String(customCPU), memory: memoryQuantity(customMiB), disk: "" },
      }
    : undefined;
  const s = customSize ?? picked;
  // The sliders reach twice the largest size, and always the machine's own shape.
  const ownShapes = [current, customFields && { cpu: customFields.cpu, memoryGi: customFields.memMiB / 1024 }].flatMap((x) =>
    x ? [{ memMiB: Math.round(x.memoryGi * 1024), cpu: x.cpu }] : [],
  );
  const limits = sliderLimits(offered, ownShapes);
  const stops = memoryStops(limits.memMiB, ownShapes.map((o) => o.memMiB));
  // The size cards: an edited machine's own shape first when no size is it.
  const cards = [...(current && !currentMatch ? [current] : []), ...offered];
  // Custom starts from the shape chosen before it, on a stop and with vCPUs its memory allows.
  const pickCustom = () => {
    if (custom) return;
    const from = Math.round((picked?.memoryGi ?? 1) * 1024);
    const memMiB = stops.includes(from) ? from : (stops.find((m) => m >= from) ?? stops[stops.length - 1]);
    setCustom({ memMiB, cpu: clampCPU(picked?.cpu ?? 1, memMiB) });
  };
  const pickSize = (name: string | undefined) => {
    setCustom(null);
    setSize(name);
  };

  // The smallest disk: on create, 2 GiB, or for a VM its image's if larger;
  // on an edit, the disk the placed machine holds, or before it is placed
  // the disk the spec asks for, in whole GiB rounded up. A size's disk is
  // where the field starts, never a minimum.
  const imageFloor =
    kind !== "vm"
      ? 1
      : chosenCustom
        ? customImageFloorGiB(customImages.items.find((c) => c.metadata?.name === chosenImage))
        : imageFloorGiB(images.data?.items.find((i) => i.metadata?.name === chosenImage));
  const floor = Math.max(minDiskGi, imageFloor);
  const kindDefaults = kind === "container" ? containerDefaults : vmDefaults;
  const startOf = (z: Size) => Math.max(z.diskGi > 0 ? Math.ceil(z.diskGi) : wholeGiB(kindDefaults.disk), floor);
  const editFloor = initial.diskWholeGi !== undefined ? (edit?.placedDiskGi ?? initial.diskWholeGi) : undefined;
  const minDisk = editFloor ?? floor;
  const diskStart = editFloor ?? (s ? startOf(s) : floor);
  const byImage = imageFloor > minDiskGi;
  // What the field holds, typed or followed; the manifest shows it as it
  // is, refused or not.
  const diskTyped = (diskText ?? String(diskStart)).trim();
  const diskGi = diskTyped === "" ? NaN : Number(diskTyped);
  // An edit's disk is checked only once the person changes it: the stored
  // one, whole GiB or not, the API took, and it is sent back as it is.
  const diskEdited = !edit || diskText !== initial.diskText;
  const diskProblem = !diskEdited
    ? undefined
    : !Number.isInteger(diskGi) || diskGi < 1
    ? "The disk is not a whole number of GiB."
    : diskGi < minDisk
      ? editFloor !== undefined
        ? `The disk is ${minDisk} GiB and never shrinks; ${diskGi} GiB is less.`
        : byImage
          ? `The image ${chosenImage} needs a disk of at least ${minDisk} GiB; ${diskGi} GiB is less.`
          : `A disk is at least ${minDisk} GiB; ${diskGi} GiB is less.`
      : undefined;
  const diskQuantity = diskTyped === "" ? "" : `${diskTyped}Gi`;
  const groupsHere = (securityGroups.data?.items ?? []).filter((g) => g.spec?.network === network).map((g) => g.metadata?.name ?? "");
  const noun = kind === "container" ? "container" : "virtual machine";
  const shortNoun = kind === "container" ? "container" : "VM";

  // The object as it is sent, and as its manifest shows it.
  const extra = extraNets.filter((x) => x.network !== network);
  const netFields = { network, groups, extra };
  const interfaces = interfacesOf(netFields);
  const onNetworks = [network, ...extra.map((x) => x.network)];
  const addInterface = (n: string, share: string) => {
    if (!onNetworks.includes(n)) setExtraNets([...extraNets, { network: n, groups: ["default"] }]);
    setJustAdded({ network: n, share });
  };
  // The file shares the machine declares: a container's FileShare volumes, a VM's shares.
  const shareRows = (kind === "container" ? volumes : vmShares).filter((v) => v.kind === "FileShare");
  // The shares this machine adds its network to, among those it declares.
  const sharesChanged = Object.entries(shareAdds).flatMap(([n, adds]) => {
    const sh = shares.items.find((x) => shareName(x) === n);
    return sh && shareRows.some((v) => v.source === n) ? [{ share: sh, adds }] : [];
  });
  const sshKeys = sshKeysOf(vmAdvanced);

  // The port forwards: the machine's own until the rows are edited, and what applying does to them.
  const forwardsHad = edit ? forwardRowsOf(forwards.items, kind === "container" ? "Container" : "VirtualMachine", name) : [];
  const forwardRows = forwardEdits ?? forwardsHad;
  const fwd = forwardPlan({ kind: kind === "container" ? "Container" : "VirtualMachine", name }, forwardRows, forwardsHad, forwards.items.map(nameOf));
  const forwardObjs = fwd.make.map((f) =>
    create(PortForwardSchema, {
      metadata: { name: f.name, project },
      spec: {
        target: { object: { case: kind === "container" ? "container" : "virtualMachine", value: name }, interface: f.iface },
        protocol: protocolEnum(f.protocol),
        targetPort: f.port,
      },
    }),
  );

  let obj: MachineObject;
  if (edit) {
    // Each part of the spec the form shows is written only once it differs
    // from what the form opened with, so the rest stays as the API wrote it.
    const json = (v: unknown) => JSON.stringify(v);
    const envOf = (a: Advanced) => a.env.filter((e) => e.name !== "" || e.value !== "").map((e) => [e.name, e.value]);
    const changed: FormSpec = {};
    if (baseC) {
      if (image !== initial.image) changed.image = image;
      if (advanced.entrypoint !== initial.advanced.entrypoint) changed.command = proc.command;
      if (advanced.command !== initial.advanced.command) changed.args = proc.args;
      if (json(envOf(advanced)) !== json(envOf(initial.advanced))) changed.env = proc.env.map((e) => ({ name: e.name, value: e.value }));
      if (advanced.restart !== initial.advanced.restart) changed.restartPolicy = proc.restartPolicy;
      if (json(volumes) !== json(initial.volumes)) changed.volumes = volumes;
      if (diskEdited) changed.disk = diskQuantity;
    } else {
      if (json(dataDisks) !== json(initial.dataDisks)) changed.disks = dataDisks;
      if (json(vmShares) !== json(initial.vmShares)) changed.fileShares = vmShares;
      if (json(sshKeys) !== json(sshKeysOf(initial.vmAdvanced))) changed.sshAuthorizedKeys = sshKeys;
      if (vmAdvanced.userData !== initial.vmAdvanced.userData) changed.userData = vmAdvanced.userData;
      // The TPM is never written back: it is chosen when the VM is created.
      if (vmAdvanced.serial !== initial.vmAdvanced.serial) changed.serialNumber = vmAdvanced.serial;
    }
    if (json(netFields) !== json(initial.net)) changed.interfaces = interfaces;
    if (s && s !== current) {
      changed.cpu = s.resources.cpu;
      changed.memory = s.resources.memory;
    }
    obj = editedObject(edit.base, changed);
  } else {
    obj =
      kind === "container"
        ? create(ContainerSchema, {
            metadata: { name, project },
            spec: {
              image,
              command: proc.command,
              args: proc.args,
              env: proc.env,
              restartPolicy: proc.restartPolicy,
              interfaces,
              volumes: volumesOf(volumes),
              resources: s && { ...s.resources, disk: diskQuantity },
            },
          })
        : create(VirtualMachineSchema, {
            metadata: { name, project },
            spec: {
              interfaces,
              resources: s && { cpu: s.resources.cpu, memory: s.resources.memory },
              rootDisk: {
                source: { case: "fromImage", value: { image: { name: chosenImage, project: chosenCustom ? project : "" }, size: s ? diskQuantity : "" } },
              },
              disks: dataDisks.filter(Boolean).map((disk) => ({ disk })),
              fileShares: vmSharesOf(vmShares.filter((v) => v.source)),
              sshAuthorizedKeys: sshKeys,
              userData: vmAdvanced.userData,
              platform: vmAdvanced.serial ? { serialNumber: vmAdvanced.serial } : undefined,
              tpm: vmAdvanced.tpm ? { enabled: true } : undefined,
            },
          });
  }
  const machineChanges = edit ? changesOf(edit.base, obj) : [];
  // The forwards are objects of their own, made and deleted beside the machine, which they do not restart.
  const forwardDetail = [...fwd.make.map((f) => `${f.name} added`), ...fwd.remove.map((n) => `${n} removed`)].join(", ");
  const changes: Change[] = forwardDetail ? [...machineChanges, { field: "portForwards", label: "Port forwards", effect: "nothing", detail: forwardDetail }] : machineChanges;

  // A size's card is checked with the disk the field starts at for it,
  // never the one typed: a raised disk's overage is the Disk field's to
  // state. An edit is checked on what it adds to the machine, never its
  // whole size.
  const delta = edit ? quotaDelta(edit.base, obj) : undefined;
  const over = delta
    ? overQuota({ machines: 0, cpu: delta.cpu, memoryGi: delta.memoryGi, diskGi: diskProblem ? 0 : delta.diskGi })
    : s
      ? overQuota({ machines: 1, cpu: s.cpu, memoryGi: s.memoryGi, diskGi: diskProblem ? minDisk : diskGi })
      : [];
  const cardOver = (z: Size) =>
    current ? overQuota({ machines: 0, cpu: z.cpu - current.cpu, memoryGi: z.memoryGi - current.memoryGi, diskGi: 0 }) : overQuota({ machines: 1, cpu: z.cpu, memoryGi: z.memoryGi, diskGi: startOf(z) });
  const diskOver = over.find((o) => o.key === "disk");

  const machineYaml = toYaml(manifestOf(obj, kind === "container" ? "Container" : "VirtualMachine"));
  // A share this container adds its network to is applied before it, as a document of its own.
  const shareDocs = sharesChanged.map(({ share, adds }) => shareYaml(withNetworks(share, networksAfter(share, { [shareName(share)]: adds }))));
  const yaml = [...shareDocs, machineYaml, ...forwardObjs.map(portForwardYaml)].join("\n---\n");
  const cli = [
    ...sharesChanged.map(({ share }) => `hangar apply -p ${project} -f ${shareName(share)}.yaml`),
    edit ? `hangar apply -p ${project} -f ${name}.yaml` : `hangar apply --create-only -p ${project} -f ${name || "machine"}.yaml`,
    ...fwd.make.map((f) => `hangar apply --create-only -p ${project} -f ${f.name}.yaml`),
    ...fwd.remove.map((n) => `hangar delete pf ${n} -p ${project}`),
  ];

  // Nothing is sent until the project's machines of the kind are known and
  // the name is not among them (lib/create.ts).
  const nameCheck = checkName(name, noun, {
    loading: existing.loading,
    error: existing.error,
    names: existing.items.map((m) => m.name),
  });
  const problems: string[] = [];
  if (!edit && !nameCheck.ok) problems.push(nameCheck.reason);
  if (kind === "container" && !image.trim()) problems.push("The image is empty.");
  if (kind === "container") problems.push(...proc.problems);
  if (kind === "container") problems.push(...volumeProblems(volumes));
  else {
    if (vmShares.some((v) => !v.source)) problems.push("A file share row has no share.");
    if (new Set(vmShares.map((v) => v.source)).size < vmShares.length) problems.push("A file share is declared twice.");
    problems.push(...dataDiskProblems(dataDisks), ...vmAdvancedProblems(vmAdvanced));
  }
  for (const v of shareRows) {
    if (!v.source) continue;
    const sh = shares.items.find((x) => shareName(x) === v.source);
    if (!sh) problems.push(shares.loading ? "The file shares are loading." : `File share ${v.source} does not exist.`);
    else if (!networksAfter(sh, shareAdds).some((n) => onNetworks.includes(n))) problems.push(`File share ${v.source} is on none of this ${noun}'s networks.`);
  }
  problems.push(...fwd.problems);
  if (fwd.make.length > 0 && forwards.loading) problems.push("The port forwards are loading.");
  if (fwdQuota.limit !== undefined && fwd.make.length > fwd.remove.length && fwdQuota.used + fwd.make.length - fwd.remove.length > fwdQuota.limit) {
    problems.push(`The plan allows ${fwdQuota.limit} port forwards; ${fwdQuota.used} are used.`);
  }
  if (!edit && kind === "vm" && !chosenImage) problems.push("No image is chosen.");
  if (!s) problems.push(machineSizes.loading ? "The machine sizes are loading." : "No machine size is chosen.");
  if (customProblem) problems.push(customProblem);
  if (s && diskProblem && !baseV) problems.push(diskProblem);
  const nameProblem = !edit && !nameCheck.ok && name !== "" && !existing.loading && !existing.error ? nameCheck.reason : undefined;

  const submit = useMutation({
    mutationFn: async () => {
      // A machine is never sent without one of the platform's sizes.
      if (!s) throw new Error("No machine size is chosen.");
      // The shares first: admission refuses a container declaring a share on none of its networks.
      for (const { share, adds } of sharesChanged) {
        const n = shareName(share);
        if (applied.some((a) => a.share === n && adds.every((x) => a.adds.includes(x)))) continue;
        let added: string[];
        try {
          added = await addNetworks(transport, project, n, adds);
        } catch (err) {
          throw new Error(`Adding ${adds.join(", ")} to file share ${n} failed: ${errorMessage(err)}`, { cause: err });
        }
        // Only what this page added is its to undo; a network already there was someone else's.
        if (added.length) setApplied((was) => [...was.filter((a) => a.share !== n), { share: n, adds: added }]);
      }
      const pfs = createClient(PortForwardService, transport);
      const makeForwards = async () => {
        const made: string[] = [];
        for (const pf of forwardObjs) {
          const n = pf.metadata?.name ?? "";
          try {
            await pfs.applyPortForward({ portForward: pf, createOnly: true });
          } catch (err) {
            throw Object.assign(new Error(`Creating port forward ${n} failed: ${errorMessage(err)}`, { cause: err }), { made });
          }
          made.push(n);
        }
        return made;
      };
      if (edit) {
        if (machineChanges.length > 0) await edit.write(obj);
        for (const n of fwd.remove) {
          try {
            await pfs.deletePortForward({ project, name: n });
          } catch (err) {
            throw new Error(`Deleting port forward ${n} failed: ${errorMessage(err)}`, { cause: err });
          }
        }
        await makeForwards();
        return;
      }
      // The forwards first, each waiting for the machine it names: one the
      // plan refuses then stops the create before the machine is made.
      const made = await makeForwards().catch(async (err: Error & { made?: string[] }) => {
        await Promise.allSettled((err.made ?? []).map((n) => pfs.deletePortForward({ project, name: n })));
        throw err;
      });
      try {
        if (kind === "container") {
          await createClient(ContainerService, transport).applyContainer({ container: obj as Container, createOnly: true });
        } else {
          await createClient(VirtualMachineService, transport).applyVirtualMachine({
            virtualMachine: obj as VirtualMachine,
            createOnly: true,
          });
        }
      } catch (err) {
        // A machine not made leaves no forward behind.
        await Promise.allSettled(made.map((n) => pfs.deletePortForward({ project, name: n })));
        throw createError(err, noun, name);
      }
    },
    onSuccess: () => {
      void queryClient.invalidateQueries();
      if (!edit) markJustCreated(createdKey(project, kind === "container" ? "Container" : "VirtualMachine", name));
      void navigate({
        to: kind === "container" ? "/projects/$project/containers/$name" : "/projects/$project/vms/$name",
        params: { project, name },
        ...(edit && { replace: true }),
      });
    },
  });
  const stale = !!edit && isStale(submit.error);
  const blocked = problems.length > 0 || over.length > 0 || submit.isPending || (!!edit && (changes.length === 0 || stale));
  const listPath = `/projects/${project}/${kind === "container" ? "containers" : "vms"}`;

  const side = (
    <>
      <FormSummary
        summary={
          <>
            {s ? (
              <div className="flex items-baseline gap-2">
                <span className="font-mono text-[20px] font-semibold">{s.label}</span>
                <span className="text-[13px] text-muted tabular">
                  {formatNumber(s.cpu)} vCPU · {formatNumber(s.memoryGi)} GiB
                  {baseV ? "" : ` · ${diskTyped || "–"} GiB disk`}
                </span>
              </div>
            ) : (
              <div className="text-[13px] text-muted">No size</div>
            )}
            <div className="mt-0.5 truncate font-mono text-[12px] text-muted">{(kind === "container" ? image : chosenImage) || "no image"}</div>
          </>
        }
        applying={edit && <WhatApplyingDoes changes={changes} noun={shortNoun} />}
        notices={
          <>
            {over.length > 0 && (
              <Notice tone="error" title={`Over the project's ${over.map((o) => o.label).join(" and ")} limit`}>
                {over.map((o) => (
                  <div key={o.key}>
                    {edit ? "The change adds" : `${o.key === "disk" ? "The disk" : s?.label} needs`} {formatNumber(o.need)}
                    {o.unit}; {formatNumber(o.left)}
                    {o.unit} of {formatNumber(o.limit)}
                    {o.unit} is left.
                  </div>
                ))}
              </Notice>
            )}
            {!edit && existing.error ? (
              <Notice tone="error" title={`The list of the project's ${noun}s did not load`}>
                <div>Which names are taken is not known, so nothing is created.</div>
                <div className="mt-1 font-mono text-[11.5px]">{errorMessage(existing.error)}</div>
              </Notice>
            ) : null}
            {stale && (
              <StaleNotice title={`${name} changed since you opened it.`} reloading={edit.reload.pending} error={edit.reload.error} onReload={edit.reload.run} />
            )}
            {applied.length > 0 && !submit.isPending && (
              <SharesChanged
                project={project}
                applied={applied}
                onUndone={(n) => {
                  setApplied((was) => was.filter((a) => a.share !== n));
                  const next = { ...shareAdds };
                  delete next[n];
                  setShareAdds(next);
                }}
              />
            )}
          </>
        }
        error={stale ? null : submit.error}
        failed={edit ? `Applying the ${noun} failed` : `Creating the ${noun} failed`}
        // A new machine's problems wait for a name, and for the names taken to be known.
        problems={edit || (name !== "" && !existing.error) ? problems : []}
        blocked={blocked}
        pending={submit.isPending}
        pendingLabel={edit ? "Applying…" : "Creating…"}
        label={edit ? (changes.length === 0 ? "No change" : "Apply changes") : existing.loading ? "Loading names…" : `Create ${noun}`}
        cancelTo={edit ? `${listPath}/${name}` : listPath}
      />
      <FormManifest yaml={yaml} cli={cli} />
    </>
  );

  return (
    <FormPage
      crumbs={edit ? undefined : ["New machine"]}
      title={edit ? `Edit ${name}` : "New machine"}
      // The browser's checks would hold back a stored disk such as 10G, sent back as it is.
      noValidate
      onSubmit={() => !blocked && submit.mutate()}
      side={side}
    >
      <Section n={1} title="Kind">
        {edit ? (
          <Locked why="Does not change.">
            <span className="flex items-center gap-2">
              {kind === "container" ? <Box className="size-4 text-muted" strokeWidth={1.75} /> : <Server className="size-4 text-muted" strokeWidth={1.75} />}
              {kind === "container" ? "Container" : "Virtual machine"}
            </span>
          </Locked>
        ) : (
          <div role="radiogroup" aria-label="Kind" className="grid max-w-xl grid-cols-1 gap-3 sm:grid-cols-2">
            {(
              [
                ["container", Box, "Container", "A container image. Starts in seconds."],
                ["vm", Server, "Virtual machine", "A full operating system."],
              ] as const
            ).map(([id, Icon, label, text]) => (
              <Choice
                key={id}
                selected={kind === id}
                onClick={() => {
                  if (id === kind) return;
                  // A kind's file shares are its own, so the interfaces and share changes made for them go with them.
                  setKind(id);
                  setExtraNets([]);
                  setShareAdds({});
                  setJustAdded(null);
                  setPickerOpen(null);
                }}
                className="p-4"
              >
                <Icon className="size-4 text-muted" strokeWidth={1.75} />
                <div className="mt-2 text-[13.5px] font-medium">{label}</div>
                <div className="mt-0.5 text-[12.5px] leading-snug text-muted">{text}</div>
              </Choice>
            ))}
          </div>
        )}
      </Section>

      <Section n={2} title="Image" hint={kind === "container" ? "Any OCI reference" : readyCustom.length > 0 ? "This project's or the platform's" : "The platform's catalogue"}>
        {kind === "container" ? (
          <div className="max-w-xl">
            <Input
              value={image}
              onChange={(e) => setImage(e.target.value)}
              placeholder="ghcr.io/owner/app:1.0"
              aria-label="Image"
              spellCheck={false}
            />
            {edit && <p className="mt-1.5 text-[12px] text-subtle">Changing it rebuilds the root disk.</p>}
          </div>
        ) : edit ? (
          <Locked why="Does not change.">
            {baseRoot?.case === "disk" ? (
              <>
                Boots from disk <span className="font-mono">{baseRoot.value}</span>
              </>
            ) : chosenCustom ? (
              <>
                Custom image <span className="font-mono">{chosenImage}</span>
              </>
            ) : (
              (() => {
                const img = images.data?.items.find((i) => i.metadata?.name === chosenImage);
                return (
                  <>
                    <span className="font-medium capitalize">{img?.spec?.os || chosenImage}</span>{" "}
                    <span className="font-mono text-[12px] text-muted">{img ? img.spec?.version : ""}</span>{" "}
                    {img && <span className="font-mono text-[12px] text-subtle">· {chosenImage}</span>}
                  </>
                );
              })()
            )}
          </Locked>
        ) : images.isPending ? (
          <div className="grid max-w-xl grid-cols-2 gap-3 sm:grid-cols-4">
            {[0, 1].map((i) => (
              <Skeleton key={i} className="h-14 rounded-lg" />
            ))}
          </div>
        ) : images.error ? (
          <ErrorNotice error={images.error} title="The images did not load" onRetry={() => void images.refetch()} className="max-w-xl p-0" />
        ) : images.data.items.length === 0 ? (
          <p className="text-[13px] text-muted">The platform's catalogue holds no image.</p>
        ) : (
          <div className="max-w-xl space-y-4">
            {readyCustom.length > 0 && (
              <div>
                <div className="mb-2 text-[12px] font-medium text-muted">This project's images</div>
                <div role="radiogroup" aria-label="This project's images" className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                  {readyCustom.map((c) => {
                    const id = c.metadata?.name ?? "";
                    return (
                      <Choice key={id} selected={chosenCustom && chosenImage === id} onClick={() => chooseImage(id, true)} className="p-3">
                        <div className="truncate font-mono text-[12.5px] font-medium">{id}</div>
                        <div className="truncate text-[11.5px] capitalize text-muted">{[c.spec?.os, c.spec?.version].filter(Boolean).join(" ") || "custom"}</div>
                      </Choice>
                    );
                  })}
                </div>
              </div>
            )}
            <div>
              {readyCustom.length > 0 && <div className="mb-2 text-[12px] font-medium text-muted">Platform images</div>}
              <div role="radiogroup" aria-label="Image" className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                {images.data.items.map((img) => {
                  const id = img.metadata?.name ?? "";
                  return (
                    <Choice key={id} selected={!chosenCustom && chosenImage === id} onClick={() => chooseImage(id, false)} className="p-3">
                      <div className="text-[13px] font-medium capitalize">{img.spec?.os || id}</div>
                      <div className="font-mono text-[11.5px] text-muted">{img.spec?.version || id}</div>
                    </Choice>
                  );
                })}
              </div>
            </div>
          </div>
        )}
      </Section>

      <Section n={3} title="Size">
        {machineSizes.loading ? (
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            {[0, 1, 2, 3].map((i) => (
              <Skeleton key={i} className="h-[118px] rounded-lg" />
            ))}
          </div>
        ) : machineSizes.error ? (
          <ErrorNotice error={machineSizes.error} title="The machine sizes did not load" onRetry={machineSizes.refetch} className="max-w-xl p-0" />
        ) : (
          <>
          {/* One row on a wide screen, whatever the number of sizes; Custom last. */}
          <div
            role="radiogroup"
            aria-label="Size"
            className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-[repeat(var(--cards),minmax(0,1fr))]"
            style={{ "--cards": cards.length + 1 } as CSSProperties}
          >
            {cards.map((z) => {
              const tooBig = cardOver(z).length > 0;
              const selected = !customFields && (z === current ? s === current : s === current ? isCurrent(z) : s?.name === z.name);
              const own = z === current || isCurrent(z);
              return (
                <Choice
                  key={z === current ? "(current)" : z.name}
                  label={`Size ${z.label}`}
                  selected={selected}
                  onClick={() => pickSize(own ? undefined : z.name)}
                  className={cn("p-4", tooBig && (selected ? "border-fail ring-fail [&>span:first-child]:bg-fail" : "opacity-60"))}
                >
                  <div
                    className={cn(
                      "flex min-h-[26px] items-center break-words pr-5 font-mono font-semibold tracking-tight",
                      z.label.length > 4 ? "text-[17px] leading-tight" : "text-[26px] leading-none",
                    )}
                  >
                    {z.label}
                  </div>
                  <div className="mt-3 space-y-1 font-mono text-[12.5px] tabular">
                    <div>
                      {formatNumber(z.cpu)} <span className="text-muted">vCPU</span>
                    </div>
                    <div>
                      {formatNumber(z.memoryGi)} <span className="text-muted">GiB memory</span>
                    </div>
                  </div>
                  {own && <div className="mt-2 text-[11.5px] font-medium text-muted">{z === current ? "Current, no listed size" : "Current"}</div>}
                  {tooBig && <div className="mt-2 text-[11.5px] font-medium text-fail">Over quota</div>}
                </Choice>
              );
            })}
            <Choice key="(custom)" label="Size Custom" selected={!!customFields} onClick={pickCustom} className="p-4">
              <div className="flex min-h-[26px] items-center pr-5 font-mono text-[17px] font-semibold leading-tight tracking-tight">Custom</div>
              <div className="mt-3 space-y-1 font-mono text-[12.5px] tabular">
                {customFields ? (
                  <>
                    <div>
                      {customCPU} <span className="text-muted">vCPU</span>
                    </div>
                    <div>
                      {gibText(customMiB)} <span className="text-muted">GiB memory</span>
                    </div>
                  </>
                ) : (
                  <SlidersHorizontal className="size-5 text-subtle" strokeWidth={1.5} />
                )}
                {/* The shape rule the rulers keep to, under the memory it is about. */}
                <div className="whitespace-nowrap font-sans text-[11px] text-subtle">
                  {formatNumber(minMiBPerCPU / 1024)}–{formatNumber(maxMiBPerCPU / 1024)} GiB per vCPU
                </div>
              </div>
              {customFields && over.length > 0 && <div className="mt-2 text-[11.5px] font-medium text-fail">Over quota</div>}
            </Choice>
          </div>
          {customFields && (
            <div className="mt-3">
              <SizeSliders
                memMiB={customMiB}
                cpu={customCPU}
                stops={stops}
                cpuMax={limits.cpu}
                over={over.length > 0}
                onChange={(shape) => setCustom(shape)}
              />
              {customProblem && <p className="mt-2 text-[12px] text-fail">{customProblem}</p>}
            </div>
          )}
          </>
        )}
      </Section>

      <Section n={4} title={kind === "vm" ? "Disks" : "Disk"}>
        <div className="max-w-xl divide-y rounded-lg border bg-surface">
          <div className="px-4 py-3">
            <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
              <span className="flex w-24 shrink-0 items-center gap-2 text-[13px] font-medium">
                <HardDrive className="size-4 text-muted" strokeWidth={1.75} />
                {kind === "vm" ? "Boot" : "Root"}
              </span>
              {baseV ? (
                <span className="flex min-w-0 flex-wrap items-center gap-x-2 text-[13px]">
                  <span className="font-mono tabular">
                    {baseRoot?.case === "fromImage" ? `${formatNumber(quantityGiB(baseRoot.value.size || vmDefaults.disk))} GiB` : "its own size"}
                  </span>
                  {baseV.status?.rootDisk && (
                    <DiskLink name={baseV.status.rootDisk} />
                  )}
                  <span className="text-[12px] text-subtle">grows on its disk's page</span>
                </span>
              ) : (
                <>
                  <Input
                    type="number"
                    inputMode="numeric"
                    min={minDisk}
                    step={1}
                    value={diskText ?? String(diskStart)}
                    onChange={(e) => setDiskText(e.target.value)}
                    aria-label="Disk, GiB"
                    aria-invalid={!!diskProblem || !!diskOver}
                    disabled={!s}
                    className={cn("h-8 w-24 tabular", (diskProblem || diskOver) && "border-fail focus:border-fail")}
                  />
                  <span className="text-[13px] text-muted">GiB</span>
                  <span className="text-[12px] text-subtle">
                    {editFloor !== undefined ? `≥ ${editFloor} GiB, never shrinks` : byImage ? `≥ ${minDisk} GiB for ${chosenImage}` : `≥ ${minDisk} GiB`}
                  </span>
                </>
              )}
            </div>
            {!baseV && s && diskProblem ? (
              <p className="mt-1.5 text-[12px] text-fail">{diskProblem}</p>
            ) : !baseV && diskOver ? (
              <p className="mt-1.5 text-[12px] text-fail">
                Over quota: {formatNumber(diskOver.left)} of {formatNumber(diskOver.limit)} GiB left.
              </p>
            ) : null}
          </div>
          {kind === "vm" && (
            <div className="px-4 py-3">
              <div className="mb-2 text-[13px] font-medium">Data</div>
              <VmDisks project={project} rows={dataDisks} onChange={setDataDisks} vm={edit ? name : undefined} />
            </div>
          )}
        </div>
      </Section>

      <Section n={5} title="Network">
        <div className="max-w-xl space-y-3">
          {networks.error ? (
            <ErrorNotice error={networks.error} title="The networks did not load" onRetry={() => void networks.refetch()} className="p-0" />
          ) : (
            <div className="rounded-lg border bg-surface px-4 py-3">
              <label className="text-[13px] font-medium" htmlFor="network">
                {extra.length > 0 && <span className="mr-2 font-mono text-[12px] font-normal text-subtle">eth0</span>}
                Network
              </label>
              <Select
                id="network"
                value={network}
                onChange={(e) => {
                  setNetwork(e.target.value);
                  setGroups(["default"]);
                }}
                className="mt-2"
              >
                {(networks.data?.items ?? [{ metadata: { name: network }, spec: { ipv4Cidr: "" } }]).map((n) => (
                  <option key={n.metadata?.name} value={n.metadata?.name}>
                    {n.metadata?.name} {n.spec?.ipv4Cidr && `· ${n.spec.ipv4Cidr}`}
                  </option>
                ))}
              </Select>
              <div className="mt-3 text-[13px] font-medium">Security groups</div>
              <div className="mt-2 flex flex-wrap gap-1.5">
                {["default", ...groupsHere.filter((g) => g !== "default"), ...groups.filter((g) => g !== "default" && !groupsHere.includes(g))].map((g) => {
                  const on = groups.includes(g);
                  return (
                    <button
                      key={g}
                      type="button"
                      aria-pressed={on}
                      onClick={() => setGroups(on ? groups.filter((x) => x !== g) : [...groups, g])}
                      className={cn(
                        "flex h-6 items-center gap-1 rounded border px-2 font-mono text-[11.5px]",
                        on ? "border-accent bg-accent-soft text-foreground" : "text-muted hover:border-border-strong",
                      )}
                    >
                      {on && <Check className="size-3 text-accent" />}
                      {g}
                    </button>
                  );
                })}
              </div>
            </div>
          )}
          {extra.map((x, i) => (
            <div
              key={x.network}
              className={cn(
                "flex items-center gap-3 rounded-lg border bg-surface px-4 py-2.5 text-[13px]",
                justAdded?.network === x.network && "border-accent ring-1 ring-accent",
              )}
            >
              <span className="font-mono text-[12px] text-subtle">eth{i + 1}</span>
              <span className="font-mono font-medium">{x.network}</span>
              <span className="font-mono text-[11.5px] text-muted">{networks.data?.items.find((n) => n.metadata?.name === x.network)?.spec?.ipv4Cidr}</span>
              {x.groups.map((g) => (
                <span key={g} className="flex h-6 items-center gap-1 rounded border border-accent bg-accent-soft px-2 font-mono text-[11.5px]">
                  <Check className="size-3 text-accent" />
                  {g}
                </span>
              ))}
              {justAdded?.network === x.network && (
                <span className="text-[12px] text-muted">
                  Added for file share <span className="font-mono">{justAdded.share}</span>
                </span>
              )}
              <button
                type="button"
                aria-label={`Remove interface on ${x.network}`}
                onClick={() => setExtraNets(extraNets.filter((y) => y.network !== x.network))}
                className="ml-auto rounded p-1 text-subtle hover:bg-surface-2 hover:text-foreground"
              >
                <X className="size-3.5" />
              </button>
            </div>
          ))}
          <p className="flex items-start gap-2 text-[12px] text-muted">
            <Globe className="mt-0.5 size-3.5 shrink-0" strokeWidth={1.75} />
            Each interface gets a public IPv6 address; its security groups decide what gets in.
          </p>
        </div>
      </Section>

      <Section n={6} title={kind === "container" ? "Volumes" : "File shares"}>
        <VolumePicker
          vm={kind === "vm"}
          project={project}
          rows={kind === "container" ? volumes : vmShares}
          onChange={kind === "container" ? setVolumes : setVmShares}
          open={pickerOpen}
          onOpen={setPickerOpen}
          networks={onNetworks}
          onAddInterface={addInterface}
          shareAdds={shareAdds}
          onShareAdds={setShareAdds}
          applied={applied.map((a) => a.share)}
          mayApplyShares={mayApplyShares}
        />
      </Section>

      <Section n={7} title="Port forwards">
        <MachinePortForwards
          kind={kind === "container" ? "Container" : "VirtualMachine"}
          machine={name}
          rows={forwardRows}
          onChange={setForwardEdits}
          nics={[{ network, groups }, ...extra]}
          forwards={forwards.items}
          groups={securityGroups.data?.items ?? []}
        />
      </Section>

      <Section n={8} title="Advanced" hint="Optional">
        {kind === "container" ? (
          <ContainerAdvanced value={advanced} onChange={setAdvanced} open={advancedOpen} onToggle={() => setAdvancedOpen(!advancedOpen)} />
        ) : (
          <VmAdvancedFields value={vmAdvanced} onChange={setVmAdvanced} open={vmAdvancedOpen} onToggle={() => setVmAdvancedOpen(!vmAdvancedOpen)} tpmLocked={!!baseV} />
        )}
      </Section>

      <Section n={9} title="Name">
        {edit ? (
          <Locked why="Does not change.">
            <span className="font-mono">{name}</span>
          </Locked>
        ) : (
          <div className="max-w-xl">
            <Input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} aria-label="Name" spellCheck={false} />
            {nameProblem && <p className="mt-1.5 text-[12px] text-fail">{nameProblem}</p>}
          </div>
        )}
      </Section>
    </FormPage>
  );
}
