import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { WhoAmIResponseSchema } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { UserSchema } from "@hangar/api/hangar/iam/v1alpha1/user_pb";
import { describe, expect, it } from "vitest";
import { sessionFrom } from "@/lib/session";

const who = (w: MessageInitShape<typeof WhoAmIResponseSchema>) => create(WhoAmIResponseSchema, w);

describe("sessionFrom", () => {
  it("waits while WhoAmI is asked", () => {
    expect(sessionFrom({ isPending: true, error: null }).state).toBe("loading");
  });

  it("is signed out on Unauthenticated", () => {
    expect(sessionFrom({ isPending: false, error: new ConnectError("no credentials", Code.Unauthenticated) }).state).toBe("signedOut");
  });

  it("shows the error on any other failure", () => {
    const err = new ConnectError("down", Code.Unavailable);
    expect(sessionFrom({ isPending: false, error: err })).toEqual({ state: "error", error: err });
  });

  it("lets a member in", () => {
    const w = who({ type: "user", id: "a@example.com", invited: true, member: true, user: create(UserSchema, {}) });
    expect(sessionFrom({ isPending: false, error: null, data: w }).state).toBe("member");
  });

  it("asks an invited person without a User to sign up", () => {
    const w = who({ type: "user", id: "a@example.com", invited: true, member: false });
    expect(sessionFrom({ isPending: false, error: null, data: w }).state).toBe("noUser");
  });

  it("keeps a person without an invitation waiting", () => {
    const w = who({ type: "user", id: "a@example.com", invited: false, member: false });
    expect(sessionFrom({ isPending: false, error: null, data: w }).state).toBe("notInvited");
  });

  it("keeps out any other caller that is not a member", () => {
    const w = who({ type: "serviceToken", id: "p/t", invited: false, member: false });
    expect(sessionFrom({ isPending: false, error: null, data: w }).state).toBe("notInvited");
  });
});
