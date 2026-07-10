"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ChevronRight, MessagesSquare, Trash2 } from "lucide-react";
import { cn } from "@multica/ui/lib/utils";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { api } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { octoInstallationsOptions, octoKeys } from "@multica/core/octo";
import { memberListOptions } from "@multica/core/workspace/queries";
import { useActorName } from "@multica/core/workspace/hooks";
import type { OctoInstallation } from "@multica/core/types";
import { ActorAvatar } from "../../common/actor-avatar";

export function OctoTab() {
  const wsId = useWorkspaceId();
  const user = useAuthStore((s) => s.user);
  const qc = useQueryClient();
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage =
    currentMember?.role === "owner" || currentMember?.role === "admin";
  const { data, isLoading } = useQuery({
    ...octoInstallationsOptions(wsId),
    enabled: !!wsId,
  });
  const installations = data?.installations ?? [];
  const configured = data?.configured === true;
  const [disconnectTarget, setDisconnectTarget] = useState<string | null>(null);
  const [disconnecting, setDisconnecting] = useState(false);

  async function handleDisconnect() {
    if (!disconnectTarget || disconnecting) return;
    setDisconnecting(true);
    try {
      await api.deleteOctoInstallation(wsId, disconnectTarget);
      await qc.invalidateQueries({ queryKey: octoKeys.installations(wsId) });
      toast.success("Octo bot disconnected");
      setDisconnectTarget(null);
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : "Failed to disconnect Octo bot",
      );
    } finally {
      setDisconnecting(false);
    }
  }

  return (
    <div className="space-y-6">
      <p className="text-sm text-muted-foreground">
        Connect Octo IM bots to agents so Octo messages can create issues and
        start agent runs.
      </p>

      {!configured ? (
        <Card>
          <CardContent className="space-y-2">
            <p className="text-sm font-medium">
              Octo integration is not enabled
            </p>
            <p className="text-xs text-muted-foreground">
              Set{" "}
              <code className="rounded bg-muted px-1 py-0.5 text-[10px]">
                MULTICA_OCTO_SECRET_KEY
              </code>{" "}
              on the server.
            </p>
          </CardContent>
        </Card>
      ) : isLoading ? (
        <Card>
          <CardContent>
            <p className="text-sm text-muted-foreground">
              Loading Octo bots...
            </p>
          </CardContent>
        </Card>
      ) : installations.length === 0 ? (
        <Card>
          <CardContent className="space-y-2">
            <p className="text-sm font-medium">No Octo bots connected</p>
            <p className="text-xs text-muted-foreground">
              Connect Octo from an agent's Integrations tab.
            </p>
          </CardContent>
        </Card>
      ) : (
        <Card>
          <CardContent className="divide-y">
            {installations.map((inst) => (
              <OctoInstallationRow
                key={inst.id}
                installation={inst}
                canManage={canManage}
                onDisconnect={() => setDisconnectTarget(inst.id)}
              />
            ))}
          </CardContent>
        </Card>
      )}

      <AlertDialog
        open={!!disconnectTarget}
        onOpenChange={(v) => {
          if (!v && !disconnecting) setDisconnectTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Disconnect Octo bot?</AlertDialogTitle>
            <AlertDialogDescription>
              The installation is revoked and the agent stops receiving Octo
              messages.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnecting}>
              Cancel
            </AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDisconnect}
              disabled={disconnecting}
            >
              {disconnecting ? "Disconnecting..." : "Disconnect"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function OctoInstallationRow({
  installation,
  canManage,
  onDisconnect,
}: {
  installation: OctoInstallation;
  canManage: boolean;
  onDisconnect: () => void;
}) {
  const { getAgentName } = useActorName();
  const isActive = installation.status === "active";
  return (
    <div className="flex items-start justify-between gap-4 py-3 first:pt-0 last:pb-0">
      <div className="flex min-w-0 items-start gap-3">
        <ActorAvatar
          actorType="agent"
          actorId={installation.agent_id}
          size="lg"
          enableHoverCard
          profileLink
        />
        <div className="min-w-0 space-y-1">
          <p className="truncate text-sm font-medium">
            {getAgentName(installation.agent_id)}
            {!isActive && (
              <span className="ml-2 rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
                revoked
              </span>
            )}
          </p>
          <p className="truncate text-xs text-muted-foreground">
            robot {installation.robot_id}
          </p>
          {installation.api_url && (
            <p className="truncate text-xs text-muted-foreground">
              {installation.api_url}
            </p>
          )}
        </div>
      </div>
      {canManage && isActive && (
        <Button variant="destructive" size="sm" onClick={onDisconnect}>
          <Trash2 className="h-3 w-3" />
          Disconnect
        </Button>
      )}
    </div>
  );
}

export function OctoAgentBindButton({
  agentId,
  agentName,
  className,
  onShowConnectedDetails,
}: {
  agentId: string;
  agentName?: string;
  className?: string;
  onShowConnectedDetails?: () => void;
}) {
  const wsId = useWorkspaceId();
  const user = useAuthStore((s) => s.user);
  const qc = useQueryClient();
  const [dialogOpen, setDialogOpen] = useState(false);
  const [apiURL, setAPIURL] = useState("");
  const [botToken, setBotToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const { data: listing } = useQuery({
    ...octoInstallationsOptions(wsId),
    enabled: !!wsId,
  });
  const { data: members = [] } = useQuery({
    ...memberListOptions(wsId),
    enabled: !!wsId,
  });
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage =
    currentMember?.role === "owner" || currentMember?.role === "admin";
  if (!canManage || listing?.install_supported !== true) return null;

  const existing = listing.installations.find(
    (inst) => inst.agent_id === agentId && inst.status === "active",
  );
  if (existing) {
    return onShowConnectedDetails ? (
      <button
        type="button"
        onClick={onShowConnectedDetails}
        className={cn(
          "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-xs text-muted-foreground transition-colors hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
          className,
        )}
        data-testid="octo-agent-bot-status"
      >
        <span className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-500" />
        <span className="truncate">Octo bot connected</span>
        <ChevronRight className="ml-auto h-3.5 w-3.5 shrink-0" />
      </button>
    ) : (
      <OctoAgentBotConnectedBadge
        installation={existing}
        className={className}
      />
    );
  }

  function closeDialog() {
    if (submitting) return;
    setDialogOpen(false);
    setAPIURL("");
    setBotToken("");
  }

  async function handleSubmit() {
    const api_url = apiURL.trim();
    const bot_token = botToken.trim();
    if (submitting || !api_url || !bot_token) return;
    setSubmitting(true);
    try {
      await api.registerOctoBYO(wsId, agentId, { api_url, bot_token });
      await qc.invalidateQueries({ queryKey: octoKeys.installations(wsId) });
      toast.success("Octo bot connected");
      setDialogOpen(false);
      setAPIURL("");
      setBotToken("");
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : "Failed to connect Octo bot",
      );
    } finally {
      setSubmitting(false);
    }
  }

  const canSubmit =
    apiURL.trim() !== "" && botToken.trim() !== "" && !submitting;
  return (
    <div
      className={cn("flex flex-wrap items-center gap-2", className)}
      data-testid="octo-agent-bind-buttons"
    >
      <Button
        variant="outline"
        size="sm"
        onClick={() => setDialogOpen(true)}
        disabled={!agentId}
        title={agentName ? `Connect Octo bot for ${agentName}` : undefined}
        data-testid="octo-agent-connect"
      >
        <MessagesSquare className="h-3 w-3" />
        Connect Octo
      </Button>
      <Dialog
        open={dialogOpen}
        onOpenChange={(v) => (v ? setDialogOpen(true) : closeDialog())}
      >
        <DialogContent className="sm:max-w-lg" data-testid="octo-byo-dialog">
          <DialogHeader>
            <DialogTitle>Connect Octo bot</DialogTitle>
          </DialogHeader>
          <div className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="octo-api-url">Octo API URL</Label>
              <Input
                id="octo-api-url"
                value={apiURL}
                onChange={(e) => setAPIURL(e.target.value)}
                placeholder="https://octo.example"
                autoComplete="off"
                spellCheck={false}
                disabled={submitting}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="octo-bot-token">Bot token</Label>
              <Input
                id="octo-bot-token"
                value={botToken}
                onChange={(e) => setBotToken(e.target.value)}
                placeholder="bf_... or app_..."
                autoComplete="off"
                spellCheck={false}
                disabled={submitting}
              />
            </div>
          </div>
          <DialogFooter>
            <Button
              variant="outline"
              size="sm"
              onClick={closeDialog}
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button
              size="sm"
              onClick={handleSubmit}
              disabled={!canSubmit}
              data-testid="octo-byo-submit"
            >
              {submitting ? "Connecting..." : "Connect"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function OctoAgentBotConnectedBadge({
  installation,
  className,
}: {
  installation: OctoInstallation;
  className?: string;
}) {
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [disconnecting, setDisconnecting] = useState(false);

  async function handleDisconnect() {
    if (disconnecting) return;
    setDisconnecting(true);
    try {
      await api.deleteOctoInstallation(wsId, installation.id);
      await qc.invalidateQueries({ queryKey: octoKeys.installations(wsId) });
      toast.success("Octo bot disconnected");
      setConfirmOpen(false);
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : "Failed to disconnect Octo bot",
      );
    } finally {
      setDisconnecting(false);
    }
  }

  return (
    <div
      className={cn("space-y-2", className)}
      data-testid="octo-agent-bot-connected"
    >
      <div className="flex items-center justify-between gap-3">
        <span className="inline-flex min-w-0 items-center gap-2 text-xs text-muted-foreground">
          <span className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-500" />
          <span className="truncate">Octo bot connected</span>
        </span>
        <Button
          variant="destructive"
          size="sm"
          onClick={() => setConfirmOpen(true)}
          disabled={disconnecting}
        >
          <Trash2 className="h-3 w-3" />
          {disconnecting ? "Disconnecting..." : "Disconnect"}
        </Button>
      </div>
      <AlertDialog
        open={confirmOpen}
        onOpenChange={(v) => {
          if (!v && !disconnecting) setConfirmOpen(false);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Disconnect Octo bot?</AlertDialogTitle>
            <AlertDialogDescription>
              The agent stops receiving messages from this Octo bot.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnecting}>
              Cancel
            </AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDisconnect}
              disabled={disconnecting}
            >
              {disconnecting ? "Disconnecting..." : "Disconnect"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
