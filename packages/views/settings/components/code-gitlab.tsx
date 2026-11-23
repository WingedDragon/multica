"use client";

import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@multica/ui/components/ui/alert-dialog";
import { useWorkspaceId } from "@multica/core/hooks";
import { gitlabConfigOptions, gitlabKeys } from "@multica/core/gitlab/queries";
import { workspaceKeys } from "@multica/core/workspace/queries";
import { api } from "@multica/core/api";
import type { GitLabProjectBinding } from "@multica/core/types";
import { useT } from "../../i18n";
import { SettingsRow } from "./settings-layout";

export function GitLabProjectRows() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const { data } = useQuery(gitlabConfigOptions(wsId));
  const [input, setInput] = useState("");
  const [target, setTarget] = useState<GitLabProjectBinding | null>(null);
  const invalidate = () => Promise.all([
    qc.invalidateQueries({ queryKey: gitlabKeys.config(wsId) }),
    qc.invalidateQueries({ queryKey: workspaceKeys.list() }),
  ]);
  const create = useMutation({
    mutationFn: (project: string) => api.createGitLabProject(wsId, { project }),
    onSuccess: async () => { setInput(""); await invalidate(); },
    onError: (e) => toast.error(e instanceof Error ? e.message : t(($) => $.gitlab.toast_project_add_failed)),
  });
  const remove = useMutation({
    mutationFn: (id: string) => api.deleteGitLabProject(wsId, id),
    onSuccess: async () => { setTarget(null); await invalidate(); },
    onError: (e) => toast.error(e instanceof Error ? e.message : t(($) => $.gitlab.toast_project_delete_failed)),
  });
  const refresh = useMutation({
    mutationFn: (id: string) => api.refreshGitLabProject(wsId, id),
    onSuccess: async () => { await invalidate(); toast.success(t(($) => $.gitlab.toast_project_refreshed)); },
    onError: (e) => toast.error(e instanceof Error ? e.message : t(($) => $.gitlab.toast_project_refresh_failed)),
  });
  if (!data) return null;

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const project = input.trim();
    if (project) create.mutate(project);
  }

  return (
    <>
      <SettingsRow label={t(($) => $.gitlab.title)} description={data.configured ? data.base_url : t(($) => $.gitlab.not_configured)}>
        {data.can_manage && data.configured ? (
          <form className="flex flex-wrap gap-2" onSubmit={submit}>
            <Input value={input} onChange={(event) => setInput(event.target.value)} aria-label={t(($) => $.gitlab.project_placeholder)} placeholder={t(($) => $.gitlab.project_placeholder)} />
            <Button type="submit" size="sm" disabled={!input.trim() || create.isPending}>{create.isPending ? t(($) => $.gitlab.adding_project) : t(($) => $.gitlab.add_project)}</Button>
          </form>
        ) : null}
      </SettingsRow>
      {data.projects.map((project) => (
        <SettingsRow key={project.id} className="pl-16" label={<a href={project.web_url} target="_blank" rel="noreferrer">{project.path_with_namespace}</a>} description={<span>{project.hook_enabled ? t(($) => $.gitlab.hook_enabled) : t(($) => $.gitlab.hook_manual_required)}{!project.hook_enabled && data.manual_webhook_url ? <> · {data.manual_webhook_url} · {t(($) => $.gitlab.manual_secret_hint)}</> : null}{project.last_sync_error ? <> · {project.last_sync_error}</> : null}{project.last_refresh_error ? <> · {project.last_refresh_error}</> : null}</span>}>
          {data.can_manage ? <span className="flex gap-2"><Button variant="ghost" size="sm" disabled={refresh.isPending} onClick={() => refresh.mutate(project.id)}>{t(($) => $.gitlab.refresh_project)}</Button><Button variant="ghost" size="sm" onClick={() => setTarget(project)}>{t(($) => $.gitlab.remove_project)}</Button></span> : null}
        </SettingsRow>
      ))}
      <AlertDialog open={!!target} onOpenChange={(open) => { if (!open && !remove.isPending) setTarget(null); }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t(($) => $.gitlab.remove_confirm_title)}</AlertDialogTitle><AlertDialogDescription>{t(($) => $.gitlab.remove_confirm_description, { project: target?.path_with_namespace ?? "" })}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel disabled={remove.isPending}>{t(($) => $.gitlab.remove_confirm_cancel)}</AlertDialogCancel><AlertDialogAction disabled={remove.isPending} onClick={(event) => { event.preventDefault(); if (target) remove.mutate(target.id); }}>{remove.isPending ? t(($) => $.gitlab.removing_project) : t(($) => $.gitlab.remove_confirm_action)}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
