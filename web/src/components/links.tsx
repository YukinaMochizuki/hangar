// Links to a project's objects by name, in the monospace face, and the line
// that names the machine an object points at.

import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { useProjectName } from "@/components/shell";
import { cn } from "@/lib/cn";
import { machinePath, type MachineKind } from "@/lib/machines";

/** kindWord is a machine kind as a sentence says it. */
export const kindWord = (k: MachineKind) => (k === "Container" ? "container" : "VM");

const objectLink = "font-mono text-[12.5px] text-accent hover:underline";

/** A network's name, linked to its page. */
export function NetworkLink({ name, className }: { name: string; className?: string }) {
  const project = useProjectName();
  return (
    <Link to="/projects/$project/networks/$name" params={{ project, name }} className={cn(objectLink, className)}>
      {name}
    </Link>
  );
}

/** A security group's name, linked to its page; the built-in default is no object and has none. */
export function SecurityGroupLink({ name, className }: { name: string; className?: string }) {
  const project = useProjectName();
  if (name === "default") return <span className={cn("font-mono text-[12.5px] text-foreground", className)}>default</span>;
  return (
    <Link to="/projects/$project/security-groups/$name" params={{ project, name }} className={cn(objectLink, className)}>
      {name}
    </Link>
  );
}

/** A Container's or VM's name, linked to its page. */
export function MachineLink({ kind, name, className }: { kind: MachineKind; name: string; className?: string }) {
  const project = useProjectName();
  return (
    <Link to={machinePath(kind)} params={{ project, name }} className={cn(objectLink, className)}>
      {name}
    </Link>
  );
}

/** A disk's name, linked to its page. */
export function DiskLink({ name, className }: { name: string; className?: string }) {
  const project = useProjectName();
  return (
    <Link to="/projects/$project/disks/$name" params={{ project, name }} className={cn(objectLink, className)}>
      {name}
    </Link>
  );
}

/** An image's name, linked to its page. */
export function ImageLink({ name }: { name: string }) {
  const project = useProjectName();
  return (
    <Link to="/projects/$project/images/$name" params={{ project, name }} className={objectLink}>
      {name}
    </Link>
  );
}

/** A custom image's name, linked to its page. */
export function CustomImageLink({ name }: { name: string }) {
  const project = useProjectName();
  return (
    <Link to="/projects/$project/custom-images/$name" params={{ project, name }} className={objectLink}>
      {name}
    </Link>
  );
}

/**
 * MachineTargetText names the machine an object points at, linked, then
 * what of it (its interface, a port) and its kind; empty when there is none.
 */
export function MachineTargetText({
  t,
  detail,
  empty = "none",
  className,
}: {
  t?: { kind: MachineKind; name: string };
  /** Drawn after the name as it is, its separator included. */
  detail?: ReactNode;
  empty?: string;
  className?: string;
}) {
  const project = useProjectName();
  if (!t) return <span className="text-subtle">{empty}</span>;
  return (
    <span className={className}>
      <Link to={machinePath(t.kind)} params={{ project, name: t.name }} className="font-medium text-accent hover:underline">
        {t.name}
      </Link>
      {detail && <span className="font-mono text-[12px] text-muted">{detail}</span>}
      <span className="ml-2 whitespace-nowrap break-normal text-[11.5px] text-subtle">{kindWord(t.kind)}</span>
    </span>
  );
}
