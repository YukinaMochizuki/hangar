// A new secret: a registry login, sent in one create-only ApplySecret.
// Values are write-only (docs/decisions/secrets.md): they live in this
// page's state, and in a ref from its submit until the call reads it;
// leaving the page empties the ref and drops the state. Its command names
// a file, never a value, so the page shows no manifest.

import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { FormField, Input, Textarea } from "@/components/form";
import { CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { useProjectName } from "@/components/shell";
import { cn } from "@/lib/cn";
import { checkName } from "@/lib/create";
import { commands, dockerConfigKey, perms, useCreateSecret, useSecrets, type SecretValues } from "@/lib/credentials";
import { useCan } from "@/lib/permissions";

/** CreateSecret is the New secret page. */
export function CreateSecret() {
  const project = useProjectName();
  const perm = useCan({ level: "project", name: project }, [perms.secretApply]);
  const crumbs = [
    <Link key="list" to="/projects/$project/secrets" params={{ project }} className="hover:text-foreground">
      Secrets
    </Link>,
    "New secret",
  ];
  return (
    <CreateGate crumbs={crumbs} title="New secret" perm={perm} may={perm.can(perms.secretApply)} refusal="creating secrets">
      <SecretForm crumbs={crumbs} project={project} />
    </CreateGate>
  );
}

function SecretForm({ crumbs, project }: { crumbs: ReactNode[]; project: string }) {
  const navigate = useNavigate();
  const secrets = useSecrets(project);
  const [name, setName] = useState("");
  const [mode, setMode] = useState<"login" | "config">("login");
  const [registry, setRegistry] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [config, setConfig] = useState("");
  const valuesRef = useRef<SecretValues | null>(null);
  const create = useCreateSecret(project, valuesRef);
  useEffect(() => {
    const ref = valuesRef;
    return () => {
      ref.current = null;
    };
  }, []);
  const nameCheck = checkName(name, "secret", {
    loading: secrets.loading,
    error: secrets.error,
    names: secrets.items.map((s) => s.metadata?.name ?? ""),
  });
  const problems: string[] = [];
  if (!nameCheck.ok) problems.push(nameCheck.reason);
  if (mode === "login" && !(registry && username && password)) problems.push("The registry, username and password are not all given.");
  if (mode === "config" && !config.trim()) problems.push("The Docker config is empty.");
  const send = () => {
    if (problems.length > 0) return;
    valuesRef.current = mode === "login" ? { mode, registry, username, password } : { mode, config };
    create.mutate(name, {
      onSuccess: () => {
        valuesRef.current = null;
        void navigate({ to: "/projects/$project/secrets/$name", params: { project, name }, replace: true });
      },
    });
  };
  return (
    <FormPage
      crumbs={crumbs}
      title="New secret"
      description="A registry login the platform pulls this project's images with. Its values are write-only: once created, no page and no answer of the API holds them."
      onSubmit={send}
      side={
        <>
          <FormSummary
            summary={
              <>
                <div className="text-[15px] font-medium">Registry login</div>
                <div className="mt-0.5 truncate text-[12px] text-muted">
                  {mode === "login" ? <span className="font-mono">{registry || "no registry"}</span> : "From a Docker config"}
                </div>
              </>
            }
            problems={problems}
            error={create.error}
            failed="Creating the secret failed"
            pending={create.isPending}
            pendingLabel="Creating…"
            label={secrets.loading ? "Loading names…" : "Create secret"}
            cancelTo={`/projects/${project}/secrets`}
          />
          <FormManifest cli={[commands.createSecret(project, nameCheck.ok ? name : "")]} />
        </>
      }
    >
      <FormSection n={1} title="Registry login" hint="Write-only">
        <div className="max-w-xl">
          <div role="radiogroup" aria-label="Values from" className="flex h-8 w-fit items-center rounded-md border bg-surface p-0.5">
            {(
              [
                ["login", "Registry and password"],
                ["config", "Docker config"],
              ] as const
            ).map(([id, label]) => (
              <button
                key={id}
                type="button"
                role="radio"
                aria-checked={mode === id}
                onClick={() => setMode(id)}
                className={cn("flex h-full items-center rounded px-2.5 text-[12.5px]", mode === id ? "bg-surface-2 font-medium text-foreground" : "text-muted hover:text-foreground")}
              >
                {label}
              </button>
            ))}
          </div>
          {mode === "login" ? (
            <div className="mt-4">
              <FormField label="Registry" hint="Its host. Docker Hub is index.docker.io.">
                <Input value={registry} onChange={(e) => setRegistry(e.target.value)} spellCheck={false} placeholder="registry.example.com" />
              </FormField>
              <FormField label="Username">
                <Input value={username} onChange={(e) => setUsername(e.target.value)} spellCheck={false} />
              </FormField>
              <FormField label="Password or access token">
                {/* Not a password for this site: "off" asks the browser neither to suggest a generated one nor to offer saving it. */}
                <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="off" />
              </FormField>
            </div>
          ) : (
            <FormField label="Docker config" hint={`As docker login writes it, ~/.docker/config.json. It is stored as ${dockerConfigKey}.`} className="mt-4">
              <Textarea
                value={config}
                onChange={(e) => setConfig(e.target.value)}
                spellCheck={false}
                rows={5}
                placeholder={'{"auths": {"registry.example.com": {"auth": "…"}}}'}
                className="text-[12px]"
              />
            </FormField>
          )}
        </div>
      </FormSection>
      <FormSection n={2} title="Secret name" hint="Unique among this project's secrets">
        <div className="max-w-xl">
          <Input value={name} onChange={(e) => setName(e.target.value)} aria-label="Secret name" spellCheck={false} placeholder="registry-login" />
        </div>
      </FormSection>
    </FormPage>
  );
}
