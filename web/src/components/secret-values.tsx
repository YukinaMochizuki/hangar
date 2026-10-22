// A registry-auth secret's value as a form takes it, the same on New secret
// and in Replace value: a registry login, or a Docker config as docker login
// writes it. The values live in the form's state alone
// (docs/decisions/secrets.md); the form hands them to its call through a ref.

import { useState } from "react";
import { FormField, Input, Textarea } from "@/components/form";
import { cn } from "@/lib/cn";
import { type SecretValues } from "@/lib/credentials";

/** The value fields' state, what stops it being sent, and the values it makes. */
export function useSecretValuesForm() {
  const [mode, setMode] = useState<"login" | "config">("login");
  const [registry, setRegistry] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [config, setConfig] = useState("");
  const problems: string[] = [];
  if (mode === "login" && !(registry && username && password)) problems.push("The registry, username and password are not all given.");
  if (mode === "config" && !config.trim()) problems.push("The Docker config is empty.");
  const values = (): SecretValues => (mode === "login" ? { mode, registry, username, password } : { mode, config });
  return { mode, setMode, registry, setRegistry, username, setUsername, password, setPassword, config, setConfig, problems, values };
}

export type SecretValuesForm = ReturnType<typeof useSecretValuesForm>;

/** The value's fields: which form it is given in, then the login or the config. */
export function SecretValuesFields({ form }: { form: SecretValuesForm }) {
  return (
    <>
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
            aria-checked={form.mode === id}
            onClick={() => form.setMode(id)}
            className={cn(
              "flex h-full items-center rounded px-2.5 text-[12.5px]",
              form.mode === id ? "bg-surface-2 font-medium text-foreground" : "text-muted hover:text-foreground",
            )}
          >
            {label}
          </button>
        ))}
      </div>
      {form.mode === "login" ? (
        <div className="mt-4">
          <FormField label="Registry" hint="Docker Hub is index.docker.io.">
            <Input value={form.registry} onChange={(e) => form.setRegistry(e.target.value)} spellCheck={false} placeholder="registry.example.com" />
          </FormField>
          <FormField label="Username">
            <Input value={form.username} onChange={(e) => form.setUsername(e.target.value)} spellCheck={false} />
          </FormField>
          <FormField label="Password or access token">
            {/* Not a password for this site: "off" asks the browser neither to suggest a generated one nor to offer saving it. */}
            <Input type="password" value={form.password} onChange={(e) => form.setPassword(e.target.value)} autoComplete="off" />
          </FormField>
        </div>
      ) : (
        <FormField label="Docker config" hint="~/.docker/config.json, as docker login writes it." className="mt-4">
          <Textarea
            value={form.config}
            onChange={(e) => form.setConfig(e.target.value)}
            spellCheck={false}
            rows={5}
            placeholder={'{"auths": {"registry.example.com": {"auth": "…"}}}'}
            className="text-[12px]"
          />
        </FormField>
      )}
    </>
  );
}
