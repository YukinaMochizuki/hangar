// @vitest-environment jsdom

// Replace value: the whole value is given again and applied over the
// secret as the dialog read it, at its resource version, with every key the
// secret's type holds, since apply replaces spec.data whole. A newer secret
// is never written over. The value never reaches what the query client
// keeps. A secret's labels are shown, and changed only with its value.

import { Code, ConnectError } from "@connectrpc/connect";
import { SecretType, type ApplySecretRequest } from "@hangar/api/hangar/secrets/v1alpha1/secret_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { SecretDetail } from "@/pages/secrets";
import { cacheText, container, credentialsApi, secret } from "@/test/credentials";
import { polyfillDialog, withApi } from "@/test/harness";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
afterEach(cleanup);

// An obviously fake value a test types, which must never be found again.
const password = "EXAMPLE-password-5a2b";

const hub = (version = 5n) => secret("hub", { registries: ["index.docker.io", "registry.example.com"], version, labels: { team: "web" } });
const type = (label: string, value: string) => fireEvent.change(within(screen.getByRole("dialog")).getByLabelText(label), { target: { value } });
const replace = () => within(screen.getByRole("dialog")).getByRole("button", { name: /Replace value|Replacing/ }) as HTMLButtonElement;

/** The secret's page over an API that records each apply and refuses with fail() when it says so; Replace value opened. */
async function open(o: { fail?: () => ConnectError | undefined; current?: () => ReturnType<typeof hub> } = {}) {
  const applied: ApplySecretRequest[] = [];
  const api = credentialsApi({
    secrets: async () => ({ items: [hub()] }),
    containers: async () => ({ items: [container("web-1", ["hub"])] }),
    getSecret: () => o.current?.() ?? hub(),
    applySecret: (req) => {
      applied.push(req);
      const err = o.fail?.();
      if (err) throw err;
    },
  });
  render(<SecretDetail name="hub" />, { wrapper: withApi(api) });
  fireEvent.click(await screen.findByRole("button", { name: /Replace value/ }));
  return { api, applied };
}

function login() {
  type("Registry", "index.docker.io");
  type("Username", "me");
  type("Password or access token", password);
}

const dockerConfigOf = (req: ApplySecretRequest) => JSON.parse(new TextDecoder().decode(req.secret!.spec!.data[".dockerconfigjson"]));

describe("Replace value", () => {
  it("says what replacing does: the old value gone, every registry's login replaced, and when containers naming it see the new one", async () => {
    await open();
    const d = screen.getByRole("dialog");
    expect(within(d).getByText(/Once applied, the old value is gone/)).toBeTruthy();
    expect(within(d).getByText(/a login for index\.docker\.io and registry\.example\.com; the new value replaces all of them\./)).toBeTruthy();
    expect(await within(d).findByText(/web-1 names it\. What runs keeps running; the new value counts at each one's next image pull\./)).toBeTruthy();
    expect(within(d).getByText(/^hangar create secret registry hub --from-docker-config FILE -p demo$/)).toBeTruthy();
    // The command carries neither the labels nor the version, and the dialog says so beside it.
    expect(
      within(d).getByText("Unlike this dialog, the command sets no labels, so it drops hub's, and it replaces the secret whatever changed since it was read."),
    ).toBeTruthy();
  });

  it("applies every key of the secret's type, over the secret as read and at its version", async () => {
    const { applied, api } = await open();
    expect(replace().disabled).toBe(true);
    login();
    fireEvent.click(replace());
    await waitFor(() => expect(applied).toHaveLength(1));
    const sent = applied[0];
    expect(sent.createOnly).toBe(false);
    expect(sent.secret?.metadata).toMatchObject({ name: "hub", project: "demo", uid: "uid-hub", resourceVersion: 5n, labels: { team: "web" } });
    expect(sent.secret?.spec?.type).toBe(SecretType.REGISTRY_AUTH);
    expect(Object.keys(sent.secret!.spec!.data)).toEqual([".dockerconfigjson"]);
    expect(dockerConfigOf(sent)).toEqual({ auths: { "index.docker.io": { username: "me", password } } });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(api.calls).not.toContain("GetSecret");
    expect(cacheText(api.client)).not.toContain(password);
  });

  it("sends a pasted Docker config as given", async () => {
    const { applied } = await open();
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("radio", { name: "Docker config" }));
    const config = '{"auths":{"ghcr.io":{"auth":"EXAMPLE"}}}';
    type("Docker config", config);
    fireEvent.click(replace());
    await waitFor(() => expect(applied).toHaveLength(1));
    expect(new TextDecoder().decode(applied[0].secret!.spec!.data[".dockerconfigjson"])).toBe(config);
  });

  it("writes nothing over a newer secret: says it changed, and reads nothing by itself", async () => {
    const { applied, api } = await open({ fail: () => new ConnectError("Secret hub is at resource version 6, not 5", Code.Aborted) });
    login();
    fireEvent.click(replace());
    await screen.findByText("hub changed since this dialog opened.");
    expect(replace().disabled).toBe(true);
    expect(applied).toHaveLength(1);
    expect(api.calls).not.toContain("GetSecret");
  });

  it("moves onto the secret as it is now on Reload, and applies at its version once asked again", async () => {
    let refused = true;
    const { applied, api } = await open({
      current: () => hub(6n),
      fail: () => (refused ? new ConnectError("Secret hub is at resource version 6, not 5", Code.Aborted) : undefined),
    });
    login();
    fireEvent.click(replace());
    await screen.findByText("hub changed since this dialog opened.");
    refused = false;
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(screen.queryByText("hub changed since this dialog opened.")).toBeNull());
    expect(api.calls.filter((c) => c === "GetSecret")).toHaveLength(1);
    expect(applied).toHaveLength(1);
    fireEvent.click(replace());
    await waitFor(() => expect(applied).toHaveLength(2));
    expect(applied[1].secret?.metadata?.resourceVersion).toBe(6n);
    expect(Object.keys(applied[1].secret!.spec!.data)).toEqual([".dockerconfigjson"]);
    expect(cacheText(api.client)).not.toContain(password);
  });
});

describe("A secret's labels", () => {
  it("are shown, with no Edit labels, and say they change only with the value", async () => {
    render(<SecretDetail name="hub" />, { wrapper: withApi(credentialsApi({ secrets: async () => ({ items: [hub()] }) })) });
    await screen.findByRole("button", { name: /Replace value/ });
    expect(screen.getAllByText((_, el) => el?.textContent === "team=web").length).toBeGreaterThan(0);
    expect(screen.getByText("Changed only with the value, by hangar apply.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Edit labels/ })).toBeNull();
  });
});
