// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { NameService } from "@hangar/api/hangar/tenancy/v1alpha1/names_pb";
import { ProjectService, type ApplyProjectRequest } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { NewProjectDialog, ProjectPicker } from "@/components/project-picker";
import { Topbar } from "@/components/shell";
import { TenantOverview } from "@/pages/tenant-overview";
import { fakeApi, never, polyfillDialog, withApi, type Api } from "@/test/harness";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  // Rendered without a router: a link is its text and its target.
  Link: ({ children, to, params, className }: { children: ReactNode; to: string; params?: Record<string, string>; className?: string }) => (
    <a className={className} href={Object.entries(params ?? {}).reduce((p, [k, v]) => p.replace(`$${k}`, v), to)}>
      {children}
    </a>
  ),
}));

beforeAll(polyfillDialog);
beforeEach(() => navigate.mockReset());
afterEach(cleanup);

const projects = [
  { metadata: { name: "acme-web" }, spec: { tenant: "acme", displayName: "Web" }, status: { usage: { machines: 2, cpu: "4", memory: "4Gi", disk: "20Gi" } } },
  { metadata: { name: "acme-agents" }, spec: { tenant: "acme", displayName: "Agents" }, status: { usage: { machines: 1, cpu: "1", memory: "1Gi", disk: "10Gi" } } },
  { metadata: { name: "zeta-lab" }, spec: { tenant: "zeta", displayName: "Research" } },
];
const create = "tenancy.projects.apply";

type Setup = {
  /** What GetTenant fails with. */
  tenant?: ConnectError;
  /** Projects the tenant holds, against a limit of 5. */
  used?: number;
  /** The tenants where the caller may create projects; acme and empty unless said. */
  creatableIn?: string[];
  permissions?: "never" | "fail";
  checkName?: "never" | "fail" | boolean;
  /** Someone else holds the name by the time the apply comes. */
  taken?: boolean;
};

function api(s: Setup = {}): Api {
  const names: Record<string, string> = { acme: "Acme", zeta: "Zeta", empty: "Empty" };
  const tenantObject = (name: string) => ({
    metadata: { name },
    spec: { displayName: names[name] },
    status: {
      plan: "default",
      limits: { cpu: "8", memory: "16Gi", disk: "100Gi", machines: 10, projects: 5 },
      usage: { cpu: "5", memory: "5Gi", disk: "30Gi", machines: 3, projects: s.used ?? 2 },
    },
  });
  return fakeApi((r) => {
    r.service(TenantService, {
      getTenant: async (req) => {
        if (s.tenant) throw s.tenant;
        return { tenant: tenantObject(req.name) };
      },
      // "empty" holds no project: only ListTenants shows it.
      listTenants: async () => ({ items: ["acme", "zeta", "empty"].map(tenantObject) }),
    });
    r.service(ProjectService, {
      listProjects: async () => ({ items: projects }),
      // As the API does: a create-only apply of a name that exists is refused, and nothing changes.
      applyProject: async (req) => {
        if (s.taken) {
          if (req.createOnly) throw new ConnectError(`Project ${req.project?.metadata?.name} already exists`, Code.AlreadyExists);
          return { project: req.project, created: false };
        }
        return { project: req.project, created: true };
      },
    });
    r.service(NameService, {
      checkName: async () => {
        if (s.checkName === "never") return never();
        if (s.checkName === "fail") throw new ConnectError("name service down", Code.Unavailable);
        return { available: s.checkName ?? true };
      },
    });
    r.service(AccountService, {
      testPermissions: async (req) => {
        if (s.permissions === "never") return never();
        if (s.permissions === "fail") throw new ConnectError("unavailable", Code.Unavailable);
        const at = req.scope?.target.case === "tenant" ? req.scope.target.value : "";
        return { permissions: (s.creatableIn ?? ["acme", "empty"]).includes(at) ? req.permissions.filter((p) => p === create) : [] };
      },
    });
  });
}

describe("TenantOverview", () => {
  function show(s: Setup = {}) {
    const a = api(s);
    render(<TenantOverview />, { wrapper: withApi(a, { kind: "tenant", name: "acme" }) });
    return a;
  }

  it("shows the plan, what the tenant holds against its limits, and its projects with theirs", async () => {
    show();
    expect(await screen.findByText("Plan default")).toBeTruthy();
    expect(screen.getByText("/ 10")).toBeTruthy();
    expect(screen.getByText("2 / 5")).toBeTruthy();
    const table = screen.getByRole("table");
    expect(within(table).getByText("Web")).toBeTruthy();
    expect(within(table).getByText("Agents")).toBeTruthy();
    // Another tenant's project is not the tenant's.
    expect(within(table).queryByText("Research")).toBeNull();
  });

  it("states that the caller may not read the tenant, and still lists its projects the caller sees", async () => {
    show({ tenant: new ConnectError("denied", Code.PermissionDenied) });
    expect(await screen.findByText(/This account's roles do not include reading the tenant/)).toBeTruthy();
    expect(screen.queryByText("/ 10")).toBeNull();
    expect(await screen.findByText("Web")).toBeTruthy();
  });

  it("states that no tenant of the name is visible, without claiming a missing role", async () => {
    show({ tenant: new ConnectError("no such tenant", Code.NotFound) });
    expect(await screen.findByText(/is visible to this account/)).toBeTruthy();
    expect(screen.queryByText(/roles do not include/)).toBeNull();
  });

  it("shows any other failure as the API's error", async () => {
    show({ tenant: new ConnectError("the apiserver is unavailable", Code.Unavailable) });
    expect(await screen.findByText("The tenant did not load")).toBeTruthy();
    expect(screen.queryByText(/roles do not include|is visible to/)).toBeNull();
  });

  it("shows no project count beside a failed project list", async () => {
    const a = fakeApi((r) => {
      r.service(TenantService, { getTenant: async () => ({ tenant: { metadata: { name: "acme" } } }), listTenants: async () => ({ items: [] }) });
      r.service(ProjectService, {
        listProjects: async () => {
          throw new ConnectError("down", Code.Unavailable);
        },
      });
      r.service(AccountService, { testPermissions: async () => ({ permissions: [] }) });
    });
    render(<TenantOverview />, { wrapper: withApi(a, { kind: "tenant", name: "acme" }) });
    expect(await screen.findByText("The projects did not load")).toBeTruthy();
    // The card's heading is the word alone: no count of 0 beside the error.
    expect(screen.getByRole("heading", { name: /^Projects/ }).textContent).toBe("Projects");
  });

  it("offers New project only to who may create one in the tenant", async () => {
    const a = show({ creatableIn: [] });
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    await screen.findByText("Web");
    expect(screen.queryByRole("button", { name: /New project/ })).toBeNull();
    cleanup();
    show();
    expect(await screen.findByRole("button", { name: /New project/ })).toBeTruthy();
  });
});

describe("NewProjectDialog", () => {
  function show(s: Setup = {}, tenant = "acme") {
    const a = api(s);
    render(<NewProjectDialog open tenant={tenant} onClose={() => {}} />, { wrapper: withApi(a, { kind: "tenant", name: tenant }) });
    return a;
  }
  const createButton = () => screen.getByRole("button", { name: "Create project" }) as HTMLButtonElement;
  const typeName = async (v: string) => fireEvent.change(await screen.findByPlaceholderText("My demo"), { target: { value: v } });

  it("suggests an ID from the display name, checks it, and creates the project in the tenant", async () => {
    const a = show();
    await typeName("Build agents");
    const id = screen.getByDisplayValue(/^build-agents-\d{6}$/) as HTMLInputElement;
    await screen.findByText(`${id.value} is free. An ID is global and never reused.`);
    expect(a.requests.find((c) => c.method === "CheckName")!.request).toMatchObject({ kind: "Project", name: id.value });
    expect(screen.getByText(`hangar create project ${id.value} --tenant acme --display-name 'Build agents'`)).toBeTruthy();
    await waitFor(() => expect(createButton().disabled).toBe(false));
    fireEvent.click(createButton());
    await waitFor(() => expect(a.calls).toContain("ApplyProject"));
    const req = a.requests.find((c) => c.method === "ApplyProject")!.request as ApplyProjectRequest;
    expect(req.createOnly).toBe(true);
    const sent = req.project!;
    expect(sent.metadata?.name).toBe(id.value);
    expect(sent.spec).toMatchObject({ tenant: "acme", displayName: "Build agents" });
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project", params: { project: id.value } }));
  });

  it("quotes the display name in the command so the shell expands nothing", async () => {
    show();
    await typeName("Bob's $HOME `x`");
    expect(screen.getByText(/--display-name 'Bob'\\''s \$HOME `x`'$/)).toBeTruthy();
  });

  it("keeps an ID typed in, and sends nothing for one that is taken", async () => {
    const a = show({ checkName: false });
    await typeName("Web");
    fireEvent.change(screen.getByDisplayValue(/^web-\d{6}$/), { target: { value: "Acme-Web" } });
    await typeName("Web two");
    expect(screen.getByDisplayValue("acme-web")).toBeTruthy();
    expect(await screen.findByText("acme-web is taken or was used before.")).toBeTruthy();
    expect(createButton().disabled).toBe(true);
    expect(a.calls).not.toContain("ApplyProject");
  });

  it("says the ID check failed, never that the ID is free, when CheckName fails", async () => {
    show({ checkName: "fail" });
    await typeName("Probe");
    expect(await screen.findByText(/Checking the ID failed: .*name service down/)).toBeTruthy();
    expect(screen.queryByText(/is free/)).toBeNull();
    expect(createButton().disabled).toBe(true);
  });

  it("states that the tenant holds as many projects as it may, and does not create, though the ID is free", async () => {
    show({ used: 5 });
    await typeName("Six");
    // The ID is checked and free: only the limit keeps the button disabled.
    await screen.findByText(/is free/);
    expect(screen.getByText("The tenant holds 5 of 5 projects.")).toBeTruthy();
    expect(createButton().disabled).toBe(true);
  });

  it("keeps the tenant it was opened for when that tenant is allowed but not first", async () => {
    show({ creatableIn: ["zeta", "empty"] }, "zeta");
    await waitFor(() => expect(screen.getAllByRole("option").length).toBe(2));
    expect((screen.getByRole("combobox") as HTMLSelectElement).value).toBe("zeta");
    await typeName("Six");
    expect(await screen.findByText(/--tenant zeta /)).toBeTruthy();
  });

  it("offers only the tenants the caller may create projects in, a tenant with no project among them", async () => {
    show({ creatableIn: ["zeta", "empty"] }, "acme");
    await waitFor(() => expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["Empty (empty)", "Zeta (zeta)"]));
    expect((screen.getByRole("combobox") as HTMLSelectElement).value).toBe("empty");
  });

  it("states that the caller may create projects in no tenant, and does not create", async () => {
    show({ creatableIn: [] });
    expect(await screen.findByText("This account's roles do not include creating projects in any tenant.")).toBeTruthy();
    await typeName("Six");
    expect(createButton().disabled).toBe(true);
  });

  it("does not create, and states no refusal, while the permission answer has not come", async () => {
    show({ permissions: "never" });
    await typeName("Six");
    await screen.findByText(/is free/);
    expect(createButton().disabled).toBe(true);
    expect(screen.queryByText(/roles do not include/)).toBeNull();
  });

  it("states no refusal when TestPermissions fails, and shows the failure", async () => {
    const a = show({ permissions: "fail" });
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    await typeName("Six");
    await screen.findByText(/is free/);
    expect(await screen.findByText("The permission check did not answer")).toBeTruthy();
    expect(screen.queryByText(/roles do not include/)).toBeNull();
    expect(createButton().disabled).toBe(true);
  });

  it("states that the name was taken after the check, and changes nothing, when the create-only apply is refused", async () => {
    const a = show({ taken: true });
    await typeName("Raced");
    await waitFor(() => expect(createButton().disabled).toBe(false));
    fireEvent.click(createButton());
    expect(await screen.findByText(/raced-\d{6} was taken after its name was checked\./)).toBeTruthy();
    expect((a.requests.find((c) => c.method === "ApplyProject")!.request as ApplyProjectRequest).createOnly).toBe(true);
    expect(navigate).not.toHaveBeenCalled();
  });
});

describe("ProjectPicker", () => {
  function show(s: Setup = {}) {
    const a = api(s);
    render(<ProjectPicker place={{ kind: "project", name: "acme-web" }} open onClose={() => {}} />, { wrapper: withApi(a, "acme-web") });
    return a;
  }
  const rows = () => screen.getAllByRole("row").slice(1).map((r) => r.textContent);

  it("says nothing of the projects while they load", () => {
    const a = fakeApi((r) => r.service(ProjectService, { listProjects: never }));
    render(<ProjectPicker place={{ kind: "project", name: "acme-web" }} open onClose={() => {}} />, { wrapper: withApi(a, "acme-web") });
    expect(screen.getByLabelText("Loading")).toBeTruthy();
    expect(screen.queryByText(/Nothing matches|sees no project/)).toBeNull();
  });

  it("lists each tenant with its projects under it, a tenant with none too, and marks where the console is", async () => {
    show();
    await screen.findByText("Agents");
    await waitFor(() => expect(rows()).toEqual(["AcmeTenantacme", "Agentsacme-agents", "Webacme-web", "EmptyTenantempty", "ZetaTenantzeta", "Researchzeta-lab"]));
    expect(screen.getByText("Web").closest("tr")!.getAttribute("aria-current")).toBe("page");
  });

  it("filters by display name or ID, keeping a match's tenant", async () => {
    show();
    await screen.findByText("Agents");
    const filter = screen.getByRole("textbox", { name: "Filter by name or ID" });
    fireEvent.change(filter, { target: { value: "resea" } });
    expect(rows()).toEqual(["ZetaTenantzeta", "Researchzeta-lab"]);
    fireEvent.change(filter, { target: { value: "zeta-l" } });
    expect(rows()).toEqual(["ZetaTenantzeta", "Researchzeta-lab"]);
  });

  it("opens a tenant's pages from its row, and a project from its row", async () => {
    show();
    fireEvent.click(await screen.findByText("Zeta"));
    expect(navigate).toHaveBeenCalledWith({ to: "/tenants/$tenant", params: { tenant: "zeta" } });
    fireEvent.click(screen.getByText("Agents"));
    expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project", params: { project: "acme-agents" } });
  });

  it("offers New project only when the caller may create one in some tenant", async () => {
    const a = show({ creatableIn: [] });
    await screen.findByText("Agents");
    await waitFor(() => expect(a.calls.filter((c) => c === "TestPermissions").length).toBeGreaterThanOrEqual(3));
    expect(screen.queryByRole("button", { name: /New project/ })).toBeNull();
    cleanup();
    show({ creatableIn: ["zeta"] });
    expect(await screen.findByRole("button", { name: /New project/ })).toBeTruthy();
  });
});

describe("Topbar", () => {
  it("links the tenant's crumb to its pages and the project's to its overview", async () => {
    render(<Topbar crumbs={["Members"]} />, { wrapper: withApi(api(), "acme-web") });
    expect((await screen.findByRole("link", { name: "Acme" })).getAttribute("href")).toBe("/tenants/acme");
    expect(screen.getByRole("link", { name: "Web" }).getAttribute("href")).toBe("/projects/acme-web");
  });
});
