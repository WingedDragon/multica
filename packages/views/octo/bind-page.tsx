"use client";

import { useEffect, useState } from "react";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Button } from "@multica/ui/components/ui/button";
import { api } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import { useNavigation } from "../navigation";

type RedeemState =
  | { kind: "idle" }
  | { kind: "redeeming" }
  | { kind: "done" }
  | { kind: "needs-auth" }
  | { kind: "error"; reason: string };

export function OctoBindPage({ token }: { token: string | null }) {
  const user = useAuthStore((s) => s.user);
  const isAuthLoading = useAuthStore((s) => s.isLoading);
  const navigation = useNavigation();
  const [state, setState] = useState<RedeemState>({ kind: "idle" });

  useEffect(() => {
    if (!token) {
      setState({ kind: "error", reason: "missing_token" });
      return;
    }
    if (isAuthLoading) return;
    if (!user) {
      setState({ kind: "needs-auth" });
      return;
    }
    if (state.kind !== "idle" && state.kind !== "needs-auth") return;
    setState({ kind: "redeeming" });
    (async () => {
      try {
        await api.redeemOctoBindingToken(token);
        setState({ kind: "done" });
      } catch (e) {
        setState({ kind: "error", reason: redemptionFailureReason(e) });
      }
    })();
  }, [token, user, isAuthLoading, state.kind]);

  return (
    <div className="mx-auto flex min-h-screen max-w-md flex-col items-center justify-center p-6">
      <Card className="w-full">
        <CardContent className="space-y-4">
          <h1 className="text-lg font-semibold">Link Octo account</h1>
          {state.kind === "idle" || state.kind === "redeeming" ? (
            <p className="text-sm text-muted-foreground">
              Linking your Octo account...
            </p>
          ) : state.kind === "needs-auth" ? (
            <>
              <p className="text-sm text-muted-foreground">
                Sign in to Multica before linking your Octo account.
              </p>
              <Button
                size="sm"
                onClick={() =>
                  navigation.push(
                    `/login?next=${encodeURIComponent(
                      `/octo/bind?token=${encodeURIComponent(token ?? "")}`,
                    )}`,
                  )
                }
              >
                Sign in
              </Button>
            </>
          ) : state.kind === "done" ? (
            <>
              <p className="text-sm font-medium">Octo account linked</p>
              <p className="text-xs text-muted-foreground">
                You can now chat with the connected Multica agent from Octo.
              </p>
            </>
          ) : (
            <>
              <p className="text-sm font-medium">Could not link Octo account</p>
              <p className="text-xs text-muted-foreground">
                {errorCopy(state.reason)}
              </p>
            </>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function redemptionFailureReason(err: unknown): string {
  const msg = err instanceof Error ? err.message : "";
  const lower = msg.toLowerCase();
  if (
    lower.includes("invalid") ||
    lower.includes("expired") ||
    lower.includes("410")
  ) {
    return "expired";
  }
  if (lower.includes("already bound") || lower.includes("409")) {
    return "already_bound";
  }
  if (lower.includes("workspace member") || lower.includes("403")) {
    return "not_member";
  }
  return "unknown";
}

function errorCopy(reason: string): string {
  switch (reason) {
    case "missing_token":
      return "The bind link is missing its token.";
    case "expired":
      return "This bind link is invalid or expired. Send another message to the bot to get a new link.";
    case "already_bound":
      return "This Octo account is already linked to a different Multica user.";
    case "not_member":
      return "Your Multica account is not a member of the workspace for this bot.";
    default:
      return "Try again, or ask a workspace admin for a new bind link.";
  }
}
