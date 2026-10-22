// @vitest-environment jsdom

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";
import { ContainerSchema, ContainerService, RestartPolicy, type Container } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { MachineService } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { MachineSizeSchema, MachineSizeService, type MachineSize } from "@hangar/api/hangar/compute/v1alpha1/machine_size_pb";
import {
  VirtualMachineSchema,
  VirtualMachineService,
  type VirtualMachine,
} from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { NetworkService } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { SecurityGroupService } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { ImageSchema, ImageService, type Image } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createdKey, isJustCreated } from "@/lib/just-created";
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

/** A made-up size; a platform's real sizes are its administrator's. */
const size = (name: string, displayName: string, cpu: string, memory: string, disk: string, order: number) =>
  create(MachineSizeSchema, { metadata: { name }, spec: { displayName, cpu, memory, disk, order } });

/** Listed out of order, as the API lists them by name. */
const madeUpSizes = [
  size("l", "L", "4", "8Gi", "40Gi", 30),
  size("s", "S", "1", "2Gi", "10Gi", 10),
  size("m", "M", "2", "4Gi", "20Gi", 20),
  size("xs", "XS", "1", "512Mi", "5Gi", 0),
];

type Fake = {
  /** Answers ListContainers; the test may change it between calls. */
  containers: () => Promise<{ items: Container[] }>;
  vms?: () => Promise<{ items: VirtualMachine[] }>;
  /** Names that exist although the list lacked them, made in between. */
  existing?: string[];
  /** What every apply fails with, when set. */
  fail?: ConnectError;
  /** The platform's sizes; four made-up ones unless set. The test may change it between calls. */
  sizes?: MachineSize[];
  /** Makes ListMachineSizes fail; the test may clear it between calls. */
  sizesFail?: boolean;
  /** Answers ListMachineSizes instead, when set. */
  sizesAnswer?: () => Promise<{ items: MachineSize[] }>;
  /** The project's limits and usage; none unless set. */
  quota?: { limits: { cpu?: string; memory?: string; disk?: string }; usage: { cpu?: string; memory?: string; disk?: string } };
  /** The platform's images; debian-13, of no stated smallest disk, unless set. */
  images?: Image[];
};

const catalogImage = (name: string, os: string, minDiskSize = "") => create(ImageSchema, { metadata: { name }, spec: { os, version: "13", minDiskSize } });

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
    r.service(MachineSizeService, {
      listMachineSizes: () => {
        if (f.sizesAnswer) return f.sizesAnswer();
        if (f.sizesFail) throw new ConnectError("the apiserver is unavailable", Code.Unavailable);
        return { items: f.sizes ?? madeUpSizes };
      },
    });
    r.service(ProjectService, {
      getProject: () => ({ project: { metadata: { name: "demo" }, status: f.quota ?? { limits: {}, usage: {} } } }),
      listProjects: () => ({ items: [] }),
    });
    r.service(TenantService, { listTenants: () => ({ items: [] }) });
    r.service(NetworkService, { listNetworks: () => ({ items: [{ metadata: { name: "default" }, spec: { ipv4Cidr: "10.0.0.0/24" } }] }) });
    r.service(SecurityGroupService, { listSecurityGroups: () => ({ items: [] }) });
    r.service(ImageService, { listImages: () => ({ items: f.images ?? [catalogImage("debian-13", "debian")] }) });
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

/** The resources the create's Apply sent. */
function sentResources(a: ReturnType<typeof api>) {
  const apply = a.requests.find((c) => c.method === "ApplyContainer" || c.method === "ApplyVirtualMachine");
  const r = apply?.request as { container?: Container; virtualMachine?: VirtualMachine } | undefined;
  if (r?.container) return { ...r.container.spec?.resources };
  const root = r?.virtualMachine?.spec?.rootDisk?.source;
  return { ...r?.virtualMachine?.spec?.resources, disk: root?.case === "fromImage" ? root.value.size : undefined };
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
    // The page it opens plays the machine's arrival once.
    expect(isJustCreated(createdKey("demo", "Container", "api"))).toBe(true);
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
    expect(isJustCreated(createdKey("demo", "VirtualMachine", "box"))).toBe(true);
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

  it("offers the platform's sizes, by their order, and sends the one chosen", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" />, { wrapper: withApi(a) });
    const cards = await screen.findAllByRole("radio", { name: /^Size / });
    expect(cards.map((c) => c.getAttribute("aria-label"))).toEqual(["Size XS", "Size S", "Size M", "Size L", "Size Custom"]);
    // None asked for: the first is chosen.
    expect(cards[0].getAttribute("aria-checked")).toBe("true");
    fireEvent.click(screen.getByRole("radio", { name: "Size M" }));
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ cpu: "2", memory: "4Gi", disk: "20Gi" });
  });

  it("takes the size ?size= names, as a VM's resources and root disk", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="vm" size="l" />, { wrapper: withApi(a) });
    await screen.findByText("debian");
    await waitFor(() => expect(screen.getByRole("radio", { name: "Size L" }).getAttribute("aria-checked")).toBe("true"));
    fill("box", false);
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    submit();
    await waitFor(() => expect(navigate).toHaveBeenCalled());
    expect(sentResources(a)).toMatchObject({ cpu: "4", memory: "8Gi", disk: "40Gi" });
  });

  it("chooses the first size when ?size= names one the platform does not list", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="xl" />, { wrapper: withApi(a) });
    await waitFor(() => expect(screen.getByRole("radio", { name: "Size XS" }).getAttribute("aria-checked")).toBe("true"));
  });

  it("with no size defined, offers a shape of one's own, and sends it", async () => {
    const f: Fake = { containers: async () => ({ items: [] }), sizes: [] };
    const a = api(f);
    render(<CreateMachine kind="container" size="m" />, { wrapper: withApi(a) });
    const cards = await screen.findAllByRole("radio", { name: /^Size / });
    expect(cards.map((c) => c.getAttribute("aria-label"))).toEqual(["Size Custom"]);
    expect(cards[0].getAttribute("aria-checked")).toBe("true");
    expect(memoryField().value).toBe("1");
    expect(cpuField().value).toBe("1");
    typeMemory("0.5");
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ cpu: "1", memory: "512Mi", disk: "10Gi" });
  });

  it("says the sizes did not load, and sends nothing", async () => {
    const f: Fake = { containers: async () => ({ items: [] }), sizesFail: true };
    const a = api(f);
    render(<CreateMachine kind="container" />, { wrapper: withApi(a) });
    await screen.findByText("The machine sizes did not load");
    expect(screen.getByText("the apiserver is unavailable")).toBeTruthy();
    fill("api");
    expect(submitButton().disabled).toBe(true);
    submit();
    // The sizes load when asked again.
    f.sizesFail = false;
    await a.client.refetchQueries();
    await createAllowed(a, "api");
  });

  it("says the sizes are loading, and sends nothing until they are", async () => {
    const sizes = deferred<{ items: MachineSize[] }>();
    const a = api({ containers: async () => ({ items: [] }), sizesAnswer: () => sizes.promise });
    render(<CreateMachine kind="container" />, { wrapper: withApi(a) });
    fill("api");
    await screen.findByText(/The machine sizes are loading\./);
    expect(screen.queryAllByRole("radio", { name: /^Size / })).toEqual([]);
    expect(submitButton().disabled).toBe(true);
    submit();
    sizes.resolve({ items: madeUpSizes });
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ cpu: "1", memory: "512Mi", disk: "5Gi" });
  });

  it("marks a size past the project's limits, and creates nothing of it", async () => {
    const a = api({ containers: async () => ({ items: [] }), quota: { limits: { cpu: "2" }, usage: { cpu: "0" } } });
    render(<CreateMachine kind="container" size="l" />, { wrapper: withApi(a) });
    const large = await screen.findByRole("radio", { name: "Size L" });
    await waitFor(() => expect(large.textContent).toContain("Over quota"));
    expect(screen.getByRole("radio", { name: "Size M" }).textContent).not.toContain("Over quota");
    expect(large.getAttribute("aria-checked")).toBe("true");
    fill("api");
    await screen.findByText("Over the project's vCPU limit");
    expect(screen.getAllByText((_, el) => el?.textContent === "L needs 4 vCPU; 2 vCPU of 2 vCPU is left.").length).toBeGreaterThan(0);
    expect(submitButton().disabled).toBe(true);
    submit();
    // A size within the limits creates, and nothing went before it.
    fireEvent.click(screen.getByRole("radio", { name: "Size M" }));
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ cpu: "2", memory: "4Gi", disk: "20Gi" });
  });
});

const diskField = () => screen.getByLabelText("Disk, GiB") as HTMLInputElement;
const memoryField = () => screen.getByLabelText("Memory, GiB") as HTMLInputElement;
const cpuField = () => screen.getByLabelText("vCPUs") as HTMLInputElement;
const typeMemory = (v: string) => fireEvent.change(memoryField(), { target: { value: v } });
const typeCPU = (v: string) => fireEvent.change(cpuField(), { target: { value: v } });

describe("CreateMachine's custom size", () => {
  it("starts from the size chosen before it, and sends the shape typed", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="m" />, { wrapper: withApi(a) });
    await waitFor(() => expect(screen.getByRole("radio", { name: "Size M" }).getAttribute("aria-checked")).toBe("true"));
    expect(screen.queryByLabelText("Memory, GiB")).toBeNull();
    fireEvent.click(screen.getByRole("radio", { name: "Size Custom" }));
    expect(screen.getByRole("radio", { name: "Size Custom" }).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByRole("radio", { name: "Size M" }).getAttribute("aria-checked")).toBe("false");
    expect(memoryField().value).toBe("4");
    expect(cpuField().value).toBe("2");
    typeMemory("1.5");
    typeCPU("3");
    expect(screen.getByText("1.5 GiB of memory goes with 1 to 3 vCPUs.")).toBeTruthy();
    expect(manifest()).toContain("memory: 1536Mi");
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ cpu: "3", memory: "1536Mi", disk: "20Gi" });
  });

  it("refuses a shape outside the rules in place, and sends nothing", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(a) });
    fireEvent.click(await screen.findByRole("radio", { name: "Size Custom" }));
    fill("api");
    for (const [mem, cpu, why] of [
      ["1", "8", "1 GiB of memory goes with 1 to 2 vCPUs, not 8."],
      ["32", "1", "32 GiB of memory goes with 4 to 64 vCPUs, not 1."],
      ["0.3", "1", "Memory is a whole number of 256 MiB steps, at least one."],
      ["1", "", "The vCPUs are a whole number, at least 1."],
    ]) {
      typeMemory(mem!);
      typeCPU(cpu!);
      await screen.findAllByText(why!);
      await waitFor(() => expect(existingLoaded()).toBe(true));
      expect(submitButton().disabled).toBe(true);
      submit();
    }
    typeMemory("0.25");
    typeCPU("1");
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ cpu: "1", memory: "256Mi" });
  });

  it("does not offer a size stored before the rules that breaks them", async () => {
    const a = api({ containers: async () => ({ items: [] }), sizes: [size("nano", "Nano", "1", "128Mi", "1Gi", 0), size("wide", "Wide", "8", "1Gi", "10Gi", 5), ...madeUpSizes] });
    render(<CreateMachine kind="container" size="nano" />, { wrapper: withApi(a) });
    const cards = await screen.findAllByRole("radio", { name: /^Size / });
    expect(cards.map((c) => c.getAttribute("aria-label"))).toEqual(["Size XS", "Size S", "Size M", "Size L", "Size Custom"]);
    // Asked for by name, it is not chosen: the first offered is.
    expect(cards[0].getAttribute("aria-checked")).toBe("true");
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ cpu: "1", memory: "512Mi" });
  });

  it("goes back to a size when one is chosen", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" />, { wrapper: withApi(a) });
    fireEvent.click(await screen.findByRole("radio", { name: "Size Custom" }));
    typeCPU("9");
    fireEvent.click(screen.getByRole("radio", { name: "Size S" }));
    expect(screen.queryByLabelText("Memory, GiB")).toBeNull();
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ cpu: "1", memory: "2Gi", disk: "10Gi" });
  });
});
/** The project's machines of the kind have loaded, so the button speaks for the form. */
const existingLoaded = () => screen.queryByText("Loading names…") === null;
const typeDisk = (v: string) => fireEvent.change(diskField(), { target: { value: v } });
/** The equivalent manifest, as the page shows it. */
const manifest = () => screen.getByText(/^apiVersion:/).textContent ?? "";
/** The VM's image, chosen in the form as a person does. */
const chooseImage = async (os: string) => fireEvent.click(await screen.findByRole("radio", { name: new RegExp(os) }));

describe("CreateMachine's disk", () => {
  it("defaults to the chosen size's disk and follows the size until it is edited", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="m" />, { wrapper: withApi(a) });
    await waitFor(() => expect(diskField().value).toBe("20"));
    // A size's disk is where the field starts; the least is the rules'.
    expect(screen.getByText("At least 2 GiB")).toBeTruthy();
    fireEvent.click(screen.getByRole("radio", { name: "Size L" }));
    expect(diskField().value).toBe("40");
    typeDisk("50");
    fireEvent.click(screen.getByRole("radio", { name: "Size S" }));
    expect(diskField().value).toBe("50");
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ cpu: "1", memory: "2Gi", disk: "50Gi" });
  });

  it("sends a raised disk as the container's resources.disk, and the manifest and summary show it", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="m" />, { wrapper: withApi(a) });
    await waitFor(() => expect(diskField().value).toBe("20"));
    typeDisk("25");
    expect(manifest()).toContain("disk: 25Gi");
    expect(screen.getByText(/25 GiB disk/)).toBeTruthy();
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ cpu: "2", memory: "4Gi", disk: "25Gi" });
  });

  it("refuses a disk below 2 GiB in place, shows it as typed, and sends nothing; one below the size's is sent", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="m" />, { wrapper: withApi(a) });
    await waitFor(() => expect(diskField().value).toBe("20"));
    typeDisk("1");
    fill("api");
    await screen.findAllByText("A disk is at least 2 GiB; 1 GiB is less.");
    expect(manifest()).toContain("disk: 1Gi");
    expect(screen.getByText(/1 GiB disk/)).toBeTruthy();
    expect(diskField().getAttribute("aria-invalid")).toBe("true");
    await waitFor(() => expect(existingLoaded()).toBe(true));
    expect(submitButton().disabled).toBe(true);
    submit();
    typeDisk("15");
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ cpu: "2", memory: "4Gi", disk: "15Gi" });
  });

  it("refuses a disk that is not a whole number of GiB, and sends nothing", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="m" />, { wrapper: withApi(a) });
    await waitFor(() => expect(diskField().value).toBe("20"));
    for (const v of ["20.5", ""]) {
      typeDisk(v);
      fill("api");
      await screen.findAllByText("The disk is not a whole number of GiB.");
      expect(submitButton().disabled).toBe(true);
      submit();
    }
    typeDisk("21");
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ disk: "21Gi" });
  });

  it("sends a raised disk as the VM's rootDisk.fromImage.size, and the manifest shows it", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="vm" size="m" />, { wrapper: withApi(a) });
    await screen.findByText("debian");
    await waitFor(() => expect(diskField().value).toBe("20"));
    typeDisk("30");
    expect(manifest()).toContain("size: 30Gi");
    fill("box", false);
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    submit();
    await waitFor(() => expect(navigate).toHaveBeenCalled());
    expect(sentResources(a)).toMatchObject({ cpu: "2", memory: "4Gi", disk: "30Gi" });
  });

  it("holds a VM's disk to its image's floor where it is above the size's, and says so", async () => {
    const a = api({
      containers: async () => ({ items: [] }),
      images: [catalogImage("debian-13", "debian"), catalogImage("ubuntu-24", "ubuntu", "30Gi")],
    });
    render(<CreateMachine kind="vm" size="m" />, { wrapper: withApi(a) });
    await chooseImage("ubuntu");
    await waitFor(() => expect(diskField().value).toBe("30"));
    expect(screen.getByText("The image ubuntu-24 needs at least 30 GiB")).toBeTruthy();
    typeDisk("25");
    fill("box", false);
    await screen.findAllByText("The image ubuntu-24 needs a disk of at least 30 GiB; 25 GiB is less.");
    expect(manifest()).toContain("size: 25Gi");
    await waitFor(() => expect(existingLoaded()).toBe(true));
    expect(submitButton().disabled).toBe(true);
    submit();
    // Of an image with no floor above it, the rules' holds.
    await chooseImage("debian");
    expect(screen.getByText("At least 2 GiB")).toBeTruthy();
    await chooseImage("ubuntu");
    typeDisk("30");
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    submit();
    await waitFor(() => expect(navigate).toHaveBeenCalled());
    expect(writes(a)).toEqual([{ method: "ApplyVirtualMachine", project: "demo", name: "box", createOnly: true }]);
    expect(sentResources(a)).toMatchObject({ disk: "30Gi" });
  });

  it("says by the field that a disk grows later, and that growing restarts the machine", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="m" />, { wrapper: withApi(a) });
    await screen.findByText(/A disk can grow later; growing restarts the machine\./);
  });

  it("states no disk on the size cards", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="m" />, { wrapper: withApi(a) });
    const cards = await screen.findAllByRole("radio", { name: /^Size / });
    for (const c of cards) expect(c.textContent).not.toMatch(/disk/i);
  });

  it("warns of a disk past the project's limit by the field and the button, not on the size cards", async () => {
    const a = api({ containers: async () => ({ items: [] }), quota: { limits: { disk: "50Gi" }, usage: { disk: "10Gi" } } });
    render(<CreateMachine kind="container" size="m" />, { wrapper: withApi(a) });
    await waitFor(() => expect(diskField().value).toBe("20"));
    // A disk within the limit warns of nothing.
    typeDisk("40");
    fill("api");
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    expect(screen.queryByText(/Over/)).toBeNull();
    typeDisk("41");
    await screen.findByText("Over the project's disk limit");
    expect(screen.getAllByText((_, el) => el?.textContent === "The disk needs 41 GiB; 40 GiB of 50 GiB is left.").length).toBeGreaterThan(0);
    expect(screen.getByText("Over quota: 40 GiB of the project's 50 GiB of disk is left.")).toBeTruthy();
    for (const c of screen.getAllByRole("radio", { name: /^Size / })) expect(c.textContent).not.toContain("Over quota");
    expect(submitButton().disabled).toBe(true);
    submit();
    typeDisk("40");
    await createAllowed(a, "api");
    expect(sentResources(a)).toMatchObject({ disk: "40Gi" });
  });

  it("marks a size card by its own smallest disk, whatever disk is typed", async () => {
    const a = api({ containers: async () => ({ items: [] }), quota: { limits: { disk: "45Gi" }, usage: { disk: "10Gi" } } });
    render(<CreateMachine kind="container" size="m" />, { wrapper: withApi(a) });
    const large = await screen.findByRole("radio", { name: "Size L" });
    await waitFor(() => expect(large.textContent).toContain("Over quota"));
    expect(screen.getByRole("radio", { name: "Size M" }).textContent).not.toContain("Over quota");
    typeDisk("30");
    expect(screen.getByRole("radio", { name: "Size M" }).textContent).not.toContain("Over quota");
    expect(large.textContent).toContain("Over quota");
  });

  it("marks a VM's size card by its image's floor where it is above the size's", async () => {
    const a = api({
      containers: async () => ({ items: [] }),
      images: [catalogImage("ubuntu-24", "ubuntu", "30Gi")],
      quota: { limits: { disk: "35Gi" }, usage: { disk: "10Gi" } },
    });
    render(<CreateMachine kind="vm" size="s" />, { wrapper: withApi(a) });
    const small = await screen.findByRole("radio", { name: "Size S" });
    await waitFor(() => expect(small.textContent).toContain("Over quota"));
  });
});

describe("CreateMachine's advanced section", () => {
  const sent = (a: ReturnType<typeof api>) =>
    (a.requests.find((c) => c.method === "ApplyContainer")?.request as { container?: Container } | undefined)?.container?.spec;

  it("is folded, and sends the image's own process unless it is opened and changed", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(a) });
    const fold = screen.getByRole("button", { name: /Entrypoint, command, restart, environment/ });
    expect(fold.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByLabelText(/^Entrypoint/)).toBeNull();
    await createAllowed(a, "api");
    const spec = sent(a);
    expect(spec?.command).toEqual([]);
    expect(spec?.args).toEqual([]);
    expect(spec?.env).toEqual([]);
    expect(spec?.restartPolicy).toBe(RestartPolicy.UNSPECIFIED);
  });

  it("sends a command split as a shell splits it, the restart chosen and the environment", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(a) });
    fireEvent.click(screen.getByRole("button", { name: /Entrypoint, command, restart, environment/ }));
    fireEvent.change(screen.getByLabelText(/^Command/), { target: { value: `sh -c 'echo hi && sleep infinity'` } });
    fireEvent.click(screen.getByRole("radio", { name: /^on-failure/ }));
    fireEvent.click(screen.getByRole("button", { name: "Add variable" }));
    fireEvent.change(screen.getByLabelText("Variable 1 name"), { target: { value: "TZ" } });
    fireEvent.change(screen.getByLabelText("Variable 1 value"), { target: { value: "Asia/Taipei" } });
    // The words it splits into are shown under the field.
    expect(screen.getByText("echo hi && sleep infinity")).toBeTruthy();
    await createAllowed(a, "api");
    const spec = sent(a);
    expect(spec?.command).toEqual([]);
    expect(spec?.args).toEqual(["sh", "-c", "echo hi && sleep infinity"]);
    expect(spec?.restartPolicy).toBe(RestartPolicy.ON_FAILURE);
    expect(spec?.env.map((e) => [e.name, e.value])).toEqual([["TZ", "Asia/Taipei"]]);
  });

  it("refuses an unclosed quote and a variable named twice, and sends nothing", async () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(a) });
    fireEvent.click(screen.getByRole("button", { name: /Entrypoint, command, restart, environment/ }));
    fireEvent.change(screen.getByLabelText(/^Entrypoint/), { target: { value: `"/bin/sh` } });
    fill("api");
    await screen.findByText("A double quote is not closed.");
    expect(submitButton().disabled).toBe(true);
    fireEvent.change(screen.getByLabelText(/^Entrypoint/), { target: { value: "/bin/sh" } });
    fireEvent.click(screen.getByRole("button", { name: "Add variable" }));
    fireEvent.click(screen.getByRole("button", { name: "Add variable" }));
    fireEvent.change(screen.getByLabelText("Variable 1 name"), { target: { value: "A" } });
    fireEvent.change(screen.getByLabelText("Variable 2 name"), { target: { value: "A" } });
    await screen.findByText(/The variable A is set twice/);
    expect(submitButton().disabled).toBe(true);
    submit();
    expect(writes(a)).toEqual([]);
  });

  it("is not offered for a virtual machine", () => {
    const a = api({ containers: async () => ({ items: [] }) });
    render(<CreateMachine kind="vm" size="s" />, { wrapper: withApi(a) });
    expect(screen.queryByRole("button", { name: /Entrypoint, command, restart, environment/ })).toBeNull();
  });
});
