// Who is using the console, from AccountService.WhoAmI, and the gate every
// page sits behind: signed out, signed in without an invitation, invited
// without a User, or a member.

import { useQuery } from "@connectrpc/connect-query";
import { AccountService, type WhoAmIResponse } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { createContext, useContext } from "react";
import { isUnauthenticated } from "@/lib/api";

export type SessionState =
  | { state: "loading" }
  | { state: "signedOut" }
  | { state: "error"; error: unknown }
  | { state: "notInvited"; who: WhoAmIResponse }
  | { state: "noUser"; who: WhoAmIResponse }
  | { state: "member"; who: WhoAmIResponse };

/** The gate's page for what WhoAmI answered. */
export function sessionFrom(q: { isPending: boolean; error: unknown; data?: WhoAmIResponse }): SessionState {
  if (q.isPending) return { state: "loading" };
  if (q.error) return isUnauthenticated(q.error) ? { state: "signedOut" } : { state: "error", error: q.error };
  const who = q.data;
  if (!who) return { state: "loading" };
  if (who.member) return { state: "member", who };
  if (who.type === "user" && who.invited && !who.user) return { state: "noUser", who };
  return { state: "notInvited", who };
}

export function useSessionState(): { session: SessionState; refetch: () => void } {
  const q = useQuery(AccountService.method.whoAmI, {});
  return { session: sessionFrom(q), refetch: () => void q.refetch() };
}

const MemberContext = createContext<WhoAmIResponse | null>(null);
export const MemberProvider = MemberContext.Provider;

/** The member using the console; only inside the gate. */
export function useMember(): WhoAmIResponse {
  const who = useContext(MemberContext);
  if (!who) throw new Error("useMember outside the gate");
  return who;
}
