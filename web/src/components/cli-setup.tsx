// The overview's card with the lines that install the CLI and sign in with
// it, for macOS and Linux or for Windows.

import { useState } from "react";
import { Segmented } from "@/components/monitoring";
import { Card, CardHeader, CopyButton } from "@/components/ui";
import { cliSetup, defaultCliPlatform, type CliPlatform } from "@/lib/cli-install";

const platformLabels: Record<CliPlatform, string> = { unix: "macOS/Linux", windows: "Windows" };

/**
 * The setup lines for a console of `release` served at `origin`, first for
 * the platform the browser runs on.
 */
export function CliSetupCard({ project, release, origin }: { project: string; release: string; origin: string }) {
  const [platform, setPlatform] = useState<CliPlatform>(() => defaultCliPlatform(navigator.platform));
  const { lines, releasesUrl } = cliSetup(release, origin, project, platform);
  const windows = platform === "windows";
  return (
    <Card className="overflow-hidden">
      <CardHeader
        title="From the CLI"
        action={
          <span className="flex items-center gap-2">
            <Segmented
              label="Platform"
              options={[platformLabels.unix, platformLabels.windows]}
              value={platformLabels[platform]}
              onChange={(l) => setPlatform(l === platformLabels.windows ? "windows" : "unix")}
            />
            <CopyButton text={lines.join("\n")} />
          </span>
        }
      />
      {releasesUrl && (
        <p className="px-4 pt-3 text-[12px] text-muted">
          Linux, macOS and Windows builds:{" "}
          <a className="text-accent underline" href={releasesUrl} target="_blank" rel="noreferrer">
            releases
          </a>
        </p>
      )}
      <pre className="overflow-x-auto px-4 pt-3 font-mono text-[12px] leading-6">
        {lines.map((l) => (
          <div key={l}>
            <span className="select-none text-accent">{windows ? "PS> " : "$ "}</span>
            {l}
          </div>
        ))}
      </pre>
      <p className="px-4 pb-3 text-[12px] text-muted">
        {windows ? (
          <>
            Installs <code className="font-mono">hangar.exe</code> in %LOCALAPPDATA%\Programs\hangar and adds it to
            your PATH.
          </>
        ) : (
          <>
            Installs <code className="font-mono">hangar</code> in ~/.local/bin.
          </>
        )}
      </p>
    </Card>
  );
}
