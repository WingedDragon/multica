import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const gitlabKeys = {
  all: (wsId: string) => ["gitlab", wsId] as const,
  config: (wsId: string) => [...gitlabKeys.all(wsId), "config"] as const,
  mergeRequests: (issueId: string) => ["gitlab", "merge-requests", issueId] as const,
};

export const gitlabConfigOptions = (wsId: string) =>
  queryOptions({
    queryKey: gitlabKeys.config(wsId),
    queryFn: () => api.getGitLabConfig(wsId),
    enabled: !!wsId,
  });

export const issueGitLabMergeRequestsOptions = (issueId: string) =>
  queryOptions({
    queryKey: gitlabKeys.mergeRequests(issueId),
    queryFn: () => api.listIssueGitLabMergeRequests(issueId),
    enabled: !!issueId,
  });
