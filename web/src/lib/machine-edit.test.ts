import { create } from "@bufbuild/protobuf";
import { ContainerRunState, ContainerSchema, Protocol, RestartPolicy, type Container } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { NetworkInterfaceSchema } from "@hangar/api/hangar/compute/v1alpha1/interface_pb";
import { VirtualMachineRunState, VirtualMachineSchema, VirtualMachineTPMSchema, type VirtualMachine } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { describe, expect, it } from "vitest";
import {
  advancedOf,
  changesOf,
  editedObject,
  interfacesOf,
  networkFieldsOf,
  quotaDelta,
  volumeRowsOf,
  type Change,
} from "@/lib/machine-edit";
import { splitWords } from "@/lib/shell-words";

const web = () =>
  create(ContainerSchema, {
    metadata: { name: "web", project: "demo", resourceVersion: 21n, labels: { team: "a" } },
    spec: {
      image: "nginx:1",
      command: ["/bin/sh", "-c"],
      args: ["echo 'hi there' && sleep infinity"],
      env: [{ name: "MODE", value: "prod" }],
      ports: [{ name: "http", port: 80, protocol: Protocol.TCP }],
      restartPolicy: RestartPolicy.ON_FAILURE,
      resources: { cpu: "1", memory: "512Mi" },
      runState: ContainerRunState.STOPPED,
      interfaces: [{ network: "default", securityGroups: [] }, { network: "data", securityGroups: ["db"] }],
      imagePullSecrets: ["ghcr"],
      volumes: [{ source: "cache", target: "/cache", readOnly: false, volume: { nocopy: true, subpath: "a" } }],
    },
  });

const db = () =>
  create(VirtualMachineSchema, {
    metadata: { name: "db", project: "demo", resourceVersion: 42n },
    spec: {
      resources: { cpu: "3", memory: "6Gi" },
      rootDisk: { source: { case: "fromImage", value: { image: { name: "ubuntu-24.04" }, size: "30Gi" } } },
      runState: VirtualMachineRunState.STOPPED,
      userData: "#cloud-config\n",
      fileShares: [{ name: "media", readOnly: true }],
      disks: [{ disk: "db-data" }],
      sshAuthorizedKeys: ["ssh-ed25519 AAAA me"],
      interfaces: [{ network: "default" }, { network: "data", securityGroups: ["db"] }],
    },
  });

const effects = (cs: Change[]) => Object.fromEntries(cs.map((c) => [c.field, c.effect]));

describe("prefilling the form from an object", () => {
  it("reads the interfaces, empty groups being the network's default", () => {
    expect(networkFieldsOf(web().spec!.interfaces)).toEqual({ network: "default", groups: ["default"], extra: [{ network: "data", groups: ["db"] }] });
    expect(networkFieldsOf([])).toEqual({ network: "default", groups: ["default"], extra: [] });
  });

  it("reads the process as lines that split back into the spec's words", () => {
    const a = advancedOf(web());
    expect(splitWords(a.entrypoint).words).toEqual(["/bin/sh", "-c"]);
    expect(splitWords(a.command).words).toEqual(["echo 'hi there' && sleep infinity"]);
    expect(a.restart).toBe("on-failure");
    expect(a.env.map((e) => [e.name, e.value])).toEqual([["MODE", "prod"]]);
  });

  it("reads the volumes", () => {
    expect(volumeRowsOf(web())).toEqual([{ source: "cache", kind: "Disk", target: "/cache", readOnly: false }]);
  });
});

describe("editedObject", () => {
  it("keeps what the form does not show: labels, version, run state, ports, pull secrets, volume options", () => {
    const o = editedObject(web(), { image: "nginx:2", volumes: [{ source: "cache", kind: "Disk", target: "/data", readOnly: true }] }) as Container;
    expect(o.metadata?.resourceVersion).toBe(21n);
    expect(o.metadata?.labels).toEqual({ team: "a" });
    expect(o.spec?.runState).toBe(ContainerRunState.STOPPED);
    expect(o.spec?.ports.map((p) => p.name)).toEqual(["http"]);
    expect(o.spec?.imagePullSecrets).toEqual(["ghcr"]);
    expect(o.spec?.volumes[0]).toMatchObject({ source: "cache", target: "/data", readOnly: true, volume: { nocopy: true, subpath: "a" } });
    expect(o.spec?.image).toBe("nginx:2");
  });

  it("changes nothing it is not asked to, and leaves its base as it was", () => {
    const base = web();
    expect(changesOf(base, editedObject(base, {}))).toEqual([]);
    editedObject(base, { image: "nginx:2" });
    expect(base.spec?.image).toBe("nginx:1");
  });

  it("keeps a VM's root disk, keys, user data and file shares when its size changes", () => {
    const o = editedObject(db(), { cpu: "4", memory: "8Gi" }) as VirtualMachine;
    expect(o.spec?.resources).toMatchObject({ cpu: "4", memory: "8Gi" });
    expect(o.spec?.rootDisk?.source).toEqual(db().spec?.rootDisk?.source);
    expect(o.spec?.userData).toBe("#cloud-config\n");
    expect(o.spec?.runState).toBe(VirtualMachineRunState.STOPPED);
    expect(o.spec?.sshAuthorizedKeys).toEqual(["ssh-ed25519 AAAA me"]);
    expect(o.spec?.fileShares.map((f) => [f.name, f.readOnly])).toEqual([["media", true]]);
    expect(o.spec?.disks.map((d) => d.disk)).toEqual(["db-data"]);
    expect(o.spec?.interfaces).toEqual(db().spec?.interfaces);
    expect({ ...o.spec, resources: undefined }).toEqual({ ...db().spec, resources: undefined });
  });

  it("keeps the interfaces' aliases, which the form does not show, through the form's fields", () => {
    const nics = [create(NetworkInterfaceSchema, { network: "data", aliases: ["db"] }), create(NetworkInterfaceSchema, { network: "default", aliases: ["pg", "sql"] })];
    const f = networkFieldsOf(nics);
    expect(f).toEqual({ network: "data", groups: ["default"], aliases: ["db"], extra: [{ network: "default", groups: ["default"], aliases: ["pg", "sql"] }] });
    expect(interfacesOf(f).map((n) => n.aliases)).toEqual([["db"], ["pg", "sql"]]);
    expect(interfacesOf(networkFieldsOf([create(NetworkInterfaceSchema, { network: "default", aliases: ["db"] })]))).toHaveLength(1);
  });

  it("writes one interface on default in its default group as no interfaces", () => {
    expect(interfacesOf({ network: "default", groups: ["default"], extra: [] })).toEqual([]);
    expect(interfacesOf({ network: "default", groups: ["default", "web"], extra: [] })).toEqual([create(NetworkInterfaceSchema, { network: "default", securityGroups: ["default", "web"] })]);
  });
});

describe("changesOf: what applying does, per field", () => {
  it("a container's image rebuilds the root disk", () => {
    expect(effects(changesOf(web(), editedObject(web(), { image: "nginx:2" })))).toEqual({ image: "rebuild" });
    expect(changesOf(web(), editedObject(web(), { image: "nginx:2" }))[0]!.detail).toBe("nginx:1 → nginx:2");
  });

  it("a container's interfaces, volumes, process and size restart it; its disk grows in place", () => {
    const next = editedObject(web(), {
      interfaces: [],
      volumes: [],
      command: [],
      args: ["serve"],
      env: [{ name: "MODE", value: "dev" }, { name: "DEBUG", value: "1" }],
      restartPolicy: RestartPolicy.NEVER,
      cpu: "2",
      memory: "4Gi",
      disk: "20Gi",
    });
    const cs = changesOf(web(), next);
    expect(effects(cs)).toEqual({
      interfaces: "restart",
      volumes: "restart",
      command: "restart",
      args: "restart",
      env: "restart",
      restartPolicy: "restart",
      size: "restart",
      disk: "nothing",
    });
    expect(cs.find((c) => c.field === "env")!.detail).toBe("MODE changed, DEBUG added");
    expect(cs.find((c) => c.field === "disk")!.detail).toBe("10 → 20 GiB");
    expect(cs.find((c) => c.field === "restartPolicy")!.detail).toBe("on-failure → no");
  });

  it("interfaces whose aliases alone changed restart nothing", () => {
    for (const base of [web(), db()]) {
      const nics = base.spec!.interfaces.map((n) => create(NetworkInterfaceSchema, { network: n.network, securityGroups: n.securityGroups, aliases: ["cache"] }));
      expect(effects(changesOf(base, editedObject(base, { interfaces: nics })))).toEqual({ interfaces: "nothing" });
      nics[0]!.network = "other";
      expect(effects(changesOf(base, editedObject(base, { interfaces: nics })))).toEqual({ interfaces: "restart" });
    }
  });

  it("a container's image pull secrets restart nothing", () => {
    const next = web();
    next.spec!.imagePullSecrets = ["ghcr", "quay"];
    expect(effects(changesOf(web(), next))).toEqual({ imagePullSecrets: "nothing" });
  });

  it("a VM's resources and interfaces restart it; its file shares and user data restart nothing", () => {
    const next = editedObject(db(), { cpu: "4", interfaces: [create(NetworkInterfaceSchema, { network: "data" })] }) as VirtualMachine;
    next.spec!.fileShares = [];
    next.spec!.userData = "";
    expect(effects(changesOf(db(), next))).toEqual({ size: "restart", interfaces: "restart", fileShares: "nothing", userData: "nothing" });
  });

  it("a VM's serial number restarts it, set and cleared alike, and keeps its TPM", () => {
    const tpm = db();
    tpm.spec!.tpm = create(VirtualMachineTPMSchema, { enabled: true });
    const set = editedObject(tpm, { serialNumber: "ARX-0042" }) as VirtualMachine;
    expect(set.spec?.platform?.serialNumber).toBe("ARX-0042");
    expect(set.spec?.tpm?.enabled).toBe(true);
    const cs = changesOf(tpm, set);
    expect(effects(cs)).toEqual({ platform: "restart" });
    expect(cs[0]!.detail).toBe("the platform's → ARX-0042");
    const cleared = editedObject(set, { serialNumber: "" }) as VirtualMachine;
    expect(cleared.spec?.platform).toBeUndefined();
    expect(changesOf(set, cleared)[0]!.detail).toBe("ARX-0042 → the platform's");
  });
});

describe("quotaDelta", () => {
  it("is what the change adds, never the machine's whole size", () => {
    expect(quotaDelta(web(), editedObject(web(), { cpu: "2", memory: "2Gi", disk: "15Gi" }))).toEqual({ cpu: 1, memoryGi: 1.5, diskGi: 5 });
    expect(quotaDelta(db(), editedObject(db(), { cpu: "2", memory: "4Gi" }))).toEqual({ cpu: -1, memoryGi: -2, diskGi: 0 });
  });

  it("counts a container's unset resources as the defaults it runs with", () => {
    const bare = create(ContainerSchema, { metadata: { name: "x", resourceVersion: 1n }, spec: { image: "a" } });
    expect(quotaDelta(bare, editedObject(bare, { cpu: "1", memory: "512Mi", disk: "10Gi" }))).toEqual({ cpu: 0, memoryGi: 0, diskGi: 0 });
  });
});
