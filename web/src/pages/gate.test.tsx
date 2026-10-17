// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import { AccountService, type WhoAmIResponse } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import type { MessageInitShape } from "@bufbuild/protobuf";
import type { WhoAmIResponseSchema } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { useMember } from "@/lib/session";
import { Gate } from "@/pages/gate";
import { fakeApi, never, withApi } from "@/test/harness";

afterEach(cleanup);

/** Renders the gate over a WhoAmI that answers whoAmI(). */
function gate(whoAmI: () => Promise<MessageInitShape<typeof WhoAmIResponseSchema>>) {
  const a = fakeApi((r) => r.service(AccountService, { whoAmI }));
  render(
    <Gate>
      <Inside />
    </Gate>,
    { wrapper: withApi(a) },
  );
  return a;
}

function Inside() {
  const who: WhoAmIResponse = useMember();
  return <p>console for {who.id}</p>;
}

describe("Gate", () => {
  it("shows nothing of the console while WhoAmI is asked", async () => {
    const a = gate(never);
    // Asked, and never answered: what shows now is what shows while asking.
    await waitFor(() => expect(a.calls).toContain("WhoAmI"));
    expect(document.querySelector('[aria-busy="true"]')).not.toBeNull();
    expect(screen.queryByText(/console for/)).toBeNull();
    expect(screen.queryByText("Not signed in")).toBeNull();
  });

  it("shows the sign-in page when signed out", async () => {
    gate(() => Promise.reject(new ConnectError("no credentials", Code.Unauthenticated)));
    await screen.findByText("Not signed in");
    expect(screen.getByRole("link", { name: "Sign in with Google" }).getAttribute("href")).toMatch(/^\/auth\/login\?return=/);
  });

  it("shows the API's error on any other failure", async () => {
    gate(() => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)));
    await screen.findByText("The API did not answer");
    expect(screen.getByText("the apiserver is unavailable")).toBeTruthy();
  });

  it("keeps a person without an invitation waiting", async () => {
    gate(async () => ({ type: "user", id: "a@example.com", invited: false, member: false }));
    await screen.findByText("No invitation yet");
    expect(screen.queryByText(/console for/)).toBeNull();
  });

  it("asks an invited person without a User to sign up", async () => {
    gate(async () => ({ type: "user", id: "a@example.com", invited: true, member: false }));
    await screen.findByText("Hangar account");
    expect(screen.queryByText(/console for/)).toBeNull();
  });

  it("lets a member into the console", async () => {
    gate(async () => ({ type: "user", id: "a@example.com", invited: true, member: true, user: { metadata: { name: "a" } } }));
    await screen.findByText("console for a@example.com");
  });
});
