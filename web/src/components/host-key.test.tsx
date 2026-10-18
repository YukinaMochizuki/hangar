// @vitest-environment jsdom

import { create } from "@bufbuild/protobuf";
import { MachineCapabilityService } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { VirtualMachineSchema } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { fingerprint, forgetPermission, HostKeys } from "@/components/host-key";
import { fakeApi, polyfillDialog, withApi } from "@/test/harness";

beforeAll(polyfillDialog);
afterEach(cleanup);

// A key and the fingerprint `ssh-keygen -lf` gives it.
const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINu2i6x0dcp0yQsLMNDwhFWovnb0v4VdOu3BDsmHPy06";
const keygen = "SHA256:kD27PSOVVEiKpvTZArTdJOI0WXKofIoUiXVggWczYTs";

const vm = (keys: string[]) =>
  create(VirtualMachineSchema, {
    metadata: { name: "db", project: "demo" },
    status: { sshHostKeys: keys.map((publicKey) => ({ publicKey, recordTime: { seconds: 1760000000n } })) },
  });

function api(held: string[]) {
  const forgets: unknown[] = [];
  const a = fakeApi((r) => {
    r.service(AccountService, { testPermissions: (req) => ({ permissions: req.permissions.filter((p) => held.includes(p)) }) });
    r.service(MachineCapabilityService, {
      forgetSSHHostKeys: (req) => {
        forgets.push({ project: req.project, kind: req.kind, name: req.name });
        return {};
      },
    });
  });
  return { a, forgets };
}

describe("fingerprint", () => {
  it("is the key's type and SHA256 as ssh-keygen -l shows it", async () => {
    expect(await fingerprint(`${key} a-comment`)).toBe(`ssh-ed25519 ${keygen}`);
  });

  it("is none for a key that is not base64", async () => {
    expect(await fingerprint("ssh-ed25519 not*base64")).toBeUndefined();
  });

  it("is none outside a secure context, where the browser gives no crypto.subtle", async () => {
    vi.stubGlobal("crypto", {});
    try {
      expect(await fingerprint(key)).toBeUndefined();
    } finally {
      vi.unstubAllGlobals();
    }
  });
});

describe("HostKeys", () => {
  it("says no key is recorded before the first terminal login", () => {
    render(<HostKeys vm={vm([])} project="demo" />, { wrapper: withApi(api([forgetPermission]).a) });
    expect(screen.getByText(/Not recorded yet/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Forget host key" })).toBeNull();
  });

  it("shows the recorded key's fingerprint and when it was recorded", async () => {
    render(<HostKeys vm={vm([key])} project="demo" />, { wrapper: withApi(api([]).a) });
    expect(await screen.findByText(`ssh-ed25519 ${keygen}`)).toBeTruthy();
    expect(screen.getByText(/recorded 2025-10-09 08:53 UTC/)).toBeTruthy();
  });

  it("shows the key itself where no fingerprint can be worked out", async () => {
    vi.stubGlobal("crypto", {});
    try {
      render(<HostKeys vm={vm([key])} project="demo" />, { wrapper: withApi(api([]).a) });
      expect(await screen.findByText(key)).toBeTruthy();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("puts its dialog outside the field it sits in, so the field's word breaking does not reach the dialog's prose", async () => {
    const { a } = api([forgetPermission]);
    render(
      <dl>
        <dd className="break-all">
          <HostKeys vm={vm([key])} project="demo" />
        </dd>
      </dl>,
      { wrapper: withApi(a) },
    );
    fireEvent.click(await screen.findByRole("button", { name: "Forget host key" }));
    expect(screen.getByText(/The next terminal login trusts/).closest("dd")).toBeNull();
  });

  it("offers Forget only to a caller who may change the VM", async () => {
    const { a } = api(["compute.machines.ssh"]);
    render(<HostKeys vm={vm([key])} project="demo" />, { wrapper: withApi(a) });
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    await waitFor(() => expect(a.client.isFetching()).toBe(0));
    expect(screen.queryByRole("button", { name: "Forget host key" })).toBeNull();
  });

  it("forgets the VM's host keys after asking, through ForgetSSHHostKeys", async () => {
    const { a, forgets } = api([forgetPermission]);
    render(<HostKeys vm={vm([key])} project="demo" />, { wrapper: withApi(a) });
    fireEvent.click(await screen.findByRole("button", { name: "Forget host key" }));
    expect(screen.getByText(/The next terminal login trusts whatever key the VM then shows/)).toBeTruthy();
    expect(forgets).toEqual([]);
    const buttons = screen.getAllByRole("button", { name: "Forget host key" });
    fireEvent.click(buttons.at(-1)!);
    await waitFor(() => expect(forgets).toEqual([{ project: "demo", kind: "VirtualMachine", name: "db" }]));
  });
});
