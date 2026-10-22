// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import type { CreateServiceTokenRequest, RotateServiceTokenRequest } from "@hangar/api/hangar/iam/v1alpha1/service_token_issuer_pb";
import type { DeleteServiceTokenRequest, ServiceToken } from "@hangar/api/hangar/iam/v1alpha1/service_token_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { CreateServiceToken } from "@/pages/create-service-token";
import { ServiceTokenDetail, ServiceTokenList } from "@/pages/service-tokens";
import { binding, cacheText, credentialsApi, issued, token } from "@/test/credentials";
import { never, polyfillDialog, withApi } from "@/test/harness";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
beforeEach(() => navigate.mockReset());
afterEach(cleanup);

const dialog = () => screen.getByRole("dialog");

/** Everything a person could read: the page, every command line in it included. */
const page = () => document.body.innerHTML;

/** Each command line the page shows, as text. */
const commandLines = () => Array.from(document.querySelectorAll("code, pre")).map((e) => e.textContent ?? "");

const owner = ["iam.servicetokens.apply", "iam.servicetokens.delete"];

describe("ServiceTokenList", () => {
  it("shows skeletons while the tokens load", async () => {
    render(<ServiceTokenList />, { wrapper: withApi(credentialsApi({ tokens: never })) });
    expect((await screen.findAllByLabelText("Loading")).length).toBeGreaterThan(0);
  });

  it("says the list failed with the API's error, and asks again on Retry", async () => {
    let list = () => Promise.reject<{ items: ServiceToken[] }>(new ConnectError("the apiserver is unavailable", Code.Unavailable));
    const a = credentialsApi({ tokens: () => list() });
    render(<ServiceTokenList />, { wrapper: withApi(a) });
    await screen.findByText("The service tokens did not load");
    list = async () => ({ items: [token("ci")] });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await within(await screen.findByRole("table")).findByText("ci");
    expect(a.calls.filter((c) => c === "ListServiceTokens").length).toBe(2);
  });

  it("says the project has no tokens", async () => {
    render(<ServiceTokenList />, { wrapper: withApi(credentialsApi({})) });
    await screen.findByText("No service tokens");
  });

  it("shows each token's state, its roles from the bindings naming it, and its secret's first characters alone", async () => {
    const a = credentialsApi({
      tokens: async () => ({
        items: [token("ci", { description: "Deploys" }), token("bot", { expires: 5 }), token("old", { expires: -3 }), token("staged", { prefix: "" })],
      }),
      bindings: async () => ({
        items: [
          binding({ serviceToken: "demo/ci" }, "project.editor"),
          binding({ serviceToken: "demo/bot" }, "project.owner"),
          binding({ user: "someone@example.com" }, "project.viewer"),
          binding({ serviceToken: "other/old" }, "project.owner", "other"),
        ],
      }),
    });
    render(<ServiceTokenList />, { wrapper: withApi(a) });
    const t = await screen.findByRole("table");
    const row = (n: string) => within(t).getAllByRole("row").find((r) => r.textContent?.startsWith(n))!;
    await waitFor(() => expect(within(row("ci")).getByText("Editor")).toBeTruthy());
    expect(within(row("ci")).getByText("Active")).toBeTruthy();
    expect(within(row("ci")).getByText("hgr_ciPREFIX…")).toBeTruthy();
    expect(within(row("bot")).getByText("Owner")).toBeTruthy();
    expect(within(row("bot")).getByText("Expiring")).toBeTruthy();
    expect(within(row("old")).getByText("Expired")).toBeTruthy();
    expect(within(row("old")).getAllByText("none").length).toBe(1);
    expect(within(row("staged")).getByText("No secret")).toBeTruthy();
    expect(a.calls).toContain("ListRoleBindings");
  });

  it("says the roles are not known when the bindings did not load", async () => {
    const a = credentialsApi({ tokens: async () => ({ items: [token("ci")] }), bindings: () => Promise.reject(new ConnectError("no", Code.PermissionDenied)) });
    render(<ServiceTokenList />, { wrapper: withApi(a) });
    await within(await screen.findByRole("table")).findByText("not known");
  });

  it("offers New token only to whoever holds iam.servicetokens.apply: an editor reads tokens, writes none", async () => {
    const a = credentialsApi({ tokens: async () => ({ items: [token("ci")] }), perms: async () => ["secrets.secrets.apply", "secrets.secrets.delete"] });
    render(<ServiceTokenList />, { wrapper: withApi(a) });
    await screen.findByRole("table");
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("link", { name: /New token/ })).toBeNull();
    expect(a.requests.find((r) => r.method === "TestPermissions")?.request).toMatchObject({ permissions: ["iam.servicetokens.apply", "iam.servicetokens.delete"] });
    cleanup();

    const b = credentialsApi({ tokens: async () => ({ items: [token("ci")] }), perms: () => never() });
    render(<ServiceTokenList />, { wrapper: withApi(b) });
    await screen.findByRole("table");
    expect(screen.queryByRole("link", { name: /New token/ })).toBeNull();
    cleanup();

    const c = credentialsApi({ tokens: async () => ({ items: [token("ci")] }), perms: () => Promise.reject(new ConnectError("no", Code.Unavailable)) });
    render(<ServiceTokenList />, { wrapper: withApi(c) });
    await screen.findByRole("table");
    await waitFor(() => expect(c.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("link", { name: /New token/ })).toBeNull();
  });
});

describe("New token", () => {
  /** Renders the New service token page over a list holding "taken", once the form is drawn. */
  async function open(f: Parameters<typeof credentialsApi>[0] = {}) {
    const a = credentialsApi({ tokens: async () => ({ items: [token("taken")] }), perms: async () => owner, ...f });
    const view = render(<CreateServiceToken />, { wrapper: withApi(a) });
    await screen.findByLabelText("Token name");
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    return Object.assign(a, { unmount: view.unmount });
  }
  const typeIn = (label: string | RegExp, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
  const submit = () => screen.getByRole("button", { name: /Create token|Loading names/ }) as HTMLButtonElement;
  const form = () => screen.getByLabelText("Token name").closest("form")!;

  it("creates a token with one CreateServiceToken, shows its secret once, and opens the token when closed", async () => {
    let sent: CreateServiceTokenRequest | undefined;
    const a = await open({ createToken: (r) => (sent = r) });
    typeIn("Token name", "release");
    typeIn("Description", "Publishes builds");
    fireEvent.click(screen.getByRole("radio", { name: /Editor/ }));
    typeIn("Secret works for", "2160h");
    expect(commandLines()).toContain("$ hangar token create release --role project.editor --duration 2160h --description 'Publishes builds' -p demo");
    fireEvent.click(submit());

    const once = await screen.findByRole("dialog", { name: "Token release created" });
    expect(within(once).getByLabelText("Secret").textContent).toBe(issued[0]);
    expect(within(once).getByText(/This secret is shown once/)).toBeTruthy();
    expect(within(once).getByText(/no page, command or answer of the API shows it again/)).toBeTruthy();
    expect(within(once).getByRole("button", { name: "Copy" })).toBeTruthy();
    expect(a.calls.filter((c) => c === "CreateServiceToken")).toHaveLength(1);
    expect(sent).toMatchObject({ project: "demo", name: "release", description: "Publishes builds", role: "project.editor" });
    expect(sent?.duration?.seconds).toBe(2160n * 3600n);
    // While shown, the secret is in this dialog alone: in no command line, and nowhere the query client keeps.
    for (const c of commandLines().filter((t) => t !== issued[0])) expect(c).not.toContain(issued[0]);
    // The cache is read in full: it holds the create's arguments, and the token list.
    expect(cacheText(a.client)).toContain("Publishes builds");
    expect(cacheText(a.client)).toContain("hgr_takPREFIX");
    expect(cacheText(a.client)).not.toContain(issued[0]);

    fireEvent.click(within(once).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/service-tokens/$name", params: { project: "demo", name: "release" }, replace: true });
    expect(page()).not.toContain(issued[0]);
    expect(cacheText(a.client)).not.toContain(issued[0]);
    // The issue leaves the query client with its page, so nothing is left to replay it.
    a.unmount();
    await waitFor(() => expect(a.client.getMutationCache().getAll()).toHaveLength(0));
    expect(page()).not.toContain(issued[0]);
  });

  it("closes the secret's dialog only with its own button, not with Escape", async () => {
    await open();
    typeIn("Token name", "release");
    fireEvent.click(submit());
    const once = (await screen.findByRole("dialog", { name: "Token release created" })) as HTMLDialogElement;
    const cancel = new Event("cancel", { cancelable: true });
    once.dispatchEvent(cancel);
    expect(cancel.defaultPrevented).toBe(true);
    fireEvent.click(once);
    expect(screen.getByRole("dialog", { name: "Token release created" })).toBeTruthy();
    expect(screen.getByLabelText("Secret").textContent).toBe(issued[0]);
    expect(navigate).not.toHaveBeenCalled();

    // Chromium, after a few Escapes, sends a cancel that cannot be prevented and closes the element itself.
    let closed = 0;
    once.addEventListener("close", () => closed++);
    once.dispatchEvent(new Event("cancel", { cancelable: false }));
    once.close();
    // The element did close; the dialog takes that close as not asked for and opens it again, with the secret.
    expect(closed).toBe(1);
    await waitFor(() => expect(once.open).toBe(true));
    expect(screen.getByRole("dialog", { name: "Token release created" })).toBe(once);
    expect(screen.getByLabelText("Secret").textContent).toBe(issued[0]);
    // Its own button still closes it, and drops the secret.
    fireEvent.click(within(once).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByLabelText("Secret")).toBeNull());
    expect(page()).not.toContain(issued[0]);
  });

  it("keeps a shown secret when the list fails to load again, until Close", async () => {
    let fail = false;
    const a = await open({
      tokens: async () => {
        if (fail) throw new ConnectError("the apiserver is unavailable", Code.Unavailable);
        return { items: [token("taken")] };
      },
    });
    typeIn("Token name", "release");
    fireEvent.click(submit());
    await screen.findByRole("dialog", { name: "Token release created" });
    fail = true;
    await a.client.refetchQueries();
    await waitFor(() => expect(a.calls.filter((c) => c === "ListServiceTokens").length).toBeGreaterThan(1));
    expect(screen.getByLabelText("Secret").textContent).toBe(issued[0]);
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(page()).not.toContain(issued[0]);
  });

  it("keeps a shown secret when the permission answer changes while it is open, until Close", async () => {
    let held = owner;
    const a = await open({ perms: async () => held });
    typeIn("Token name", "release");
    fireEvent.click(submit());
    await screen.findByRole("dialog", { name: "Token release created" });
    held = [];
    await a.client.refetchQueries();
    await waitFor(() => expect(a.calls.filter((c) => c === "TestPermissions").length).toBeGreaterThan(1));
    expect(screen.getByLabelText("Secret").textContent).toBe(issued[0]);
    expect(screen.queryByText(/roles do not include/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(page()).not.toContain(issued[0]);
  });

  it("offers NAME, never a name the form would not send", async () => {
    await open();
    for (const bad of ["$(whoami)", "Bad_Name", "taken"]) {
      typeIn("Token name", bad);
      expect(commandLines()).toContain("$ hangar token create NAME -p demo");
      expect(screen.getByText("It authenticates as serviceToken:demo/NAME.")).toBeTruthy();
    }
    typeIn("Token name", "ci-bot");
    expect(commandLines()).toContain("$ hangar token create ci-bot -p demo");
  });

  it("sends no role and no duration when none is chosen, and binds nothing", async () => {
    let sent: CreateServiceTokenRequest | undefined;
    await open({ createToken: (r) => (sent = r) });
    typeIn("Token name", "plain");
    expect(commandLines()).toContain("$ hangar token create plain -p demo");
    fireEvent.click(submit());
    await screen.findByRole("dialog", { name: "Token plain created" });
    expect(sent?.role).toBe("");
    expect(sent?.duration).toBeUndefined();
  });

  it("sends nothing for a name the list holds, and says a name taken since exists", async () => {
    let items = [token("taken")];
    const a = await open({ tokens: async () => ({ items }) });
    typeIn("Token name", "taken");
    expect(screen.getByText(/A service token named taken exists in this project\./)).toBeTruthy();
    expect(submit().disabled).toBe(true);
    expect(a.calls).not.toContain("CreateServiceToken");

    items = [token("taken"), token("late")];
    typeIn("Token name", "late");
    fireEvent.click(submit());
    // The refusal itself, in the form's words: not the API's, and not the name check's after the list reloads.
    const refusal = await screen.findByRole("alert");
    expect(within(refusal).getByText("Creating the token failed")).toBeTruthy();
    expect(within(refusal).getByText("A service token named late exists in this project.")).toBeTruthy();
    expect(refusal.textContent).not.toContain("already exists");
    expect(screen.queryByLabelText("Secret")).toBeNull();
    // Submitting the form some other way, with the taken name, sends nothing more: the next create is the next one sent.
    fireEvent.submit(form());
    typeIn("Token name", "fresh");
    fireEvent.submit(form());
    await screen.findByRole("dialog", { name: "Token fresh created" });
    expect(a.requests.filter((r) => r.method === "CreateServiceToken").map((r) => (r.request as CreateServiceTokenRequest).name)).toEqual(["late", "fresh"]);
  });
});

describe("ServiceTokenDetail", () => {
  const fake = (f: Parameters<typeof credentialsApi>[0] = {}) =>
    credentialsApi({
      tokens: async () => ({ items: [token("ci", { description: "Deploys", used: 1 })] }),
      bindings: async () => ({ items: [binding({ serviceToken: "demo/ci" }, "project.editor")] }),
      perms: async () => owner,
      ...f,
    });

  it("shows the token, its principal and its roles, and never a secret", async () => {
    render(<ServiceTokenDetail name="ci" />, { wrapper: withApi(fake()) });
    await screen.findByRole("heading", { name: "ci" });
    expect(screen.getByText("serviceToken:demo/ci")).toBeTruthy();
    expect(screen.getByText("hgr_ciPREFIX…")).toBeTruthy();
    await screen.findByText("project.editor");
    expect(screen.getByText("deleted with the token")).toBeTruthy();
  });

  it("says a token the project lacks does not exist", async () => {
    render(<ServiceTokenDetail name="gone" />, { wrapper: withApi(fake()) });
    await screen.findByText("No such service token");
  });

  it("says a token no binding names holds nothing", async () => {
    render(<ServiceTokenDetail name="ci" />, { wrapper: withApi(fake({ bindings: async () => ({ items: [] }) })) });
    await screen.findByText("No binding names this token, so it holds nothing.");
  });

  it("rotates with one RotateServiceToken, shows the new secret once, and drops it when closed", async () => {
    let sent: RotateServiceTokenRequest | undefined;
    const a = fake({ rotateToken: (r) => (sent = r) });
    render(<ServiceTokenDetail name="ci" />, { wrapper: withApi(a) });
    fireEvent.click(await screen.findByRole("button", { name: /Rotate/ }));
    expect(within(dialog()).getByText(/stops working the moment the new one is stored/)).toBeTruthy();
    fireEvent.change(within(dialog()).getByLabelText("Secret works for"), { target: { value: "720h" } });
    expect(commandLines()).toContain("$ hangar token rotate ci --duration 720h -p demo");
    fireEvent.click(within(dialog()).getByRole("button", { name: /Rotate secret/ }));

    const once = await screen.findByRole("dialog", { name: "New secret of ci" });
    expect(within(once).getByLabelText("Secret").textContent).toBe(issued[0]);
    expect(sent).toMatchObject({ project: "demo", name: "ci" });
    expect(sent?.duration?.seconds).toBe(720n * 3600n);
    expect(cacheText(a.client)).not.toContain(issued[0]);

    fireEvent.click(within(once).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(page()).not.toContain(issued[0]);
    expect(cacheText(a.client)).not.toContain(issued[0]);
    // A second rotate shows the second secret, never the first again.
    fireEvent.click(screen.getByRole("button", { name: /Rotate/ }));
    fireEvent.click(within(dialog()).getByRole("button", { name: /Rotate secret/ }));
    await screen.findByRole("dialog", { name: "New secret of ci" });
    expect(screen.getByLabelText("Secret").textContent).toBe(issued[1]);
    expect(page()).not.toContain(issued[0]);
  });

  it("keeps a rotated secret, and the page's last answer, when the list fails to load again", async () => {
    let fail = false;
    const a = fake({
      tokens: async () => {
        if (fail) throw new ConnectError("the apiserver is unavailable", Code.Unavailable);
        return { items: [token("ci", { description: "Deploys", used: 1 })] };
      },
    });
    render(<ServiceTokenDetail name="ci" />, { wrapper: withApi(a) });
    fireEvent.click(await screen.findByRole("button", { name: /Rotate/ }));
    fail = true;
    // The rotate's own invalidate asks the list again, and it fails.
    fireEvent.click(within(dialog()).getByRole("button", { name: /Rotate secret/ }));
    await screen.findByRole("dialog", { name: "New secret of ci" });
    await screen.findByText("The service tokens did not load again; this page shows the last answer.");
    expect(screen.getByLabelText("Secret").textContent).toBe(issued[0]);
    expect(screen.getByRole("heading", { name: "ci" })).toBeTruthy();
    expect(screen.queryByText("The service token did not load")).toBeNull();
    // As does the poll, again.
    await a.client.refetchQueries();
    expect(screen.getByLabelText("Secret").textContent).toBe(issued[0]);
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByLabelText("Secret")).toBeNull());
    expect(page()).not.toContain(issued[0]);
  });

  it("keeps a rotated secret when the list no longer holds the token, until Close", async () => {
    let items = [token("ci")];
    const a = fake({ tokens: async () => ({ items }) });
    render(<ServiceTokenDetail name="ci" />, { wrapper: withApi(a) });
    fireEvent.click(await screen.findByRole("button", { name: /Rotate/ }));
    items = [];
    fireEvent.click(within(dialog()).getByRole("button", { name: /Rotate secret/ }));
    await screen.findByRole("dialog", { name: "New secret of ci" });
    await screen.findByText("No such service token");
    expect(screen.getByLabelText("Secret").textContent).toBe(issued[0]);
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByLabelText("Secret")).toBeNull());
  });

  it("shows the API's refusal to rotate in the dialog, and no secret", async () => {
    const a = fake({
      rotateToken: () => {
        throw new ConnectError("permission denied", Code.PermissionDenied);
      },
    });
    render(<ServiceTokenDetail name="ci" />, { wrapper: withApi(a) });
    fireEvent.click(await screen.findByRole("button", { name: /Rotate/ }));
    fireEvent.click(within(dialog()).getByRole("button", { name: /Rotate secret/ }));
    await within(dialog()).findByText("Rotating failed");
    expect(screen.queryByLabelText("Secret")).toBeNull();
  });

  it("deletes after the name is typed, saying the bindings go with it", async () => {
    let sent: DeleteServiceTokenRequest | undefined;
    render(<ServiceTokenDetail name="ci" />, { wrapper: withApi(fake({ deleteToken: (r) => (sent = r) })) });
    await screen.findByText("project.editor");
    fireEvent.click(await screen.findByRole("button", { name: /Delete/ }));
    expect(within(dialog()).getByText(/the bindings that grant it Editor are deleted with it/)).toBeTruthy();
    expect(commandLines()).toContain("$ hangar token delete ci -p demo");
    const del = within(dialog()).getByRole("button", { name: /^Delete$/ }) as HTMLButtonElement;
    expect(del.disabled).toBe(true);
    fireEvent.change(within(dialog()).getByLabelText(/Name, to confirm/), { target: { value: "ci" } });
    fireEvent.click(del);
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/service-tokens", params: { project: "demo" } }));
    expect(sent).toMatchObject({ project: "demo", name: "ci" });
  });

  it("is read-only for an editor: no Rotate, no Delete, and only the get command", async () => {
    const a = fake({ perms: async () => ["secrets.secrets.apply", "secrets.secrets.delete"] });
    render(<ServiceTokenDetail name="ci" />, { wrapper: withApi(a) });
    await screen.findByRole("heading", { name: "ci" });
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("button", { name: /Rotate/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Delete/ })).toBeNull();
    expect(screen.getByText(/hangar token get ci -p demo/)).toBeTruthy();
    expect(screen.queryByText(/hangar token rotate/)).toBeNull();
    expect(screen.queryByText(/hangar token delete/)).toBeNull();
    expect(a.requests.find((r) => r.method === "TestPermissions")?.request).toMatchObject({
      permissions: ["iam.servicetokens.apply", "iam.servicetokens.delete"],
    });
  });
});
