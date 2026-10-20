// @vitest-environment jsdom

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SeriesName } from "@hangar/api/hangar/monitoring/v1alpha1/monitoring_pb";
import { Monitoring } from "@/pages/monitoring";
import { withApi } from "@/test/harness";
import { monitoringApi, nodeOverview, overview, unavailable } from "@/test/monitoring";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

afterEach(cleanup);

const platform = { kind: "project" as const, name: "" };

describe("Monitoring", () => {
  it("shows each layer, the node's host and services named by the node, and UNKNOWN as no data", async () => {
    const api = monitoringApi();
    render(<Monitoring />, { wrapper: withApi(api, platform) });
    const host = (await screen.findByText("Host")).closest(".rounded-lg") as HTMLElement;
    expect(within(host).getByText("dev")).toBeTruthy();
    expect(within(host).getByText("md127 mirror")).toBeTruthy();
    // A check of no data whose value is empty says so.
    expect(within(host).getAllByText("no data")).toHaveLength(2);
    const reach = (await screen.findByText("Reachability")).closest(".rounded-lg") as HTMLElement;
    expect(within(reach).getByText("No data")).toBeTruthy();
    expect(screen.getByText("Platform services").closest(".rounded-lg")?.textContent).toContain("dev");
    for (const p of ["Running", "Pending", "Creating", "Stopped", "Suspended", "Failed"]) expect(screen.getByText(p)).toBeTruthy();
    await screen.findByText("TestAlert");
    // With one node there is no node to pick, and the host's series ask for it.
    expect(screen.queryByRole("radiogroup", { name: "Node" })).toBeNull();
    await waitFor(() => expect(api.requests.filter((r) => r.method === "GetSeries")).toHaveLength(6));
    const asked = api.requests.filter((r) => r.method === "GetSeries").map((r) => r.request as { name: SeriesName; node: string });
    expect(asked.filter((r) => r.node === "dev").map((r) => r.name).sort()).toEqual([SeriesName.HOST_CPU, SeriesName.HOST_MEMORY, SeriesName.POOL_USED]);
    expect(asked.filter((r) => r.node === "")).toHaveLength(3);
  });

  it("draws no threshold for a series the API gives none", async () => {
    render(<Monitoring />, { wrapper: withApi(monitoringApi(), platform) });
    const card = (title: string) => screen.getByText(title).closest(".rounded-lg") as HTMLElement;
    // The chart takes its placeholder's place once the series came.
    await waitFor(() => expect(within(card("Host CPU")).getByRole("img")).toBeTruthy());
    expect(within(card("Host CPU")).queryByText(/alert at/)).toBeNull();
    await waitFor(() => expect(within(card("Host memory")).getByText(/alert at/)).toBeTruthy());
  });

  it("gives each node its cards, and a node picker for the host's charts", async () => {
    const api = monitoringApi({ overview: () => overview([nodeOverview("a"), nodeOverview("b")]) });
    render(<Monitoring />, { wrapper: withApi(api, platform) });
    const picker = await screen.findByRole("radiogroup", { name: "Node" });
    expect(within(picker).getAllByRole("radio").map((r) => r.textContent)).toEqual(["a", "b"]);
    expect(screen.getAllByText("Host")).toHaveLength(2);
    expect(screen.getAllByText("Platform services")).toHaveLength(2);
    within(picker).getByRole("radio", { name: "b" }).click();
    await waitFor(() => expect(api.requests.some((r) => r.method === "GetSeries" && (r.request as { node: string }).node === "b")).toBe(true));
    expect(screen.getByText("Host CPU · b")).toBeTruthy();
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
    render(<Monitoring />, { wrapper: withApi(api, platform) });
    await screen.findByText("Prometheus did not answer, so no layer can be read.");
    expect(screen.queryByText("Host")).toBeNull();
    expect(screen.queryByText("Over time")).toBeNull();
    await screen.findByText("TestAlert");
    // Unavailable is not asked again at once.
    expect(api.calls.filter((c) => c === "GetOverview")).toHaveLength(1);
  });

  it("says when Alertmanager is unavailable, inside the alerts card", async () => {
    render(<Monitoring />, {
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
    render(<Monitoring />, { wrapper: withApi(api, platform) });
    await screen.findByText("This account's roles do not include reading the platform's monitoring.");
    expect(api.calls).toContain("TestPermissions");
    expect(api.calls.filter((c) => ["GetOverview", "GetSeries", "ListAlerts"].includes(c))).toEqual([]);
  });

  it("asks TestPermissions at the platform", async () => {
    const api = monitoringApi();
    render(<Monitoring />, { wrapper: withApi(api, platform) });
    await screen.findByText("Reachability");
    const req = api.requests.find((r) => r.method === "TestPermissions")?.request as { scope: { target: { case: string } }; permissions: string[] };
    expect(req.scope.target.case).toBe("platform");
    expect(req.permissions).toEqual(["platform.monitoring.read"]);
  });
});
