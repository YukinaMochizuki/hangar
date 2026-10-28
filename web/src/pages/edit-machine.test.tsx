// @vitest-environment jsdom

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";
import { ContainerRunState, ContainerSchema, ContainerService, type Container } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { MachineSchema, MachineService } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { MachineSizeSchema, MachineSizeService } from "@hangar/api/hangar/compute/v1alpha1/machine_size_pb";
import { VirtualMachineRunState, VirtualMachineSchema, VirtualMachineService, VirtualMachineTPMSchema, type VirtualMachine } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { NetworkService } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { SecurityGroupService } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { CustomImageService } from "@hangar/api/hangar/storage/v1alpha1/custom_image_pb";
import { DiskSchema, DiskService } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { FileShareService } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { PortForwardSchema, PortForwardService, Protocol, type PortForward } from "@hangar/api/hangar/network/v1alpha1/port_forward_pb";
import { ImageSchema, ImageService } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { MachineActions, MachineRowMenu, useMachineActions } from "@/components/machine-actions";
import { containerView, vmView, type MachineView } from "@/lib/machines";
import { useMachineCan } from "@/lib/permissions";
import { EditMachine } from "@/pages/edit-machine";
import { fakeApi, never, polyfillDialog, withApi } from "@/test/harness";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
beforeEach(() => navigate.mockReset());
afterEach(cleanup);

const web = (rv = 21n, image = "nginx:1") =>
  create(ContainerSchema, {
    metadata: { name: "web", project: "demo", uid: "u-web", resourceVersion: rv, labels: { team: "a" } },
    spec: { image, env: [{ name: "MODE", value: "prod" }], resources: { cpu: "1", memory: "512Mi" }, runState: ContainerRunState.STOPPED, imagePullSecrets: ["ghcr"] },
  });

const db = () =>
  create(VirtualMachineSchema, {
    metadata: { name: "db", project: "demo", uid: "u-db", resourceVersion: 42n },
    spec: {
      resources: { cpu: "3", memory: "6Gi" },
      rootDisk: { source: { case: "fromImage", value: { image: { name: "ubuntu-24.04" }, size: "30Gi" } } },
      sshAuthorizedKeys: ["ssh-ed25519 AAAA me"],
    },
    status: { rootDisk: "db-root" },
  });

const size = (name: string, cpu: string, memory: string, disk: string, order: number) =>
  create(MachineSizeSchema, { metadata: { name }, spec: { displayName: name.toUpperCase(), cpu, memory, disk, order } });

type Fake = {
  container?: () => Container;
  vm?: () => VirtualMachine;
  /** The container's Machine: on node, holding disk. */
  machine?: { node: string; disk: string };
  /** What ApplyContainer and ApplyVirtualMachine fail with, when set. */
  fail?: () => ConnectError | undefined;
  perms?: () => Promise<string[]> | string[];
  quota?: { limits: { memory?: string }; usage: { memory?: string } };
  /** The project's port forwards; none unless set. */
  forwards?: PortForward[];
};

const applyPerms = ["compute.containers.apply", "compute.virtualmachines.apply"];

function api(f: Fake = {}) {
  return fakeApi((r: ConnectRouter) => {
    r.service(ContainerService, {
      listContainers: () => ({ items: [f.container?.() ?? web()] }),
      getContainer: () => ({ container: f.container?.() ?? web() }),
      applyContainer: (req) => {
        const err = f.fail?.();
        if (err) throw err;
        return { container: req.container };
      },
    });
    r.service(VirtualMachineService, {
      listVirtualMachines: () => ({ items: [f.vm?.() ?? db()] }),
      getVirtualMachine: () => ({ virtualMachine: f.vm?.() ?? db() }),
      applyVirtualMachine: (req) => {
        const err = f.fail?.();
        if (err) throw err;
        return { virtualMachine: req.virtualMachine };
      },
    });
    r.service(MachineService, {
      listMachines: () => ({
        items: f.machine
          ? [
              create(MachineSchema, {
                metadata: { name: "m-web", project: "demo", uid: "m-u-web", ownerReferences: [{ kind: "Container", name: "web", controller: true }] },
                spec: { node: f.machine.node, resources: { cpu: "1", memory: "512Mi", disk: f.machine.disk } },
              }),
            ]
          : [],
      }),
    });
    r.service(MachineSizeService, {
      listMachineSizes: () => ({ items: [size("xs", "1", "512Mi", "5Gi", 0), size("s", "1", "2Gi", "10Gi", 10), size("m", "2", "4Gi", "20Gi", 20)] }),
    });
    r.service(AccountService, { testPermissions: async () => ({ permissions: await (f.perms?.() ?? applyPerms) }) });
    r.service(ProjectService, {
      getProject: () => ({ project: { metadata: { name: "demo" }, status: f.quota ?? { limits: {}, usage: {} } } }),
      listProjects: () => ({ items: [] }),
    });
    r.service(TenantService, { listTenants: () => ({ items: [] }) });
    r.service(NetworkService, { listNetworks: () => ({ items: [{ metadata: { name: "default" }, spec: { ipv4Cidr: "10.0.0.0/24" } }] }) });
    r.service(SecurityGroupService, { listSecurityGroups: () => ({ items: [{ metadata: { name: "web" }, spec: { network: "default" } }] }) });
    r.service(CustomImageService, { listCustomImages: () => ({ items: [] }) });
    r.service(ImageService, { listImages: () => ({ items: [create(ImageSchema, { metadata: { name: "ubuntu-24.04" }, spec: { os: "ubuntu", version: "24.04" } })] }) });
    r.service(DiskService, { listDisks: () => ({ items: [create(DiskSchema, { metadata: { name: "pg-data", project: "demo" }, spec: { size: "5Gi" } })] }) });
    r.service(FileShareService, { listFileShares: () => ({ items: [] }) });
    r.service(PortForwardService, {
      listPortForwards: () => ({ items: f.forwards ?? [] }),
      applyPortForward: (req) => ({ portForward: req.portForward, created: true }),
      deletePortForward: () => ({}),
    });
  });
}

/** A machine's page head: its actions as the page makes them, by the answer the page asks. */
function Head({ m }: { m: MachineView }) {
  return <MachineActions set={useMachineActions(m, useMachineCan("demo").can)} />;
}

/** A machine's row menu, by the answer its list asks. */
function Row({ m }: { m: MachineView }) {
  return <MachineRowMenu m={m} can={useMachineCan("demo").can} />;
}

const applyButton = () => screen.getByRole("button", { name: /Apply changes|No change|Applying/ }) as HTMLButtonElement;
const sizeCard = (label: string) => screen.getByRole("radio", { name: `Size ${label}` });

/** The applies sent, with what they carried. */
function applies(a: ReturnType<typeof api>) {
  return a.requests
    .filter((c) => c.method === "ApplyContainer" || c.method === "ApplyVirtualMachine")
    .map((c) => {
      const r = c.request as { container?: Container; virtualMachine?: VirtualMachine; createOnly: boolean };
      return { obj: (r.container ?? r.virtualMachine)!, createOnly: r.createOnly };
    });
}

async function openEdit(kind: "Container" | "VirtualMachine", f: Fake = {}) {
  const a = api(f);
  render(<EditMachine kind={kind} name={kind === "Container" ? "web" : "db"} />, { wrapper: withApi(a) });
  await screen.findByRole("heading", { name: kind === "Container" ? "Edit web" : "Edit db" });
  await waitFor(() => expect(screen.queryAllByRole("radio", { name: /^Size / }).length).toBeGreaterThan(0));
  return a;
}

describe("the edit page", () => {
  it("prefills a container's fields, locks its kind and name, and says No change", async () => {
    await openEdit("Container");
    expect((screen.getByLabelText("Image") as HTMLInputElement).value).toBe("nginx:1");
    // The kind and the name are locked.
    expect(screen.getAllByText("Does not change.")).toHaveLength(2);
    expect(screen.queryByLabelText("Name")).toBeNull();
    expect(screen.queryByRole("radiogroup", { name: "Kind" })).toBeNull();
    expect(sizeCard("XS").getAttribute("aria-checked")).toBe("true");
    expect(within(sizeCard("XS")).getByText("Current")).toBeTruthy();
    expect((screen.getByLabelText("Disk, GiB") as HTMLInputElement).value).toBe("10");
    fireEvent.click(screen.getByRole("button", { name: /Entrypoint, command, restart, environment/ }));
    expect((screen.getByLabelText("Variable 1 value") as HTMLInputElement).value).toBe("prod");
    expect(screen.getByText("No change.")).toBeTruthy();
    expect(applyButton().textContent).toBe("No change");
    expect(applyButton().disabled).toBe(true);
    expect(screen.getByText("hangar apply -p demo -f web.yaml", { exact: false })).toBeTruthy();
    expect(screen.queryByText(/--create-only/)).toBeNull();
  });

  it("locks a VM's image and root disk, and shows a size no listed one matches as Current", async () => {
    await openEdit("VirtualMachine");
    // The kind, the image and the name are locked.
    expect(screen.getAllByText("Does not change.")).toHaveLength(3);
    expect(screen.queryByRole("radiogroup", { name: "Image" })).toBeNull();
    expect(screen.queryByLabelText("Disk, GiB")).toBeNull();
    expect(screen.getByText("grows on its disk's page")).toBeTruthy();
    expect(sizeCard("Current").getAttribute("aria-checked")).toBe("true");
    expect(within(sizeCard("Current")).getByText("Current, no listed size")).toBeTruthy();
    expect(sizeCard("Custom").getAttribute("aria-checked")).toBe("false");
    expect(applyButton().disabled).toBe(true);
  });

  it("edits a shape with Custom, starting from the machine's own, and sends nothing outside the rules", async () => {
    const a = await openEdit("VirtualMachine");
    fireEvent.click(sizeCard("Custom"));
    expect(sizeCard("Custom").getAttribute("aria-checked")).toBe("true");
    expect(sizeCard("Current").getAttribute("aria-checked")).toBe("false");
    const memory = screen.getByLabelText("Memory, GiB") as HTMLInputElement;
    const cpu = screen.getByLabelText("vCPUs, number") as HTMLInputElement;
    expect([memory.value, cpu.value]).toEqual(["6", "3"]);
    // 6 GiB allows 1 to 12 vCPUs: 13 is marked and not taken, so nothing differs.
    fireEvent.change(cpu, { target: { value: "13" } });
    expect(cpu.getAttribute("aria-invalid")).toBe("true");
    expect(applyButton().disabled).toBe(true);
    fireEvent.change(memory, { target: { value: "8" } });
    fireEvent.change(cpu, { target: { value: "4" } });
    await waitFor(() => expect(applyButton().disabled).toBe(false));
    expect(screen.getByText("Restarts the VM")).toBeTruthy();
    fireEvent.click(applyButton());
    await waitFor(() => expect(applies(a)).toHaveLength(1));
    const vm = applies(a)[0]!.obj as VirtualMachine;
    expect(vm.spec?.resources).toMatchObject({ cpu: "4", memory: "8Gi" });
    // The root disk is the VM's own, untouched.
    expect(vm.spec?.rootDisk?.source).toMatchObject({ case: "fromImage", value: { size: "30Gi" } });
  });

  it("keeps a size no listed one matches unless another is chosen", async () => {
    const a = await openEdit("VirtualMachine");
    fireEvent.click(screen.getByRole("button", { name: "web" }));
    await waitFor(() => expect(applyButton().disabled).toBe(false));
    expect(screen.getByText("Restarts the VM")).toBeTruthy();
    fireEvent.click(applyButton());
    await waitFor(() => expect(applies(a)).toHaveLength(1));
    const vm = applies(a)[0]!.obj as VirtualMachine;
    expect(vm.spec?.resources).toMatchObject({ cpu: "3", memory: "6Gi" });
    expect(vm.spec?.interfaces.map((i) => i.securityGroups)).toEqual([["default", "web"]]);
    expect(vm.spec?.sshAuthorizedKeys).toEqual(["ssh-ed25519 AAAA me"]);
    expect(vm.metadata?.resourceVersion).toBe(42n);
  });

  it("refuses a container disk smaller than the one it has", async () => {
    await openEdit("Container");
    fireEvent.change(screen.getByLabelText("Disk, GiB"), { target: { value: "5" } });
    expect(screen.getAllByText("The disk is 10 GiB and never shrinks; 5 GiB is less.").length).toBeGreaterThan(0);
    expect(applyButton().disabled).toBe(true);
  });

  it("lists what applying does, and applies at the version read, keeping what the form does not show", async () => {
    const a = await openEdit("Container");
    fireEvent.change(screen.getByLabelText("Image"), { target: { value: "nginx:2" } });
    expect(screen.getByText("Rebuilds the root disk")).toBeTruthy();
    expect(screen.getByText("nginx:1 → nginx:2")).toBeTruthy();
    expect(screen.queryByText("Restarts the container")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Entrypoint, command, restart, environment/ }));
    fireEvent.change(screen.getByLabelText("Variable 1 value"), { target: { value: "dev" } });
    expect(screen.getByText("Restarts the container")).toBeTruthy();
    expect(screen.getByText("MODE changed")).toBeTruthy();
    fireEvent.click(applyButton());
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/containers/$name", params: { project: "demo", name: "web" }, replace: true }));
    expect(a.calls).not.toContain("GetContainer");
    const [sent] = applies(a);
    expect(applies(a)).toHaveLength(1);
    expect(sent!.createOnly).toBe(false);
    const c = sent!.obj as Container;
    expect(c.metadata).toMatchObject({ resourceVersion: 21n, labels: { team: "a" } });
    expect(c.spec).toMatchObject({ image: "nginx:2", runState: ContainerRunState.STOPPED, imagePullSecrets: ["ghcr"], resources: { cpu: "1", memory: "512Mi", disk: "" } });
    expect(c.spec?.env.map((e) => [e.name, e.value])).toEqual([["MODE", "dev"]]);
  });

  it("warns on quota only when the difference takes the project past a limit", async () => {
    // XS to S adds 1.5 GiB: 3.5 + 1.5 fits 5 GiB, though S's whole 2 GiB would not.
    await openEdit("Container", { quota: { limits: { memory: "5Gi" }, usage: { memory: "3584Mi" } } });
    // M adds 3.5 GiB, past the limit; S adds 1.5 GiB, within it, whatever its whole size.
    await waitFor(() => expect(within(sizeCard("M")).queryByText("Over quota")).toBeTruthy());
    expect(within(sizeCard("S")).queryByText("Over quota")).toBeNull();
    fireEvent.click(sizeCard("S"));
    await waitFor(() => expect(applyButton().disabled).toBe(false));
    expect(screen.queryByText(/Over the project's/)).toBeNull();
    cleanup();
    const a = await openEdit("Container", { quota: { limits: { memory: "4608Mi" }, usage: { memory: "3584Mi" } } });
    await waitFor(() => expect(within(sizeCard("S")).queryByText("Over quota")).toBeTruthy());
    fireEvent.click(sizeCard("S"));
    expect(screen.getByText("Over the project's memory limit")).toBeTruthy();
    expect(screen.getByText(/The change adds 1\.5 GiB; 1 GiB of 4\.5 GiB is left\./)).toBeTruthy();
    expect(applyButton().disabled).toBe(true);
    fireEvent.submit(applyButton().closest("form")!);
    expect(applies(a)).toEqual([]);
  });

  it("says the object changed since it opened, writes nothing over it, and starts again from it on Reload", async () => {
    let current = web();
    let conflict = true;
    const a = await openEdit("Container", {
      container: () => current,
      fail: () => (conflict ? new ConnectError("Container web is at resource version 25, not 21", Code.Aborted) : undefined),
    });
    current = web(25n, "nginx:1.5");
    fireEvent.change(screen.getByLabelText("Image"), { target: { value: "nginx:2" } });
    fireEvent.click(applyButton());
    await screen.findByText("web changed since you opened it.");
    expect(screen.getByText("Nothing was applied.")).toBeTruthy();
    expect(applyButton().disabled).toBe(true);
    // One write, at the version opened; nothing read again and written over.
    expect(applies(a).map((x) => x.obj.metadata?.resourceVersion)).toEqual([21n]);
    expect(a.calls).not.toContain("GetContainer");
    expect(navigate).not.toHaveBeenCalled();

    conflict = false;
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect((screen.getByLabelText("Image") as HTMLInputElement).value).toBe("nginx:1.5"));
    expect(screen.queryByText("web changed since you opened it.")).toBeNull();
    fireEvent.change(screen.getByLabelText("Image"), { target: { value: "nginx:2" } });
    fireEvent.click(applyButton());
    await waitFor(() => expect(applies(a)).toHaveLength(2));
    expect(applies(a)[1]!.obj.metadata?.resourceVersion).toBe(25n);
  });

  it("sends a stored disk that is not whole GiB back as it is, and lets another change through", async () => {
    const ten = web();
    ten.spec!.resources!.disk = "10G";
    const a = await openEdit("Container", { container: () => ten });
    expect((screen.getByLabelText("Disk, GiB") as HTMLInputElement).value).toBe("9.3");
    fireEvent.change(screen.getByLabelText("Image"), { target: { value: "nginx:2" } });
    await waitFor(() => expect(applyButton().disabled).toBe(false));
    expect(screen.queryByText(/not a whole number/)).toBeNull();
    expect(screen.queryByText("Disk", { selector: "li span" })).toBeNull();
    fireEvent.click(applyButton());
    await waitFor(() => expect(applies(a)).toHaveLength(1));
    expect((applies(a)[0]!.obj as Container).spec?.resources?.disk).toBe("10G");
  });

  it("edits a container whose stored disk growth the form would not write, keeping it as stored", async () => {
    const growing = web();
    growing.spec!.resources!.disk = "2Gi";
    growing.spec!.resources!.diskGrow = { $typeName: "hangar.compute.v1alpha1.DiskGrow", max: "20480Mi", threshold: "", step: "2048Mi" };
    const a = await openEdit("Container", { container: () => growing });
    fireEvent.change(screen.getByLabelText("Image"), { target: { value: "nginx:2" } });
    await waitFor(() => expect(applyButton().disabled).toBe(false));
    fireEvent.click(applyButton());
    await waitFor(() => expect(applies(a)).toHaveLength(1));
    expect((applies(a)[0]!.obj as Container).spec?.resources?.diskGrow).toMatchObject({ max: "20480Mi", step: "2048Mi" });
  });

  it("holds the disk to what the placed machine has, or before it is placed to the spec's", async () => {
    const grown = web();
    grown.spec!.resources!.disk = "30Gi";
    // Placed on a 20 GiB disk: a growth to 30 GiB not yet taken may be withdrawn to 25.
    await openEdit("Container", { container: () => grown, machine: { node: "n1", disk: "20Gi" } });
    await screen.findByText("≥ 20 GiB, never shrinks");
    fireEvent.change(screen.getByLabelText("Disk, GiB"), { target: { value: "25" } });
    expect(screen.queryByText(/never shrinks; 25 GiB is less/)).toBeNull();
    await waitFor(() => expect(applyButton().disabled).toBe(false));
    cleanup();
    // Not placed: the spec's 30 GiB holds, whatever the machine's spec says.
    await openEdit("Container", { container: () => grown, machine: { node: "", disk: "20Gi" } });
    await screen.findByText("≥ 30 GiB, never shrinks");
    fireEvent.change(screen.getByLabelText("Disk, GiB"), { target: { value: "25" } });
    expect(screen.getAllByText("The disk is 30 GiB and never shrinks; 25 GiB is less.").length).toBeGreaterThan(0);
    expect(applyButton().disabled).toBe(true);
  });

  it("leaves the interfaces as they were written, aliases and all, when they are not changed", async () => {
    const explicit = web();
    explicit.spec!.interfaces = [{ $typeName: "hangar.compute.v1alpha1.NetworkInterface", network: "default", securityGroups: [], aliases: ["www"] }];
    const a = await openEdit("Container", { container: () => explicit });
    fireEvent.change(screen.getByLabelText("Image"), { target: { value: "nginx:2" } });
    expect(screen.queryByText("Restarts the container")).toBeNull();
    fireEvent.click(applyButton());
    await waitFor(() => expect(applies(a)).toHaveLength(1));
    expect((applies(a)[0]!.obj as Container).spec?.interfaces).toEqual(explicit.spec?.interfaces);
  });

  it("shows a VM's own interfaces after the first, and keeps them", async () => {
    const two = db();
    two.spec!.interfaces = [
      { $typeName: "hangar.compute.v1alpha1.NetworkInterface", network: "default", securityGroups: [], aliases: [] },
      { $typeName: "hangar.compute.v1alpha1.NetworkInterface", network: "data", securityGroups: ["db"], aliases: ["pg"] },
    ];
    const a = await openEdit("VirtualMachine", { vm: () => two });
    expect(screen.getByText("eth1")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove interface on data" })).toBeTruthy();
    fireEvent.click(sizeCard("M"));
    expect(screen.queryByText(/^Interfaces$/)).toBeNull();
    fireEvent.click(applyButton());
    await waitFor(() => expect(applies(a)).toHaveLength(1));
    const sent = applies(a)[0]!.obj as VirtualMachine;
    expect(sent.spec?.interfaces).toEqual(two.spec?.interfaces);
    expect(sent.spec?.resources).toMatchObject({ cpu: "2", memory: "4Gi" });
  });

  it("says a machine being deleted is, and shows no form", async () => {
    const going = web();
    going.metadata!.deleteTime = { $typeName: "google.protobuf.Timestamp", seconds: 1n, nanos: 0 };
    const a = api({ container: () => going });
    render(<EditMachine kind="Container" name="web" />, { wrapper: withApi(a) });
    await screen.findByText("web is being deleted");
    expect(screen.queryByLabelText("Image")).toBeNull();
  });

  it("states a refusal when the caller may not apply the kind, and shows no form", async () => {
    const a = api({ perms: () => [] });
    render(<EditMachine kind="Container" name="web" />, { wrapper: withApi(a) });
    await screen.findByText("This account's roles do not include changing containers in this project.");
    expect(screen.queryByLabelText("Image")).toBeNull();
  });

  it("shows nothing while the permission is not known", async () => {
    const a = api({ perms: () => never() });
    render(<EditMachine kind="Container" name="web" />, { wrapper: withApi(a) });
    await waitFor(() => expect(a.calls).toContain("ListContainers"));
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByLabelText("Image")).toBeNull();
    expect(screen.queryByText(/roles do not include/)).toBeNull();
  });
});

describe("Edit on the machine's page and its row", () => {
  const m = () => containerView(web(), undefined);

  it("show Edit only when the kind's apply permission is held, and no Resize: the edit page resizes", async () => {
    const a = api({ perms: () => ["compute.virtualmachines.apply"] });
    render(<Head m={m()} />, { wrapper: withApi(a) });
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    // The answer came, and named only the VM's permission.
    await waitFor(() =>
      expect(
        a.client
          .getQueryCache()
          .getAll()
          .some((q) => JSON.stringify(q.queryKey).includes("TestPermissions") && q.state.status === "success"),
      ).toBe(true),
    );
    expect(screen.queryByRole("link", { name: /Edit/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Resize/ })).toBeNull();
    // Start applies the container too, so it goes with them.
    expect(screen.queryByRole("button", { name: /Start/ })).toBeNull();
    cleanup();
    render(<Head m={m()} />, { wrapper: withApi(api()) });
    expect((await screen.findByRole("link", { name: /Edit/ })).getAttribute("href")).toBe("/projects/demo/containers/web/edit");
    expect(screen.queryByRole("button", { name: /Resize/ })).toBeNull();
  });

  it("put Edit in the row's menu only where it may be used, and nothing that changes a machine being deleted", async () => {
    render(<Row m={m()} />, { wrapper: withApi(api({ perms: () => ["compute.virtualmachines.apply"] })) });
    // With no permission of its kind held, the row has no menu at all.
    await waitFor(() => expect(screen.queryByRole("button", { name: "Actions for web" })).toBeNull());
    cleanup();
    render(<Row m={m()} />, { wrapper: withApi(api()) });
    fireEvent.click(await screen.findByRole("button", { name: "Actions for web" }));
    expect(screen.queryByRole("menuitem", { name: /Resize/ })).toBeNull();
    fireEvent.click(screen.getByRole("menuitem", { name: /Edit$/ }));
    expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/containers/$name/edit", params: { project: "demo", name: "web" } });
    cleanup();
    render(<Row m={{ ...m(), deleting: true }} />, { wrapper: withApi(api({ perms: () => [...applyPerms, "compute.containers.delete"] })) });
    fireEvent.click(await screen.findByRole("button", { name: "Actions for web" }));
    expect(screen.queryAllByRole("menuitem")).toHaveLength(0);
    expect(screen.getByText("Being deleted: nothing to change.")).toBeTruthy();
  });

  it("offer a VM's Create image, on its root disk, in the head's menu, for whoever may capture, and only once it is stopped", async () => {
    const stopped = () => {
      const v = db();
      v.spec!.runState = VirtualMachineRunState.STOPPED;
      return vmView(v, undefined);
    };
    const capturing = () => [...applyPerms, "storage.customimages.apply"];
    const createImage = async () => {
      fireEvent.click(await screen.findByRole("button", { name: "More actions" }));
      return screen.getByRole("menuitem", { name: /Create image/ }) as HTMLButtonElement;
    };
    render(<Head m={stopped()} />, { wrapper: withApi(api({ perms: capturing })) });
    // Seldom used, so never a button of the head's own.
    await screen.findByRole("button", { name: /Start/ });
    expect(screen.queryByRole("link", { name: /Create image/ })).toBeNull();
    fireEvent.click(await createImage());
    expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/new/custom-image", params: { project: "demo" }, search: { disk: "db-root" } });
    cleanup();
    render(<Head m={vmView(db(), undefined)} />, { wrapper: withApi(api({ perms: capturing })) });
    expect((await createImage()).disabled).toBe(true);
    cleanup();
    const a = api({ perms: () => applyPerms });
    render(<Head m={stopped()} />, { wrapper: withApi(a) });
    await screen.findByRole("button", { name: /Start/ });
    expect(screen.queryByRole("button", { name: "More actions" })).toBeNull();
    // A container has no root disk to capture.
    cleanup();
    render(<Head m={containerView(web(), undefined)} />, { wrapper: withApi(api({ perms: capturing })) });
    await screen.findByRole("link", { name: /Edit/ });
    expect(screen.queryByRole("button", { name: "More actions" })).toBeNull();
  });
});

describe("the edit page's port forwards and a VM's fields", () => {
  const ssh = create(PortForwardSchema, {
    metadata: { name: "web-22", project: "demo" },
    spec: { target: { object: { case: "container", value: "web" }, interface: "eth0" }, protocol: Protocol.TCP, targetPort: 22 },
    status: { ipv4: "198.51.100.3", port: 23817 },
  });
  const writes = (a: ReturnType<typeof api>) => a.requests.filter((c) => /^(Apply|Delete)/.test(c.method)).map((c) => c.method);

  it("lists the machine's forwards, and on Apply deletes the one removed and makes the one added, restarting nothing", async () => {
    const a = await openEdit("Container", { forwards: [ssh] });
    expect(await screen.findByText("198.51.100.3:23817")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove port forward 1" }));
    fireEvent.click(screen.getByRole("button", { name: /Add port forward/ }));
    fireEvent.change(screen.getByLabelText("Port of port forward 1"), { target: { value: "80" } });
    await waitFor(() => expect(applyButton().disabled).toBe(false));
    expect(screen.getByText("Restarts nothing")).toBeTruthy();
    expect(screen.getByText("web-80 added, web-22 removed")).toBeTruthy();
    fireEvent.click(applyButton());
    // The container itself is not written: nothing of it changed.
    await waitFor(() => expect(writes(a)).toEqual(["DeletePortForward", "ApplyPortForward"]));
  });

  it("says a VM's data disks restart it, and its SSH keys do not", async () => {
    await openEdit("VirtualMachine");
    fireEvent.click(screen.getByRole("button", { name: /cloud-init, hardware/ }));
    // Prefilled from the VM's own.
    expect((screen.getByLabelText("SSH keys") as HTMLTextAreaElement).value).toBe("ssh-ed25519 AAAA me");
    fireEvent.change(screen.getByLabelText("SSH keys"), { target: { value: "ssh-ed25519 AAAA me\nssh-ed25519 BBBB you" } });
    await waitFor(() => expect(applyButton().disabled).toBe(false));
    expect(screen.getByText("Restarts nothing")).toBeTruthy();
    expect(screen.queryByText("Restarts the VM")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Attach disk/ }));
    await screen.findByRole("option", { name: /^pg-data/ });
    fireEvent.change(screen.getByRole("combobox", { name: "Data disk 1" }), { target: { value: "pg-data" } });
    expect(await screen.findByText("Restarts the VM")).toBeTruthy();
  });

  it("locks a VM's TPM, and says its serial number restarts it", async () => {
    const tpm = db();
    tpm.spec!.tpm = create(VirtualMachineTPMSchema, { enabled: true });
    const a = await openEdit("VirtualMachine", { vm: () => tpm });
    fireEvent.click(screen.getByRole("button", { name: /cloud-init, hardware/ }));
    const box = screen.getByRole("checkbox", { name: /^TPM 2\.0/ }) as HTMLInputElement;
    expect(box.checked).toBe(true);
    expect(box.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Serial number"), { target: { value: "ARX-0042" } });
    expect(await screen.findByText("Restarts the VM")).toBeTruthy();
    expect(screen.getByText("the platform's → ARX-0042")).toBeTruthy();
    fireEvent.click(applyButton());
    await waitFor(() => expect(applies(a)).toHaveLength(1));
    const sent = applies(a)[0]!.obj as VirtualMachine;
    expect(sent.spec?.platform?.serialNumber).toBe("ARX-0042");
    expect(sent.spec?.tpm?.enabled).toBe(true);
  });
});
