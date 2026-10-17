// Creating a machine without replacing one. The API's apply creates or
// replaces, and offers callers no create-only form, so the console guards
// twice: the form sends nothing until the project's machines of the kind are
// known and the name is not among them, and the write itself asks for the
// object first and applies only when it is not there.

import { Code, ConnectError } from "@connectrpc/connect";

const dnsLabel = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

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
  if (known.names.includes(name)) return { ok: false, reason: `A ${noun} named ${name} exists in this project.` };
  return { ok: true };
}

/** The write's two calls, for one kind. */
export type CreateCalls = {
  get: () => Promise<unknown>;
  apply: () => Promise<{ created: boolean }>;
};

/**
 * Applies only when a Get finds no object of the name, and says so when the
 * apply still replaced one, written by someone else in between.
 */
export async function createIfAbsent(calls: CreateCalls, noun: string, name: string): Promise<void> {
  try {
    await calls.get();
    throw new ConnectError(`A ${noun} named ${name} exists in this project.`, Code.AlreadyExists);
  } catch (err) {
    if (!(err instanceof ConnectError) || err.code !== Code.NotFound) throw err;
  }
  const res = await calls.apply();
  if (!res.created) {
    throw new ConnectError(`A ${noun} named ${name} was made by someone else just before, and this apply replaced it.`, Code.AlreadyExists);
  }
}
