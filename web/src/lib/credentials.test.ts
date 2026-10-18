import { create } from "@bufbuild/protobuf";
import { ListSecretsResponseSchema, SecretSchema, SecretType } from "@hangar/api/hangar/secrets/v1alpha1/secret_pb";
import { describe, expect, it } from "vitest";
import { commands, dockerConfig, secretUse, tokenRoles, tokenState, withoutValues, withoutValuesAll } from "@/lib/credentials";
import { binding, container, token } from "@/test/credentials";

describe("withoutValues", () => {
  it("keeps a secret's type and status and drops data and sealed, should an answer hold them", () => {
    const s = create(SecretSchema, {
      metadata: { name: "leaky", project: "demo" },
      spec: {
        type: SecretType.REGISTRY_AUTH,
        data: { ".dockerconfigjson": new TextEncoder().encode("EXAMPLE-VALUE") },
        sealed: { kekId: "k", ciphertext: new TextEncoder().encode("EXAMPLE-CIPHER"), contentHmac: "h" },
      },
      status: { keys: [".dockerconfigjson"], registries: ["registry.example.com"] },
    });
    const out = withoutValues(s);
    expect(out.spec?.type).toBe(SecretType.REGISTRY_AUTH);
    expect(out.spec?.data).toEqual({});
    expect(out.spec?.sealed).toBeUndefined();
    expect(out.status?.registries).toEqual(["registry.example.com"]);
    expect(out.metadata?.name).toBe("leaky");
    // The original is left as it was.
    expect(Object.keys(s.spec?.data ?? {})).toEqual([".dockerconfigjson"]);
  });
});

describe("withoutValuesAll", () => {
  it("drops every secret's value fields from a ListSecrets answer", () => {
    const leaky = create(SecretSchema, {
      metadata: { name: "a" },
      spec: { type: SecretType.REGISTRY_AUTH, data: { k: new TextEncoder().encode("EXAMPLE-VALUE") }, sealed: { kekId: "k" } },
    });
    const out = withoutValuesAll(create(ListSecretsResponseSchema, { items: [leaky, leaky], revision: 7n }));
    expect(out.revision).toBe(7n);
    expect(out.items.map((s) => [s.metadata?.name, s.spec?.data, s.spec?.sealed])).toEqual([
      ["a", {}, undefined],
      ["a", {}, undefined],
    ]);
  });
});

describe("dockerConfig", () => {
  it("writes a registry login as a Docker config, as docker login does", () => {
    const text = new TextDecoder().decode(dockerConfig({ mode: "login", registry: " registry.example.com ", username: "robot", password: "example-pass" }));
    expect(JSON.parse(text)).toEqual({ auths: { "registry.example.com": { username: "robot", password: "example-pass" } } });
  });

  it("sends a pasted config as given", () => {
    const config = '{"auths":{"mirror.example.com":{"auth":"ZXhhbXBsZQ=="}}}';
    expect(new TextDecoder().decode(dockerConfig({ mode: "config", config }))).toBe(config);
  });
});

describe("commands", () => {
  it("holds names and options alone, whatever the description", () => {
    expect(commands.createToken("demo", "ci", { role: "project.editor", duration: "720h", description: "Deploys it's builds" })).toBe(
      "hangar token create ci --role project.editor --duration 720h --description 'Deploys it'\\''s builds' -p demo",
    );
    expect(commands.createToken("demo", "", {})).toBe("hangar token create NAME -p demo");
    expect(commands.rotateToken("demo", "ci")).toBe("hangar token rotate ci -p demo");
    expect(commands.rotateToken("demo", "ci", "2160h")).toBe("hangar token rotate ci --duration 2160h -p demo");
    expect(commands.createSecret("demo", "login")).toBe("hangar create secret registry login --create-only --from-docker-config FILE -p demo");
    expect(commands.createSecret("demo", "")).toBe("hangar create secret registry NAME --create-only --from-docker-config FILE -p demo");
    expect(commands.deleteSecret("demo", "login")).toBe("hangar delete secret login -p demo");
    expect(commands.deleteToken("demo", "ci")).toBe("hangar token delete ci -p demo");
  });
});

describe("tokenState", () => {
  const now = Date.now();
  it("says whether a token's secret works", () => {
    expect(tokenState(token("a"), now)).toEqual({ label: "Active", tone: "ok" });
    expect(tokenState(token("b", { expires: 9 }), now)).toEqual({ label: "Expiring", tone: "pending" });
    expect(tokenState(token("c", { expires: -1 }), now)).toEqual({ label: "Expired", tone: "fail" });
    expect(tokenState(token("d", { prefix: "" }), now)).toEqual({ label: "No secret", tone: "off" });
  });
});

describe("tokenRoles", () => {
  it("takes the project's bindings of the project's tokens alone", () => {
    const roles = tokenRoles(
      [
        binding({ serviceToken: "demo/ci" }, "project.editor"),
        binding({ serviceToken: "demo/ci" }, "project.viewer"),
        binding({ serviceToken: "demo/bot" }, "project.owner"),
        binding({ user: "someone@example.com" }, "project.owner"),
        binding({ serviceToken: "other/ci" }, "project.owner", "other"),
        binding({ serviceToken: "demo/ci" }, "project.owner", "other"),
        binding({ serviceToken: "demo-two/ci" }, "project.owner", "demo-two"),
      ],
      "demo",
    );
    expect(roles).toEqual({ ci: ["project.editor", "project.viewer"], bot: ["project.owner"] });
  });
});

describe("secretUse", () => {
  it("lists, by secret, the containers naming it", () => {
    expect(secretUse([container("worker", ["login", "mirror"]), container("api", ["login"]), container("cron", [])])).toEqual({
      login: ["api", "worker"],
      mirror: ["worker"],
    });
  });
});
