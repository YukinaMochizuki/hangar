// Mock data for the visual proof of concept. Nothing here comes from the API.
//
// Machine phases reuse the generated enum so the screens speak the API's
// states. Host information has no API yet: the shape below is a proposal for
// a tenant-readable view of a Node that carries hardware only, never how full
// the host is.

import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";

export { MachinePhase };

export type Chassis = "1u-2u2" | "storage-2u" | "mini" | "unknown";

export type HostInfo = {
  node: string;
  region: string;
  site: string;
  provider: string;
  cpuModel: string;
  cpuDetail: string;
  memory: string;
  storage: string;
  network: string;
  chassis: Chassis;
};

export type SizeId = "s" | "m" | "l" | "xl";

/** Placeholder sizes: the real catalogue is decided elsewhere. */
export const sizes: Record<SizeId, { label: string; cpu: number; memoryGi: number; diskGi: number }> = {
  s: { label: "S", cpu: 1, memoryGi: 2, diskGi: 10 },
  m: { label: "M", cpu: 2, memoryGi: 4, diskGi: 20 },
  l: { label: "L", cpu: 4, memoryGi: 8, diskGi: 40 },
  xl: { label: "XL", cpu: 8, memoryGi: 16, diskGi: 80 },
};

export type ConditionView = {
  type: string;
  status: "True" | "False" | "Unknown";
  reason: string;
  message?: string;
  ago: string;
};

export type InterfaceView = {
  name: string;
  network: string;
  ipv4: string;
  ipv6?: string;
  securityGroups: string[];
};

export type PortView = {
  name: string;
  port: number;
  protocol: "TCP" | "UDP";
  publicHost?: string;
};

export type MachineView = {
  kind: "Container" | "VirtualMachine";
  name: string;
  project: string;
  phase: MachinePhase;
  size: SizeId;
  image: string;
  imageDigest?: string;
  node: string;
  interfaces: InterfaceView[];
  ports: PortView[];
  restartPolicy: string;
  created: string;
  uptime?: string;
  uid: string;
  labels: Record<string, string>;
  conditions: ConditionView[];
};

export const hosts: HostInfo[] = [
  {
    node: "sgp02-01",
    region: "Singapore",
    site: "OVHcloud SGP-2",
    provider: "OVHcloud",
    cpuModel: "AMD EPYC 7313",
    cpuDetail: "16 cores · 32 threads · Zen 3 · 3.0 GHz",
    memory: "128 GiB DDR4-3200 ECC",
    storage: "2 × 960 GB NVMe (U.2)",
    network: "2 × 10 GbE · IPv6 /64",
    chassis: "1u-2u2",
  },
  {
    node: "sgp02-02",
    region: "Singapore",
    site: "OVHcloud SGP-2",
    provider: "OVHcloud",
    cpuModel: "AMD EPYC 9124",
    cpuDetail: "16 cores · Zen 4 · 3.0 GHz",
    memory: "128 GiB DDR5 ECC",
    storage: "12 × 3.5\" HDD + NVMe cache",
    network: "10 Gbit/s · IPv6 /64",
    chassis: "storage-2u",
  },
  {
    node: "tpe-home-01",
    region: "Taipei",
    site: "Home lab",
    provider: "Self-hosted",
    cpuModel: "AMD Ryzen 7 7840HS",
    cpuDetail: "8 cores · Zen 4 · 3.8 GHz",
    memory: "64 GiB DDR5",
    storage: "NVMe",
    network: "1 Gbit/s · IPv6 /64",
    chassis: "mini",
  },
];

export const tenant = { name: "mochizuki", displayName: "Mochizuki" };

export const projects = [
  { name: "agents-438212", displayName: "Agents" },
  { name: "blog-902114", displayName: "Blog" },
  { name: "sandbox-117530", displayName: "Sandbox" },
];

export const currentProject = projects[0];

export const machines: MachineView[] = [
  {
    kind: "Container",
    name: "agent-runner",
    project: "agents-438212",
    phase: MachinePhase.RUNNING,
    size: "m",
    image: "ghcr.io/mochizuki/agent-runner:2026.10.16",
    imageDigest: "sha256:7c1e9a40d2b35f8e61a0c94b7e2d3f18a6c05b9e4d7f2a1c3b8e6d9f0a4c2b17",
    node: "sgp02-01",
    interfaces: [
      {
        name: "eth0",
        network: "default",
        ipv4: "10.128.0.14",
        ipv6: "2001:db8:0:a2e::14",
        securityGroups: ["default", "agent-egress"],
      },
    ],
    ports: [
      { name: "http", port: 8080, protocol: "TCP", publicHost: "agent-runner-http-agents-438212.hangarlab.dev" },
      { name: "metrics", port: 9090, protocol: "TCP" },
    ],
    restartPolicy: "Always",
    created: "2026-10-16 09:42 UTC",
    uptime: "1d 4h",
    uid: "5f0c2a7e-3b1d-4e9a-8c6f-2d71b0e4a913",
    labels: { app: "agent-runner", tier: "worker" },
    conditions: [
      { type: "Scheduled", status: "True", reason: "Placed", message: "Placed on sgp02-01", ago: "1d" },
      { type: "Ready", status: "True", reason: "Running", ago: "1d" },
      { type: "Stopped", status: "False", reason: "Running", ago: "1d" },
    ],
  },
  {
    kind: "VirtualMachine",
    name: "build-box",
    project: "agents-438212",
    phase: MachinePhase.RUNNING,
    size: "l",
    image: "ubuntu-24.04",
    node: "sgp02-01",
    interfaces: [
      { name: "eth0", network: "default", ipv4: "10.128.0.21", ipv6: "2001:db8:0:a2e::21", securityGroups: ["default", "ssh"] },
    ],
    ports: [],
    restartPolicy: "On failure",
    created: "2026-10-14 13:05 UTC",
    uptime: "3d 1h",
    uid: "a83d61f2-0c47-4b5e-9d12-7e4f8b3a6c05",
    labels: {},
    conditions: [
      { type: "Scheduled", status: "True", reason: "Placed", ago: "3d" },
      { type: "Ready", status: "True", reason: "Running", ago: "3d" },
    ],
  },
  {
    kind: "Container",
    name: "test-shard-3",
    project: "agents-438212",
    phase: MachinePhase.CREATING,
    size: "s",
    image: "ghcr.io/mochizuki/pytest-env:3.13",
    node: "sgp02-01",
    interfaces: [{ name: "eth0", network: "default", ipv4: "10.128.0.33", ipv6: "2001:db8:0:a2e::33", securityGroups: ["default"] }],
    ports: [],
    restartPolicy: "Never",
    created: "2026-10-17 11:58 UTC",
    uid: "c2e7b915-6d3a-4f08-b1e4-9a5d0c8f7e26",
    labels: { job: "ci-1182" },
    conditions: [
      { type: "Scheduled", status: "True", reason: "Placed", ago: "12s" },
      { type: "Ready", status: "False", reason: "PullingImage", message: "Pulling ghcr.io/mochizuki/pytest-env:3.13", ago: "12s" },
    ],
  },
  {
    kind: "Container",
    name: "scraper",
    project: "agents-438212",
    phase: MachinePhase.FAILED,
    size: "s",
    image: "ghcr.io/mochizuki/scraper:latest",
    node: "sgp02-01",
    interfaces: [{ name: "eth0", network: "default", ipv4: "10.128.0.9", ipv6: "2001:db8:0:a2e::9", securityGroups: ["default"] }],
    ports: [],
    restartPolicy: "On failure",
    created: "2026-10-15 22:10 UTC",
    uid: "e91f03b6-2a8c-4d57-a6e0-3b4c7d1f9a82",
    labels: {},
    conditions: [
      { type: "Scheduled", status: "True", reason: "Placed", ago: "2d" },
      { type: "Ready", status: "False", reason: "CrashLoop", message: "Process exited with 1 (5 restarts)", ago: "3m" },
    ],
  },
  {
    kind: "Container",
    name: "notebook",
    project: "agents-438212",
    phase: MachinePhase.STOPPED,
    size: "m",
    image: "quay.io/jupyter/minimal-notebook:2026-10",
    node: "sgp02-01",
    interfaces: [{ name: "eth0", network: "default", ipv4: "10.128.0.40", ipv6: "2001:db8:0:a2e::40", securityGroups: ["default"] }],
    ports: [{ name: "lab", port: 8888, protocol: "TCP" }],
    restartPolicy: "Always",
    created: "2026-10-12 08:30 UTC",
    uid: "0d4b8e27-91c3-4a6f-8e5b-c7a2f1d03e94",
    labels: {},
    conditions: [{ type: "Stopped", status: "True", reason: "Stopped", ago: "5h" }],
  },
];

export const logTail = [
  "2026-10-17T12:01:04Z  INFO  runner  picked task t-91c2 (repo=hangar, kind=test)",
  "2026-10-17T12:01:04Z  INFO  runner  exec: go test ./internal/apiserver/...",
  "2026-10-17T12:01:39Z  INFO  runner  ok  internal/apiserver  34.812s",
  "2026-10-17T12:01:40Z  INFO  runner  task t-91c2 done in 36.1s",
  "2026-10-17T12:01:52Z  INFO  runner  idle, polling queue",
];

export function hostOf(node: string) {
  return hosts.find((h) => h.node === node);
}

/** What the current project runs on a node: the only usage a tenant sees. */
export function footprintOn(node: string) {
  const mine = machines.filter((m) => m.node === node && m.project === currentProject.name);
  const live = mine.filter((m) => m.phase !== MachinePhase.STOPPED);
  return {
    machines: mine.length,
    running: live.length,
    cpu: live.reduce((n, m) => n + sizes[m.size].cpu, 0),
    memoryGi: live.reduce((n, m) => n + sizes[m.size].memoryGi, 0),
    diskGi: mine.reduce((n, m) => n + sizes[m.size].diskGi, 0),
  };
}

/** The current project's quota, in the shape of Project status limits and usage. */
export const quota = {
  compute: [
    { key: "machines", label: "Machines", used: 5, limit: 8, unit: "" },
    { key: "cpu", label: "vCPU", used: 8, limit: 16, unit: "" },
    { key: "memory", label: "Memory", used: 16, limit: 24, unit: "GiB" },
    { key: "disk", label: "Disk", used: 100, limit: 200, unit: "GiB" },
  ],
  other: [
    { key: "networks", label: "Networks", used: 1, limit: 3 },
    { key: "addresses", label: "Addresses", used: 1, limit: 2 },
    { key: "publicEndpoints", label: "Public endpoints", used: 1, limit: 5 },
    { key: "serviceTokens", label: "Service tokens", used: 2, limit: 10 },
  ],
};

export type ActivityView = {
  ago: string;
  actor: string;
  verb: string;
  object: string;
  tone: "ok" | "pending" | "fail" | "neutral";
};

export const activity: ActivityView[] = [
  { ago: "12s", actor: "serviceToken:agents/ci", verb: "created", object: "container/test-shard-3", tone: "pending" },
  { ago: "3m", actor: "system", verb: "restarted", object: "container/scraper (exit 1, 5th restart)", tone: "fail" },
  { ago: "5h", actor: "mochizuki", verb: "stopped", object: "container/notebook", tone: "neutral" },
  { ago: "1d", actor: "serviceToken:agents/ci", verb: "updated", object: "container/agent-runner (image)", tone: "ok" },
  { ago: "3d", actor: "mochizuki", verb: "created", object: "virtualmachine/build-box", tone: "ok" },
];

export const vmImages = [
  { id: "ubuntu-24.04", label: "Ubuntu", version: "24.04 LTS" },
  { id: "debian-13", label: "Debian", version: "13" },
  { id: "fedora-43", label: "Fedora", version: "43" },
  { id: "alpine-3.22", label: "Alpine", version: "3.22" },
];

export const sizeHints: Record<SizeId, string> = {
  s: "Scripts, small services, one agent task",
  m: "Agent runners, web apps, test shards",
  l: "Builds, CI, databases",
  xl: "Heavy builds, parallel test suites",
};
