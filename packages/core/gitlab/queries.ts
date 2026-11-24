import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const gitlabKeys = {
  all: (wsId: string) => ["gitlab", wsId] as const,
  projects: (wsId: string) => [...gitlabKeys.all(wsId), "projects"] as const,
  mergeRequestDetails: (issueId: string, prId: string) =>
    ["gitlab", "merge-request-details", issueId, prId] as const,
};

export const gitlabProjectsOptions = (wsId: string) =>
  queryOptions({
    queryKey: gitlabKeys.projects(wsId),
    queryFn: () => api.listGitLabProjects(wsId),
    enabled: !!wsId,
  });

export const gitlabMergeRequestDetailsOptions = (issueId: string, prId: string) =>
  queryOptions({
    queryKey: gitlabKeys.mergeRequestDetails(issueId, prId),
    queryFn: () => api.getGitLabMergeRequestDetails(issueId, prId),
    enabled: !!issueId && !!prId,
  });

