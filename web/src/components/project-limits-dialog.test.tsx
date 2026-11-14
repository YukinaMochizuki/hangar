// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { ProjectService, type ApplyProjectRequest, type Project } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { clone, create } from "@bufbuild/protobuf";
import { LimitsSchema } from "@hangar/api/hangar/tenancy/v1alpha1/plan_pb";
import { ProjectSchema } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it } from "vitest";
import { EditLimitsButton } from "@/components/project-limits-dialog";
import { fakeApi, polyfillDialog, withApi, type Api } from "@/test/harness";

beforeAll(polyfillDialog);
afterEach(cleanup);

const opened = create(ProjectSchema, {
  metadata: { name: "web", resourceVersion: 7n },
  spec: { tenant: "acme", displayName: "Web", limits: { cpu: "2", machines: 3 } },
  status: { usage: { machines: 1, cpu: "1", memory: "2Gi", disk: "10Gi" } },
});

type Setup = {
  /** The caller holds apply on projects at the tenant. */
  may?: boolean;
  /** What each ApplyProject answers, in turn; the last stays. */
  applies?: (ConnectError | "ok")[];
  /** What GetProject reads after a refused apply. */
  fresh?: Project;
};

function api(s: Setup): Api {
  let n = 0;
  return fakeApi((r) => {
    r.service(AccountService, {
      testPermissions: async (req) => ({ permissions: s.may ? req.permissions : [] }),
    });
    r.service(TenantService, {
      getTenant: async () => ({ tenant: { metadata: { name: "acme" }, status: { limits: { cpu: "8", memory: "16Gi", machines: 10 } } } }),
    });
    r.service(ProjectService, {
      getProject: async () => ({ project: s.fresh ?? opened }),
      applyProject: async (req) => {
        const a = (s.applies ?? ["ok"])[Math.min(n++, (s.applies ?? ["ok"]).length - 1)];
        if (a !== "ok") throw a;
        return { project: req.project, created: false };
      },
    });
  });
}

/** The project as read anew at the version, changed by edit. */
function readAt(version: bigint, edit: (p: Project) => void): Project {
  const p = clone(ProjectSchema, opened);
  p.metadata!.resourceVersion = version;
  edit(p);
  return p;
}

const stale = () => new ConnectError("Project web is at resource version 8, not 7", Code.Aborted);

function show(s: Setup) {
  const a = api(s);
  render(<EditLimitsButton project={opened} />, { wrapper: withApi(a, { kind: "tenant", name: "acme" }) });
  return a;
}

async function open() {
  fireEvent.click(await screen.findByRole("button", { name: "Edit limits of web" }));
  await screen.findByText("Limits of Web");
  // The plan has come once its limit is shown beside a field.
  await screen.findByText(/plan 8/);
}

const applyButton = () => screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement;
const applied = (a: Api) => a.requests.filter((c) => c.method === "ApplyProject").map((c) => (c.request as ApplyProjectRequest).project!);

describe("Edit limits", () => {
  it("is not drawn for someone who may not change the tenant's projects", async () => {
    const a = show({ may: false });
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("button", { name: /Edit limits/ })).toBeNull();
  });

  it("applies the project as it opened, with the limits edited alone changed, at its resource version", async () => {
    const a = show({ may: true });
    await open();
    expect(applyButton().disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("vCPU"), { target: { value: "4" } });
    fireEvent.change(screen.getByLabelText("Memory"), { target: { value: "8" } });
    fireEvent.change(screen.getByLabelText("Machines"), { target: { value: "" } });
    expect(screen.getByText("Machines 3 → plan's · vCPU 2 → 4 · Memory plan's → 8 GiB")).toBeTruthy();
    expect(screen.getByText(/hangar project set-limits web --cpu 4 --memory 8Gi --unset machines/)).toBeTruthy();
    fireEvent.click(applyButton());
    await waitFor(() => expect(screen.queryByText("Limits of Web")).toBeNull());
    const [p] = applied(a);
    expect(p.metadata?.resourceVersion).toBe(7n);
    expect(p.spec?.displayName).toBe("Web");
    expect(p.spec?.limits).toMatchObject({ cpu: "4", memory: "8Gi" });
    expect(p.spec?.limits?.machines).toBeUndefined();
  });

  it("refuses a limit above the plan's before anything is sent", async () => {
    const a = show({ may: true });
    await open();
    fireEvent.change(screen.getByLabelText("vCPU"), { target: { value: "16" } });
    expect(screen.getByText("vCPU 16 is above the plan's 8.")).toBeTruthy();
    expect(applyButton().disabled).toBe(true);
    expect(a.calls).not.toContain("ApplyProject");
  });

  it("makes the change again on the project read anew when only its status moved", async () => {
    const moved = readAt(8n, (p) => (p.spec!.displayName = "Web app"));
    const a = show({ may: true, applies: [stale(), "ok"], fresh: moved });
    await open();
    fireEvent.change(screen.getByLabelText("vCPU"), { target: { value: "4" } });
    fireEvent.click(applyButton());
    await waitFor(() => expect(screen.queryByText("Limits of Web")).toBeNull());
    const [first, second] = applied(a);
    expect(first.metadata?.resourceVersion).toBe(7n);
    expect(second.metadata?.resourceVersion).toBe(8n);
    // What changed meanwhile is kept.
    expect(second.spec?.displayName).toBe("Web app");
    expect(second.spec?.limits?.cpu).toBe("4");
  });

  it("says when the limits themselves changed meanwhile, and starts again from them on Reload", async () => {
    const changed = readAt(8n, (p) => (p.spec!.limits = create(LimitsSchema, { cpu: "6" })));
    const a = show({ may: true, applies: [stale(), "ok"], fresh: changed });
    await open();
    fireEvent.change(screen.getByLabelText("vCPU"), { target: { value: "4" } });
    fireEvent.click(applyButton());
    expect(await screen.findByText("The limits of web changed since this dialog opened.")).toBeTruthy();
    expect(applied(a)).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect((screen.getByLabelText("vCPU") as HTMLInputElement).value).toBe("6"));
    expect((screen.getByLabelText("Machines") as HTMLInputElement).value).toBe("");
    expect(screen.queryByText(/changed since this dialog opened/)).toBeNull();
  });

  it("shows the API's refusal in the dialog", async () => {
    show({ may: true, applies: [new ConnectError("user:bob may not change spec.limits of project \"web\"", Code.PermissionDenied)] });
    await open();
    fireEvent.change(screen.getByLabelText("vCPU"), { target: { value: "4" } });
    fireEvent.click(applyButton());
    expect(await screen.findByText("Changing the limits failed")).toBeTruthy();
    expect(screen.getByText(/may not change spec.limits/)).toBeTruthy();
  });
});
