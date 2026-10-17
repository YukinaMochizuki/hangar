// A Container's or VirtualMachine's manifest as `hangar apply -f` takes it:
// the object's protobuf JSON (enum values by their full names, as apply
// reads them) cut to what a person writes, then printed as YAML.

import { toJson, type JsonValue } from "@bufbuild/protobuf";
import { ContainerSchema, type Container } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { VirtualMachineSchema, type VirtualMachine } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";

type Json = { [k: string]: JsonValue };

/** What a person writes of an object: its type, name, labels and spec. */
export function manifestOf(obj: Container | VirtualMachine, kind: "Container" | "VirtualMachine"): Json {
  const json = (
    kind === "Container" ? toJson(ContainerSchema, obj as Container) : toJson(VirtualMachineSchema, obj as VirtualMachine)
  ) as Json;
  const meta = (json.metadata ?? {}) as Json;
  const metadata: Json = { name: meta.name ?? "" };
  // Labels under a *.hangarlab.cloud/ prefix are the system's; apply refuses them.
  const labels = Object.entries((meta.labels ?? {}) as Json).filter(([k]) => !/\.hangarlab\.cloud\//.test(k));
  if (labels.length) metadata.labels = Object.fromEntries(labels);
  return { apiVersion: "compute.hangarlab.cloud/v1alpha1", kind, metadata, spec: json.spec ?? {} };
}

const plain = /^[A-Za-z0-9_./:@+-][A-Za-z0-9_ ./:@+-]*$/;
const reserved = /^(true|false|yes|no|on|off|null|~|[-+]?[0-9.]+(e[-+]?[0-9]+)?)$/i;

function scalar(v: JsonValue): string {
  if (v === null) return "null";
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  const s = String(v);
  // Quoted when YAML would read it as something else, such as cpu: "2".
  if (s === "" || !plain.test(s) || reserved.test(s) || s.endsWith(" ") || s.includes(": ") || s.includes(" #")) {
    return JSON.stringify(s);
  }
  return s;
}

function lines(v: JsonValue, indent: string): string[] {
  if (Array.isArray(v)) {
    if (v.length === 0) return [];
    return v.flatMap((item) => {
      if (item !== null && typeof item === "object") {
        const inner = lines(item, indent + "  ");
        if (inner.length === 0) return [`${indent}- {}`];
        return [`${indent}- ${inner[0].trimStart()}`, ...inner.slice(1)];
      }
      return [`${indent}- ${scalar(item)}`];
    });
  }
  if (v !== null && typeof v === "object") {
    return Object.entries(v).flatMap(([k, val]) => {
      if (val !== null && typeof val === "object") {
        const empty = Array.isArray(val) ? val.length === 0 : Object.keys(val).length === 0;
        if (empty) return [`${indent}${k}: ${Array.isArray(val) ? "[]" : "{}"}`];
        return [`${indent}${k}:`, ...lines(val, indent + "  ")];
      }
      return [`${indent}${k}: ${scalar(val)}`];
    });
  }
  return [`${indent}${scalar(v)}`];
}

/** v as block YAML. */
export function toYaml(v: JsonValue): string {
  return lines(v, "").join("\n");
}
