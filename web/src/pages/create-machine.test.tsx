// @vitest-environment jsdom

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";
import { ContainerSchema, ContainerService, type Container } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { MachineService } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import {
  VirtualMachineSchema,
  VirtualMachineService,
  type VirtualMachine,
} from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { NetworkService } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { SecurityGroupService } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { ImageService } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CreateMachine } from "@/pages/create-machine";
import { deferred, fakeApi, withApi } from "@/test/harness";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  // The top bar's crumbs are links; the page is rendered without a router.
  Link: ({ children, className }: { children: ReactNode; className?: string }) => <a className={className}>{children}</a>,
}));

const container = (name: string) => create(ContainerSchema, { metadata: { name, project: "demo" }, spec: { image: "nginx:1" } });
const vm = (name: string) => create(VirtualMachineSchema, { metadata: { name, project: "demo" }, spec: {} });

type Fake = {
  /** Answers ListContainers; the test may change it between calls. */
  containers: () => Promise<{ items: Container[] }>;
  vms?: () => Promise<{ items: VirtualMachine[] }>;
  /** Names that exist although the list lacked them, made in between. */
  existing?: string[];
  /** What every apply fails with, when set. */
  fail?: ConnectError;
};

/** Applies as the API does: create-only refuses an existing name. */
function apply<T extends Container | VirtualMachine>(f: Fake, kind: string, obj: T | undefined, createOnly: boolean): { obj?: T; created: boolean } {
  const name = obj?.metadata?.name ?? "";
  if (f.fail) throw f.fail;
  const exists = f.existing?.includes(name) ?? false;
  if (exists && createOnly) throw new ConnectError(`${kind} ${name} already exists`, Code.AlreadyExists);
  return { obj, created: !exists };
}

/** An API with a project, a network, an image and what f says. */
function api(f: Fake) {
  return fakeApi((r: ConnectRouter) => {
    r.service(ContainerService, {
      listContainers: () => f.containers(),
      applyContainer: (req) => {
        const res = apply(f, "Container", req.container, req.createOnly);
        return { container: res.obj, created: res.created };
      },
    });
    r.service(VirtualMachineService, {
      listVirtualMachines: () => (f.vms ? f.vms() : { items: [] }),
      applyVirtualMachine: (req) => {
        const res = apply(f, "VirtualMachine", req.virtualMachine, req.createOnly);
        return { virtualMachine: res.obj, created: res.created };
      },
    });
    r.service(MachineService, { listMachines: () => ({ items: [] }) });
    r.service(ProjectService, {
      getProject: () => ({ project: { metadata: { name: "demo" }, status: { limits: {}, usage: {} } } }),
      listProjects: () => ({ items: [] }),
    });
    r.service(TenantService, { listTenants: () => ({ items: [] }) });
    r.service(NetworkService, { listNetworks: () => ({ items: [{ metadata: { name: "default" }, spec: { ipv4Cidr: "10.0.0.0/24" } }] }) });
    r.service(SecurityGroupService, { listSecurityGroups: () => ({ items: [] }) });
    r.service(ImageService, { listImages: () => ({ items: [{ metadata: { name: "debian-13" }, spec: { os: "debian", version: "13" } }] }) });
  });
}

function fill(name: string, image = true) {
  if (image) fireEvent.change(screen.getByLabelText("Image"), { target: { value: "nginx:1" } });
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: name } });
}

const submitButton = () =>
  screen.getByRole("button", { name: /Create container|Create virtual machine|Loading names/ }) as HTMLButtonElement;

/** Submits the form as Enter would, whatever the button's state. */
const submit = () => fireEvent.submit(submitButton().closest("form")!);

const writeMethods = ["GetContainer", "ApplyContainer", "GetVirtualMachine", "ApplyVirtualMachine"];

/** The create's calls, with what they asked for. */
function writes(a: ReturnType<typeof api>) {
  return a.requests
    .filter((c) => writeMethods.includes(c.method))
    .map((c) => {
      const r = c.request as {
        project?: string;
        name?: string;
        container?: Container;
        virtualMachine?: VirtualMachine;
        createOnly?: boolean;
      };
      const meta = r.container?.metadata ?? r.virtualMachine?.metadata;
      return { method: c.method, project: r.project ?? meta?.project, name: r.name ?? meta?.name, createOnly: r.createOnly };
    });
}

/**
 * Sends an allowed create of name and waits until it is done: the control
 * that a blocked submit before it sent nothing. Calls are recorded in the
 * order they are sent, and a blocked submit came first, so had it sent
 * anything, it would stand before this create's Apply.
 */
async function createAllowed(a: ReturnType<typeof api>, name: string) {
  fill(name);
  await waitFor(() => expect(submitButton().disabled).toBe(false));
  submit();
  await waitFor(() => expect(navigate).toHaveBeenCalled());
  expect(writes(a)).toEqual([{ method: "ApplyContainer", project: "demo", name, createOnly: true }]);
}

beforeEach(() => navigate.mockReset());
afterEach(cleanup);

describe("CreateMachine", () => {
  it("sends nothing while the project's containers load", async () => {
    const list = deferred<{ items: Container[] }>();
    const a = api({ containers: () => list.promise });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(a) });
    fill("web");
    await screen.findByText("Loading names…");
    expect(submitButton().disabled).toBe(true);
    submit();
    list.resolve({ items: [] });
    await createAllowed(a, "web");
  });

  it("says the list failed, and sends nothing", async () => {
    let containers = () => Promise.reject<{ items: Container[] }>(new ConnectError("the apiserver is unavailable", Code.Unavailable));
    const a = api({ containers: () => containers() });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(a) });
    fill("web");
    await screen.findByText("The list of the project's containers did not load");
    expect(screen.getByText("the apiserver is unavailable")).toBeTruthy();
    expect(submitButton().disabled).toBe(true);
    submit();
    // The list loads when asked again.
    containers = async () => ({ items: [] });
    await a.client.refetchQueries();
    await createAllowed(a, "web");
  });

  it("refuses a name the list holds", async () => {
    const a = api({ containers: async () => ({ items: [container("web")] }) });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(a) });
    fill("web");
    // Under the name, and again by the button.
    await screen.findAllByText("A container named web exists in this project.");
    expect(submitButton().disabled).toBe(true);
    submit();
    await createAllowed(a, "api");
  });

  it("creates a name the list lacks with one create-only Apply, then shows the machine's page", async () => {
    const a = api({ containers: async () => ({ items: [container("web")] }) });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(a) });
    await createAllowed(a, "api");
    expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/containers/$name", params: { project: "demo", name: "api" } });
  });

  it("creates a VM with one create-only Apply", async () => {
    const a = api({ containers: async () => ({ items: [] }), vms: async () => ({ items: [vm("web")] }) });
    render(<CreateMachine kind="vm" size="s" />, { wrapper: withApi(a) });
    await screen.findByText("debian");
    fill("box", false);
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    submit();
    await waitFor(() => expect(navigate).toHaveBeenCalled());
    expect(writes(a)).toEqual([{ method: "ApplyVirtualMachine", project: "demo", name: "box", createOnly: true }]);
    expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/vms/$name", params: { project: "demo", name: "box" } });
  });

  it("says a container made since the list loaded holds the name, when the API refuses it", async () => {
    const a = api({ containers: async () => ({ items: [] }), existing: ["api"] });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(a) });
    fill("api");
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    submit();
    await screen.findByText("Creating the container failed");
    expect(screen.getByText("A container named api exists in this project.")).toBeTruthy();
    expect(screen.queryByText("Container api already exists")).toBeNull();
    expect(writes(a)).toEqual([{ method: "ApplyContainer", project: "demo", name: "api", createOnly: true }]);
    expect(navigate).not.toHaveBeenCalled();
  });

  it("shows any other refusal as the API gave it", async () => {
    const a = api({ containers: async () => ({ items: [] }), fail: new ConnectError("quota exceeded for cpu", Code.ResourceExhausted) });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(a) });
    fill("api");
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    submit();
    await screen.findByText("Creating the container failed");
    expect(screen.getByText("quota exceeded for cpu")).toBeTruthy();
    expect(navigate).not.toHaveBeenCalled();
  });

  it("offers the command that does what the form does", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(a) });
    fill("api");
    await screen.findByText((_, el) => el?.textContent === "$ hangar apply --create-only -p demo -f api.yaml");
  });

  it("says a VM made since the list loaded holds the name, when the API refuses it", async () => {
    const a = api({ containers: async () => ({ items: [] }), existing: ["box"] });
    render(<CreateMachine kind="vm" size="s" />, { wrapper: withApi(a) });
    await screen.findByText("debian");
    fill("box", false);
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    submit();
    await screen.findByText("Creating the virtual machine failed");
    expect(screen.getByText("A virtual machine named box exists in this project.")).toBeTruthy();
    expect(navigate).not.toHaveBeenCalled();
  });
});
