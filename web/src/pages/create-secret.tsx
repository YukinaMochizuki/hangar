// A new secret: a registry login, sent in one create-only ApplySecret.
// Values are write-only (docs/decisions/secrets.md): they live in this
// page's state, and in a ref from its submit until the call reads it;
// leaving the page empties the ref and drops the state. Its command names
// a file, never a value, so the page shows no manifest.

import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Input } from "@/components/form";
import { CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { SecretValuesFields, useSecretValuesForm } from "@/components/secret-values";
import { useProjectName } from "@/components/shell";
import { checkName } from "@/lib/create";
import { commands, perms, useCreateSecret, useSecrets, type SecretValues } from "@/lib/credentials";
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
  const form = useSecretValuesForm();
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
  problems.push(...form.problems);
  const send = () => {
    if (problems.length > 0) return;
    valuesRef.current = form.values();
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
      onSubmit={send}
      side={
        <>
          <FormSummary
            summary={
              <>
                <div className="text-[15px] font-medium">Registry login</div>
                <div className="mt-0.5 truncate text-[12px] text-muted">
                  {form.mode === "login" ? <span className="font-mono">{form.registry || "no registry"}</span> : "From a Docker config"}
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
          <SecretValuesFields form={form} />
        </div>
      </FormSection>
      <FormSection n={2} title="Secret name">
        <div className="max-w-xl">
          <Input value={name} onChange={(e) => setName(e.target.value)} aria-label="Secret name" spellCheck={false} placeholder="registry-login" />
        </div>
      </FormSection>
    </FormPage>
  );
}
