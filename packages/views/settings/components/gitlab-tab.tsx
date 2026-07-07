"use client";

import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ExternalLink, RefreshCw, Trash2 } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Input } from "@multica/ui/components/ui/input";
import { cn } from "@multica/ui/lib/utils";
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
import { gitlabConfigOptions, gitlabKeys } from "@multica/core/gitlab";
import { workspaceKeys } from "@multica/core/workspace/queries";
import { api } from "@multica/core/api";
import type { GitLabProjectBinding } from "@multica/core/types";
import { useNavigation } from "../../navigation";
import { useT } from "../../i18n";
import { GitLabMark } from "./gitlab-mark";

export function GitLabTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const navigation = useNavigation();
  const [projectInput, setProjectInput] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<GitLabProjectBinding | null>(null);

  const { data, isLoading } = useQuery(gitlabConfigOptions(wsId));
  const configured = data?.configured === true;
  const canManage = data?.can_manage === true;
  const projects = data?.projects ?? [];
  const repositoriesHref = `${navigation.pathname}?tab=repositories`;

  const createProject = useMutation({
    mutationFn: (project: string) => api.createGitLabProject(wsId, { project }),
    onSuccess: async () => {
      setProjectInput("");
      await Promise.all([
        qc.invalidateQueries({ queryKey: gitlabKeys.config(wsId) }),
        qc.invalidateQueries({ queryKey: workspaceKeys.list() }),
      ]);
      toast.success(t(($) => $.gitlab.toast_project_added));
    },
    onError: (e) => {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.gitlab.toast_project_add_failed),
      );
    },
  });

  const deleteProject = useMutation({
    mutationFn: (bindingId: string) => api.deleteGitLabProject(wsId, bindingId),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: gitlabKeys.config(wsId) });
      toast.success(t(($) => $.gitlab.toast_project_deleted));
      setDeleteTarget(null);
    },
    onError: (e) => {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.gitlab.toast_project_delete_failed),
      );
    },
  });

  const refreshProject = useMutation({
    mutationFn: (bindingId: string) => api.refreshGitLabProject(wsId, bindingId),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: gitlabKeys.config(wsId) });
      toast.success(t(($) => $.gitlab.toast_project_refreshed));
    },
    onError: (e) => {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.gitlab.toast_project_refresh_failed),
      );
    },
  });

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const project = projectInput.trim();
    if (!project || createProject.isPending || !configured || !canManage) return;
    createProject.mutate(project);
  }

  function handleConfirmDelete() {
    if (!deleteTarget || deleteProject.isPending) return;
    deleteProject.mutate(deleteTarget.id);
  }

  return (
    <div className="space-y-8">
      <section className="space-y-1">
        <p className="text-sm text-muted-foreground">
          {t(($) => $.gitlab.page_description)}
        </p>
      </section>

      <section className="space-y-3">
        <Card>
          <CardContent className="space-y-4">
            <div className="flex items-start justify-between gap-4">
              <div className="flex items-start gap-3">
                <GitLabMark className="mt-0.5 h-6 w-6 shrink-0" />
                <div className="space-y-1">
                  <p className="text-sm font-medium">{t(($) => $.gitlab.title)}</p>
                  {isLoading ? (
                    <p className="text-xs text-muted-foreground">
                      {t(($) => $.gitlab.loading)}
                    </p>
                  ) : configured ? (
                    <div className="space-y-1">
                      <p className="text-xs text-muted-foreground">
                        {t(($) => $.gitlab.configured)}
                      </p>
                      {data?.base_url && (
                        <p className="text-xs text-muted-foreground">{data.base_url}</p>
                      )}
                    </div>
                  ) : (
                    <p className="text-xs text-muted-foreground">
                      {t(($) => $.gitlab.not_configured)}
                    </p>
                  )}
                </div>
              </div>
            </div>

            {!isLoading &&
              (canManage ? (
                <form className="flex flex-col gap-2 sm:flex-row" onSubmit={handleSubmit}>
                  <Input
                    value={projectInput}
                    onChange={(e) => setProjectInput(e.target.value)}
                    aria-label={t(($) => $.gitlab.project_placeholder)}
                    name="gitlab-project"
                    autoComplete="off"
                    placeholder={t(($) => $.gitlab.project_placeholder)}
                    disabled={!configured || createProject.isPending}
                  />
                  <Button
                    type="submit"
                    size="sm"
                    disabled={!configured || createProject.isPending || !projectInput.trim()}
                    className="sm:shrink-0"
                  >
                    {createProject.isPending
                      ? t(($) => $.gitlab.adding_project)
                      : t(($) => $.gitlab.add_project)}
                  </Button>
                </form>
              ) : (
                <p className="text-xs text-muted-foreground">
                  {t(($) => $.gitlab.read_only_hint)}
                </p>
              ))}
          </CardContent>
        </Card>
      </section>

      <section className="space-y-3">
        <h2 className="text-sm font-semibold">{t(($) => $.gitlab.projects_title)}</h2>
        {isLoading ? (
          <Card>
            <CardContent>
              <p className="text-sm text-muted-foreground">{t(($) => $.gitlab.loading)}</p>
            </CardContent>
          </Card>
        ) : projects.length === 0 ? (
          <Card>
            <CardContent>
              <p className="text-sm text-muted-foreground">
                {t(($) => $.gitlab.projects_empty)}
              </p>
            </CardContent>
          </Card>
        ) : (
          <Card>
            <CardContent className="divide-y">
              {projects.map((project) => (
                <ProjectRow
                  key={project.id}
                  project={project}
                  manualWebhookUrl={data?.manual_webhook_url}
                  canManage={canManage}
                  deleting={deleteProject.isPending}
                  refreshing={refreshProject.isPending && refreshProject.variables === project.id}
                  onDelete={() => setDeleteTarget(project)}
                  onRefresh={() => refreshProject.mutate(project.id)}
                />
              ))}
            </CardContent>
          </Card>
        )}
      </section>

      <section className="space-y-3">
        <h2 className="text-sm font-semibold">{t(($) => $.gitlab.section_repositories)}</h2>
        <Card>
          <CardContent>
            <div className="flex flex-wrap items-center justify-between gap-3">
              <p className="text-sm font-medium">
                {t(($) => $.gitlab.repositories_shortcut_label)}
              </p>
              <Button
                variant="outline"
                size="sm"
                onClick={() => navigation.push(repositoriesHref)}
              >
                <ExternalLink className="h-3 w-3" />
                {t(($) => $.gitlab.repositories_shortcut_link)}
              </Button>
            </div>
          </CardContent>
        </Card>
      </section>

      <AlertDialog
        open={!!deleteTarget}
        onOpenChange={(open) => {
          if (!open && !deleteProject.isPending) setDeleteTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.gitlab.remove_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.gitlab.remove_confirm_description, {
                project: deleteTarget?.path_with_namespace ?? "",
              })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleteProject.isPending}>
              {t(($) => $.gitlab.remove_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction
              onClick={(event) => {
                event.preventDefault();
                handleConfirmDelete();
              }}
              disabled={deleteProject.isPending}
            >
              {deleteProject.isPending
                ? t(($) => $.gitlab.removing_project)
                : t(($) => $.gitlab.remove_confirm_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function ProjectRow({
  project,
  manualWebhookUrl,
  canManage,
  deleting,
  refreshing,
  onDelete,
  onRefresh,
}: {
  project: GitLabProjectBinding;
  manualWebhookUrl?: string;
  canManage: boolean;
  deleting: boolean;
  refreshing: boolean;
  onDelete: () => void;
  onRefresh: () => void;
}) {
  const { t } = useT("settings");

  return (
    <div className="flex items-start justify-between gap-4 py-3 first:pt-0 last:pb-0">
      <div className="min-w-0 space-y-2">
        <div className="space-y-1">
          <p className="truncate text-sm font-medium">{project.path_with_namespace}</p>
          <a
            href={project.web_url}
            target="_blank"
            rel="noreferrer"
            className="inline-flex max-w-full items-center gap-1 truncate text-xs text-muted-foreground hover:text-foreground"
          >
            <span className="truncate">{project.web_url}</span>
            <ExternalLink className="h-3 w-3 shrink-0" />
          </a>
        </div>

        <div className="space-y-1">
          <p className="text-xs text-muted-foreground">
            {project.hook_enabled
              ? t(($) => $.gitlab.hook_enabled)
              : t(($) => $.gitlab.hook_manual_required)}
          </p>
          {!project.hook_enabled && manualWebhookUrl && (
            <p className="break-all text-xs text-muted-foreground">{manualWebhookUrl}</p>
          )}
          {!project.hook_enabled && project.last_sync_error && (
            <p className="text-xs text-muted-foreground">
              {redactGitLabSecretText(project.last_sync_error)}
            </p>
          )}
          {!project.hook_enabled && (
            <p className="text-xs text-muted-foreground">
              {t(($) => $.gitlab.manual_secret_hint)}
            </p>
          )}
          <div className="space-y-0.5 text-xs text-muted-foreground">
            {project.last_event_at || project.last_event_type ? (
              <p>
                {t(($) => $.gitlab.last_event, {
                  type: project.last_event_type ?? "unknown",
                  time: project.last_event_at ?? "unknown",
                })}
              </p>
            ) : null}
            {project.last_refresh_at ? (
              <p>{t(($) => $.gitlab.last_refresh, { time: project.last_refresh_at })}</p>
            ) : null}
            {project.refresh_in_progress_at ? (
              <p>{t(($) => $.gitlab.refresh_in_progress, { time: project.refresh_in_progress_at })}</p>
            ) : null}
            {project.last_refresh_error ? (
              <p>{redactGitLabSecretText(project.last_refresh_error)}</p>
            ) : null}
          </div>
        </div>
      </div>

      {canManage && (
        <div className="flex shrink-0 items-center gap-1">
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label={t(($) => $.gitlab.refresh_project)}
            disabled={refreshing}
            onClick={onRefresh}
          >
            <RefreshCw className={cn("h-4 w-4", refreshing && "animate-spin")} />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label={t(($) => $.gitlab.remove_project)}
            disabled={deleting}
            onClick={onDelete}
          >
            <Trash2 className="h-4 w-4" />
          </Button>
        </div>
      )}
    </div>
  );
}

function redactGitLabSecretText(text: string) {
  return text
    .replace(/\bglpat-[A-Za-z0-9_-]{20,}\b/g, "[redacted]")
    .replace(/(GITLAB_WEBHOOK_SECRET\s*[=:]\s*["']?)[^"',\s}]+(["']?)/gi, "$1[redacted]$2")
    .replace(/(["']?(?:secret-token|hook-secret|token)["']?\s*[=:]\s*["']?)[^"',\s}]+(["']?)/gi, "$1[redacted]$2")
    .replace(/hook-secret/gi, "[redacted]")
    .replace(/secret-token/gi, "[redacted]");
}
