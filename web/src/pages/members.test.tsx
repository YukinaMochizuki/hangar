// @vitest-environment jsdom

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { AccountService, WhoAmIResponseSchema } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { MemberService, MemberSchema, type Member } from "@hangar/api/hangar/iam/v1alpha1/member_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import type { Place } from "@/components/shell";
import { MemberProvider } from "@/lib/session";
import { ProjectMembers, TenantMembers } from "@/pages/members";
import { fakeApi, polyfillDialog, withApi, type Api } from "@/test/harness";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  // The pages are rendered without a router; a link is its text and target.
  Link: ({ children, to, params }: { children: ReactNode; to: string; params?: Record<string, string> }) => (
    <a href={Object.entries(params ?? {}).reduce((p, [k, v]) => p.replace(`$${k}`, v), to)}>{children}</a>
  ),
}));

beforeAll(polyfillDialog);
afterEach(cleanup);

const member = (email: string, role: string, scope: "project" | "tenant", extra: { username?: string; platformMember?: boolean } = {}) =>
  create(MemberSchema, {
    email,
    role,
    username: email.split("@")[0],
    platformMember: true,
    scope: { target: { case: scope, value: scope === "project" ? "demo" : "acme" } },
    ...extra,
  });

type Setup = {
  project?: Member[];
  tenant?: Member[] | ConnectError;
  /** What TestPermissions says the caller holds, at every scope. */
  holds?: string[];
  addMember?: () => Promise<object>;
  setMemberRole?: () => Promise<object>;
  removeMember?: () => Promise<object>;
  /** TestPermissions fails instead of answering. */
  permissionsFail?: boolean;
};

function show(page: "project" | "tenant", s: Setup = {}): Api {
  const a = fakeApi((r) => {
    r.service(MemberService, {
      listMembers: async (req) => {
        if (req.scope?.target.case === "tenant") {
          if (s.tenant instanceof ConnectError) throw s.tenant;
          return { members: s.tenant ?? [] };
        }
        return { members: s.project ?? [] };
      },
      addMember: s.addMember ?? (async () => ({})),
      setMemberRole: s.setMemberRole ?? (async () => ({})),
      removeMember: s.removeMember ?? (async () => ({})),
    });
    r.service(AccountService, {
      testPermissions: async (req) => {
        if (s.permissionsFail) throw new ConnectError("unavailable", Code.Unavailable);
        return { permissions: req.permissions.filter((p) => (s.holds ?? []).includes(p)) };
      },
    });
    r.service(ProjectService, {
      listProjects: async () => ({ items: [{ metadata: { name: "demo" }, spec: { tenant: "acme", displayName: "Demo" } }] }),
    });
    r.service(TenantService, {
      listTenants: async () => ({ items: [{ metadata: { name: "acme" }, spec: { displayName: "Acme" } }] }),
    });
  });
  const place: Place = page === "project" ? { kind: "project", name: "demo" } : { kind: "tenant", name: "acme" };
  const Api = withApi(a, place);
  const who = create(WhoAmIResponseSchema, { type: "user", id: "ada@example.com", member: true });
  render(page === "project" ? <ProjectMembers /> : <TenantMembers />, {
    wrapper: ({ children }) => (
      <Api>
        <MemberProvider value={who}>{children}</MemberProvider>
      </Api>
    ),
  });
  return a;
}

const manage = ["iam.rolebindings.apply", "iam.rolebindings.delete"];

describe("ProjectMembers", () => {
  it("lists the project's people, marks the caller, and states who is not a platform member yet", async () => {
    show("project", {
      project: [
        member("ada@example.com", "project.owner", "project"),
        member("carol@example.com", "project.viewer", "project", { username: "", platformMember: false }),
      ],
    });
    await screen.findByText("carol@example.com");
    const ada = screen.getByText("ada@example.com").closest("tr")!;
    expect(within(ada).getByText("You")).toBeTruthy();
    const carol = screen.getByText("carol@example.com").closest("tr")!;
    expect(within(carol).getByText("Not signed up yet")).toBeTruthy();
    // The hint never says whether the person was invited.
    expect(document.body.textContent).not.toMatch(/invit/i);
  });

  it("shows roles as text and no controls when the caller may not write bindings", async () => {
    const a = show("project", { project: [member("bob@example.com", "project.editor", "project")], holds: [] });
    await screen.findByText("bob@example.com");
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.getByText("Editor")).toBeTruthy();
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("button", { name: /Add member/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Actions" })).toBeNull();
  });

  it("asks TestPermissions about the project for the binding write", async () => {
    const a = show("project", { holds: manage });
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    const asked = a.requests.find((c) => c.method === "TestPermissions")!.request as { scope: { target: { case: string; value: string } }; permissions: string[] };
    expect(asked.scope.target).toEqual({ case: "project", value: "demo" });
    expect(asked.permissions).toEqual(["iam.rolebindings.apply", "iam.rolebindings.delete"]);
  });

  it("adds a member with the role chosen, at the project", async () => {
    const a = show("project", { holds: manage });
    fireEvent.click(await screen.findByRole("button", { name: /Add member/ }));
    fireEvent.change(screen.getByPlaceholderText("name@example.com"), { target: { value: " Dan@Example.com " } });
    fireEvent.click(screen.getByRole("radio", { name: /Viewer/ }));
    expect(screen.getByText(/hangar member add dan@example.com --role project.viewer --project demo/)).toBeTruthy();
    fireEvent.click(screen.getAllByRole("button", { name: "Add member" }).at(-1)!);
    await waitFor(() => expect(a.calls).toContain("AddMember"));
    const req = a.requests.find((c) => c.method === "AddMember")!.request as { email: string; role: string; scope: { target: { case: string; value: string } } };
    expect(req.email).toBe("dan@example.com");
    expect(req.role).toBe("project.viewer");
    expect(req.scope.target).toEqual({ case: "project", value: "demo" });
  });

  it("refuses a non-ASCII email rather than lower-casing it into another address", async () => {
    show("project", { holds: manage });
    fireEvent.click(await screen.findByRole("button", { name: /Add member/ }));
    const field = screen.getByPlaceholderText("name@example.com") as HTMLInputElement;
    // A text field: an email field's domain is IDNA-converted by the
    // browser before the check could see it.
    expect(field.type).toBe("text");
    expect(field.inputMode).toBe("email");
    // U+212A KELVIN SIGN, which lower-cases to k, in the local part and in
    // the domain; and a domain past ASCII.
    for (const value of ["\u212aate@example.com", "kate@\u212aexample.com", "gil@ex\u00e4mple.com"]) {
      fireEvent.change(field, { target: { value } });
      expect(screen.getByText("ASCII email addresses only.")).toBeTruthy();
      expect((screen.getAllByRole("button", { name: "Add member" }).at(-1) as HTMLButtonElement).disabled).toBe(true);
      expect(screen.queryByText(/kate@(k?)example.com/)).toBeNull();
    }
  });

  it("shows the API's refusal of an add in the dialog", async () => {
    show("project", { holds: manage, addMember: () => Promise.reject(new ConnectError("dan@example.com already holds a role here", Code.AlreadyExists)) });
    fireEvent.click(await screen.findByRole("button", { name: /Add member/ }));
    fireEvent.change(screen.getByPlaceholderText("name@example.com"), { target: { value: "dan@example.com" } });
    fireEvent.click(screen.getAllByRole("button", { name: "Add member" }).at(-1)!);
    expect(await screen.findByText("Adding failed")).toBeTruthy();
    expect(screen.getByText(/already holds a role here/)).toBeTruthy();
  });

  it("changes a role with SetMemberRole, and shows a refusal under the person's row", async () => {
    const a = show("project", {
      project: [member("bob@example.com", "project.editor", "project")],
      holds: manage,
      setMemberRole: () => Promise.reject(new ConnectError("the tenant would have no owner", Code.FailedPrecondition)),
    });
    const select = await screen.findByRole("combobox", { name: "Role of bob@example.com" });
    fireEvent.change(select, { target: { value: "project.owner" } });
    await waitFor(() => expect(a.calls).toContain("SetMemberRole"));
    const req = a.requests.find((c) => c.method === "SetMemberRole")!.request as { email: string; role: string };
    expect(req).toMatchObject({ email: "bob@example.com", role: "project.owner" });
    const failed = await screen.findByText("Changing the role failed");
    // The refusal is the row under the person's.
    const under = screen.getByText("bob@example.com").closest("tr")!.nextElementSibling as HTMLElement;
    expect(under.contains(failed)).toBe(true);
    expect(within(under).getByText(/the tenant would have no owner/)).toBeTruthy();
  });

  it("filters the people by email or username", async () => {
    show("project", {
      project: [member("bob@example.com", "project.editor", "project", { username: "builder" }), member("carol@example.com", "project.viewer", "project")],
    });
    const filter = await screen.findByRole("textbox", { name: "Filter by email or username" });
    fireEvent.change(filter, { target: { value: "build" } });
    expect(screen.getByText("bob@example.com")).toBeTruthy();
    expect(screen.queryByText("carol@example.com")).toBeNull();
    fireEvent.change(filter, { target: { value: "CAROL@" } });
    expect(screen.getByText("carol@example.com")).toBeTruthy();
    expect(screen.queryByText("bob@example.com")).toBeNull();
    fireEvent.change(filter, { target: { value: "dan" } });
    expect(screen.getByText("No member matches.")).toBeTruthy();
  });

  it("removes a member after saying what ends and what stays", async () => {
    const a = show("project", { project: [member("bob@example.com", "project.editor", "project")], holds: manage });
    fireEvent.click(await screen.findByRole("button", { name: "Actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: /Remove/ }));
    expect(screen.getByText(/Their Editor role in Demo ends\. A role they hold in the tenant stays\./)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Remove member/ }));
    await waitFor(() => expect(a.calls).toContain("RemoveMember"));
    expect(a.requests.find((c) => c.method === "RemoveMember")!.request).toMatchObject({ email: "bob@example.com" });
  });

  it("offers changing roles to who may apply bindings, and removing only to who may delete them", async () => {
    show("project", { project: [member("bob@example.com", "project.editor", "project")], holds: ["iam.rolebindings.apply"] });
    expect(await screen.findByRole("combobox", { name: "Role of bob@example.com" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Actions" })).toBeNull();
    cleanup();
    show("project", { project: [member("bob@example.com", "project.editor", "project")], holds: ["iam.rolebindings.delete"] });
    expect(await screen.findByRole("button", { name: "Actions" })).toBeTruthy();
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("button", { name: /Add member/ })).toBeNull();
  });

  it("shows no controls, and states nothing about them, when TestPermissions fails", async () => {
    const a = show("project", { project: [member("bob@example.com", "project.editor", "project")], permissionsFail: true });
    await screen.findByText("bob@example.com");
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("button", { name: /Add member|Actions/ })).toBeNull();
    expect(document.body.textContent).not.toMatch(/roles do not include/);
  });

  it("shows the API's refusal of a remove in the dialog", async () => {
    show("project", {
      project: [member("ada@example.com", "tenant.owner", "project")],
      holds: manage,
      removeMember: () => Promise.reject(new ConnectError("the tenant would have no owner", Code.FailedPrecondition)),
    });
    fireEvent.click(await screen.findByRole("button", { name: "Actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: /Remove/ }));
    fireEvent.click(screen.getByRole("button", { name: /Remove member/ }));
    expect(await screen.findByText("Removing failed")).toBeTruthy();
    expect(screen.getByText(/the tenant would have no owner/)).toBeTruthy();
  });

  it("states how many people hold a role through the tenant, linking to its members", async () => {
    show("project", { tenant: [member("ada@example.com", "tenant.owner", "tenant"), member("dan@example.com", "tenant.viewer", "tenant")] });
    const line = await screen.findByText(/2 people hold a role here through the tenant/);
    expect(within(line).getByRole("link", { name: "Acme" }).getAttribute("href")).toBe("/tenants/acme/members");
  });

  it("states that tenant roles hold here when the caller may not list them", async () => {
    show("project", { tenant: new ConnectError("denied", Code.PermissionDenied) });
    const line = await screen.findByText("Roles held in the tenant Acme also hold here.");
    expect(within(line).queryByRole("link")).toBeNull();
  });
});

describe("TenantMembers", () => {
  it("lists the tenant's people and offers only tenant roles", async () => {
    show("tenant", { tenant: [member("ada@example.com", "tenant.owner", "tenant")], holds: manage });
    const select = await screen.findByRole("combobox", { name: "Role of ada@example.com" });
    const options = within(select).getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual(["Owner", "Viewer"]);
  });

  it("states that the caller's roles do not include listing the members", async () => {
    show("tenant", { tenant: new ConnectError("denied", Code.PermissionDenied), holds: manage });
    expect(await screen.findByText("This account's roles do not include listing the members here.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Add member/ })).toBeNull();
  });
});
