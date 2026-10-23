import { describe, expect, it } from "vitest";
import { apiEndpoint, cliSetup, defaultCliPlatform, releasesUrl } from "@/lib/cli-install";

describe("apiEndpoint", () => {
  it("is api.<domain> for a console at console.<domain>", () => {
    expect(apiEndpoint("https://console.hangarlab.cloud")).toBe("https://api.hangarlab.cloud");
    expect(apiEndpoint("https://console.example.test:8443")).toBe("https://api.example.test:8443");
  });

  it("is undefined for any other host", () => {
    expect(apiEndpoint("http://localhost:5173")).toBeUndefined();
    expect(apiEndpoint("https://hangar.example.test")).toBeUndefined();
    expect(apiEndpoint("not a url")).toBeUndefined();
  });
});

describe("defaultCliPlatform", () => {
  it("is windows for a browser on Windows", () => {
    expect(defaultCliPlatform("Win32")).toBe("windows");
    expect(defaultCliPlatform("Win64")).toBe("windows");
  });

  it("is unix for any other", () => {
    expect(defaultCliPlatform("MacIntel")).toBe("unix");
    expect(defaultCliPlatform("Linux x86_64")).toBe("unix");
    expect(defaultCliPlatform("")).toBe("unix");
  });
});

describe("cliSetup", () => {
  it("installs the console's own release and makes the context in one line", () => {
    expect(cliSetup("v0.2.0", "https://console.hangarlab.cloud", "web", "unix")).toEqual({
      lines: [
        "curl -fsSL https://github.com/YukinaMochizuki/hangar/releases/download/v0.2.0/install.sh | sh -s -- --endpoint https://api.hangarlab.cloud",
        "hangar login",
        "hangar get containers -p web",
      ],
    });
  });

  it("installs the console's own release with install.ps1 on Windows", () => {
    expect(cliSetup("v0.2.0", "https://console.hangarlab.cloud", "web", "windows")).toEqual({
      lines: [
        "& ([scriptblock]::Create((irm https://github.com/YukinaMochizuki/hangar/releases/download/v0.2.0/install.ps1))) -Endpoint https://api.hangarlab.cloud",
        "hangar login",
        "hangar get containers -p web",
      ],
    });
  });

  it.each(["unix", "windows"] as const)("points at the releases and spells out the context without a release (%s)", (platform) => {
    expect(cliSetup("", "https://console.hangarlab.cloud", "web", platform)).toEqual({
      lines: [
        "hangar config set-context hangarlab.cloud --endpoint https://api.hangarlab.cloud --auth oidc --use",
        "hangar login",
        "hangar get containers -p web",
      ],
      releasesUrl,
    });
  });

  it.each(["unix", "windows"] as const)("gives no install line where it cannot tell the API's URL (%s)", (platform) => {
    const s = cliSetup("v0.2.0", "http://localhost:5173", "web", platform);
    expect(s.releasesUrl).toBe(releasesUrl);
    expect(s.lines[0]).toBe("hangar config set-context NAME --endpoint URL --auth oidc --use");
  });
});
