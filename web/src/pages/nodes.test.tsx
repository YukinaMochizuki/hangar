// @vitest-environment jsdom

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { SeriesName } from "@hangar/api/hangar/monitoring/v1alpha1/monitoring_pb";
import { NodeDetail, NodeList } from "@/pages/nodes";
import { withApi } from "@/test/harness";
import { alerts, monitoringApi, node, nodeOverview, overview, unavailable } from "@/test/monitoring";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

afterEach(cleanup);

const platform = { kind: "project" as const, name: "" };

describe("NodeList", () => {
  const row = (name: string) => screen.getAllByRole("row").find((tr) => tr.querySelector("td")?.textContent?.startsWith(name)) as HTMLElement;
  const cell = (name: string, i: number) => row(name).querySelectorAll("td")[2 + i] as HTMLElement;

  it("lists each node with its state and machines, each opening its page", async () => {
    const api = monitoringApi({ nodes: [node("a"), node("b", { cordoned: true }), node("c", { ready: ConditionStatus.UNKNOWN })], machinesOn: ["a", "b", "b"] });
    render(<NodeList />, { wrapper: withApi(api, platform) });
    await waitFor(() => expect(row("c")).toBeTruthy());
    expect(within(row("a")).getByText("Example CPU 1000 — region-1 · site-a")).toBeTruthy();
    expect(row("a").querySelector("a")?.getAttribute("href")).toBe("/platform/nodes/a");
    expect(within(row("a")).getByText("Ready")).toBeTruthy();
    expect(within(row("b")).getByText("Cordoned")).toBeTruthy();
    expect(within(row("c")).getByText("Not reporting")).toBeTruthy();
    await waitFor(() => expect(Array.from(row("b").querySelectorAll("td")).at(-2)?.textContent).toBe("2"));
  });

  // What machines hold is their resources summed, against what the node
  // offers them; what the host keeps for itself is never counted as held.
  it("counts what the machines on a node hold, against what it offers them", async () => {
    const api = monitoringApi({
      nodes: [node("empty"), node("busy", { allocatable: { cpu: "30", memory: "120Gi", disk: "800Gi" } })],
      machinesOn: ["busy", "busy"],
    });
    render(<NodeList />, { wrapper: withApi(api, platform) });
    await waitFor(() => expect(within(cell("busy", 1)).getByText("112 GiB free")).toBeTruthy());
    expect(within(cell("busy", 0)).getByText("32 vCPU")).toBeTruthy();
    expect(within(cell("busy", 0)).getByText("4 vCPU assigned")).toBeTruthy();
    expect(within(cell("busy", 1)).getByText("7% held")).toBeTruthy();
    expect(within(cell("busy", 2)).getByText("760 GiB free")).toBeTruthy();
    // A node with no machine holds nothing, however much its host keeps.
    expect(within(cell("empty", 1)).getByText("0% held")).toBeTruthy();
    expect(within(cell("empty", 1)).getByText("76 GiB free")).toBeTruthy();
  });

  it("says nothing of what machines hold without platform.monitoring.read, and asks for none of it", async () => {
    const api = monitoringApi({ perms: ["compute.nodes.list"], machinesOn: ["a"], nodes: [node("a")] });
    render(<NodeList />, { wrapper: withApi(api, platform) });
    await waitFor(() => expect(within(cell("a", 1)).getByText("machines not known")).toBeTruthy());
    expect(api.calls).not.toContain("ListNodeUsage");
  });

  // A sponsor holds the Nodes and the monitoring, and no machine: what is
  // placed on each node comes summed, never as the machines themselves.
  it("counts what each node holds without listing any machine", async () => {
    const api = monitoringApi({ perms: ["compute.nodes.list", "platform.monitoring.read"], machinesOn: ["a", "a"], nodes: [node("a")] });
    render(<NodeList />, { wrapper: withApi(api, platform) });
    await waitFor(() => expect(within(cell("a", 0)).getByText("4 vCPU assigned")).toBeTruthy());
    expect(Array.from(row("a").querySelectorAll("td")).at(-2)?.textContent).toBe("2");
    expect(api.calls).not.toContain("ListMachines");
  });

  it("states the refusal, and lists no node, without compute.nodes.list", async () => {
    const api = monitoringApi({ perms: ["platform.monitoring.read"] });
    render(<NodeList />, { wrapper: withApi(api, platform) });
    await screen.findByText("This account's roles do not include reading the platform's nodes.");
    expect(api.calls).not.toContain("ListNodes");
  });

  it("asks for no node while the permission answer has not come, and says nothing", async () => {
    const api = monitoringApi({ permissions: "never" });
    render(<NodeList />, { wrapper: withApi(api, platform) });
    await waitFor(() => expect(api.calls).toContain("TestPermissions"));
    expect(api.calls).not.toContain("ListNodes");
    expect(screen.queryByText(/roles do not include/)).toBeNull();
  });
});

describe("NodeDetail", () => {
  function show(f: Parameters<typeof monitoringApi>[0] = {}, name = "a") {
    const api = monitoringApi({ overview: () => overview([nodeOverview("a"), nodeOverview("b")]), nodes: [node("a"), node("b")], alerts: () => alerts(["a", "b"]), ...f });
    render(<NodeDetail name={name} />, { wrapper: withApi(api, platform) });
    return api;
  }

  it("shows the node's own alerts, host and services, and nothing of another node's", async () => {
    show();
    await screen.findByText("HostAlert-a");
    expect(screen.queryByText("HostAlert-b")).toBeNull();
    expect(screen.queryByText("TestAlert")).toBeNull();
    await screen.findByText("md127 mirror");
    expect(screen.getByText("Node a")).toBeTruthy();
    expect(screen.queryByText("Node b")).toBeNull();
  });

  it("asks the host's series for the node alone", async () => {
    const api = show();
    await waitFor(() => expect(api.requests.filter((r) => r.method === "GetSeries")).toHaveLength(3));
    const asked = api.requests.filter((r) => r.method === "GetSeries").map((r) => r.request as { name: SeriesName; node: string });
    expect(asked.map((r) => r.name).sort()).toEqual([SeriesName.HOST_CPU, SeriesName.HOST_MEMORY, SeriesName.POOL_USED]);
    expect(asked.every((r) => r.node === "a")).toBe(true);
  });

  it("draws no threshold for a series the API gives none", async () => {
    show();
    const card = (title: string) => screen.getByText(title, { selector: "h3, h3 *" }).closest(".rounded-lg") as HTMLElement;
    // The chart takes its placeholder's place once the series came.
    await waitFor(() => expect(within(card("CPU")).getByRole("img")).toBeTruthy());
    expect(within(card("CPU")).queryByText(/alert at/)).toBeNull();
    await waitFor(() => expect(within(card("Memory")).getByText(/alert at/)).toBeTruthy());
  });

  it("shows what the host is and has", async () => {
    show();
    await screen.findByText("Example CPU 1000");
    expect(screen.getByText("generic-1u-2x-u2")).toBeTruthy();
    expect(screen.getByText("v0.14.2")).toBeTruthy();
    // What the host keeps for itself is said on its page: 128Gi less the 76Gi it offers.
    expect(screen.getByText(/52 GiB kept for the host/)).toBeTruthy();
  });

  it("says a host check Prometheus has no data for is no data", async () => {
    show();
    const host = (await screen.findByText("md127 mirror")).closest(".rounded-lg") as HTMLElement;
    // One check reads "no data" as the API words it, one has no value at all.
    expect(within(host).getAllByText("no data")).toHaveLength(2);
  });

  it("shows the node without its monitoring, asking none of it, without platform.monitoring.read", async () => {
    const api = show({ perms: ["compute.nodes.list"] });
    await screen.findByText(/do not include reading the platform's monitoring/);
    expect(screen.getByText("Example CPU 1000")).toBeTruthy();
    expect(api.calls.filter((c) => ["GetOverview", "GetSeries", "ListAlerts", "ListNodeUsage"].includes(c))).toEqual([]);
  });

  // A Node is system-managed: no one changes it from here (docs/decisions/apply-semantics.md).
  it("offers no control that changes the node", async () => {
    show({ nodes: [node("a", { cordoned: true })] });
    await screen.findByText("Cordoned");
    expect(screen.queryByRole("button", { name: /cordon/i })).toBeNull();
  });

  it("still shows the node when Prometheus is unavailable, with one notice for its layers", async () => {
    show({
      overview: () => {
        throw unavailable();
      },
      series: () => {
        throw unavailable();
      },
    });
    await screen.findByText("Prometheus did not answer, so the host's layers cannot be read.");
    expect(screen.getByText("Example CPU 1000")).toBeTruthy();
    expect(screen.queryByText("Over time")).toBeNull();
  });

  it("says the platform has no node of the name", async () => {
    show({}, "zzz");
    await screen.findByText("No such node");
  });
});
