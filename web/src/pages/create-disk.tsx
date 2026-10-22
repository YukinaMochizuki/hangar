// A new disk: a boot disk, a copy of an image, or a blank data disk. The
// type is written as the disk's hangar/intended-use label, which records
// intent and restricts nothing (docs/decisions/console-design.md). Nothing
// is sent until the project's disks are known and the name is not among
// them, and the one apply is create-only (lib/create.ts).

import { Link, useNavigate } from "@tanstack/react-router";
import { HardDrive, Power } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Input } from "@/components/form";
import { Choice, CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { useProjectName } from "@/components/shell";
import { diskYaml } from "@/components/storage";
import { ErrorNotice, Skeleton } from "@/components/states";
import { cn } from "@/lib/cn";
import { checkName } from "@/lib/create";
import { useCan } from "@/lib/permissions";
import { imageFloorGiB, imageState, newDisk, useCreateDisk, useDisks, useImages, type DiskUse } from "@/lib/storage";

const applyPerm = "storage.disks.apply";

// The size until one is typed, raised to a boot disk's image's smallest disk.
const defaultSizeGi = 20;

const types: { id: DiskUse; icon: typeof Power; label: string; text: string }[] = [
  { id: "boot", icon: Power, label: "Boot disk", text: "A copy of an image." },
  { id: "data", icon: HardDrive, label: "Data disk", text: "Blank." },
];

/** CreateDisk is the New disk page; image opens it on a boot disk of that image. */
export function CreateDisk({ image }: { image?: string }) {
  const project = useProjectName();
  const perm = useCan({ level: "project", name: project }, [applyPerm]);
  const crumbs = [
    <Link key="list" to="/projects/$project/disks" params={{ project }} className="hover:text-foreground">
      Disks
    </Link>,
    "New disk",
  ];
  return (
    <CreateGate crumbs={crumbs} title="New disk" perm={perm} may={perm.can(applyPerm)} refusal="creating disks">
      <DiskForm crumbs={crumbs} project={project} initialImage={image} />
    </CreateGate>
  );
}

function DiskForm({ crumbs, project, initialImage }: { crumbs: ReactNode[]; project: string; initialImage?: string }) {
  const navigate = useNavigate();
  const [use, setUse] = useState<DiskUse>(initialImage ? "boot" : "data");
  const [image, setImage] = useState(initialImage ?? "");
  // The size as typed; null until edited, while it follows the type and image.
  const [sizeText, setSizeText] = useState<string | null>(null);
  const [name, setName] = useState("");
  const images = useImages();
  const disks = useDisks(project);
  const create = useCreateDisk();

  const usable = images.items.filter((i) => imageState(i).tone !== "fail");
  const chosen = use === "boot" ? image || usable[0]?.metadata?.name || "" : "";
  const img = images.items.find((i) => i.metadata?.name === chosen);
  const floor = use === "boot" ? imageFloorGiB(img) : 1;
  const sizeTyped = (sizeText ?? String(Math.max(defaultSizeGi, floor))).trim();
  const size = sizeTyped === "" ? NaN : Number(sizeTyped);
  const sizeProblem =
    !Number.isInteger(size) || size < 1
      ? "The size is not a whole number of GiB."
      : chosen && size < floor
        ? `${chosen} needs ${floor} GiB; ${size} GiB is less.`
        : undefined;

  const nameCheck = checkName(name, "disk", {
    loading: disks.loading,
    error: disks.error,
    names: disks.items.map((d) => d.metadata?.name ?? ""),
  });
  const problems: string[] = [];
  if (!nameCheck.ok) problems.push(nameCheck.reason);
  if (use === "boot") {
    if (images.loading) problems.push("The images are loading.");
    else if (!chosen) problems.push("No image is chosen.");
    else if (!usable.some((i) => i.metadata?.name === chosen)) problems.push(`${chosen} is not a usable image.`);
  }
  if (sizeProblem) problems.push(sizeProblem);

  // A whole number as the browser reads it ("1e1" is 10) goes out as its digits: the API takes no exponent.
  const sizeSent = Number.isInteger(size) ? String(size) : sizeTyped;
  const disk = newDisk(project, name, use, sizeSent, chosen);
  const yaml = diskYaml(newDisk(project, name || "NAME", use, sizeSent, chosen));
  const submit = () => {
    if (problems.length > 0) return;
    create.mutate(disk, {
      onSuccess: () => void navigate({ to: "/projects/$project/disks/$name", params: { project, name }, replace: true }),
    });
  };

  // The Image section is a boot disk's alone; the sections after it follow it.
  const after = use === "boot" ? 1 : 0;
  return (
    <FormPage
      crumbs={crumbs}
      title="New disk"
      onSubmit={submit}
      side={
        <>
          <FormSummary
            summary={
              <>
                <div className="flex items-baseline gap-2">
                  <span className="text-[15px] font-medium">{use === "boot" ? "Boot disk" : "Data disk"}</span>
                  <span className="text-[13px] text-muted tabular">{sizeTyped || "–"} GiB</span>
                </div>
                <div className="mt-0.5 truncate text-[12px] text-muted">
                  {use === "boot" ? (
                    <>
                      Image <span className="font-mono">{chosen || "none"}</span>
                    </>
                  ) : (
                    "Blank"
                  )}
                </div>
              </>
            }
            problems={problems}
            error={create.error}
            failed="Creating the disk failed"
            pending={create.isPending}
            pendingLabel="Creating…"
            label={disks.loading ? "Loading names…" : "Create disk"}
            cancelTo={`/projects/${project}/disks`}
          />
          <FormManifest yaml={yaml} cli={[`hangar apply --create-only -p ${project} -f ${name || "disk"}.yaml`]} />
        </>
      }
    >
      <FormSection n={1} title="Type">
        <div role="radiogroup" aria-label="Type" className="grid max-w-xl grid-cols-1 gap-3 sm:grid-cols-2">
          {types.map((t) => (
            <Choice key={t.id} selected={use === t.id} onClick={() => setUse(t.id)} label={t.label}>
              <t.icon className="size-4 text-muted" strokeWidth={1.75} />
              <div className="mt-2 text-[13.5px] font-medium">{t.label}</div>
              <div className="mt-0.5 text-[12.5px] leading-snug text-muted">{t.text}</div>
            </Choice>
          ))}
        </div>
      </FormSection>

      {use === "boot" && (
        <FormSection n={2} title="Image">
          {images.loading ? (
            <div className="grid max-w-xl grid-cols-2 gap-3 sm:grid-cols-3">
              {[0, 1].map((i) => (
                <Skeleton key={i} className="h-[72px] rounded-lg" />
              ))}
            </div>
          ) : images.error ? (
            <ErrorNotice error={images.error} title="The images did not load" onRetry={images.refetch} className="max-w-xl p-0" />
          ) : usable.length === 0 ? (
            <p className="text-[13px] text-muted">The platform's catalogue holds no usable image.</p>
          ) : (
            <div role="radiogroup" aria-label="Image" className="grid max-w-xl grid-cols-2 gap-3 sm:grid-cols-3">
              {usable.map((i) => {
                const id = i.metadata?.name ?? "";
                return (
                  <Choice key={id} selected={chosen === id} onClick={() => setImage(id)} label={id} className="p-3">
                    <div className="truncate pr-5 font-mono text-[12.5px] font-medium">{id}</div>
                    <div className="mt-0.5 text-[12px] capitalize text-muted">{[i.spec?.os, i.spec?.version].filter(Boolean).join(" ")}</div>
                    <div className="mt-1.5 text-[11.5px] text-subtle tabular">{imageFloorGiB(i)} GiB or more</div>
                  </Choice>
                );
              })}
            </div>
          )}
        </FormSection>
      )}

      <FormSection n={2 + after} title="Size" hint={chosen ? `At least ${floor} GiB` : undefined}>
        <div className="max-w-xl">
          <div className="flex items-center gap-2">
            <Input
              type="number"
              inputMode="numeric"
              min={floor}
              step={1}
              value={sizeText ?? sizeTyped}
              onChange={(e) => setSizeText(e.target.value)}
              aria-label="Size, GiB"
              aria-invalid={!!sizeProblem}
              className={cn("w-32 tabular", sizeProblem && "border-fail focus:border-fail")}
            />
            <span className="text-[13px] text-muted">GiB</span>
          </div>
          {sizeProblem && <p className="mt-1.5 text-[12px] text-fail">{sizeProblem}</p>}
          <p className="mt-1.5 text-[12px] text-subtle">Can grow later, never shrink.</p>
        </div>
      </FormSection>

      <FormSection n={3 + after} title="Disk name">
        <div className="max-w-xl">
          <Input value={name} onChange={(e) => setName(e.target.value)} aria-label="Disk name" spellCheck={false} placeholder="data" />
        </div>
      </FormSection>
    </FormPage>
  );
}
