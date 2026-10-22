// What the Secrets and Service tokens pages share: the banner over a list
// that did not load again.

import { PageBanner } from "@/components/notice";
import { Button } from "@/components/ui";
import { errorMessage } from "@/lib/api";

/**
 * StaleBanner says a list that loaded before did not load again, while the
 * page goes on showing its last answer, and whatever dialog is open on it.
 */
export function StaleBanner({ what, error, onRetry }: { what: string; error: unknown; onRetry: () => void }) {
  return (
    <PageBanner tone="warning" title={`The ${what} did not reload; showing the last answer.`}>
      <span className="flex flex-wrap items-center justify-between gap-2">
        <span className="font-mono text-[12px]">{errorMessage(error)}</span>
        <Button size="sm" onClick={onRetry}>
          Retry
        </Button>
      </span>
    </PageBanner>
  );
}
