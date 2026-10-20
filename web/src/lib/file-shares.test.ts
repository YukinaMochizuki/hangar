import { FileSharePhase, FileShareSquash } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { describe, expect, it } from "vitest";
import {
  addressOn,
  declarationsOf,
  mountCommand,
  newFileShare,
  orList,
  phaseWords,
  shareNetworks,
  shareUses,
  shareYaml,
  usesVia,
  vmMountCommands,
  withNetworks,
} from "@/lib/file-shares";
import { containerView, vmView } from "@/lib/machines";
import { container, machineOf, share, vm } from "@/test/file-shares";

const view = (c: ReturnType<typeof container>) => containerView(c, machineOf("Container", c));

describe("a share's networks and addresses", () => {
  it("is on default when spec.networks is empty", () => {
    expect(shareNetworks(share("a"))).toEqual(["default"]);
    expect(shareNetworks(share("a", { networks: ["data", "ops"] }))).toEqual(["data", "ops"]);
  });

  it("has an address on a network only once its server has one", () => {
    const s = share("a", { networks: ["default", "data"], addresses: { data: "10.1.0.9" } });
    expect(addressOn(s, "data")).toBe("10.1.0.9");
    expect(addressOn(s, "default")).toBeUndefined();
  });

  it("names its phase, Pending for a phase not written", () => {
    expect(phaseWords(share("a")).label).toBe("Pending");
    expect(phaseWords(share("a", { phase: FileSharePhase.AVAILABLE })).label).toBe("Available");
    expect(phaseWords(share("a", { phase: FileSharePhase.IN_USE })).label).toBe("In use");
  });
});

describe("who uses a share", () => {
  it("reads a container's FileShare volumes and a VM's spec.fileShares, with what holds them back", () => {
    const c = container("web", ["data"], [{ share: "cache", target: "/cache" }], "failed_precondition: file share cache is on no node yet");
    expect(declarationsOf(view(c))).toEqual([
      expect.objectContaining({ kind: "Container", name: "web", share: "cache", path: "/cache", networks: ["data"], heldBack: expect.stringContaining("no node") }),
    ]);
    const v = vm("db", [], [{ name: "media", readOnly: true }], "file share media is not on node n1");
    expect(declarationsOf(vmView(v, undefined))).toEqual([
      expect.objectContaining({ kind: "VirtualMachine", share: "media", readOnly: true, networks: ["default"], notOnNode: "file share media is not on node n1" }),
    ]);
  });

  it("lists every declaration before status.users does, mounting from the first of its networks the share is on", () => {
    const s = share("cache", { networks: ["default", "data"] });
    const decls = [container("etl", ["ops", "data"], [{ share: "cache", target: "/c" }]), container("web", [], [{ share: "other", target: "/o" }])].flatMap((c) =>
      declarationsOf(view(c)),
    );
    const uses = shareUses(s, decls);
    expect(uses.map((u) => [u.name, u.network, u.address])).toEqual([["etl", "data", undefined]]);
    expect(usesVia(uses, "data")).toHaveLength(1);
    expect(usesVia(uses, "default")).toHaveLength(0);
  });

  it("takes the network and address status.users gives, once", () => {
    const s = share("cache", {
      networks: ["default", "data"],
      addresses: { default: "10.0.0.8", data: "10.1.0.10" },
      users: [{ kind: "Container", name: "etl", network: "default", path: "/c" }],
    });
    const decls = declarationsOf(view(container("etl", ["default", "data"], [{ share: "cache", target: "/c" }])));
    expect(shareUses(s, decls).map((u) => [u.name, u.network, u.address])).toEqual([["etl", "default", "10.0.0.8"]]);
  });
});

describe("what a share is written as", () => {
  it("writes its spec as apply takes it, the squash by its enum name", () => {
    const yaml = shareYaml(newFileShare("demo", "scratch", { networks: ["default", "data"], size: "10", squash: "All", anonUid: "65534", anonGid: "65534" }));
    expect(yaml).toContain("kind: FileShare");
    expect(yaml).toContain("size: 10Gi");
    expect(yaml).toContain("squash: FILE_SHARE_SQUASH_ALL");
    expect(yaml).toContain("anonUid: 65534");
    expect(yaml).toMatch(/networks:\n\s+- default\n\s+- data/);
  });

  it("sets no anonymous IDs unless every user is squashed", () => {
    const s = newFileShare("demo", "a", { networks: ["default"], size: "1", squash: "Root", anonUid: "1", anonGid: "1" });
    expect(s.spec?.squash).toBe(FileShareSquash.ROOT);
    expect(s.spec?.anonUid).toBeUndefined();
  });

  it("changes only the networks, at the version read", () => {
    const s = share("a", { networks: ["default"], version: 9n, size: "20Gi" });
    const o = withNetworks(s, ["default", "data"]);
    expect(o.spec?.networks).toEqual(["default", "data"]);
    expect(o.spec?.size).toBe("20Gi");
    expect(o.metadata?.resourceVersion).toBe(9n);
    expect(s.spec?.networks).toEqual(["default"]);
  });

  it("mounts with the options containers get, and the short timeout", () => {
    expect(mountCommand("assets", "10.0.0.7")).toBe("mount -t nfs4 -o nosuid,nodev,timeo=50 10.0.0.7:/ /mnt/assets");
    expect(mountCommand("assets", "10.0.0.7", true)).toContain("-o ro,nosuid,nodev,timeo=50");
    expect(vmMountCommands("assets", "10.0.0.7", false, "muid-db").split("\n")).toEqual([
      "modprobe nfs",
      "echo muid-db > /sys/module/nfs/parameters/nfs4_unique_id",
      "mount -t nfs4 -o nosuid,nodev,timeo=50 10.0.0.7:/ /mnt/assets",
    ]);
  });

  it("joins names as a sentence does", () => {
    expect(orList(["a"])).toBe("a");
    expect(orList(["a", "b", "c"])).toBe("a, b or c");
    expect(orList(["a", "b"], "and")).toBe("a and b");
  });
});
