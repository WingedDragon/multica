"use client";

import { useEffect, useState } from "react";
import { ChevronRight, MessagesSquare } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { agentListOptions } from "@multica/core/workspace/queries";
import { useWorkspacePaths } from "@multica/core/paths";
import { octoInstallationsOptions } from "@multica/core/octo";
import { Button } from "@multica/ui/components/ui/button";
import { ActorAvatar } from "../../common/actor-avatar";
import { AppLink, useNavigation } from "../../navigation";
import { useT } from "../../i18n";
import { ChannelConnectDialog } from "./channel-connect-dialog";
import { ChannelDisconnectDialog } from "./channel-disconnect-dialog";
import { IntegrationChannelIcon } from "./integration-channel-icon";
import {
  OctoTab,
  OCTO_CHANNEL_LABEL,
} from "./octo-tab";
import { SettingsCard, SettingsSection, SettingsTab } from "./settings-layout";
import {
  CHANNEL_INTEGRATIONS,
  type ChannelIntegration,
  resolveSettingsLocation,
  settingsHref,
} from "./settings-navigation";
import {
  useMessagingChannels,
  type ChannelConnection,
} from "./use-messaging-channels";

export function ChannelsTab() {
  const { t } = useT("settings");
  const navigation = useNavigation();
  const paths = useWorkspacePaths();
  const { wsId, member, queries, connections, isAdmin, available } =
    useMessagingChannels();
  const octo = useQuery({
    ...octoInstallationsOptions(wsId),
    enabled: !!wsId && !!member,
  });
  const octoConfigured = octo.data?.configured === true;
  const octoInstallations = (octo.data?.installations ?? []).filter(
    (installation) => installation.status === "active",
  );
  const agentsQuery = useQuery(agentListOptions(wsId));
  const agents = agentsQuery.data ?? [];
  const octoAgents = agents.filter((agent) =>
    octoInstallations.some(
      (installation) => installation.agent_id === agent.id,
    ),
  );
  const orphanedOcto =
    isAdmin && agentsQuery.isSuccess
      ? octoInstallations.filter(
          (installation) =>
            !agents.some((agent) => agent.id === installation.agent_id),
        )
      : [];
  const [connecting, setConnecting] = useState<ChannelIntegration | null>(null);
  const [disconnecting, setDisconnecting] =
    useState<ChannelConnection | null>(null);
  const legacyDetail =
    resolveSettingsLocation(navigation.searchParams).integration ??
    navigation.searchParams.get("integration");
  useEffect(() => {
    if (legacyDetail && legacyDetail !== "octo")
      navigation.replace(
        settingsHref(navigation.pathname, navigation.searchParams, "channels"),
      );
  }, [legacyDetail, navigation]);
  const channelsHref = settingsHref(
    navigation.pathname,
    navigation.searchParams,
    "channels",
  );
  const octoHref = settingsHref(
    navigation.pathname,
    navigation.searchParams,
    "channels",
    { integration: "octo" },
  );

  if (legacyDetail === "octo") {
    return (
      <SettingsTab
        title={OCTO_CHANNEL_LABEL}
        scope="workspace"
        breadcrumb={
          <nav
            aria-label={t(($) => $.channels.breadcrumb)}
            className="flex items-center gap-1.5 text-caption text-muted-foreground"
          >
            <AppLink
              href={channelsHref}
              className="rounded-sm hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring"
            >
              {t(($) => $.page.tabs.channels)}
            </AppLink>
            <ChevronRight aria-hidden="true" className="size-3" />
            <span aria-current="page" className="text-foreground">
              {OCTO_CHANNEL_LABEL}
            </span>
          </nav>
        }
      >
        <OctoTab />
      </SettingsTab>
    );
  }

  return (
    <SettingsTab title={t(($) => $.page.tabs.channels)} scope="workspace">
      <SettingsCard>
        {CHANNEL_INTEGRATIONS.filter(
          (id) =>
            isAdmin ||
            queries[id].data?.configured ||
            queries[id].isPending ||
            queries[id].isError,
        ).map((id) => {
          const query = queries[id];
          const orphaned =
            isAdmin && agentsQuery.isSuccess
              ? connections.filter(
                  (entry) =>
                    entry.channel === id &&
                    !agents.some(
                      (agent) => agent.id === entry.installation.agent_id,
                    ),
                )
              : [];
          const connectedAgents = agents.filter((agent) =>
            connections.some(
              (entry) =>
                entry.channel === id &&
                entry.installation.agent_id === agent.id,
            ),
          );
          return (
            <div
              key={id}
              className="flex flex-wrap items-center gap-3 px-4 py-4"
            >
              <IntegrationChannelIcon channel={id} />
              <span className="w-24 shrink-0 text-body font-medium">
                {t(($) => $[id].section_title)}
              </span>
              <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">
                {query.isError || agentsQuery.isError ? (
                  <span
                    role="alert"
                    className="text-caption text-muted-foreground"
                  >
                    {t(($) => $.integrations.status_unknown)}
                  </span>
                ) : query.isPending || agentsQuery.isPending ? (
                  <span className="text-caption text-muted-foreground">
                    {t(($) => $.integrations.status_loading)}
                  </span>
                ) : connectedAgents.length || orphaned.length ? (
                  connectedAgents.map((agent) => (
                    <AppLink
                      key={agent.id}
                      href={`${paths.agentDetail(agent.id)}?view=channels`}
                      className="flex min-w-0 items-center gap-1.5 rounded-md px-1 py-0.5 text-caption hover:bg-surface-hover focus-visible:outline-2 focus-visible:outline-ring"
                    >
                      <ActorAvatar
                        actorType="agent"
                        actorId={agent.id}
                        size="xs"
                        profileLink={false}
                      />
                      <span className="max-w-32 truncate">{agent.name}</span>
                    </AppLink>
                  ))
                ) : (
                  <span className="text-caption text-muted-foreground">
                    {t(($) => $.channels.disconnected)}
                  </span>
                )}
                {orphaned.map((entry) => (
                  <span
                    key={entry.installation.id}
                    className="inline-flex items-center gap-2 text-caption text-muted-foreground"
                  >
                    {t(($) => $.channels.agent_unavailable)}
                    <Button
                      size="xs"
                      variant="ghost"
                      onClick={() => setDisconnecting(entry)}
                    >
                      {t(($) => $.channels.disconnect)}
                    </Button>
                  </span>
                ))}
                {!query.isPending &&
                !query.isError &&
                !query.data?.configured &&
                isAdmin ? (
                  <span className="text-caption text-muted-foreground">
                    {t(($) => $.channels.unavailable)}
                  </span>
                ) : null}
              </div>
              {query.isError ? (
                <Button
                  size="xs"
                  variant="outline"
                  onClick={() => void query.refetch()}
                >
                  {t(($) => $.lark.install_retry)}
                </Button>
              ) : isAdmin && available(id) ? (
                <Button
                  size="sm"
                  variant="outline"
                  disabled={
                    !agents.some(
                      (agent) =>
                        !agent.archived_at &&
                        !connectedAgents.some(
                          (connected) => connected.id === agent.id,
                        ),
                    )
                  }
                  onClick={() => setConnecting(id)}
                >
                  {t(($) => $.channels.connect)}
                </Button>
              ) : null}
            </div>
          );
        })}
        {isAdmin || octoConfigured || octo.isPending || octo.isError ? (
          <div className="flex flex-wrap items-center gap-3 px-4 py-4">
            <MessagesSquare className="size-5" />
            <span className="w-24 shrink-0 text-body font-medium">
              <AppLink
                href={octoHref}
                className="rounded-sm hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring"
              >
                {OCTO_CHANNEL_LABEL}
              </AppLink>
            </span>
            <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">
              {octo.isError || agentsQuery.isError ? (
                <span
                  role="alert"
                  className="text-caption text-muted-foreground"
                >
                  {t(($) => $.integrations.status_unknown)}
                </span>
              ) : octo.isPending || agentsQuery.isPending ? (
                <span className="text-caption text-muted-foreground">
                  {t(($) => $.integrations.status_loading)}
                </span>
              ) : octoAgents.length || orphanedOcto.length ? (
                <>
                  {octoAgents.map((agent) => (
                    <AppLink
                      key={agent.id}
                      href={`${paths.agentDetail(agent.id)}?view=channels`}
                      className="flex min-w-0 items-center gap-1.5 rounded-md px-1 py-0.5 text-caption hover:bg-surface-hover focus-visible:outline-2 focus-visible:outline-ring"
                    >
                      <ActorAvatar
                        actorType="agent"
                        actorId={agent.id}
                        size="xs"
                        profileLink={false}
                      />
                      <span className="max-w-32 truncate">{agent.name}</span>
                    </AppLink>
                  ))}
                  {orphanedOcto.map((installation) => (
                    <span
                      key={installation.id}
                      className="text-caption text-muted-foreground"
                    >
                      {t(($) => $.channels.agent_unavailable)}
                    </span>
                  ))}
                </>
              ) : (
                <span className="text-caption text-muted-foreground">
                  {t(($) => $.channels.disconnected)}
                </span>
              )}
              {!octo.isPending &&
              !octo.isError &&
              !octoConfigured &&
              isAdmin ? (
                <span className="text-caption text-muted-foreground">
                  {t(($) => $.channels.unavailable)}
                </span>
              ) : null}
            </div>
            {octo.isError ? (
              <Button
                size="xs"
                variant="outline"
                onClick={() => void octo.refetch()}
              >
                {t(($) => $.lark.install_retry)}
              </Button>
            ) : null}
          </div>
        ) : null}
      </SettingsCard>
      <SettingsSection title={t(($) => $.channels.permissions_title)}>
        <SettingsCard>
          <div className="flex flex-wrap justify-between gap-2 px-4 py-3 text-caption">
            <span>
              {t(($) => $.lark.section_title)},{" "}
              {t(($) => $.dingtalk.section_title)}
            </span>
            <span className="text-muted-foreground">
              {t(($) => $.channels.owner_or_admin)}
            </span>
          </div>
          <div className="flex flex-wrap justify-between gap-2 px-4 py-3 text-caption">
            <span>
              {t(($) => $.slack.section_title)},{" "}
              {t(($) => $.wecom.section_title)},{" "}
              {t(($) => $.weixin.section_title)},{" "}
              {OCTO_CHANNEL_LABEL}
            </span>
            <span className="text-muted-foreground">
              {t(($) => $.channels.admin_only)}
            </span>
          </div>
        </SettingsCard>
      </SettingsSection>
      <ChannelConnectDialog
        key={connecting ?? "closed"}
        agents={agents}
        initialChannel={connecting ?? undefined}
        open={connecting !== null}
        onOpenChange={(open) => {
          if (!open) setConnecting(null);
        }}
      />
      {disconnecting ? (
        <ChannelDisconnectDialog
          connection={disconnecting}
          open
          onOpenChange={(open) => {
            if (!open) setDisconnecting(null);
          }}
        />
      ) : null}
    </SettingsTab>
  );
}
