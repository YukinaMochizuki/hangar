// A new disk: a boot disk, a copy of an image (the platform's, or one of the
// project's custom images), or a blank data disk. The
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
import { customImageFloorGiB, customImageState, useCustomImages } from "@/lib/custom-images";
import { imageFloorGiB, imageState, newDisk, useCreateDisk, useDisks, useImages, type DiskUse, type ImageChoice } from "@/lib/storage";

const applyPerm = "storage.disks.apply";

// The size until one is typed, raised to a boot disk's image's smallest disk.
const defaultSizeGi = 20;

const types: { id: DiskUse; icon: typeof Power; label: string; text: string }[] = [
  { id: "boot", icon: Power, label: "Boot disk", text: "A copy of an image." },
  { id: "data", icon: HardDrive, label: "Data disk", text: "Blank." },
];

/** One image the form offers: what it is called, its kind, and the smallest disk it fits on. */
type Offer = ImageChoice & { os: string; floor: number };

const sameImage = (a: ImageChoice | undefined, b: ImageChoice | undefined) => !!a && !!b && a.name === b.name && a.custom === b.custom;

/** ImageGroup is one group of the Image section's cards: the project's own images, or the platform's. */
function ImageGroup({ title, offers, chosen, onChoose }: { title?: string; offers: Offer[]; chosen?: ImageChoice; onChoose: (c: ImageChoice) => void }) {
  return (
    <div>
      {title && <div className="mb-2 text-[12px] font-medium text-muted">{title}</div>}
      <div role="radiogroup" aria-label={title ?? "Image"} className="grid grid-cols-2 gap-3 sm:grid-cols-3">
        {offers.map((o) => (
          <Choice key={o.name} selected={sameImage(o, chosen)} onClick={() => onChoose({ name: o.name, custom: o.custom })} label={o.name} className="p-3">
            <div className="truncate pr-5 font-mono text-[12.5px] font-medium">{o.name}</div>
            <div className="mt-0.5 text-[12px] capitalize text-muted">{o.os || "–"}</div>
            <div className="mt-1.5 text-[11.5px] text-subtle tabular">{o.floor} GiB or more</div>
          </Choice>
        ))}
      </div>
    </div>
  );
}

/** CreateDisk is the New disk page; image opens it on a boot disk of that image. */
export function CreateDisk({ image }: { image?: ImageChoice }) {
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

function DiskForm({ crumbs, project, initialImage }: { crumbs: ReactNode[]; project: string; initialImage?: ImageChoice }) {
  const navigate = useNavigate();
  const [use, setUse] = useState<DiskUse>(initialImage ? "boot" : "data");
  const [image, setImage] = useState<ImageChoice | undefined>(initialImage);
  // The size as typed; null until edited, while it follows the type and image.
  const [sizeText, setSizeText] = useState<string | null>(null);
  const [name, setName] = useState("");
  const images = useImages();
  const customImages = useCustomImages(project);
  const disks = useDisks(project);
  const create = useCreateDisk();

  // The project's own Ready images first, as a person who made one most likely wants it.
  const customOffers: Offer[] = customImages.items
    .filter((c) => customImageState(c).tone === "ok")
    .map((c) => ({ name: c.metadata?.name ?? "", custom: true, os: [c.spec?.os, c.spec?.version].filter(Boolean).join(" "), floor: customImageFloorGiB(c) }));
  const platformOffers: Offer[] = images.items
    .filter((i) => imageState(i).tone !== "fail")
    .map((i) => ({ name: i.metadata?.name ?? "", custom: false, os: [i.spec?.os, i.spec?.version].filter(Boolean).join(" "), floor: imageFloorGiB(i) }));
  const usable = [...customOffers, ...platformOffers];
  const chosenRef: ImageChoice | undefined = use === "boot" ? (image ?? usable[0]) : undefined;
  const offer = usable.find((o) => sameImage(o, chosenRef));
  const chosen = chosenRef?.name ?? "";
  const floor = use === "boot" ? (offer?.floor ?? 1) : 1;
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
    if (images.loading || customImages.loading) problems.push("The images are loading.");
    else if (!chosen) problems.push("No image is chosen.");
    else if (!offer) problems.push(`${chosen} is not a usable image.`);
  }
  if (sizeProblem) problems.push(sizeProblem);

  // A whole number as the browser reads it ("1e1" is 10) goes out as its digits: the API takes no exponent.
  const sizeSent = Number.isInteger(size) ? String(size) : sizeTyped;
  const disk = newDisk(project, name, use, sizeSent, chosenRef);
  const yaml = diskYaml(newDisk(project, name || "NAME", use, sizeSent, chosenRef));
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
                      {chosenRef?.custom ? "Custom image" : "Image"} <span className="font-mono">{chosen || "none"}</span>
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
          {images.loading || customImages.loading ? (
            <div className="grid max-w-xl grid-cols-2 gap-3 sm:grid-cols-3">
              {[0, 1].map((i) => (
                <Skeleton key={i} className="h-[72px] rounded-lg" />
              ))}
            </div>
          ) : images.error ? (
            <ErrorNotice error={images.error} title="The images did not load" onRetry={images.refetch} className="max-w-xl p-0" />
          ) : usable.length === 0 ? (
            <p className="text-[13px] text-muted">There is no usable image.</p>
          ) : (
            <div className="max-w-xl space-y-4">
              {customOffers.length > 0 && <ImageGroup title="This project's images" offers={customOffers} chosen={chosenRef} onChoose={setImage} />}
              <ImageGroup title={customOffers.length > 0 ? "Platform images" : undefined} offers={platformOffers} chosen={chosenRef} onChoose={setImage} />
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
