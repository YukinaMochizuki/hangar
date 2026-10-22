// @vitest-environment jsdom

// The Edit labels dialog: it applies the object as it read it with its
// labels alone changed, at the resource version read; it refuses what the
// API would; and when the object changed since, it says so and applies
// nothing until Reload has read it again and Apply is pressed again.

import { create, equals } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { DiskSchema, type ApplyDiskRequest, type Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { polyfillDialog, withApi } from "@/test/harness";
import { disk, storageApi } from "@/test/storage";

beforeAll(polyfillDialog);
afterEach(cleanup);

const system = "storage.hangarlab.cloud/root-disk-of";
const pg = (version = 7n, labels: Record<string, string> = { app: "pg", tier: "db", [system]: "uid-db" }) => disk("pg", { version, labels });

/** The dialog over an API that records each apply, refuses with fail() when it says so, and answers GetDisk with current(). */
function open(o: { fail?: () => ConnectError | undefined; current?: () => Disk } = {}) {
  const applied: ApplyDiskRequest[] = [];
  const api = storageApi({
    get: () => o.current?.() ?? pg(),
    apply: (req) => {
      applied.push(req);
      const err = o.fail?.();
      if (err) throw err;
      return { disk: req.disk, created: false };
    },
  });
  const onClose = vi.fn();
  render(<EditLabelsDialog kind="disk" object={pg()} onClose={onClose} />, { wrapper: withApi(api) });
  return { api, applied, onClose, dialog: screen.getByRole("dialog") };
}

const type = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
const applyButton = () => screen.getByRole("button", { name: /Apply labels|Applying/ }) as HTMLButtonElement;
const gets = (api: ReturnType<typeof storageApi>) => api.calls.filter((c) => c === "GetDisk");

describe("Edit labels", () => {
  it("applies the object as read, at its version, with only its labels changed", async () => {
    const { applied, onClose, dialog, api } = open();
    // The system's label is shown locked, not as a field.
    expect(within(dialog).getByText(system)).toBeTruthy();
    expect(screen.getAllByRole("textbox", { name: /^Key/ })).toHaveLength(2);
    type("Value 2", "data");
    fireEvent.click(screen.getByRole("button", { name: "Remove app" }));
    fireEvent.click(screen.getByRole("button", { name: /Add label/ }));
    type("Key 2", "team");
    type("Value 2", "ops");
    expect(within(dialog).getByText("Adds team · Changes tier · Removes app")).toBeTruthy();
    fireEvent.click(applyButton());
    await waitFor(() => expect(applied).toHaveLength(1));
    const want = pg();
    want.metadata!.labels = { tier: "data", team: "ops", [system]: "uid-db" };
    expect(equals(DiskSchema, applied[0].disk!, want)).toBe(true);
    expect(applied[0].disk!.metadata!.resourceVersion).toBe(7n);
    expect(applied[0].createOnly).toBe(false);
    expect(gets(api)).toHaveLength(0);
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("says No change and applies nothing until a label changes", () => {
    const { applied, dialog } = open();
    expect(within(dialog).getByText("No change.")).toBeTruthy();
    expect(applyButton().disabled).toBe(true);
    fireEvent.click(applyButton());
    expect(applied).toHaveLength(0);
  });

  it("refuses on its row what the API would refuse, and sends nothing", () => {
    const { applied, dialog } = open();
    fireEvent.click(screen.getByRole("button", { name: /Add label/ }));
    type("Value 3", "orphan");
    expect(within(dialog).getByText("A label needs a key.")).toBeTruthy();
    type("Key 3", "hangarlab.cloud/owner");
    expect(within(dialog).getByText("Keys under hangarlab.cloud/ are the system's.")).toBeTruthy();
    expect((screen.getByLabelText("Key 3") as HTMLInputElement).getAttribute("aria-invalid")).toBe("true");
    type("Key 3", "app");
    expect(within(dialog).getByText("This key is given above.")).toBeTruthy();
    type("Key 3", "owner");
    type("Value 3", "x".repeat(64));
    expect(within(dialog).getByText("A value is at most 63 bytes.")).toBeTruthy();
    expect(applyButton().disabled).toBe(true);
    fireEvent.click(applyButton());
    expect(applied).toHaveLength(0);
  });

  it("writes nothing over a newer object: says it changed, and reads nothing by itself", async () => {
    const { applied, api, dialog } = open({ fail: () => new ConnectError("Disk pg is at resource version 9, not 7", Code.Aborted) });
    type("Value 2", "data");
    fireEvent.click(applyButton());
    await within(dialog).findByText("pg changed since this dialog opened.");
    expect(within(dialog).getByText("Nothing was applied.")).toBeTruthy();
    expect(applyButton().disabled).toBe(true);
    expect(applied).toHaveLength(1);
    expect(gets(api)).toHaveLength(0);
  });

  it("starts again from the object as it is now on Reload, and applies at its version", async () => {
    let refused = true;
    const now = pg(9n, { app: "pg", tier: "gold", owner: "ops", [system]: "uid-db" });
    const { applied, api, dialog } = open({
      current: () => now,
      fail: () => (refused ? new ConnectError("Disk pg is at resource version 9, not 7", Code.Aborted) : undefined),
    });
    type("Value 2", "data");
    fireEvent.click(applyButton());
    await within(dialog).findByText("pg changed since this dialog opened.");
    refused = false;
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(within(dialog).queryByText("pg changed since this dialog opened.")).toBeNull());
    expect(gets(api)).toHaveLength(1);
    // The rows are the object's as it is now; what was typed before is gone.
    expect((screen.getByLabelText("Value 3") as HTMLInputElement).value).toBe("gold");
    expect(screen.queryByDisplayValue("data")).toBeNull();
    expect(applied).toHaveLength(1);
    type("Value 3", "silver");
    fireEvent.click(applyButton());
    await waitFor(() => expect(applied).toHaveLength(2));
    expect(applied[1].disk!.metadata!.resourceVersion).toBe(9n);
    expect(applied[1].disk!.metadata!.labels).toEqual({ app: "pg", tier: "silver", owner: "ops", [system]: "uid-db" });
  });

  it("applies nothing for an object read without a version", async () => {
    const applied: ApplyDiskRequest[] = [];
    const api = storageApi({ apply: (req) => (applied.push(req), { disk: req.disk, created: false }) });
    const unversioned = create(DiskSchema, { metadata: { name: "pg", project: "demo", labels: { app: "pg" } }, spec: { size: "20Gi" } });
    render(<EditLabelsDialog kind="disk" object={unversioned} onClose={() => {}} />, { wrapper: withApi(api) });
    type("Value 1", "web");
    fireEvent.click(applyButton());
    await screen.findByText("pg carries no resource version to apply at.");
    expect(applied).toHaveLength(0);
  });
});
