import { useNavigate } from "@tanstack/react-router";
import { Box, Check, Globe, MapPin, Server, Shuffle } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Notice } from "@/components/notice";
import { Topbar } from "@/components/shell";
import { Badge, Button, Card, CopyButton } from "@/components/ui";
import { cn } from "@/lib/cn";
import { currentProject, quota, sizeHints, sizes, tenant, vmImages, type SizeId } from "@/mock/data";

export type CreateKind = "container" | "vm";

function Section({ n, title, hint, children }: { n: number; title: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <section className="border-b py-6 last:border-0">
      <div className="mb-3 flex items-baseline gap-3">
        <span className="font-mono text-[12px] text-subtle">{String(n).padStart(2, "0")}</span>
        <h2 className="text-[15px] font-medium">{title}</h2>
        {hint && <span className="text-[12.5px] text-muted">{hint}</span>}
      </div>
      <div className="md:pl-8">{children}</div>
    </section>
  );
}

function Choice({
  selected,
  onClick,
  className,
  children,
}: {
  selected: boolean;
  onClick: () => void;
  className?: string;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "relative flex flex-col items-stretch justify-start rounded-lg border bg-surface text-left transition-colors",
        selected ? "border-accent bg-accent-soft ring-1 ring-accent" : "hover:border-border-strong",
        className,
      )}
    >
      {selected && (
        <span className="absolute right-2.5 top-2.5 flex size-4 items-center justify-center rounded-full bg-accent text-accent-fg">
          <Check className="size-2.5" strokeWidth={3} />
        </span>
      )}
      {children}
    </button>
  );
}

/** The compute limits a machine of this size would take the project past. */
function overQuota(size: SizeId) {
  const z = sizes[size];
  const need: Record<string, number> = { machines: 1, cpu: z.cpu, memory: z.memoryGi, disk: z.diskGi };
  return quota.compute
    .filter((q) => q.used + need[q.key] > q.limit)
    .map((q) => ({
      key: q.key,
      label: q.label,
      need: need[q.key],
      left: q.limit - q.used,
      limit: q.limit,
      unit: q.unit ? ` ${q.unit}` : "",
    }));
}

function suggestName(base: string) {
  return `${base}-${String(Math.floor(100000 + Math.random() * 900000))}`;
}

export function CreateMachine({ kind: initialKind, size: initialSize }: { kind: CreateKind; size: SizeId }) {
  const navigate = useNavigate();
  const [kind, setKind] = useState<CreateKind>(initialKind);
  const [image, setImage] = useState("ghcr.io/mochizuki/agent-runner:2026.10.16");
  const [vmImage, setVmImage] = useState(vmImages[0].id);
  const [size, setSize] = useState<SizeId>(initialSize);
  const [groups, setGroups] = useState<string[]>(["default"]);
  const [name, setName] = useState("agent-runner-2");

  const s = sizes[size];
  const over = overQuota(size);

  const yaml =
    kind === "container"
      ? `apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata:
  name: ${name}
  project: ${currentProject.name}
spec:
  image: ${image}
  resources: { cpu: "${s.cpu}", memory: ${s.memoryGi}Gi, disk: ${s.diskGi}Gi }`
      : `apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata:
  name: ${name}
  project: ${currentProject.name}
spec:
  resources: { cpu: "${s.cpu}", memory: ${s.memoryGi}Gi }
  rootDisk:
    fromImage: { image: { name: ${vmImage} }, size: ${s.diskGi}Gi }`;

  return (
    <>
      <Topbar crumbs={[tenant.name, currentProject.displayName, "New machine"]} />
      <div className="grid grid-cols-1 gap-8 px-4 md:px-8 pb-12 pt-6 xl:grid-cols-[minmax(0,1fr)_360px]">
        <div className="min-w-0">
          <h1 className="text-[22px] font-semibold tracking-tight">New machine</h1>
          <p className="mt-1 text-[13px] text-muted">
            Runs as a microVM in <span className="font-medium text-foreground">{currentProject.displayName}</span>.
          </p>

          <Section n={1} title="Kind">
            <div className="grid max-w-xl grid-cols-1 gap-3 sm:grid-cols-2">
              {(
                [
                  ["container", Box, "Container", "An OCI image booted as its own VM. Fast to start; exec and logs."],
                  ["vm", Server, "Virtual machine", "A full OS from a disk image, booted by firmware. SSH and cloud-init."],
                ] as const
              ).map(([id, Icon, label, text]) => (
                <Choice key={id} selected={kind === id} onClick={() => setKind(id)} className="p-4">
                  <Icon className="size-4 text-muted" strokeWidth={1.75} />
                  <div className="mt-2 text-[13.5px] font-medium">{label}</div>
                  <div className="mt-0.5 text-[12.5px] leading-snug text-muted">{text}</div>
                </Choice>
              ))}
            </div>
          </Section>

          <Section n={2} title="Image" hint={kind === "container" ? "Any OCI reference" : "Platform catalogue"}>
            {kind === "container" ? (
              <div className="max-w-xl">
                <input
                  value={image}
                  onChange={(e) => setImage(e.target.value)}
                  className="h-9 w-full rounded-md border bg-surface px-3 font-mono text-[12.5px] outline-none focus:border-accent"
                />
                <p className="mt-1.5 text-[12px] text-subtle">
                  Private images are pulled with a registry-auth secret of this project.
                </p>
              </div>
            ) : (
              <div className="grid max-w-xl grid-cols-2 gap-3 sm:grid-cols-4">
                {vmImages.map((img) => (
                  <Choice key={img.id} selected={vmImage === img.id} onClick={() => setVmImage(img.id)} className="p-3">
                    <div className="text-[13px] font-medium">{img.label}</div>
                    <div className="font-mono text-[11.5px] text-muted">{img.version}</div>
                  </Choice>
                ))}
              </div>
            )}
          </Section>

          <Section n={3} title="Size" hint={<Badge>placeholder sizes</Badge>}>
            <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
              {(Object.keys(sizes) as SizeId[]).map((id) => {
                const z = sizes[id];
                return (
                  <Choice
                    key={id}
                    selected={size === id}
                    onClick={() => setSize(id)}
                    className={cn(
                      "p-4",
                      overQuota(id).length > 0 &&
                        (size === id ? "border-fail ring-fail [&>span:first-child]:bg-fail" : "opacity-60"),
                    )}
                  >
                    <div className="font-mono text-[26px] font-semibold leading-none tracking-tight">{z.label}</div>
                    <div className="mt-4 space-y-1 font-mono text-[12.5px] tabular">
                      <div>
                        {z.cpu} <span className="text-muted">vCPU</span>
                      </div>
                      <div>
                        {z.memoryGi} <span className="text-muted">GiB memory</span>
                      </div>
                      <div>
                        {z.diskGi} <span className="text-muted">GiB disk</span>
                      </div>
                    </div>
                    <div className="mt-3 border-t pt-2.5 text-[12px] leading-snug text-muted">{sizeHints[id]}</div>
                    {overQuota(id).length > 0 && (
                      <div className="mt-2 text-[11.5px] font-medium text-fail">Over quota</div>
                    )}
                  </Choice>
                );
              })}
            </div>
          </Section>

          <Section n={4} title="Network">
            <div className="max-w-xl space-y-3">
              <div className="flex items-center justify-between rounded-lg border bg-surface px-4 py-3">
                <div>
                  <div className="text-[13px] font-medium">default</div>
                  <div className="font-mono text-[11.5px] text-muted">10.128.0.0/20</div>
                </div>
                <Button size="sm" variant="ghost">
                  Change
                </Button>
              </div>
              <div className="rounded-lg border bg-surface px-4 py-3">
                <div className="text-[13px] font-medium">Security groups</div>
                <div className="mt-2 flex flex-wrap gap-1.5">
                  {["default", "ssh", "web", "agent-egress"].map((g) => {
                    const on = groups.includes(g);
                    return (
                      <button
                        key={g}
                        type="button"
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
              <p className="flex items-start gap-2 text-[12px] text-muted">
                <Globe className="mt-0.5 size-3.5 shrink-0" strokeWidth={1.75} />
                Every interface gets a public IPv6 /128. Inbound traffic from the internet is denied unless one of its
                security groups allows it.
              </p>
            </div>
          </Section>

          <Section n={5} title="Name" hint="Unique in the project; never reused">
            <div className="flex max-w-xl gap-2">
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                className="h-9 min-w-0 flex-1 rounded-md border bg-surface px-3 font-mono text-[12.5px] outline-none focus:border-accent"
              />
              <Button onClick={() => setName(suggestName(kind === "container" ? "container" : "vm"))}>
                <Shuffle /> Suggest
              </Button>
            </div>
          </Section>
        </div>

        <aside className="xl:pt-14">
          <div className="sticky top-20 space-y-4">
            <Card>
              <div className="border-b px-4 py-3">
                <div className="text-[11.5px] text-subtle">Summary</div>
                <div className="mt-1 flex items-baseline gap-2">
                  <span className="font-mono text-[20px] font-semibold">{s.label}</span>
                  <span className="text-[13px] text-muted tabular">
                    {s.cpu} vCPU · {s.memoryGi} GiB · {s.diskGi} GiB
                  </span>
                </div>
                <div className="mt-0.5 truncate font-mono text-[12px] text-muted">
                  {kind === "container" ? image : vmImage}
                </div>
              </div>
              <div className="flex items-start gap-2.5 px-4 py-3 text-[12.5px]">
                <MapPin className="mt-0.5 size-3.5 shrink-0 text-muted" />
                <span className="text-muted">
                  Placed by the scheduler in <span className="text-foreground">Singapore</span>, on the host with the
                  most free memory. You see which one once it is placed.
                </span>
              </div>
              <div className="space-y-3 border-t px-4 py-3">
                {over.length > 0 && (
                  <Notice tone="error" title={`Over the project's ${over.map((o) => o.label.toLowerCase()).join(" and ")} quota`}>
                    {over.map((o) => (
                      <div key={o.key}>
                        {s.label} needs {o.need}
                        {o.unit}; {o.left}
                        {o.unit} of {o.limit}
                        {o.unit} is left.
                      </div>
                    ))}
                  </Notice>
                )}
                <Button
                  variant="primary"
                  className="w-full"
                  disabled={over.length > 0}
                  onClick={() => navigate({ to: "/containers" })}
                >
                  Create {kind === "container" ? "container" : "virtual machine"}
                </Button>
              </div>
            </Card>
            <Card className="overflow-hidden">
              <div className="flex h-10 items-center justify-between border-b px-4">
                <span className="text-[12px] font-medium">Equivalent manifest</span>
                <CopyButton text={yaml} />
              </div>
              <pre className="overflow-x-auto px-4 py-3 font-mono text-[11.5px] leading-5 text-muted">{yaml}</pre>
              <div className="border-t px-4 py-2.5 font-mono text-[11.5px]">
                <span className="select-none text-accent">$ </span>hangar apply -f {name}.yaml
              </div>
            </Card>
          </div>
        </aside>
      </div>
    </>
  );
}
