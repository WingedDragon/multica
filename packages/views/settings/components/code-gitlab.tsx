"use client";

import { Fragment, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Copy, GitBranch } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import { IconButton } from "@multica/ui/components/common/icon-button";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
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
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { useWorkspaceId } from "@multica/core/hooks";
import { gitlabConfigOptions, gitlabKeys } from "@multica/core/gitlab/queries";
import { workspaceKeys } from "@multica/core/workspace/queries";
import { api } from "@multica/core/api";
import type {
  CreateGitLabConnectionResponse,
  CreateGitLabProjectRequest,
  GitLabConnection,
  GitLabProjectBinding,
} from "@multica/core/types";
import { useT } from "../../i18n";
import { SettingsRow } from "./settings-layout";
import { HostMark, HostStatus } from "./code-host";

export function GitLabProjectRows() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const { data } = useQuery(gitlabConfigOptions(wsId));
  const [projectInputs, setProjectInputs] = useState<Record<string, string>>({});
  const [instanceUrl, setInstanceUrl] = useState("");
  const [accessToken, setAccessToken] = useState("");
  const [connecting, setConnecting] = useState(false);
  const [createdConnection, setCreatedConnection] =
    useState<CreateGitLabConnectionResponse | null>(null);
  const [target, setTarget] = useState<GitLabProjectBinding | null>(null);

  const invalidate = () =>
    Promise.all([
      qc.invalidateQueries({ queryKey: gitlabKeys.config(wsId) }),
      qc.invalidateQueries({ queryKey: workspaceKeys.list() }),
    ]);

  const createProject = useMutation({
    mutationFn: (input: CreateGitLabProjectRequest) =>
      api.createGitLabProject(wsId, input),
    onSuccess: async (_response, input) => {
      const key = input.connection_id
        ? `connection:${input.connection_id}`
        : "legacy";
      setProjectInputs((current) => ({ ...current, [key]: "" }));
      await invalidate();
    },
    onError: (error) =>
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.gitlab.toast_project_add_failed),
      ),
  });
  const removeProject = useMutation({
    mutationFn: (id: string) => api.deleteGitLabProject(wsId, id),
    onSuccess: async () => {
      setTarget(null);
      await invalidate();
    },
    onError: (error) =>
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.gitlab.toast_project_delete_failed),
      ),
  });
  const refreshProject = useMutation({
    mutationFn: (id: string) => api.refreshGitLabProject(wsId, id),
    onSuccess: async () => {
      await invalidate();
      toast.success(t(($) => $.gitlab.toast_project_refreshed));
    },
    onError: (error) =>
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.gitlab.toast_project_refresh_failed),
      ),
  });

  if (!data) return null;
  const config = data;

  const connections = config.connections ?? [];
  const projects = config.projects ?? [];
  const connectionIds = new Set(connections.map((connection) => connection.id));
  const serverConfiguredConnection = connections.find(
    (connection) => connection.server_configured === true,
  );
  const legacyProjects = projects.filter((project) => !project.connection_id);
  const orphanProjects = projects.filter(
    (project) =>
      !!project.connection_id && !connectionIds.has(project.connection_id),
  );
  const canManage = config.can_manage === true;
  const connectionCredentialsAvailable =
    config.credential_storage_configured;
  const connected =
    config.configured ||
    connections.some(
      (connection) =>
        connection.server_configured === true ||
        connectionCredentialsAvailable,
    );
  const description = serverConfiguredConnection
    ? t(($) => $.gitlab.instances_configured)
    : config.configured
      ? config.base_url
      : connections.length > 0
        ? t(($) => $.gitlab.instances_configured)
        : t(($) => $.gitlab.not_configured);

  async function submitConnection(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const baseUrl = instanceUrl.trim();
    const token = accessToken.trim();
    if (
      !canManage ||
      !config.credential_storage_configured ||
      !baseUrl ||
      !token
    ) {
      return;
    }
    setConnecting(true);
    try {
      const response = await api.createGitLabConnection(wsId, {
        base_url: baseUrl,
        access_token: token,
      });
      setCreatedConnection(response);
      setInstanceUrl("");
      setAccessToken("");
      await invalidate();
    } catch (error) {
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.gitlab.toast_connection_add_failed),
      );
    } finally {
      setConnecting(false);
    }
  }

  function projectForm(connectionId?: string, baseUrl?: string) {
    const key = connectionId ? `connection:${connectionId}` : "legacy";
    const placeholder = t(($) => $.gitlab.project_placeholder);
    const label = baseUrl ? `${placeholder} — ${baseUrl}` : placeholder;

    function submit(event: FormEvent<HTMLFormElement>) {
      event.preventDefault();
      const project = projectInputs[key]?.trim();
      if (!project) return;
      createProject.mutate({
        ...(connectionId ? { connection_id: connectionId } : {}),
        project,
      });
    }

    return (
      <form className="flex flex-wrap gap-2" onSubmit={submit}>
        <Input
          value={projectInputs[key] ?? ""}
          onChange={(event) =>
            setProjectInputs((current) => ({
              ...current,
              [key]: event.target.value,
            }))
          }
          aria-label={label}
          placeholder={placeholder}
        />
        <Button
          type="submit"
          size="sm"
          disabled={!projectInputs[key]?.trim() || createProject.isPending}
        >
          {createProject.isPending
            ? t(($) => $.gitlab.adding_project)
            : t(($) => $.gitlab.add_project)}
        </Button>
      </form>
    );
  }

  function projectRow(
    project: GitLabProjectBinding,
    canRefresh = true,
    connection?: GitLabConnection,
  ) {
    const isServerConfigured = connection?.server_configured === true;
    const webhookUrl =
      connection?.webhook_url ||
      connection?.webhook_path ||
      (project.connection_id
        ? isServerConfigured
          ? config.manual_webhook_url
          : undefined
        : config.manual_webhook_url);
    return (
      <SettingsRow
        key={project.id}
        className="pl-16"
        label={
          <a href={project.web_url} target="_blank" rel="noreferrer">
            {project.path_with_namespace}
          </a>
        }
        description={
          <span>
            {project.hook_enabled
              ? t(($) => $.gitlab.hook_enabled)
              : t(($) => $.gitlab.hook_manual_required)}
            {!project.hook_enabled && webhookUrl ? (
              <>
                {" · "}
                <code className="break-all rounded bg-muted/50 px-1 py-0.5 font-mono select-all">
                  {webhookUrl}
                </code>
                {!project.connection_id || isServerConfigured ? (
                  <>
                    {" · "}
                    {t(($) => $.gitlab.manual_secret_hint)}
                  </>
                ) : null}
              </>
            ) : null}
            {project.last_sync_error ? <> · {project.last_sync_error}</> : null}
            {project.last_refresh_error ? (
              <> · {project.last_refresh_error}</>
            ) : null}
          </span>
        }
      >
        {canManage ? (
          <span className="flex gap-2">
            <Button
              variant="ghost"
              size="sm"
              disabled={refreshProject.isPending || !canRefresh}
              onClick={() => refreshProject.mutate(project.id)}
            >
              {t(($) => $.gitlab.refresh_project)}
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setTarget(project)}
            >
              {t(($) => $.gitlab.remove_project)}
            </Button>
          </span>
        ) : null}
      </SettingsRow>
    );
  }

  async function copy(value: string) {
    try {
      await navigator.clipboard.writeText(value);
      toast.success(t(($) => $.vcs.copied));
    } catch {
      toast.error(t(($) => $.vcs.copy_failed));
    }
  }

  return (
    <>
      <SettingsRow
        label={
          <span className="flex min-w-0 items-center gap-3">
            <HostMark>
              <GitBranch className="size-4" />
            </HostMark>
            <span className="flex min-w-0 flex-wrap items-center gap-x-2">
              {t(($) => $.gitlab.title)}
              <HostStatus
                tone={connected ? "success" : "muted"}
                label={
                  connected
                    ? t(($) => $.integrations.status_connected)
                    : t(($) => $.integrations.status_not_connected)
                }
              />
            </span>
          </span>
        }
        description={
          <span className="block pl-12">
            {description}
            {!canManage ? (
              <>
                {" "}
                {t(($) => $.gitlab.read_only_hint)}
              </>
            ) : null}
          </span>
        }
      >
        {canManage ? (
          config.credential_storage_configured ? (
            <form
              className="flex flex-wrap gap-2"
              onSubmit={submitConnection}
            >
              <Input
                type="url"
                autoComplete="url"
                value={instanceUrl}
                onChange={(event) => setInstanceUrl(event.target.value)}
                aria-label={t(($) => $.gitlab.instance_url_label)}
                placeholder={t(($) => $.gitlab.instance_url_placeholder)}
                disabled={connecting}
              />
              <Input
                type="password"
                autoComplete="off"
                spellCheck={false}
                value={accessToken}
                onChange={(event) => setAccessToken(event.target.value)}
                aria-label={t(($) => $.gitlab.access_token_label)}
                placeholder={t(($) => $.gitlab.access_token_placeholder)}
                disabled={connecting}
              />
              <Button
                type="submit"
                size="sm"
                disabled={
                  !instanceUrl.trim() ||
                  !accessToken.trim() ||
                  connecting
                }
                aria-busy={connecting || undefined}
              >
                {connecting
                  ? t(($) => $.gitlab.connecting_instance)
                  : t(($) => $.gitlab.add_instance)}
              </Button>
              <p className="w-full text-caption text-muted-foreground">
                {t(($) => $.gitlab.connection_form_hint)}
              </p>
            </form>
          ) : (
            <p className="max-w-64 text-caption text-muted-foreground">
              {t(($) => $.gitlab.credential_storage_unavailable)}
            </p>
          )
        ) : null}
      </SettingsRow>

      {connections.map((connection) => {
        const connectionProjects = projects.filter(
          (project) =>
            project.connection_id === connection.id ||
            (connection.id === serverConfiguredConnection?.id &&
              !project.connection_id),
        );
        const connectionUsable =
          connection.server_configured === true ||
          connectionCredentialsAvailable;
        return (
          <Fragment key={connection.id}>
            <SettingsRow
              className="pl-12"
              label={
                <span className="flex min-w-0 flex-wrap items-center gap-x-2">
                  <span className="break-all">{connection.base_url}</span>
                  <HostStatus
                    tone={connectionUsable ? "success" : "muted"}
                    label={
                      connectionUsable
                        ? t(($) => $.integrations.status_connected)
                        : t(($) => $.integrations.status_not_connected)
                    }
                  />
                </span>
              }
              description={
                connection.host === connection.base_url
                  ? undefined
                  : connection.host
              }
            >
              {canManage && connectionUsable
                ? projectForm(connection.id, connection.base_url)
                : null}
            </SettingsRow>
            {connectionProjects.map((project) =>
              projectRow(project, connectionUsable, connection),
            )}
          </Fragment>
        );
      })}

      {!serverConfiguredConnection &&
      (config.configured || legacyProjects.length > 0) ? (
        <>
          <SettingsRow
            className="pl-12"
            label={
              <span className="flex min-w-0 flex-wrap items-center gap-x-2">
                <span className="break-all">
                  {config.base_url ??
                    t(($) =>
                      config.configured
                        ? $.gitlab.configured
                        : legacyProjects.length > 0
                          ? $.gitlab.projects_title
                          : $.gitlab.not_configured,
                    )}
                </span>
                <HostStatus
                  tone={config.configured ? "success" : "muted"}
                  label={
                    config.configured
                      ? t(($) => $.integrations.status_connected)
                      : t(($) => $.integrations.status_not_connected)
                  }
                />
              </span>
            }
          >
            {canManage && config.configured ? projectForm() : null}
          </SettingsRow>
          {legacyProjects.map((project) => projectRow(project))}
        </>
      ) : null}

      {orphanProjects.map((project) =>
        projectRow(
          project,
          !project.connection_id || connectionCredentialsAvailable,
        ),
      )}

      <Dialog
        open={!!createdConnection}
        onOpenChange={(open) => {
          if (!open) setCreatedConnection(null);
        }}
      >
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t(($) => $.vcs.webhook_setup_title)}</DialogTitle>
            <DialogDescription>
              {t(($) => $.vcs.webhook_setup_description)}
            </DialogDescription>
          </DialogHeader>
          {createdConnection ? (
            <div className="space-y-3">
              <CopyField
                id="gitlab-webhook-url"
                label={t(($) => $.vcs.webhook_url_label)}
                value={
                  createdConnection.webhook_url ||
                  createdConnection.webhook_path
                }
                onCopy={copy}
                copyLabel={t(($) => $.vcs.copy)}
              />
              {createdConnection.webhook_secret ? (
                <>
                  <CopyField
                    id="gitlab-webhook-secret"
                    label={t(($) => $.vcs.webhook_secret_label)}
                    value={createdConnection.webhook_secret}
                    onCopy={copy}
                    copyLabel={t(($) => $.vcs.copy)}
                    mono
                  />
                  <p className="text-caption text-warning">
                    {t(($) => $.vcs.webhook_secret_warning)}
                  </p>
                </>
              ) : (
                <p className="text-caption text-muted-foreground">
                  {t(($) => $.gitlab.webhook_secret_unavailable)}
                </p>
              )}
            </div>
          ) : null}
          <DialogFooter>
            <Button
              type="button"
              onClick={() => setCreatedConnection(null)}
            >
              {t(($) => $.vcs.webhook_done)}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog
        open={!!target}
        onOpenChange={(open) => {
          if (!open && !removeProject.isPending) setTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.gitlab.remove_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.gitlab.remove_confirm_description, {
                project: target?.path_with_namespace ?? "",
              })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={removeProject.isPending}>
              {t(($) => $.gitlab.remove_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction
              disabled={removeProject.isPending}
              onClick={(event) => {
                event.preventDefault();
                if (target) removeProject.mutate(target.id);
              }}
            >
              {removeProject.isPending
                ? t(($) => $.gitlab.removing_project)
                : t(($) => $.gitlab.remove_confirm_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

function CopyField({
  id,
  label,
  value,
  onCopy,
  copyLabel,
  mono,
}: {
  id: string;
  label: string;
  value: string;
  onCopy: (value: string) => void;
  copyLabel: string;
  mono?: boolean;
}) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id} className="text-caption">
        {label}
      </Label>
      <div className="flex items-center gap-2">
        <Input
          id={id}
          readOnly
          value={value}
          className={mono ? "min-w-0 font-mono text-caption" : "min-w-0 text-caption"}
        />
        <IconButton
          variant="outline"
          size="icon-sm"
          className="shrink-0"
          onClick={() => onCopy(value)}
          label={`${copyLabel}: ${label}`}
        >
          <Copy />
        </IconButton>
      </div>
    </div>
  );
}
