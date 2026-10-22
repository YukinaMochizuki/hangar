// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import type { ApplyFileShareRequest } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CreateMachine } from "@/pages/create-machine";
import { never, withApi } from "@/test/harness";
import { container, share, sharesApi, type Fake } from "@/test/file-shares";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: ({ children, className }: { children: ReactNode; className?: string }) => <a className={className}>{children}</a>,
}));

beforeEach(() => navigate.mockReset());
afterEach(cleanup);

// media is on data alone; the new container is on default.
const media = share("media", { networks: ["data"], addresses: { data: "10.1.0.9" } });
const assets = share("assets", { networks: ["default"], addresses: { default: "10.0.0.7" } });
const users = ["etl", "thumbs", "indexer"].map((n) => container(n, ["data"], [{ share: "media", target: "/m" }]));

const submitButton = () => screen.getByRole("button", { name: /^(Create container|Creating)/ }) as HTMLButtonElement;

/** The element whose own text, children's included, is exactly text: copy with names in the monospace face spans several. */
const text = (t: string) => (_: string, el: Element | null) => el?.textContent === t && [...(el?.children ?? [])].every((c) => c.textContent !== t);

/** Clicks "Add default to this share" on one share's option in an open list. */
async function addDefaultTo(list: HTMLElement, share: string) {
  const option = (await within(list).findByRole("option", { name: new RegExp(`^${share}`) })).parentElement!;
  fireEvent.click(await within(option).findByRole("button", { name: /Add default to this share/ }));
}

/** Chooses media in the open list by adding the container's network to it, and fills what the form needs. */
async function fillWithShareAdd(list: HTMLElement, target: string) {
  await addDefaultTo(list, "media");
  fireEvent.click(screen.getByRole("button", { name: "Source of volume 1" }));
  fireEvent.change(screen.getAllByLabelText("Target")[0]!, { target: { value: target } });
  fireEvent.change(screen.getByLabelText("Image"), { target: { value: "nginx:1" } });
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "thumbs2" } });
  await waitFor(() => expect(submitButton().disabled).toBe(false));
}

async function openPicker(f: Fake) {
  const api = sharesApi({ shares: [media, assets], containers: users, ...f });
  render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(api) });
  fireEvent.click(await screen.findByRole("button", { name: /Add volume/ }));
  fireEvent.click(screen.getByRole("button", { name: "Source of volume 1" }));
  const list = await screen.findByRole("listbox", { name: "Source" });
  await within(list).findByRole("option", { name: /media/ });
  return { api, list };
}

describe("the volume picker", () => {
  it("groups disks and file shares, and offers a share on none of the container's networks off, with two ways to reach it in order", async () => {
    const { list } = await openPicker({});
    expect(within(list).getByText("Disks")).toBeTruthy();
    expect(within(list).getByText("File shares")).toBeTruthy();
    expect((within(list).getByRole("option", { name: /media/ }) as HTMLButtonElement).disabled).toBe(true);
    expect((within(list).getByRole("option", { name: /assets/ }) as HTMLButtonElement).disabled).toBe(false);
    expect(within(list).getAllByText("Not on this container's networks.").length).toBeGreaterThan(0);
    await within(list).findByRole("button", { name: /Add default to this share/ });
    const names = within(within(list).getByRole("group", { name: "Ways to use it" }))
      .getAllByRole("button")
      .map((b) => b.textContent)
      .filter((t) => t?.startsWith("Add"));
    expect(names).toEqual(["Add default to this share", "Add interface on data"]);
    // Also used by names two and folds the rest.
    expect(within(list).getByText("etl, indexer")).toBeTruthy();
  });

  it("disables adding the container's network, saying why, for whom may not change shares", async () => {
    const { list } = await openPicker({ perms: () => [] });
    const add = await within(list).findByRole("button", { name: /Add default to this share/ });
    expect((add as HTMLButtonElement).disabled).toBe(true);
    expect(within(list).getByText("This account's roles do not include changing file shares in this project.")).toBeTruthy();
    expect((within(list).getByRole("button", { name: /Add interface on data/ }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("hides adding the container's network while whether the caller may is unknown", async () => {
    const { list } = await openPicker({ perms: never });
    expect(within(list).getByRole("button", { name: /Add interface on data/ })).toBeTruthy();
    expect(within(list).queryByRole("button", { name: /Add default to this share/ })).toBeNull();
  });

  it("adds an interface on the share's network to the container", async () => {
    const { list } = await openPicker({});
    fireEvent.click(within(list).getByRole("button", { name: /Add interface on data/ }));
    expect(await screen.findByText("eth1")).toBeTruthy();
    expect(screen.getByText(/Added for file share/)).toBeTruthy();
    expect(screen.getByText(/network: data/)).toBeTruthy();
  });

  it("drops the interface added for a share when the kind is switched to a virtual machine", async () => {
    const { list } = await openPicker({});
    fireEvent.click(within(list).getByRole("button", { name: /Add interface on data/ }));
    expect(await screen.findByText("eth1")).toBeTruthy();
    fireEvent.click(screen.getByRole("radio", { name: /^Virtual machine/ }));
    await waitFor(() => expect(screen.queryByText("eth1")).toBeNull());
    expect(screen.queryByText(/network: data/)).toBeNull();
    expect(screen.queryByText(/interfaces:/)).toBeNull();
  });

  it("applies the share with the container's network before creating the container", async () => {
    const order: string[] = [];
    const apply = vi.fn((r: ApplyFileShareRequest) => void order.push(`share:${r.fileShare?.spec?.networks.join(",")}`));
    const { api, list } = await openPicker({ apply });
    fireEvent.click(await within(list).findByRole("button", { name: /Add default to this share/ }));
    expect(within(list).getByText(text("default is added to media on create."))).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Source of volume 1" }));
    fireEvent.change(screen.getByLabelText("Target"), { target: { value: "/media" } });
    fireEvent.change(screen.getByLabelText("Image"), { target: { value: "nginx:1" } });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "thumbs2" } });
    // The manifest holds the share's change before the container.
    expect(screen.getByText((_, el) => el?.textContent === "$ hangar apply -p demo -f media.yaml")).toBeTruthy();
    const submit = screen.getByRole("button", { name: "Create container" }) as HTMLButtonElement;
    await waitFor(() => expect(submit.disabled).toBe(false));
    fireEvent.click(submit);
    await waitFor(() => expect(navigate).toHaveBeenCalled());
    expect(order).toEqual(["share:data,default"]);
    const calls = api.calls.filter((c) => c === "ApplyFileShare" || c === "ApplyContainer");
    expect(calls).toEqual(["ApplyFileShare", "ApplyContainer"]);
    const sent = api.requests.find((r) => r.method === "ApplyContainer")!.request as { container: { spec: { volumes: { source: string; sourceKind: string; target: string }[] } } };
    expect(sent.container.spec.volumes).toEqual([expect.objectContaining({ source: "media", sourceKind: "FileShare", target: "/media" })]);
  });

  it("names the network the container mounts from in its own interfaces' order, as the API does", async () => {
    // logs is on ops and data, in that order; the container is on data, then ops.
    const logs = share("logs", { networks: ["ops", "data"], addresses: { ops: "10.2.0.9", data: "10.1.0.19" } });
    const opsOnly = share("metrics", { networks: ["ops"], addresses: { ops: "10.2.0.8" } });
    render(<CreateMachine kind="container" size="s" />, { wrapper: withApi(sharesApi({ shares: [logs, opsOnly] })) });
    const select = await screen.findByRole("combobox", { name: "Network" });
    await within(select).findByRole("option", { name: /^data/ });
    fireEvent.change(select, { target: { value: "data" } });
    fireEvent.click(screen.getByRole("button", { name: /Add volume/ }));
    fireEvent.click(screen.getByRole("button", { name: "Source of volume 1" }));
    const list = await screen.findByRole("listbox", { name: "Source" });
    // An interface on ops, for metrics, after the one on data.
    fireEvent.click(await within(list).findByRole("button", { name: /Add interface on ops/ }));
    fireEvent.click(await within(list).findByRole("option", { name: /^logs/ }));
    fireEvent.click(screen.getByRole("button", { name: "Source of volume 1" }));
    expect(await screen.findByText("file share, from data")).toBeTruthy();
    expect(screen.getByText("10.1.0.19")).toBeTruthy();
    expect(screen.queryByText("10.2.0.9")).toBeNull();
  });

  it("states a share it changed when the container is then refused, skips it on a retry, and creates the container", async () => {
    let refuse = true;
    const applyContainer = vi.fn(() => {
      if (refuse) throw new ConnectError("quota exceeded", Code.ResourceExhausted);
    });
    const { api, list } = await openPicker({ applyContainer });
    await fillWithShareAdd(list, "/media");
    fireEvent.click(submitButton());
    expect(await screen.findByText(text("File share media was changed"))).toBeTruthy();
    expect(screen.getByText(text("default was added to media."))).toBeTruthy();
    expect(screen.getByText(/quota exceeded/)).toBeTruthy();
    expect(screen.queryByText(/nothing was created/)).toBeNull();
    // The picker no longer says the share is changed on create: it is changed.
    fireEvent.click(screen.getByRole("button", { name: "Source of volume 1" }));
    expect(await screen.findByText(text("media has network default now."))).toBeTruthy();
    expect(screen.queryByText(/on create\./)).toBeNull();
    refuse = false;
    fireEvent.click(submitButton());
    await waitFor(() => expect(navigate).toHaveBeenCalled());
    expect(api.calls.filter((c) => c === "ApplyFileShare")).toHaveLength(1);
    expect(applyContainer).toHaveBeenCalledTimes(2);
  });

  it("undoes a share it changed, taking off only the network it added", async () => {
    const sent: string[][] = [];
    const { list } = await openPicker({
      apply: (r) => void sent.push([...(r.fileShare?.spec?.networks ?? [])]),
      applyContainer: () => {
        throw new ConnectError("quota exceeded", Code.ResourceExhausted);
      },
    });
    await fillWithShareAdd(list, "/media");
    fireEvent.click(submitButton());
    const undo = await screen.findByRole("button", { name: /^Undo$/ });
    await waitFor(() => expect((undo as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(undo);
    await waitFor(() => expect(sent).toEqual([["data", "default"], ["data"]]));
    await waitFor(() => expect(screen.queryByText(text("File share media was changed"))).toBeNull());
    // Not on the container's network any more, the share holds the create back again.
    expect(await screen.findByText(/File share media is on none of this container's networks\./)).toBeTruthy();
  });

  it("does not undo a network a machine now mounts the share from", async () => {
    const sent: string[][] = [];
    // late is on default and declares media: once media is on default, late mounts it from there.
    const late = container("late", [], [{ share: "media", target: "/m" }]);
    const { list } = await openPicker({
      containers: [...users, late],
      apply: (r) => void sent.push([...(r.fileShare?.spec?.networks ?? [])]),
      applyContainer: () => {
        throw new ConnectError("quota exceeded", Code.ResourceExhausted);
      },
    });
    await fillWithShareAdd(list, "/media");
    fireEvent.click(submitButton());
    const undo = await screen.findByRole("button", { name: /^Undo$/ });
    await waitFor(() => expect((undo as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(undo);
    expect(await screen.findByText("default stays on media: a machine mounts it from there now.")).toBeTruthy();
    expect(sent).toEqual([["data", "default"]]);
    expect(screen.getByText(text("File share media was changed"))).toBeTruthy();
  });

  it("states the first share changed when the second is refused, and a retry applies only the second", async () => {
    const logs = share("logs", { networks: ["ops"], addresses: { ops: "10.2.0.9" } });
    let refuseLogs = true;
    const applied: string[] = [];
    const { api, list } = await openPicker({
      shares: [media, logs],
      apply: (r) => {
        if (r.fileShare?.metadata?.name === "logs" && refuseLogs) throw new ConnectError("logs is being deleted", Code.FailedPrecondition);
        applied.push(r.fileShare?.metadata?.name ?? "");
      },
    });
    await fillWithShareAdd(list, "/media");
    fireEvent.click(screen.getByRole("button", { name: /Add volume/ }));
    fireEvent.click(screen.getByRole("button", { name: "Source of volume 2" }));
    await addDefaultTo(await screen.findByRole("listbox", { name: "Source" }), "logs");
    fireEvent.click(screen.getByRole("button", { name: "Source of volume 2" }));
    fireEvent.change(screen.getAllByLabelText("Target")[1]!, { target: { value: "/logs" } });
    fireEvent.click(submitButton());
    expect(await screen.findByText(/Adding default to file share logs failed: .*logs is being deleted/)).toBeTruthy();
    expect(screen.getByText(text("File share media was changed"))).toBeTruthy();
    expect(applied).toEqual(["media"]);
    expect(api.calls).not.toContain("ApplyContainer");
    refuseLogs = false;
    fireEvent.click(submitButton());
    await waitFor(() => expect(navigate).toHaveBeenCalled());
    expect(applied).toEqual(["media", "logs"]);
  });

  it("sends nothing while a volume has no source or target", async () => {
    await openPicker({});
    fireEvent.change(screen.getByLabelText("Image"), { target: { value: "nginx:1" } });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "x" } });
    expect(await screen.findByText(/Volume 1 has no source\./)).toBeTruthy();
    expect((screen.getByRole("button", { name: "Create container" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
