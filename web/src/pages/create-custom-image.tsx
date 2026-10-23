// A new custom image, captured from one of the project's disks: the disk
// must have been made on a node and its machine, if any, stopped, which the
// API holds to; the form only says beforehand which disks it would refuse.
// What the image holds is the disk as it is: the platform changes nothing in
// it, so the page says what to clean in the guest before stopping it
// (docs/runbook/vm-images.md). The one apply is create-only (lib/create.ts).

import { Link, useNavigate } from "@tanstack/react-router";
import { useState, type ReactNode } from "react";
import { Input } from "@/components/form";
import { Camera, Check, Upload } from "lucide-react";
import { Choice, CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { bytes, FileDrop, type ChosenFile } from "@/components/image-upload";
import { useProjectName } from "@/components/shell";
import { SourceDiskPicker } from "@/components/source-disk-picker";
import { ErrorNotice, Skeleton } from "@/components/states";
import { CopyButton } from "@/components/ui";
import { checkName } from "@/lib/create";
import { captureRefusal, newCustomImage, useCreateCustomImage, useCustomImages, type NewImageSource } from "@/lib/custom-images";
import { startUpload } from "@/lib/upload";
import { useMachines } from "@/lib/machines";
import { toYaml } from "@/lib/manifest";
import { useCan } from "@/lib/permissions";
import { diskImage, gib, useDisks, useImages } from "@/lib/storage";

const applyPerm = "storage.customimages.apply";

/** The command that clears what a guest keeps of its first boot, so each machine of the image starts as itself. */
export const cleanCommand = "sudo cloud-init clean --logs --machine-id";

/** Command is one shell line to copy. */
function Command({ text }: { text: string }) {
  return (
    <div className="mt-2 flex items-center gap-2 rounded-md border bg-surface-2 px-3 py-1.5 font-mono text-[12.5px]">
      <span className="select-none text-subtle">$</span>
      <span className="min-w-0 flex-1 truncate">{text}</span>
      <CopyButton text={text} />
    </div>
  );
}

/** Step is one numbered step to take before capturing. */
function Step({ n, title, text, children }: { n: number; title: string; text: string; children: ReactNode }) {
  return (
    <li className="flex gap-3 px-4 py-3">
      <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full bg-surface-2 text-[11px] font-medium text-muted tabular">{n}</span>
      <div className="min-w-0 flex-1">
        <div className="font-medium">{title}</div>
        <div className="mt-0.5 text-[12.5px] text-muted">{text}</div>
        {children}
      </div>
    </li>
  );
}

/** How a new image is made: captured from a disk, or uploaded from a file. */
export type ImageMode = "capture" | "upload";

// The largest file a node takes unless the platform says otherwise
// (docs/decisions/custom-images.md); the API refuses past it whatever this
// says.
const maxUploadBytes = 16 * 2 ** 30;

const modes: { id: ImageMode; icon: typeof Camera; label: string; text: string }[] = [
  { id: "capture", icon: Camera, label: "Capture a disk", text: "A stopped machine's disk, as it is." },
  { id: "upload", icon: Upload, label: "Upload a file", text: "A qcow2 or raw image of your own." },
];

/** CreateCustomImage is the New image page; disk opens it on that source disk, mode on upload. */
export function CreateCustomImage({ disk, mode }: { disk?: string; mode?: ImageMode }) {
  const project = useProjectName();
  const perm = useCan({ level: "project", name: project }, [applyPerm]);
  const crumbs = [
    <Link key="list" to="/projects/$project/images" params={{ project }} className="hover:text-foreground">
      Images
    </Link>,
    "New image",
  ];
  return (
    <CreateGate crumbs={crumbs} title="New image" perm={perm} may={perm.can(applyPerm)} refusal="creating custom images">
      <ImageForm crumbs={crumbs} project={project} initialDisk={disk} initialMode={disk ? "capture" : (mode ?? "capture")} />
    </CreateGate>
  );
}

/** nameOfFile suggests an image name from a file's: "nixos-24.11.qcow2" is "nixos-24-11". */
export function nameOfFile(file: string): string {
  return file
    .replace(/\.(qcow2|img|raw|bin)(\.\w+)?$/i, "")
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63);
}

function ImageForm({ crumbs, project, initialDisk, initialMode }: { crumbs: ReactNode[]; project: string; initialDisk?: string; initialMode: ImageMode }) {
  const navigate = useNavigate();
  const disks = useDisks(project);
  const machines = useMachines(project);
  const images = useImages();
  const customImages = useCustomImages(project);
  const create = useCreateCustomImage();
  const [mode, setMode] = useState<ImageMode>(initialMode);
  const [disk, setDisk] = useState(initialDisk ?? "");
  const [file, setFile] = useState<ChosenFile>();
  // The name as typed; null until edited, while it follows the file's.
  const [nameText, setName] = useState<string | null>(null);
  // OS, version and default user as typed; null until edited, while they follow the disk's image.
  const [os, setOs] = useState<string | null>(null);
  const [version, setVersion] = useState<string | null>(null);
  const [user, setUser] = useState<string | null>(null);

  const capture = mode === "capture";
  const chosen = capture ? disks.items.find((d) => d.metadata?.name === disk) : undefined;
  const from = chosen ? images.items.find((i) => i.metadata?.name === diskImage(chosen)) : undefined;
  const name = nameText ?? (!capture && file ? nameOfFile(file.file.name) : "");
  const osV = os ?? from?.spec?.os ?? "";
  const versionV = version ?? from?.spec?.version ?? "";
  const userV = user ?? from?.spec?.defaultUser ?? "";
  const refusal = chosen ? captureRefusal(chosen, machines.items) : undefined;
  const owner = chosen?.status?.machine ? machines.items.find((m) => m.machine?.metadata?.name === chosen.status?.machine) : undefined;
  const machineName = owner?.name;
  const machineKind = owner?.kind;

  const nameCheck = checkName(name, "image", {
    loading: customImages.loading,
    error: customImages.error,
    names: customImages.items.map((c) => c.metadata?.name ?? ""),
  });
  const problems: string[] = [];
  if (capture) {
    if (!chosen) problems.push("No disk is chosen.");
    else if (refusal) problems.push(`${disk}: ${refusal.reason}`);
  } else if (!file) problems.push("No file is chosen.");
  else if (file.file.size > maxUploadBytes) problems.push(`${file.file.name} is more than ${bytes(maxUploadBytes)}.`);
  else if (file.format === "unknown") problems.push(`${file.file.name} is not a qcow2 or raw image.`);
  if (!nameCheck.ok) problems.push(nameCheck.reason);

  const src: NewImageSource = capture ? { case: "disk", disk } : { case: "upload", size: file?.file.size ?? 0 };
  const ci = newCustomImage(project, name, src, osV, versionV, userV);
  const yaml = toYaml({
    apiVersion: "storage.hangarlab.cloud/v1alpha1",
    kind: "CustomImage",
    metadata: { name: name || "NAME" },
    spec: {
      source: capture ? { disk: { name: disk || "DISK" } } : { upload: { size: file?.file.size ?? 0 } },
      ...(osV && { os: osV }),
      ...(versionV && { version: versionV }),
      ...(userV && { defaultUser: userV }),
    },
  });
  const cli = capture
    ? [`hangar apply --create-only -p ${project} -f ${name || "image"}.yaml`]
    : [`hangar image upload ${file?.file.name ?? "FILE"} --name ${name || "NAME"} -p ${project}`];
  const submit = () => {
    if (problems.length > 0) return;
    create.mutate(ci, {
      onSuccess: () => {
        if (!capture && file) startUpload(project, name, file.file, 0);
        void navigate({ to: "/projects/$project/custom-images/$name", params: { project, name }, replace: true });
      },
    });
  };

  return (
    <FormPage
      crumbs={crumbs}
      title="New image"
      onSubmit={submit}
      side={
        <>
          <FormSummary
            summary={
              <>
                <div className="text-[15px] font-medium">Custom image</div>
                <div className="mt-0.5 truncate text-[12px] text-muted">
                  {capture ? (
                    <>
                      From <span className="font-mono">{disk || "no disk"}</span>
                      {chosen && ` · up to ${gib(chosen.status?.size || chosen.spec?.size)}`}
                    </>
                  ) : file ? (
                    <>
                      Upload <span className="font-mono">{file.file.name}</span> · {bytes(file.file.size)}
                    </>
                  ) : (
                    "Upload: no file chosen"
                  )}
                </div>
                <p className="mt-2 text-[12px] leading-snug text-subtle">
                  {capture
                    ? "While it is captured it counts at the disk's size toward the project's disk quota, then at the space its data takes."
                    : "Until it is ready it counts at the file's size toward the project's disk quota, then at the space its data takes. The upload runs in this tab: keep it open, or choose the file again on the image's page."}
                </p>
              </>
            }
            problems={problems}
            error={create.error}
            failed="Creating the image failed"
            pending={create.isPending}
            pendingLabel="Creating…"
            label={customImages.loading ? "Loading names…" : capture ? "Capture image" : "Create and upload"}
            cancelTo={`/projects/${project}/images`}
          />
          <FormManifest yaml={yaml} cli={cli} />
        </>
      }
    >
      <FormSection n={1} title="Source">
        <div role="radiogroup" aria-label="Source" className="grid max-w-2xl grid-cols-1 gap-3 sm:grid-cols-2">
          {modes.map((m) => (
            <Choice key={m.id} selected={mode === m.id} onClick={() => setMode(m.id)} label={m.label}>
              <m.icon className="size-4 text-muted" strokeWidth={1.75} />
              <div className="mt-2 text-[13.5px] font-medium">{m.label}</div>
              <div className="mt-0.5 text-[12.5px] leading-snug text-muted">{m.text}</div>
            </Choice>
          ))}
        </div>
      </FormSection>

      {capture ? (
        <>
          <FormSection n={2} title="Source disk" hint="Its machine must be stopped">
            {disks.loading || machines.loading ? (
              <Skeleton className="h-40 max-w-2xl rounded-lg" />
            ) : disks.error ? (
              <ErrorNotice error={disks.error} title="The disks did not load" onRetry={disks.refetch} className="max-w-2xl p-0" />
            ) : disks.items.length === 0 ? (
              <p className="text-[13px] text-muted">This project has no disks yet. Create a virtual machine, set it up, and stop it.</p>
            ) : (
              <SourceDiskPicker disks={disks.items} machines={machines.items} value={disk} onChange={setDisk} />
            )}
          </FormSection>

          <FormSection n={3} title="Before you capture" hint="The image holds the disk exactly as it is">
            <ol className="max-w-2xl divide-y overflow-hidden rounded-lg border bg-surface text-[13px]">
              <Step n={1} title="Clear the guest's identity" text="So each machine made from the image gets its own machine ID, SSH host keys and cloud-init run.">
                <Command text={cleanCommand} />
              </Step>
              <Step
                n={2}
                title="Stop the machine"
                text={machineName ? `Hangar captures ${disk} only while ${machineName} is stopped, so the copy is consistent.` : "Hangar captures a disk only while its machine is stopped, so the copy is consistent."}
              >
                <Command text={`hangar stop ${machineKind === "Container" ? "ct" : "vm"} ${machineName ?? "NAME"} -p ${project} --wait`} />
              </Step>
            </ol>
          </FormSection>
        </>
      ) : (
        <>
          <FormSection n={2} title="File" hint={`qcow2 or raw, up to ${bytes(maxUploadBytes)}`}>
            <FileDrop value={file} onChange={setFile} maxBytes={maxUploadBytes} />
          </FormSection>

          <FormSection n={3} title="What the image needs" hint="Hangar boots it as it boots a distribution's own cloud image">
            <ul className="max-w-2xl divide-y overflow-hidden rounded-lg border bg-surface text-[13px]">
              <Need title="An EFI boot loader" text="Machines boot through UEFI firmware, without Secure Boot." />
              <Need title="cloud-init reading NoCloud" text="It sets the hostname, the network and your SSH keys from the platform's metadata." />
              <Need title="sshd reading /etc/ssh/sshd_config.d" text="For hangar ssh, which logs in with the platform's certificate." />
            </ul>
          </FormSection>
        </>
      )}

      <FormSection n={4} title="Describe it" hint="For people choosing an image">
        <div className="grid max-w-2xl grid-cols-1 gap-3 sm:grid-cols-3">
          <label className="block">
            <span className="text-[12.5px] text-muted">OS</span>
            <Input value={osV} onChange={(e) => setOs(e.target.value)} aria-label="OS" spellCheck={false} placeholder={capture ? "ubuntu" : "nixos"} />
          </label>
          <label className="block">
            <span className="text-[12.5px] text-muted">Version</span>
            <Input value={versionV} onChange={(e) => setVersion(e.target.value)} aria-label="Version" spellCheck={false} placeholder={capture ? "24.04" : "24.11"} />
          </label>
          <label className="block">
            <span className="text-[12.5px] text-muted">Default user</span>
            <Input value={userV} onChange={(e) => setUser(e.target.value)} aria-label="Default user" spellCheck={false} placeholder={capture ? "ubuntu" : "nixos"} />
          </label>
        </div>
        {from && os === null && <p className="mt-1.5 text-[12px] text-subtle">Filled in from {from.metadata?.name}, the image the disk was made from.</p>}
      </FormSection>

      <FormSection n={5} title="Image name">
        <div className="max-w-xl">
          <Input value={name} onChange={(e) => setName(e.target.value)} aria-label="Image name" spellCheck={false} placeholder="web-golden" />
          {!capture && file && nameText === null && <p className="mt-1.5 text-[12px] text-subtle">From the file's name.</p>}
        </div>
      </FormSection>
    </FormPage>
  );
}

/** Need is one thing an uploaded image must have to boot on Hangar. */
function Need({ title, text }: { title: string; text: string }) {
  return (
    <li className="flex gap-3 px-4 py-3">
      <Check className="mt-0.5 size-4 shrink-0 text-ok" strokeWidth={2} />
      <div className="min-w-0">
        <div className="font-medium">{title}</div>
        <div className="mt-0.5 text-[12.5px] text-muted">{text}</div>
      </div>
    </li>
  );
}
