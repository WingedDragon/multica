import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const gitlabKeys = {
  all: (wsId: string) => ["gitlab", wsId] as const,
  config: (wsId: string) => [...gitlabKeys.all(wsId), "config"] as const,
  mergeRequests: (issueId: string) => ["gitlab", "merge-requests", issueId] as const,
  mergeRequestDetails: (issueId: string, mrId: string) =>
    ["gitlab", "merge-request-details", issueId, mrId] as const,
  jobTrace: (issueId: string, jobId: string) => ["gitlab", "job-trace", issueId, jobId] as const,
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

export const gitlabMergeRequestDetailsOptions = (issueId: string, mrId: string) =>
  queryOptions({
    queryKey: gitlabKeys.mergeRequestDetails(issueId, mrId),
    queryFn: () => api.getGitLabMergeRequestDetails(issueId, mrId),
    enabled: !!issueId && !!mrId,
  });

export const gitlabJobTraceOptions = (issueId: string, jobId: string) =>
  queryOptions({
    queryKey: gitlabKeys.jobTrace(issueId, jobId),
    queryFn: () => api.getGitLabJobTrace(issueId, jobId),
    enabled: !!issueId && !!jobId,
  });
