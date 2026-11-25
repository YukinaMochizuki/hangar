// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { PlatformBilling, StatementPage, TenantBilling } from "@/pages/billing";
import { billingApi, fixedAccount, paygAccount, statement } from "@/test/billing";
import { polyfillDialog, withApi } from "@/test/harness";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
afterEach(cleanup);

const tenant = (name: string) => ({ kind: "tenant" as const, name });
const platform = { kind: "project" as const, name: "" };
// A figure card by its label: the first element of the text, as the figures lead the page.
const stat = (label: string) => screen.getAllByText(label)[0].closest(".rounded-lg") as HTMLElement;

describe("TenantBilling", () => {
  it("leads a pay-as-you-go tenant with what the period has come to, its projection and what is due", async () => {
    render(<TenantBilling />, { wrapper: withApi(billingApi(), tenant("acme")) });
    await screen.findByText("This period so far");
    expect(within(stat("This period so far")).getByText("TWD 9,025.84")).toBeTruthy();
    expect(stat("Projected for the period")).toBeTruthy();
    expect(within(stat("Outstanding")).getByText("TWD 10,220.16")).toBeTruthy();
    expect(within(stat("Outstanding")).getByText("1 statement due")).toBeTruthy();
    // What the period is made of, item by item.
    const byItem = screen.getByText("This period by item").closest(".rounded-lg") as HTMLElement;
    expect(within(byItem).getByText("vCPU")).toBeTruthy();
    expect(within(byItem).getByText("TWD 4,523.20")).toBeTruthy();
    // Each statement with whether it is due or paid.
    const rows = await screen.findAllByRole("row");
    expect(rows.some((r) => /Jun 1, 2026 – Aug 31, 2026.*Due/.test(r.textContent ?? ""))).toBe(true);
    expect(rows.some((r) => /Paid/.test(r.textContent ?? ""))).toBe(true);
  });

  it("shows a fixed-fee tenant its plan and fee alone, never what its holdings would cost", async () => {
    render(<TenantBilling />, { wrapper: withApi(billingApi(), tenant("globex")) });
    await screen.findByText("Next statement");
    expect(within(stat("Monthly fee")).getByText("TWD 300.00")).toBeTruthy();
    expect(within(stat("Monthly fee")).getByText("Plan standard")).toBeTruthy();
    expect(stat("Next statement")).toBeTruthy();
    expect(screen.queryByText(/list prices/i)).toBeNull();
    expect(screen.queryByText("TWD 258.56")).toBeNull();
    expect(screen.queryByText("This period by item")).toBeNull();
    expect(screen.queryByText("Prices")).toBeNull();
  });

  it("says a tenant with no account is not billed", async () => {
    render(<TenantBilling />, { wrapper: withApi(billingApi({ statements: [] }), tenant("initech")) });
    expect(await screen.findByText("Not billed")).toBeTruthy();
  });

  it("states a refusal", async () => {
    render(<TenantBilling />, { wrapper: withApi(billingApi({ denied: true }), tenant("acme")) });
    expect(await screen.findByText("This account's roles do not include reading the tenant's billing.")).toBeTruthy();
  });
});

describe("StatementPage", () => {
  it("bills an item once, each price's part under it, and the total", async () => {
    render(<StatementPage statement="acme-202606010000" />, { wrapper: withApi(billingApi(), tenant("acme")) });
    await screen.findByText("vCPU");
    const cpu = screen.getByText("vCPU").closest("tr") as HTMLElement;
    expect(within(cpu).getByText("TWD 5,150.40")).toBeTruthy();
    expect(screen.getByText(/5,856 vCPU-hour × TWD 0.60/)).toBeTruthy();
    expect(screen.getByText(/2,976 vCPU-hour × TWD 0.55/)).toBeTruthy();
    expect(screen.getAllByText("TWD 10,220.16").length).toBeGreaterThan(0);
  });

  it("offers recording a payment to whoever holds billing.statements.pay alone", async () => {
    const { unmount } = render(<StatementPage statement="acme-202606010000" />, { wrapper: withApi(billingApi(), tenant("acme")) });
    await screen.findByText("vCPU");
    expect(screen.queryByRole("button", { name: /Record payment/ })).toBeNull();
    unmount();
    const api = billingApi({ perms: ["billing.statements.pay"] });
    render(<StatementPage statement="acme-202606010000" />, { wrapper: withApi(api, tenant("acme")) });
    fireEvent.click(await screen.findByRole("button", { name: /Record payment/ }));
    const dialog = screen.getByRole("dialog", { name: "Record payment" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Record payment" }));
    await waitFor(() => expect(api.calls).toContain("MarkStatementPaid"));
    const req = api.requests.find((r) => r.method === "MarkStatementPaid")?.request as { name: string; paidTime?: unknown; unpaid: boolean };
    expect(req.name).toBe("acme-202606010000");
    expect(req.paidTime).toBeTruthy();
    expect(req.unpaid).toBe(false);
  });

  it("shows a fixed statement's value at list prices to root alone", async () => {
    const statements = [statement("globex-202610150000", { tenant: "globex", fixed: true, total: "300" })];
    const { unmount } = render(<StatementPage statement="globex-202610150000" />, { wrapper: withApi(billingApi({ statements }), tenant("globex")) });
    await screen.findByText("Monthly fee");
    expect(screen.queryByText(/list prices/)).toBeNull();
    unmount();
    render(<StatementPage statement="globex-202610150000" />, { wrapper: withApi(billingApi({ statements, perms: ["billing.statements.pay"] }), tenant("globex")) });
    expect(await screen.findByText(/At list prices this period's holdings came to TWD 258.56/)).toBeTruthy();
  });
});

describe("PlatformBilling", () => {
  it("lists every account, what is due, and a fixed account's value at list prices", async () => {
    const api = billingApi({ perms: ["billing.billingaccounts.list"], accounts: [paygAccount(), fixedAccount()] });
    render(<PlatformBilling />, { wrapper: withApi(api, platform) });
    const row = (name: string) => screen.getAllByRole("row").find((r) => r.querySelector("td")?.textContent === name) as HTMLElement;
    await waitFor(() => expect(row("globex")).toBeTruthy());
    expect(within(row("globex")).getByText("TWD 258.56")).toBeTruthy();
    expect(within(row("acme")).getByText("TWD 10,220.16")).toBeTruthy();
    expect(within(stat("Outstanding")).getByText("TWD 10,220.16")).toBeTruthy();
  });

  it("states a refusal without the permission, and lists nothing", async () => {
    const api = billingApi();
    render(<PlatformBilling />, { wrapper: withApi(api, platform) });
    expect(await screen.findByText("This account's roles do not include reading every tenant's billing.")).toBeTruthy();
    expect(api.calls).not.toContain("ListBillingAccounts");
  });
});
