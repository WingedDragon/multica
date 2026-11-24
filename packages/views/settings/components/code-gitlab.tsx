"use client";

import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
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
import { useWorkspaceId } from "@multica/core/hooks";
import { api } from "@multica/core/api";
import { gitlabKeys } from "@multica/core/gitlab";
import { useT } from "../../i18n";
import type { GitLabProjectBinding, VCSConnection } from "@multica/core/types";
import { SettingsRow } from "./settings-layout";

export function GitLabProjectRows({
  connection,
  projects,
  canManage,
  hasLoaded,
}: {
  connection: VCSConnection;
  projects: GitLabProjectBinding[];
  canManage: boolean;
  hasLoaded: boolean;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const queryClient = useQueryClient();
  const [projectName, setProjectName] = useState("");
  const [target, setTarget] = useState<GitLabProjectBinding | null>(null);

  const invalidateProjects = () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: gitlabKeys.projects(wsId) }),
      queryClient.invalidateQueries({ queryKey: ["github", "pull-requests"] }),
    ]);

  const createProject = useMutation({
    mutationFn: (project: string) =>
      api.createGitLabProject(wsId, {
        connection_id: connection.id,
        project,
      }),
    onSuccess: async () => {
      setProjectName("");
      await invalidateProjects();
    },
    onError: (error) =>
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.gitlab.toast_project_add_failed),
      ),
  });

  const refreshProject = useMutation({
    mutationFn: (projectId: string) => api.refreshGitLabProject(wsId, projectId),
    onSuccess: async () => {
      await invalidateProjects();
      toast.success(t(($) => $.gitlab.toast_project_refreshed));
    },
    onError: (error) =>
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.gitlab.toast_project_refresh_failed),
      ),
  });

  const removeProject = useMutation({
    mutationFn: (projectId: string) => api.deleteGitLabProject(wsId, projectId),
    onSuccess: async () => {
      setTarget(null);
      await invalidateProjects();
    },
    onError: (error) =>
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.gitlab.toast_project_delete_failed),
      ),
  });

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const project = projectName.trim();
    if (canManage && project) createProject.mutate(project);
  }

  const webhookAddress = connection.webhook_url || connection.webhook_path;

  return (
    <>
      <SettingsRow
        className="pl-16"
        label={t(($) => $.gitlab.projects_title)}
        description={
          !canManage
            ? t(($) => $.gitlab.read_only_hint)
            : hasLoaded && projects.length === 0
              ? t(($) => $.gitlab.projects_empty)
              : undefined
        }
      >
        {canManage ? (
          <form className="flex flex-wrap gap-2" onSubmit={submit}>
            <Input
              value={projectName}
              onChange={(event) => setProjectName(event.target.value)}
              aria-label={`${t(($) => $.gitlab.project_placeholder)} — ${connection.instance_url}`}
              placeholder={t(($) => $.gitlab.project_placeholder)}
            />
            <Button
              type="submit"
              size="sm"
              disabled={!projectName.trim() || createProject.isPending}
              aria-busy={createProject.isPending || undefined}
            >
              {createProject.isPending
                ? t(($) => $.gitlab.adding_project)
                : t(($) => $.gitlab.add_project)}
            </Button>
          </form>
        ) : null}
      </SettingsRow>

      {projects.map((project) => (
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
                {!project.hook_enabled && webhookAddress ? (
                  <>
                    {" · "}
                    <code className="break-all rounded bg-muted/50 px-1 py-0.5 font-mono select-all">
                      {webhookAddress}
                    </code>
                    {" · "}
                    {t(($) => $.gitlab.manual_secret_hint)}
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
                  disabled={refreshProject.isPending}
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
      ))}

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
