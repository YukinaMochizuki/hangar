// Creating a machine without replacing one. The form sends nothing until the
// project's machines of the kind are known and the name is not among them,
// and sends the apply create-only, so the API refuses a name taken since.

import { Code, ConnectError } from "@connectrpc/connect";

const dnsLabel = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

const taken = (noun: string, name: string) => `A ${noun} named ${name} exists in this project.`;

/** What the form knows of the project's machines of the kind. */
export type KnownNames = { loading: boolean; error: unknown; names: string[] };

export type NameCheck = { ok: true } | { ok: false; reason: string };

/** Whether name may be sent: a DNS label, and known not to be taken. */
export function checkName(name: string, noun: string, known: KnownNames): NameCheck {
  if (known.error) return { ok: false, reason: `The list of the project's ${noun}s did not load, so which names are taken is not known.` };
  if (known.loading) return { ok: false, reason: `The project's ${noun}s are still loading.` };
  if (!name) return { ok: false, reason: "The name is empty." };
  if (!dnsLabel.test(name)) {
    return { ok: false, reason: "The name is not a DNS label: lowercase letters, digits and “-”, at most 63." };
  }
  if (known.names.includes(name)) return { ok: false, reason: taken(noun, name) };
  return { ok: true };
}

/**
 * The create's error as the form says it: a refusal for a taken name in the
 * form's words, the same as checkName's; any other error as the API gave it.
 */
export function createError(err: unknown, noun: string, name: string): unknown {
  if (err instanceof ConnectError && err.code === Code.AlreadyExists) return new ConnectError(taken(noun, name), Code.AlreadyExists);
  return err;
}
