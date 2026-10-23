// A new custom image, captured from one of the project's disks: the disk
// must have been made on a node and its machine, if any, stopped, which the
// API holds to; the form only says beforehand which disks it would refuse.
// What the image holds is the disk as it is: the platform changes nothing in
// it, so the page says what to clean in the guest before stopping it
// (docs/runbook/vm-images.md). The one apply is create-only (lib/create.ts).

import { Link, useNavigate } from "@tanstack/react-router";
import { useState, type ReactNode } from "react";
import { Input } from "@/components/form";
import { CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { useProjectName } from "@/components/shell";
import { SourceDiskPicker } from "@/components/source-disk-picker";
import { ErrorNotice, Skeleton } from "@/components/states";
import { CopyButton } from "@/components/ui";
import { checkName } from "@/lib/create";
import { captureRefusal, newCustomImage, useCreateCustomImage, useCustomImages } from "@/lib/custom-images";
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

/** CreateCustomImage is the New image page; disk opens it on that source disk. */
export function CreateCustomImage({ disk }: { disk?: string }) {
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
      <ImageForm crumbs={crumbs} project={project} initialDisk={disk} />
    </CreateGate>
  );
}

function ImageForm({ crumbs, project, initialDisk }: { crumbs: ReactNode[]; project: string; initialDisk?: string }) {
  const navigate = useNavigate();
  const disks = useDisks(project);
  const machines = useMachines(project);
  const images = useImages();
  const customImages = useCustomImages(project);
  const create = useCreateCustomImage();
  const [disk, setDisk] = useState(initialDisk ?? "");
  const [name, setName] = useState("");
  // OS, version and default user as typed; null until edited, while they follow the disk's image.
  const [os, setOs] = useState<string | null>(null);
  const [version, setVersion] = useState<string | null>(null);
  const [user, setUser] = useState<string | null>(null);

  const chosen = disks.items.find((d) => d.metadata?.name === disk);
  const from = chosen ? images.items.find((i) => i.metadata?.name === diskImage(chosen)) : undefined;
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
  if (!chosen) problems.push("No disk is chosen.");
  else if (refusal) problems.push(`${disk}: ${refusal.reason}`);
  if (!nameCheck.ok) problems.push(nameCheck.reason);

  const ci = newCustomImage(project, name, disk, osV, versionV, userV);
  const yaml = toYaml({
    apiVersion: "storage.hangarlab.cloud/v1alpha1",
    kind: "CustomImage",
    metadata: { name: name || "NAME" },
    spec: {
      source: { disk: { name: disk || "DISK" } },
      ...(osV && { os: osV }),
      ...(versionV && { version: versionV }),
      ...(userV && { defaultUser: userV }),
    },
  });
  const submit = () => {
    if (problems.length > 0) return;
    create.mutate(ci, {
      onSuccess: () => void navigate({ to: "/projects/$project/custom-images/$name", params: { project, name }, replace: true }),
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
                  From <span className="font-mono">{disk || "no disk"}</span>
                  {chosen && ` · up to ${gib(chosen.status?.size || chosen.spec?.size)}`}
                </div>
                <p className="mt-2 text-[12px] leading-snug text-subtle">
                  While it is captured it counts at the disk's size toward the project's disk quota, then at the space its data takes.
                </p>
              </>
            }
            problems={problems}
            error={create.error}
            failed="Creating the image failed"
            pending={create.isPending}
            pendingLabel="Creating…"
            label={customImages.loading ? "Loading names…" : "Capture image"}
            cancelTo={`/projects/${project}/images`}
          />
          <FormManifest yaml={yaml} cli={[`hangar apply --create-only -p ${project} -f ${name || "image"}.yaml`]} />
        </>
      }
    >
      <FormSection n={1} title="Source disk" hint="Its machine must be stopped">
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

      <FormSection n={2} title="Before you capture" hint="The image holds the disk exactly as it is">
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

      <FormSection n={3} title="Describe it" hint="For people choosing an image">
        <div className="grid max-w-2xl grid-cols-1 gap-3 sm:grid-cols-3">
          <label className="block">
            <span className="text-[12.5px] text-muted">OS</span>
            <Input value={osV} onChange={(e) => setOs(e.target.value)} aria-label="OS" spellCheck={false} placeholder="ubuntu" />
          </label>
          <label className="block">
            <span className="text-[12.5px] text-muted">Version</span>
            <Input value={versionV} onChange={(e) => setVersion(e.target.value)} aria-label="Version" spellCheck={false} placeholder="24.04" />
          </label>
          <label className="block">
            <span className="text-[12.5px] text-muted">Default user</span>
            <Input value={userV} onChange={(e) => setUser(e.target.value)} aria-label="Default user" spellCheck={false} placeholder="ubuntu" />
          </label>
        </div>
        {from && os === null && <p className="mt-1.5 text-[12px] text-subtle">Filled in from {from.metadata?.name}, the image the disk was made from.</p>}
      </FormSection>

      <FormSection n={4} title="Image name">
        <div className="max-w-xl">
          <Input value={name} onChange={(e) => setName(e.target.value)} aria-label="Image name" spellCheck={false} placeholder="web-golden" />
        </div>
      </FormSection>
    </FormPage>
  );
}
