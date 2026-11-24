"use client";

import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Badge } from "@multica/ui/components/ui/badge";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { LoaderCircle } from "@multica/ui/icons";
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
import type { CreateGitLabProjectResponse, GitLabGroupProject, GitLabProjectBinding, VCSConnection } from "@multica/core/types";
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
  const [groupPickerOpen, setGroupPickerOpen] = useState(false);
  const [groupInput, setGroupInput] = useState("");
  const [groupSearch, setGroupSearch] = useState("");
  const [selectedGroupProjectIds, setSelectedGroupProjectIds] = useState(
    () => new Set<number>(),
  );
  const [importedGroupProjectIds, setImportedGroupProjectIds] = useState(
    () => new Set<number>(),
  );
  const [groupImportManualHookProjects, setGroupImportManualHookProjects] =
    useState<GitLabGroupProject[]>([]);
  const [groupImportFailures, setGroupImportFailures] = useState<
    { project: GitLabGroupProject; error: string }[]
  >([]);
  const [isImportingGroupProjects, setIsImportingGroupProjects] = useState(false);

  const groupProjectsQuery = useQuery({
    queryKey: ["gitlab", "group-projects", wsId, connection.id, groupSearch],
    queryFn: () =>
      api.listGitLabGroupProjects(wsId, connection.id, groupSearch),
    enabled: !!wsId && canManage && !!groupSearch,
    retry: false,
  });
  const groupProjects = groupProjectsQuery.data?.projects ?? [];
  const boundGroupProjectIds = new Set(
    projects.map((project) => project.gitlab_project_id),
  );
  for (const projectId of importedGroupProjectIds) {
    boundGroupProjectIds.add(projectId);
  }
  const selectableGroupProjects = groupProjects.filter(
    (project) => !boundGroupProjectIds.has(project.id),
  );
  const allSelectableGroupProjectsSelected =
    selectableGroupProjects.length > 0 &&
    selectableGroupProjects.every((project) =>
      selectedGroupProjectIds.has(project.id),
    );

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

  function openGroupPicker() {
    setGroupInput("");
    setGroupSearch("");
    setSelectedGroupProjectIds(new Set());
    setGroupImportFailures([]);
    setGroupImportManualHookProjects([]);
    setGroupPickerOpen(true);
  }

  function closeGroupPicker() {
    setGroupPickerOpen(false);
  }

  function resetGroupPicker() {
    setGroupInput("");
    setGroupSearch("");
    setSelectedGroupProjectIds(new Set());
    setImportedGroupProjectIds(new Set());
    setGroupImportFailures([]);
    setGroupImportManualHookProjects([]);
  }

  function searchGroupProjects(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const group = groupInput.trim();
    if (!canManage || !group || isImportingGroupProjects) return;
    setGroupImportFailures([]);
    setSelectedGroupProjectIds(new Set());
    setGroupSearch(group);
    if (group === groupSearch) void groupProjectsQuery.refetch();
  }

  function toggleGroupProject(project: GitLabGroupProject, checked: boolean) {
    if (boundGroupProjectIds.has(project.id)) return;
    setSelectedGroupProjectIds((current) => {
      const next = new Set(current);
      if (checked) next.add(project.id);
      else next.delete(project.id);
      return next;
    });
  }

  function toggleAllGroupProjects(checked: boolean) {
    setSelectedGroupProjectIds(
      checked
        ? new Set(selectableGroupProjects.map((project) => project.id))
        : new Set(),
    );
  }

  async function importGroupProjects() {
    const selectedProjects = groupProjects.filter(
      (project) =>
        selectedGroupProjectIds.has(project.id) &&
        !boundGroupProjectIds.has(project.id),
    );
    if (!canManage || selectedProjects.length === 0 || isImportingGroupProjects) {
      return;
    }

    setIsImportingGroupProjects(true);
    setGroupImportFailures([]);
    try {
      const results: PromiseSettledResult<CreateGitLabProjectResponse>[] = [];
      for (const project of selectedProjects) {
        results.push(...await Promise.allSettled([
          api.createGitLabProject(wsId, {
            connection_id: connection.id,
            project: project.path_with_namespace,
          }),
        ]));
      }
      const importedIds: number[] = [];
      const manualHookProjects: GitLabGroupProject[] = [];
      const failures: { project: GitLabGroupProject; error: string }[] = [];
      results.forEach((result, index) => {
        const project = selectedProjects[index]!;
        if (result.status === "fulfilled") {
          importedIds.push(project.id);
          if (!result.value.project.hook_enabled) manualHookProjects.push(project);
        } else {
          failures.push({
            project,
            error:
              result.reason instanceof Error && result.reason.message
                ? result.reason.message
                : t(($) => $.gitlab.toast_project_add_failed),
          });
        }
      });

      if (importedIds.length > 0) {
        setImportedGroupProjectIds((current) => {
          const next = new Set(current);
          for (const projectId of importedIds) next.add(projectId);
          return next;
        });
      }
      if (manualHookProjects.length > 0) {
        setGroupImportManualHookProjects((current) => {
          const next = [...current];
          const knownIds = new Set(current.map((project) => project.id));
          for (const project of manualHookProjects) {
            if (!knownIds.has(project.id)) next.push(project);
          }
          return next;
        });
      }
      if (importedIds.length > 0) await invalidateProjects();
      setGroupImportFailures(failures);
      setSelectedGroupProjectIds(
        new Set(failures.map(({ project }) => project.id)),
      );
      if (
        failures.length === 0 &&
        groupImportManualHookProjects.length === 0 &&
        manualHookProjects.length === 0
      ) {
        closeGroupPicker();
      }
    } finally {
      setIsImportingGroupProjects(false);
    }
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
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={openGroupPicker}
            >
              {t(($) => $.gitlab.choose_group_projects)}
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

      <Dialog
        open={groupPickerOpen}
        onOpenChange={(open) => {
          if (open) setGroupPickerOpen(true);
          else if (!isImportingGroupProjects) closeGroupPicker();
        }}
        onOpenChangeComplete={(open) => {
          if (!open) resetGroupPicker();
        }}
      >
        <DialogContent className="flex max-h-[90dvh] w-[calc(100vw_-_2rem)] flex-col gap-0 overflow-hidden p-0 sm:max-w-2xl">
          <DialogHeader className="border-b px-4 py-4 sm:px-6">
            <DialogTitle>{t(($) => $.gitlab.group_picker_title)}</DialogTitle>
            <DialogDescription>
              {t(($) => $.gitlab.group_picker_description)}
            </DialogDescription>
          </DialogHeader>

          <form
            className="flex flex-col gap-2 px-4 py-3 sm:flex-row sm:px-6"
            onSubmit={searchGroupProjects}
          >
            <Input
              value={groupInput}
              onChange={(event) => {
                setGroupInput(event.target.value);
                setGroupSearch("");
                setSelectedGroupProjectIds(new Set());
                setGroupImportFailures([]);
                setGroupImportManualHookProjects([]);
              }}
              aria-label={t(($) => $.gitlab.group_path_placeholder)}
              placeholder={t(($) => $.gitlab.group_path_placeholder)}
              autoComplete="off"
              spellCheck={false}
              disabled={isImportingGroupProjects}
              className="min-w-0 flex-1"
            />
            <Button
              type="submit"
              disabled={!groupInput.trim() || groupProjectsQuery.isFetching || isImportingGroupProjects}
              aria-busy={groupProjectsQuery.isFetching || undefined}
              className="w-full sm:w-auto"
            >
              {groupProjectsQuery.isFetching
                ? t(($) => $.gitlab.group_searching)
                : t(($) => $.gitlab.group_search)}
            </Button>
          </form>
          <div className="min-h-0 flex-1 overflow-y-auto border-y">
            {groupImportManualHookProjects.length > 0 ? (
              <div
                role="status"
                className="mx-4 mt-3 rounded-md border border-warning/30 bg-warning/5 px-3 py-2 text-caption sm:mx-6"
              >
                <p className="font-medium">
                  {t(($) => $.gitlab.group_manual_hook_title)}
                </p>
                <p className="mt-1 text-muted-foreground">
                  {t(($) => $.gitlab.group_manual_hook_description)}
                </p>
                <ul className="mt-1 list-inside list-disc space-y-1">
                  {groupImportManualHookProjects.map((project) => (
                    <li key={project.id}>{project.path_with_namespace}</li>
                  ))}
                </ul>
              </div>
            ) : null}

            {groupImportFailures.length > 0 ? (
              <div
                role="alert"
                className="mx-4 mt-3 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-caption sm:mx-6"
              >
                <p className="font-medium">
                  {t(($) => $.gitlab.group_import_failures)}
                </p>
                <ul className="mt-1 list-inside list-disc space-y-1">
                  {groupImportFailures.map(({ project, error }) => (
                    <li key={project.id}>
                      <span className="font-medium">
                        {project.path_with_namespace}
                      </span>
                      : {error}
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}

            {!groupSearch ? (
              <div className="px-4 py-10 text-center text-caption text-muted-foreground sm:px-6">
                {t(($) => $.gitlab.group_search_prompt)}
              </div>
            ) : groupProjectsQuery.isPending ? (
              <div className="flex items-center justify-center gap-2 px-4 py-10 text-caption text-muted-foreground sm:px-6">
                <LoaderCircle className="size-4 animate-spin" />
                {t(($) => $.gitlab.group_projects_loading)}
              </div>
            ) : groupProjectsQuery.isError ? (
              <div className="flex flex-col items-center gap-2 px-4 py-10 text-center text-caption text-muted-foreground sm:px-6">
                <p role="alert">
                  {t(($) => $.gitlab.group_projects_load_failed)}
                  {groupProjectsQuery.error instanceof Error
                    ? ` ${groupProjectsQuery.error.message}`
                    : ""}
                </p>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => void groupProjectsQuery.refetch()}
                  disabled={groupProjectsQuery.isFetching}
                >
                  {t(($) => $.gitlab.retry)}
                </Button>
              </div>
            ) : groupProjects.length === 0 ? (
              <div className="px-4 py-10 text-center text-caption text-muted-foreground sm:px-6">
                {t(($) => $.gitlab.group_projects_empty)}
              </div>
            ) : (
              <>
                <div className="flex items-center justify-between gap-3 border-b px-4 py-2.5 sm:px-6">
                  <p className="text-caption font-medium">
                    {t(($) => $.gitlab.group_select_all)}
                  </p>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    disabled={
                      selectableGroupProjects.length === 0 ||
                      isImportingGroupProjects
                    }
                    aria-pressed={allSelectableGroupProjectsSelected}
                    onClick={() =>
                      toggleAllGroupProjects(
                        !allSelectableGroupProjectsSelected,
                      )
                    }
                  >
                    {allSelectableGroupProjectsSelected
                      ? t(($) => $.gitlab.group_deselect_all)
                      : t(($) => $.gitlab.group_select_all)}
                  </Button>
                </div>
                <div className="divide-y">
                  {groupProjects.map((project) => {
                    const alreadyAdded = boundGroupProjectIds.has(project.id);
                    const checkboxId = `gitlab-group-project-${connection.id}-${project.id}`;
                    return (
                      <div
                        key={project.id}
                        className="flex items-start gap-3 px-4 py-3 sm:px-6"
                      >
                        <Checkbox
                          id={checkboxId}
                          aria-label={project.path_with_namespace}
                          checked={
                            alreadyAdded ||
                            selectedGroupProjectIds.has(project.id)
                          }
                          disabled={alreadyAdded || isImportingGroupProjects}
                          onCheckedChange={(checked) =>
                            toggleGroupProject(project, checked === true)
                          }
                          className="mt-0.5"
                        />
                        <div className="min-w-0 flex-1">
                          <div className="flex flex-wrap items-center gap-2">
                            <span className="min-w-0 break-all text-body font-medium">
                              <a
                                href={project.web_url}
                                target="_blank"
                                rel="noreferrer"
                                className="hover:underline"
                              >
                                {project.path_with_namespace}
                              </a>
                            </span>
                            {alreadyAdded ? (
                              <Badge variant="outline">
                                {t(($) => $.gitlab.group_project_added)}
                              </Badge>
                            ) : null}
                          </div>
                        </div>
                      </div>
                    );
                  })}
                </div>
              </>
            )}
          </div>

          <DialogFooter className="m-0 flex flex-col gap-2 border-t bg-muted/30 px-4 py-3 sm:flex-row sm:items-center sm:justify-between sm:px-6">
            <p className="text-caption text-muted-foreground">
              {t(($) => $.gitlab.group_selected_count, {
                count: selectedGroupProjectIds.size,
              })}
            </p>
            <div className="flex w-full gap-2 sm:w-auto">
              <Button
                variant="ghost"
                onClick={closeGroupPicker}
                disabled={isImportingGroupProjects}
                className="flex-1 sm:flex-none"
              >
                {t(($) => $.gitlab.group_cancel)}
              </Button>
              <Button
                onClick={() => void importGroupProjects()}
                disabled={
                  selectedGroupProjectIds.size === 0 ||
                  isImportingGroupProjects ||
                  !groupSearch
                }
                aria-busy={isImportingGroupProjects || undefined}
                className="flex-1 sm:flex-none"
              >
                {isImportingGroupProjects
                  ? t(($) => $.gitlab.group_importing)
                  : t(($) => $.gitlab.group_import_selected, {
                      count: selectedGroupProjectIds.size,
                    })}
              </Button>
            </div>
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
