// @vitest-environment jsdom

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { Health, SeriesName } from "@hangar/api/hangar/monitoring/v1alpha1/monitoring_pb";
import { PlatformOverview } from "@/pages/monitoring";
import { withApi } from "@/test/harness";
import { alerts, monitoringApi, node, nodeOverview, overview, unavailable } from "@/test/monitoring";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

afterEach(cleanup);

const platform = { kind: "project" as const, name: "" };
// A card by its title, never a table's header of the same word.
const card = (title: string) => screen.getByText(title, { selector: "h3, h3 *" }).closest(".rounded-lg") as HTMLElement;
// A node's row of the Nodes table, by the name its first cell starts with.
const nodeRow = (name: string) => screen.getAllByRole("row").find((tr) => tr.querySelector("td")?.textContent?.startsWith(name)) as HTMLElement;

describe("PlatformOverview", () => {
  it("shows the platform's own layers, the machines by phase, and UNKNOWN as no data", async () => {
    render(<PlatformOverview />, { wrapper: withApi(monitoringApi(), platform) });
    const reach = (await screen.findByText("Reachability")).closest(".rounded-lg") as HTMLElement;
    expect(within(reach).getByText("No data")).toBeTruthy();
    const phases = within(card("Machines")).getByRole("list", { name: "Machines by phase" });
    expect(within(phases).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["3Running", "0Pending", "0Creating", "1Stopped", "0Suspended", "2Failed"]);
    await screen.findByText("TestAlert");
  });

  // A node's host and services are on its own page; the Overview gives each node a row.
  it("draws no node's cards, and gives each node a row with its layers' state that opens its page", async () => {
    const api = monitoringApi({ overview: () => overview([nodeOverview("a"), nodeOverview("b", Health.CRITICAL)]), nodes: [node("a"), node("b")] });
    render(<PlatformOverview />, { wrapper: withApi(api, platform) });
    await screen.findByText("Reachability");
    expect(screen.queryByText("md127 mirror")).toBeNull();
    expect(nodeRow("a").querySelector("a")?.getAttribute("href")).toBe("/platform/nodes/a");
    expect(within(nodeRow("a")).getAllByText("OK")).toHaveLength(2);
    expect(within(nodeRow("b")).getByText("Critical")).toBeTruthy();
  });

  it("asks only the platform's series, never a node's", async () => {
    const api = monitoringApi({ overview: () => overview([nodeOverview("a"), nodeOverview("b")]) });
    render(<PlatformOverview />, { wrapper: withApi(api, platform) });
    await waitFor(() => expect(api.requests.filter((r) => r.method === "GetSeries")).toHaveLength(3));
    const asked = api.requests.filter((r) => r.method === "GetSeries").map((r) => r.request as { name: SeriesName; node: string });
    expect(asked.map((r) => r.name).sort()).toEqual([SeriesName.API_REQUEST_RATE, SeriesName.API_ERROR_RATE, SeriesName.API_LATENCY_P99]);
    expect(asked.every((r) => r.node === "")).toBe(true);
    expect(screen.queryByRole("radiogroup", { name: "Node" })).toBeNull();
  });

  it("names each alert's node, linked to its page, and the platform's as the platform's", async () => {
    render(<PlatformOverview />, { wrapper: withApi(monitoringApi({ alerts: () => alerts(["a"]) }), platform) });
    const row = (await screen.findByText("HostAlert-a")).closest("li") as HTMLElement;
    expect(within(row).getByRole("link", { name: "a" }).getAttribute("href")).toBe("/platform/nodes/a");
    expect(within(screen.getByText("TestAlert").closest("li") as HTMLElement).getByText("platform")).toBeTruthy();
  });

  it("counts each node's alerts and machines, and says which reports and which is cordoned", async () => {
    const api = monitoringApi({
      overview: () => overview([nodeOverview("a"), nodeOverview("b")]),
      alerts: () => alerts(["b"]),
      nodes: [node("a", { cordoned: true }), node("b", { ready: ConditionStatus.UNKNOWN })],
      machinesOn: ["a", "a", "b"],
    });
    render(<PlatformOverview />, { wrapper: withApi(api, platform) });
    await screen.findByText("HostAlert-b");
    await waitFor(() => expect(within(nodeRow("a")).getByText("Cordoned")).toBeTruthy());
    expect(within(nodeRow("b")).getByText("Not reporting")).toBeTruthy();
    // Alerts, then machines, after the three states.
    const counts = (name: string) => Array.from(nodeRow(name).querySelectorAll("td")).slice(4, 6).map((td) => td.textContent);
    await waitFor(() => expect(counts("a")).toEqual(["0", "2"]));
    expect(counts("b")).toEqual(["1", "1"]);
  });

  it("asks for no Node and no machine without their permissions, still giving each node its row", async () => {
    const api = monitoringApi({ perms: ["platform.monitoring.read"], overview: () => overview([nodeOverview("a")]) });
    render(<PlatformOverview />, { wrapper: withApi(api, platform) });
    await screen.findByText("Reachability");
    await waitFor(() => expect(api.calls.filter((c) => c === "TestPermissions").length).toBeGreaterThanOrEqual(2));
    expect(within(nodeRow("a")).getAllByText("OK")).toHaveLength(2);
    expect(api.calls.filter((c) => c === "ListNodes" || c === "ListMachines")).toEqual([]);
    expect(screen.queryByText(/Did not load/)).toBeNull();
  });

  it("says under the nodes when the Nodes did not load, rather than leaving the cells empty", async () => {
    const api = monitoringApi({ overview: () => overview([nodeOverview("a")]), nodesFail: true });
    render(<PlatformOverview />, { wrapper: withApi(api, platform) });
    await screen.findByText(/Did not load the nodes: /);
  });

  it("states one notice in the cards' place when Prometheus is unavailable, and keeps the alerts", async () => {
    const api = monitoringApi({
      overview: () => {
        throw unavailable();
      },
      series: () => {
        throw unavailable();
      },
    });
    render(<PlatformOverview />, { wrapper: withApi(api, platform) });
    await screen.findByText("Prometheus did not answer, so no layer can be read.");
    expect(screen.queryByText("Reachability")).toBeNull();
    expect(screen.queryByText("Over time")).toBeNull();
    await screen.findByText("TestAlert");
    // Unavailable is not asked again at once.
    expect(api.calls.filter((c) => c === "GetOverview")).toHaveLength(1);
  });

  it("says when Alertmanager is unavailable, inside the alerts card", async () => {
    render(<PlatformOverview />, {
      wrapper: withApi(
        monitoringApi({
          alerts: () => {
            throw unavailable();
          },
        }),
        platform,
      ),
    });
    await screen.findByText(/Alertmanager did not answer/);
    await screen.findByText("Reachability");
  });

  it("states the refusal, and asks MonitoringService nothing, without platform.monitoring.read", async () => {
    const api = monitoringApi({ perms: [] });
    render(<PlatformOverview />, { wrapper: withApi(api, platform) });
    await screen.findByText("This account's roles do not include reading the platform's monitoring.");
    expect(api.calls).toContain("TestPermissions");
    expect(api.calls.filter((c) => ["GetOverview", "GetSeries", "ListAlerts"].includes(c))).toEqual([]);
  });

  it("asks TestPermissions at the platform", async () => {
    const api = monitoringApi();
    render(<PlatformOverview />, { wrapper: withApi(api, platform) });
    await screen.findByText("Reachability");
    const req = api.requests.find((r) => r.method === "TestPermissions")?.request as { scope: { target: { case: string } }; permissions: string[] };
    expect(req.scope.target.case).toBe("platform");
    expect(req.permissions).toEqual(["platform.monitoring.read"]);
  });
});
