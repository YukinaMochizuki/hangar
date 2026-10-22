// @vitest-environment jsdom

import { create } from "@bufbuild/protobuf";
import { WhoAmIResponseSchema } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it } from "vitest";
import { MemberFooter } from "@/components/shell";
import { PrefsProvider } from "@/lib/prefs";
import { MemberProvider } from "@/lib/session";

beforeAll(() => {
  // The theme switch asks whether the system prefers dark.
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
});
afterEach(cleanup);

function footer(platformRoles: string[]) {
  const who = create(WhoAmIResponseSchema, { type: "user", id: "a@example.com", invited: true, member: true, user: { metadata: { name: "a" } }, platformRoles });
  render(
    <PrefsProvider>
      <MemberProvider value={who}>
        <MemberFooter />
      </MemberProvider>
    </PrefsProvider>,
  );
}

// The badge says who someone is, from WhoAmI's platform roles: whoever is
// bound platform.sponsor, root too, and not root alone, who holds what a
// sponsor does without funding anything.
describe("MemberFooter", () => {
  it("badges a sponsor", () => {
    footer(["platform.sponsor"]);
    expect(screen.getByText("Sponsor")).toBeTruthy();
  });

  it("badges a root who is bound sponsor too", () => {
    footer(["platform.root", "platform.sponsor"]);
    expect(screen.getByText("Sponsor")).toBeTruthy();
  });

  it("badges no member and no root", () => {
    footer(["platform.member"]);
    expect(screen.queryByText("Sponsor")).toBeNull();
    cleanup();
    footer(["platform.root"]);
    expect(screen.queryByText("Sponsor")).toBeNull();
  });
});
