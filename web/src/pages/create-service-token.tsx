// A new service token, with its first secret and a role bound in the
// project when one is chosen, in one CreateServiceToken, which refuses a
// name that exists. The secret it returns is shown once, in SecretOnce,
// held in this page's state alone; closing it opens the token
// (docs/decisions/authentication.md).

import { Link, useNavigate } from "@tanstack/react-router";
import { useState, type ReactNode } from "react";
import { FormField, Input } from "@/components/form";
import { CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { useProjectName } from "@/components/shell";
import { checkName } from "@/lib/create";
import { commands, durations, perms, useIssueToken, useServiceTokens } from "@/lib/credentials";
import { roleLabel, roles } from "@/lib/members";
import { useCan } from "@/lib/permissions";
import { DurationSelect, SecretOnce } from "@/pages/service-tokens";

/** CreateServiceToken is the New service token page. */
export function CreateServiceToken() {
  const project = useProjectName();
  const perm = useCan({ level: "project", name: project }, [perms.tokenApply]);
  const crumbs = [
    <Link key="list" to="/projects/$project/service-tokens" params={{ project }} className="hover:text-foreground">
      Service tokens
    </Link>,
    "New service token",
  ];
  return (
    <CreateGate crumbs={crumbs} title="New service token" perm={perm} may={perm.can(perms.tokenApply)} refusal="creating service tokens">
      <TokenForm crumbs={crumbs} project={project} />
    </CreateGate>
  );
}

function TokenForm({ crumbs, project }: { crumbs: ReactNode[]; project: string }) {
  const navigate = useNavigate();
  const tokens = useServiceTokens(project);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [role, setRole] = useState("");
  const [duration, setDuration] = useState("");
  const [secret, setSecret] = useState<string | null>(null);
  const issue = useIssueToken(project, setSecret);
  const nameCheck = checkName(name, "service token", {
    loading: tokens.loading,
    error: tokens.error,
    names: tokens.items.map((t) => t.metadata?.name ?? ""),
  });
  const send = () => {
    if (nameCheck.ok) issue.mutate({ name, description, role, duration });
  };
  const close = () => {
    setSecret(null);
    issue.reset();
    void navigate({ to: "/projects/$project/service-tokens/$name", params: { project, name }, replace: true });
  };
  return (
    <>
      <FormPage
        crumbs={crumbs}
        title="New service token"
        onSubmit={send}
        side={
          <>
            <FormSummary
              summary={
                <>
                  <div className="text-[15px] font-medium">{role ? roleLabel(role) : "No role"}</div>
                  <div className="mt-0.5 text-[12px] text-muted">Secret works for {durations.find((d) => d.id === duration)?.label.toLowerCase()}</div>
                </>
              }
              problems={nameCheck.ok ? [] : [nameCheck.reason]}
              error={issue.error}
              failed="Creating the token failed"
              pending={issue.isPending}
              pendingLabel="Creating…"
              label={tokens.loading ? "Loading names…" : "Create token"}
              cancelTo={`/projects/${project}/service-tokens`}
            />
            <FormManifest cli={[commands.createToken(project, nameCheck.ok ? name : "", { role, duration, description })]} />
          </>
        }
      >
        <FormSection n={1} title="Role in this project">
          <fieldset className="max-w-xl">
            <legend className="sr-only">Role in this project</legend>
            <div className="divide-y rounded-md border bg-surface">
              {[{ id: "", label: "None", holds: "Roles can be bound later." }, ...roles.project].map((r) => (
                <label key={r.id} className="flex cursor-pointer gap-2.5 px-3 py-2 hover:bg-surface-2/60">
                  <input type="radio" name="role" value={r.id} checked={role === r.id} onChange={() => setRole(r.id)} className="mt-0.5 accent-[var(--accent)]" />
                  <span className="min-w-0">
                    <span className="flex items-baseline gap-2 text-[13px] text-foreground">
                      {r.label}
                      {r.id && <span className="font-mono text-[11px] text-subtle">{r.id}</span>}
                    </span>
                    <span className="block text-[12px] text-muted">{r.holds}</span>
                  </span>
                </label>
              ))}
            </div>
          </fieldset>
        </FormSection>
        <FormSection n={2} title="Secret">
          <div className="max-w-xl">
            <DurationSelect value={duration} onChange={setDuration} />
          </div>
        </FormSection>
        <FormSection n={3} title="Token name">
          <div className="max-w-xl">
            <Input value={name} onChange={(e) => setName(e.target.value)} aria-label="Token name" spellCheck={false} placeholder="ci" />
            <p className="mt-1.5 text-[12px] text-subtle">It authenticates as serviceToken:{project}/{nameCheck.ok ? name : "NAME"}.</p>
            <FormField label="Description">
              <Input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Deploys from the main branch" className="font-sans" />
            </FormField>
          </div>
        </FormSection>
      </FormPage>
      {secret !== null && <SecretOnce title={`Token ${name} created`} secret={secret} onClose={close} />}
    </>
  );
}
