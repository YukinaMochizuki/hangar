// Which machines the console has just created, so that the page the create
// form opens shows the machine's arrival on its host once, as it would have
// had the person watched it: scheduling is usually done before that page
// opens. The marks live in memory only: a reload, or coming back to the page
// later, plays nothing.

import { useEffect, useState } from "react";

const marks = new Set<string>();

export function createdKey(project: string, kind: "Container" | "VirtualMachine", name: string): string {
  return `${project}/${kind}/${name}`;
}

/** Marks a machine as just created by this console. */
export function markJustCreated(key: string) {
  marks.add(key);
}

/** Whether the machine was just created and its page has not yet taken the mark. */
export function isJustCreated(key: string): boolean {
  return marks.has(key);
}

/** Takes the mark, so the arrival plays on the first visit alone. */
export function clearJustCreated(key: string) {
  marks.delete(key);
}

/**
 * Whether the machine's page is the first one opened since the console
 * created it: read once when the page mounts, and the mark taken after.
 */
export function useJustCreated(key: string): boolean {
  const [fresh] = useState(() => isJustCreated(key));
  useEffect(() => clearJustCreated(key), [key]);
  return fresh;
}
