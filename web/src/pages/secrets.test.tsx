// @vitest-environment jsdom

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { SecretSpecSchema, SecretType, type ApplySecretRequest, type DeleteSecretRequest, type Secret } from "@hangar/api/hangar/secrets/v1alpha1/secret_pb";
import { cleanup, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { errNoValues, useSecrets } from "@/lib/credentials";
import { SecretDetail, SecretList } from "@/pages/secrets";
import { cacheText, container, credentialsApi, secret } from "@/test/credentials";
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

// Obviously fake values a test types, which must never be found again.
const password = "EXAMPLE-password-0f9e";
const configValue = "EXAMPLE-config-auth-7c1d";

const dialog = () => screen.getByRole("dialog");
const form = () => within(dialog()).getByLabelText("Name").closest("form")!;
/** The open dialog's command line, as shown. */
const cli = () => within(dialog()).getByText(/^hangar /).closest("code")!.textContent;
const type = (label: string | RegExp, value: string) => fireEvent.change(within(dialog()).getByLabelText(label), { target: { value } });

/** A secret answered with values, as no API should answer: the page must still show none of them. */
function leakySecret(name: string): Secret {
  const s = secret(name);
  s.spec = create(SecretSpecSchema, {
    type: SecretType.REGISTRY_AUTH,
    data: { ".dockerconfigjson": new TextEncoder().encode(`{"auths":{"registry.example.com":{"password":"${password}"}}}`) },
    sealed: { kekId: "dev", ciphertext: new TextEncoder().encode("EXAMPLE-ciphertext"), contentHmac: "hmac-sha256:EXAMPLESEALED" },
  });
  return s;
}

describe("SecretList", () => {
  it("shows skeletons while the secrets load", async () => {
    render(<SecretList />, { wrapper: withApi(credentialsApi({ secrets: never })) });
    expect((await screen.findAllByLabelText("Loading")).length).toBeGreaterThan(0);
  });

  it("says the list failed with the API's error, and asks again on Retry", async () => {
    let list = () => Promise.reject<{ items: Secret[] }>(new ConnectError("the apiserver is unavailable", Code.Unavailable));
    const a = credentialsApi({ secrets: () => list() });
    render(<SecretList />, { wrapper: withApi(a) });
    await screen.findByText("The secrets did not load");
    expect(screen.getByText("the apiserver is unavailable")).toBeTruthy();
    list = async () => ({ items: [secret("login")] });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await within(await screen.findByRole("table")).findByText("login");
    expect(a.calls.filter((c) => c === "ListSecrets").length).toBe(2);
  });

  it("says the project has no secrets, offering New secret to whoever may create one", async () => {
    render(<SecretList />, { wrapper: withApi(credentialsApi({})) });
    await screen.findByText("No secrets");
    await waitFor(() => expect(screen.getAllByRole("button", { name: /New secret/ }).length).toBe(2));
  });

  it("shows registries, keys and the containers naming each secret", async () => {
    const a = credentialsApi({
      secrets: async () => ({ items: [secret("login", { registries: ["registry.example.com", "index.docker.io"] }), secret("spare")] }),
      containers: async () => ({ items: [container("worker", ["login"]), container("api", ["login"])] }),
    });
    render(<SecretList />, { wrapper: withApi(a) });
    const t = await screen.findByRole("table");
    const row = (n: string) => within(t).getAllByRole("row").find((r) => r.textContent?.includes(n))!;
    await waitFor(() => expect(within(row("login")).getAllByRole("link").map((l) => l.textContent)).toEqual(["login", "api", "worker"]));
    expect(within(row("login")).getByText("registry.example.com, index.docker.io")).toBeTruthy();
    expect(within(row("login")).getByText(".dockerconfigjson")).toBeTruthy();
    expect(within(row("login")).getByRole("link", { name: "api" }).getAttribute("href")).toBe("/projects/demo/containers/api");
    expect(within(row("spare")).getByText("none")).toBeTruthy();
    expect(a.calls).toContain("ListContainers");
  });

  it("says which containers name a secret is not known when they did not load", async () => {
    const a = credentialsApi({
      secrets: async () => ({ items: [secret("login")] }),
      containers: () => Promise.reject(new ConnectError("no", Code.Unavailable)),
    });
    render(<SecretList />, { wrapper: withApi(a) });
    await within(await screen.findByRole("table")).findByText("not known: the containers did not load");
  });

  it("shows a secret the apiserver cannot open, and one not checked yet", async () => {
    const a = credentialsApi({
      secrets: async () => ({
        items: [secret("broken", { ready: [ConditionStatus.FALSE, "Undecryptable", "the key is wrong"] }), secret("fresh", { ready: null })],
      }),
    });
    render(<SecretList />, { wrapper: withApi(a) });
    const t = await screen.findByRole("table");
    expect(within(t).getByText("Cannot be opened")).toBeTruthy();
    expect(within(t).getByText("Undecryptable")).toBeTruthy();
    expect(within(t).getByText("Not checked yet")).toBeTruthy();
  });

  it("never shows a value, even when an answer holds one", async () => {
    render(<SecretList />, { wrapper: withApi(credentialsApi({ secrets: async () => ({ items: [leakySecret("login")] }) })) });
    await within(await screen.findByRole("table")).findByText("login");
    expect(document.body.innerHTML).not.toContain(password);
    expect(document.body.innerHTML).not.toContain("EXAMPLE-ciphertext");
    expect(document.body.innerHTML).not.toContain("EXAMPLESEALED");
    expect(document.body.innerHTML).not.toContain("EXAMPLEHMAC");
  });

  it("hides New secret while the permission is unknown, when the answer failed, and when it is not held", async () => {
    let answer = () => never<string[]>();
    const a = credentialsApi({ perms: () => answer() });
    render(<SecretList />, { wrapper: withApi(a) });
    await screen.findByText("No secrets");
    expect(screen.queryByRole("button", { name: /New secret/ })).toBeNull();
    cleanup();

    answer = () => Promise.reject(new ConnectError("no", Code.Unavailable));
    render(<SecretList />, { wrapper: withApi(credentialsApi({ perms: () => answer() })) });
    await screen.findByText("No secrets");
    await waitFor(() => expect(screen.queryByRole("button", { name: /New secret/ })).toBeNull());
    cleanup();

    const b = credentialsApi({ perms: async () => ["secrets.secrets.delete"] });
    render(<SecretList />, { wrapper: withApi(b) });
    await screen.findByText("No secrets");
    await waitFor(() => expect(b.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("button", { name: /New secret/ })).toBeNull();
    expect(b.requests.find((r) => r.method === "TestPermissions")?.request).toMatchObject({ permissions: ["secrets.secrets.apply"] });
  });
});

describe("New secret", () => {
  async function open(f: Parameters<typeof credentialsApi>[0] = {}) {
    const a = credentialsApi({ secrets: async () => ({ items: [secret("taken")] }), ...f });
    render(<SecretList />, { wrapper: withApi(a) });
    await screen.findByRole("table");
    fireEvent.click(await screen.findByRole("button", { name: /New secret/ }));
    return a;
  }

  it("creates a registry login with one create-only ApplySecret, then drops the values", async () => {
    let sent: ApplySecretRequest | undefined;
    const a = await open({ applySecret: (r) => (sent = r) });
    type("Name", "app-registry");
    type("Registry", "registry.example.com");
    type("Username", "robot");
    type("Password or access token", password);
    // The command beside it names a file, never a value, and is create-only as the form is.
    expect(cli()).toBe("$ hangar create secret registry app-registry --create-only --from-docker-config FILE -p demo");
    fireEvent.click(within(dialog()).getByRole("button", { name: /Create secret/ }));
    // The mutation as the query client holds it, to replay once the form is gone.
    const [kept] = a.client.getMutationCache().getAll();
    expect(kept).toBeTruthy();
    // The mutation's variables are the name, a plain string, and nothing hangs on them.
    expect(kept.state.variables).toBe("app-registry");
    // The cache is read in full: it holds the mutation's name while it runs, never its values.
    expect(cacheText(a.client)).toContain("app-registry");
    expect(cacheText(a.client)).not.toContain(password);
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/secrets/$name", params: { project: "demo", name: "app-registry" } }));

    expect(a.calls.filter((c) => c === "ApplySecret")).toHaveLength(1);
    expect(sent?.createOnly).toBe(true);
    expect(sent?.secret?.metadata).toMatchObject({ name: "app-registry", project: "demo" });
    expect(sent?.secret?.spec?.type).toBe(SecretType.REGISTRY_AUTH);
    expect(sent?.secret?.spec?.sealed).toBeUndefined();
    const data = new TextDecoder().decode(sent?.secret?.spec?.data[".dockerconfigjson"]);
    expect(JSON.parse(data)).toEqual({ auths: { "registry.example.com": { username: "robot", password } } });

    // Closed: the form is gone, and nothing the query client keeps holds the value.
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.body.innerHTML).not.toContain(password);
    expect(cacheText(a.client)).not.toContain(password);
    // The mutation leaves the cache with its form, and a replay of it sends nothing.
    await waitFor(() => expect(a.client.getMutationCache().getAll()).toHaveLength(0));
    await expect(kept.execute("app-registry")).rejects.toThrow(errNoValues);
    expect(a.calls.filter((c) => c === "ApplySecret")).toHaveLength(1);
  });

  it("creates from a pasted Docker config, sent as given", async () => {
    let sent: ApplySecretRequest | undefined;
    const a = await open({ applySecret: (r) => (sent = r) });
    type("Name", "mirror");
    fireEvent.click(within(dialog()).getByRole("radio", { name: "Docker config" }));
    const config = `{"auths":{"mirror.example.com":{"auth":"${configValue}"}}}`;
    type("Docker config", config);
    fireEvent.click(within(dialog()).getByRole("button", { name: /Create secret/ }));
    await waitFor(() => expect(navigate).toHaveBeenCalled());
    expect(new TextDecoder().decode(sent?.secret?.spec?.data[".dockerconfigjson"])).toBe(config);
    expect(document.body.innerHTML).not.toContain(configValue);
    expect(cacheText(a.client)).not.toContain(configValue);
  });

  it("drops what was typed when cancelled", async () => {
    const a = await open();
    type("Name", "app-registry");
    type("Registry", "registry.example.com");
    type("Username", "robot");
    type("Password or access token", password);
    fireEvent.click(within(dialog()).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: /New secret/ }));
    expect((within(dialog()).getByLabelText("Password or access token") as HTMLInputElement).value).toBe("");
    expect(document.body.innerHTML).not.toContain(password);
    expect(cacheText(a.client)).not.toContain(password);
    expect(a.calls).not.toContain("ApplySecret");
  });

  it("sends nothing for a name the list holds, or before every value is given", async () => {
    const a = await open();
    type("Name", "taken");
    expect(within(dialog()).getByText("A secret named taken exists in this project.")).toBeTruthy();
    type("Registry", "registry.example.com");
    type("Username", "robot");
    type("Password or access token", password);
    expect((within(dialog()).getByRole("button", { name: /Create secret/ }) as HTMLButtonElement).disabled).toBe(true);
    // Submitting the form some other way, such as Enter in a field, sends nothing either.
    type("Name", "taken");
    fireEvent.submit(form());
    type("Name", "fresh");
    type("Password or access token", "");
    expect((within(dialog()).getByRole("button", { name: /Create secret/ }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.submit(form());
    // A submit the form takes sends at once; the first ApplySecret is that one, so neither before it sent anything.
    type("Password or access token", password);
    fireEvent.submit(form());
    await waitFor(() => expect(a.calls).toContain("ApplySecret"));
    expect(a.requests.filter((r) => r.method === "ApplySecret").map((r) => (r.request as ApplySecretRequest).secret?.metadata?.name)).toEqual(["fresh"]);
  });

  it("says a name taken since the list loaded exists, in the form's words, without the value", async () => {
    let items = [secret("taken")];
    const a = await open({ secrets: async () => ({ items }) });
    items = [secret("taken"), secret("late")];
    type("Name", "late");
    type("Registry", "registry.example.com");
    type("Username", "robot");
    type("Password or access token", password);
    fireEvent.click(within(dialog()).getByRole("button", { name: /Create secret/ }));
    const [kept] = a.client.getMutationCache().getAll();
    // The refusal itself, in the form's words: not the API's, and not the name check's after the list reloads.
    const refusal = await within(dialog()).findByRole("alert");
    expect(within(refusal).getByText("Creating the secret failed")).toBeTruthy();
    expect(within(refusal).getByText("A secret named late exists in this project.")).toBeTruthy();
    expect(refusal.textContent).not.toContain("already exists");
    expect(a.requests.find((r) => r.method === "ApplySecret")?.request).toMatchObject({ createOnly: true });
    expect(navigate).not.toHaveBeenCalled();
    // The form is still open with the values typed, yet the call took them: a replay of it sends nothing.
    await expect(kept.execute("late")).rejects.toThrow(errNoValues);
    expect(a.calls.filter((c) => c === "ApplySecret")).toHaveLength(1);
    fireEvent.click(within(dialog()).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(cacheText(a.client)).not.toContain(password);
  });
});

describe("New secret while the permission answer changes", () => {
  it("stays open with what was typed until it is closed", async () => {
    let held = ["secrets.secrets.apply"];
    const a = credentialsApi({ secrets: async () => ({ items: [secret("taken")] }), perms: async () => held });
    render(<SecretList />, { wrapper: withApi(a) });
    await screen.findByRole("table");
    fireEvent.click(await screen.findByRole("button", { name: /New secret/ }));
    type("Name", "app-registry");
    type("Registry", "registry.example.com");
    held = [];
    await a.client.refetchQueries();
    // The page's own New secret goes; the form open on it does not.
    await waitFor(() => expect(screen.queryByRole("button", { name: /New secret/ })).toBeNull());
    expect((within(dialog()).getByLabelText("Name") as HTMLInputElement).value).toBe("app-registry");
    expect((within(dialog()).getByLabelText("Registry") as HTMLInputElement).value).toBe("registry.example.com");
    fireEvent.click(within(dialog()).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });
});

describe("New secret closed before its call starts", () => {
  it("sends nothing: closing the form took the values", async () => {
    const a = credentialsApi({ secrets: async () => ({ items: [secret("taken")] }) });
    render(<SecretList />, { wrapper: withApi(a) });
    await screen.findByRole("table");
    fireEvent.click(await screen.findByRole("button", { name: /New secret/ }));
    type("Name", "app-registry");
    type("Registry", "registry.example.com");
    type("Username", "robot");
    type("Password or access token", password);
    fireEvent.submit(form());
    const [kept] = a.client.getMutationCache().getAll();
    // Cancel in the same moment, before the call has read the values.
    fireEvent.click(within(dialog()).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(kept.state.status).toBe("error"));
    expect(kept.state.error?.message).toBe(errNoValues);
    expect(a.calls).not.toContain("ApplySecret");
    expect(cacheText(a.client)).not.toContain(password);
  });
});

describe("New secret's fields and command line", () => {
  async function open() {
    const a = credentialsApi({ secrets: async () => ({ items: [secret("taken")] }) });
    render(<SecretList />, { wrapper: withApi(a) });
    await screen.findByRole("table");
    fireEvent.click(await screen.findByRole("button", { name: /New secret/ }));
    return a;
  }

  it("masks the password, and asks the browser neither to generate nor to save it", async () => {
    await open();
    const field = within(dialog()).getByLabelText("Password or access token") as HTMLInputElement;
    expect(field.type).toBe("password");
    expect(field.getAttribute("autocomplete")).toBe("off");
    expect(form().getAttribute("autocomplete")).toBe("off");
  });

  it("offers NAME, never a name the form would not send", async () => {
    await open();
    expect(cli()).toBe("$ hangar create secret registry NAME --create-only --from-docker-config FILE -p demo");
    for (const bad of ["$(whoami)", "Bad_Name", "a;rm -rf x", "taken"]) {
      type("Name", bad);
      expect(cli()).toBe("$ hangar create secret registry NAME --create-only --from-docker-config FILE -p demo");
    }
    type("Name", "good-name");
    expect(cli()).toBe("$ hangar create secret registry good-name --create-only --from-docker-config FILE -p demo");
  });
});

describe("a list that fails to load again", () => {
  it("keeps showing the last answer, and says it did not load again", async () => {
    let fail = false;
    const a = credentialsApi({
      secrets: async () => {
        if (fail) throw new ConnectError("the apiserver is unavailable", Code.Unavailable);
        return { items: [secret("login")] };
      },
    });
    render(<SecretList />, { wrapper: withApi(a) });
    await within(await screen.findByRole("table")).findByText("login");
    fail = true;
    await a.client.refetchQueries();
    await screen.findByText("The secrets did not load again; this page shows the last answer.");
    expect(within(screen.getByRole("table")).getByText("login")).toBeTruthy();
    expect(screen.queryByText("The secrets did not load")).toBeNull();
  });
});

describe("useSecrets", () => {
  it("drops value fields as the answer arrives, so neither the pages nor the query cache hold them", async () => {
    const a = credentialsApi({ secrets: async () => ({ items: [leakySecret("login")] }) });
    const { result } = renderHook(() => useSecrets("demo"), { wrapper: withApi(a) });
    await waitFor(() => expect(result.current.items).toHaveLength(1));
    const s = result.current.items[0];
    expect(s.spec?.type).toBe(SecretType.REGISTRY_AUTH);
    expect(s.spec?.data).toEqual({});
    expect(s.spec?.sealed).toBeUndefined();
    expect(s.status?.registries).toEqual(["registry.example.com"]);
    expect(cacheText(a.client)).toContain("registry.example.com");
    expect(cacheText(a.client)).not.toContain(password);
    expect(cacheText(a.client)).not.toContain("EXAMPLE-ciphertext");
    expect(cacheText(a.client)).not.toContain("EXAMPLESEALED");
  });
});

describe("SecretDetail", () => {
  it("shows what a secret holds a login for, and no value even when an answer holds one", async () => {
    const a = credentialsApi({
      secrets: async () => ({ items: [leakySecret("login")] }),
      containers: async () => ({ items: [container("api", ["login"])] }),
    });
    render(<SecretDetail name="login" />, { wrapper: withApi(a) });
    await screen.findByRole("heading", { name: "login" });
    expect(screen.getByText("Write-only; no answer of the API holds them.")).toBeTruthy();
    await screen.findByText("Named by 1 container");
    expect(document.body.innerHTML).not.toContain(password);
    expect(document.body.innerHTML).not.toContain("EXAMPLE-ciphertext");
    expect(document.body.innerHTML).not.toContain("EXAMPLEHMAC");
    expect(document.body.innerHTML).not.toContain("EXAMPLESEALED");
  });

  it("says a secret the project lacks does not exist", async () => {
    render(<SecretDetail name="gone" />, { wrapper: withApi(credentialsApi({})) });
    await screen.findByText("No such secret");
  });

  it("puts a secret the apiserver cannot open in a banner", async () => {
    const a = credentialsApi({ secrets: async () => ({ items: [secret("broken", { ready: [ConditionStatus.FALSE, "Undecryptable", "the key is wrong"] })] }) });
    render(<SecretDetail name="broken" />, { wrapper: withApi(a) });
    await screen.findByText("broken cannot be opened: Undecryptable");
    expect(screen.getAllByText("the key is wrong").length).toBeGreaterThan(0);
  });

  it("deletes after the name is typed, saying which containers name it", async () => {
    let sent: DeleteSecretRequest | undefined;
    const a = credentialsApi({
      secrets: async () => ({ items: [secret("login")] }),
      containers: async () => ({ items: [container("api", ["login"]), container("worker", ["login"])] }),
      deleteSecret: (r) => (sent = r),
    });
    render(<SecretDetail name="login" />, { wrapper: withApi(a) });
    await screen.findByText("Named by 2 containers");
    fireEvent.click(await screen.findByRole("button", { name: /Delete/ }));
    expect(within(dialog()).getByText(/2 containers name it: api, worker/)).toBeTruthy();
    expect(within(dialog()).getByText("hangar delete secret login -p demo", { exact: false })).toBeTruthy();
    const del = within(dialog()).getByRole("button", { name: /^Delete$/ }) as HTMLButtonElement;
    expect(del.disabled).toBe(true);
    fireEvent.change(within(dialog()).getByLabelText(/Name, to confirm/), { target: { value: "login" } });
    fireEvent.click(del);
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/secrets", params: { project: "demo" } }));
    expect(sent).toMatchObject({ project: "demo", name: "login" });
  });

  it("shows the API's refusal to delete in the dialog", async () => {
    const a = credentialsApi({
      secrets: async () => ({ items: [secret("login")] }),
      deleteSecret: () => {
        throw new ConnectError("permission denied", Code.PermissionDenied);
      },
    });
    render(<SecretDetail name="login" />, { wrapper: withApi(a) });
    fireEvent.click(await screen.findByRole("button", { name: /Delete/ }));
    fireEvent.change(within(dialog()).getByLabelText(/Name, to confirm/), { target: { value: "login" } });
    fireEvent.click(within(dialog()).getByRole("button", { name: /^Delete$/ }));
    await within(dialog()).findByText("Deleting failed");
    expect(within(dialog()).getByText("permission denied")).toBeTruthy();
    expect(navigate).not.toHaveBeenCalled();
  });

  it("offers no Delete, and no delete command, without secrets.secrets.delete", async () => {
    const a = credentialsApi({ secrets: async () => ({ items: [secret("login")] }), perms: async () => ["secrets.secrets.apply"] });
    render(<SecretDetail name="login" />, { wrapper: withApi(a) });
    await screen.findByRole("heading", { name: "login" });
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("button", { name: /Delete/ })).toBeNull();
    expect(screen.queryByText(/hangar delete secret/)).toBeNull();
    expect(screen.getByText(/hangar get secret login -p demo/)).toBeTruthy();
  });
});
