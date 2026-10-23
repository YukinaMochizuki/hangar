/**
 * The lines the console shows for getting the CLI and signing in with it:
 * the install line of the release this console came with
 * (deploy/cli/install.sh.in, or deploy/cli/install.ps1.in on Windows), which
 * also makes a context for this deployment's API, then `hangar login`.
 */

export const releasesUrl = "https://github.com/YukinaMochizuki/hangar/releases";

/**
 * The release this console was built for, as deploy/release.sh hands it to
 * the build; "" for a build of no release (a test build, `vite`).
 */
export const consoleRelease: string = import.meta.env.VITE_HANGAR_RELEASE ?? "";

/**
 * The API's URL for a console served at `origin`: hangar-edge serves the
 * console at console.<domain> and the API at api.<domain>
 * (docs/decisions/front-door.md). undefined for any other host, such as the
 * dev server's.
 */
export function apiEndpoint(origin: string): string | undefined {
  let url: URL;
  try {
    url = new URL(origin);
  } catch {
    return undefined;
  }
  if (!url.hostname.startsWith("console.")) return undefined;
  url.hostname = "api." + url.hostname.slice("console.".length);
  return url.origin;
}

/** The systems the CLI is installed on in different ways. */
export type CliPlatform = "unix" | "windows";

/** The platform to show first for a browser on `platform` (navigator.platform). */
export function defaultCliPlatform(platform: string): CliPlatform {
  return /^win/i.test(platform) ? "windows" : "unix";
}

export interface CliSetup {
  /** The lines to run, in order. */
  lines: string[];
  /**
   * Set when there is no install line to give, so the card points at the
   * releases instead.
   */
  releasesUrl?: string;
}

/** The setup lines for a console of `release` served at `origin`, on `platform`. */
export function cliSetup(release: string, origin: string, project: string, platform: CliPlatform): CliSetup {
  const endpoint = apiEndpoint(origin);
  const after = ["hangar login", `hangar get containers -p ${project}`];
  if (release && endpoint) {
    const download = `${releasesUrl}/download/${release}`;
    return {
      lines: [
        platform === "windows"
          ? `& ([scriptblock]::Create((irm ${download}/install.ps1))) -Endpoint ${endpoint}`
          : `curl -fsSL ${download}/install.sh | sh -s -- --endpoint ${endpoint}`,
        ...after,
      ],
    };
  }
  const context = endpoint ? new URL(endpoint).hostname.replace(/^api\./, "") : "NAME";
  return {
    lines: [`hangar config set-context ${context} --endpoint ${endpoint ?? "URL"} --auth oidc --use`, ...after],
    releasesUrl,
  };
}
