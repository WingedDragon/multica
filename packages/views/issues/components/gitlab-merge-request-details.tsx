"use client";

import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { ExternalLink, RefreshCw } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { api } from "@multica/core/api";
import type {
  GitLabApprovalRule,
  GitLabJobTraceResponse,
  GitLabMergeRequestDetailsResponse,
  GitLabUserRef,
} from "@multica/core/types";
import { useT } from "../../i18n";

export function GitLabMergeRequestDetails({
  issueId,
  details,
}: {
  issueId: string;
  details: GitLabMergeRequestDetailsResponse;
}) {
  const { t } = useT("issues");
  const [loadedTraces, setLoadedTraces] = useState<
    Record<string, GitLabJobTraceResponse>
  >({});
  const loadTrace = useMutation({
    mutationFn: (jobId: string) => api.getGitLabJobTrace(issueId, jobId),
    onSuccess: (trace, jobId) => {
      setLoadedTraces((prev) => ({ ...prev, [jobId]: trace }));
    },
  });
  const failedJobs = details.jobs.filter((job) => job.status === "failed");
  const jobs = failedJobs.length > 0 ? failedJobs : details.jobs;
  const unresolved = details.discussions.filter(
    (discussion) => discussion.resolved !== true,
  );
  const approval = details.approval;
  const approvedBy =
    approval?.approved_by
      .map(formatGitLabUser)
      .filter((name): name is string => Boolean(name)) ?? [];
  const approvalRules = approval?.rules.filter(hasVisibleApprovalRule) ?? [];

  return (
    <div className="mt-2 space-y-2 border-l border-border pl-3 text-[11px] text-muted-foreground">
      {approval ? (
        <div className="space-y-1">
          <p className="font-medium text-foreground">
            {approval.approved
              ? t(($) => $.detail.gitlab_approvals_approved)
              : t(($) => $.detail.gitlab_approvals_remaining, {
                  count: approval.approvals_left ?? 0,
                })}
          </p>
          {approvedBy.length > 0 ? (
            <p className="line-clamp-2">
              <span className="font-medium text-foreground">
                {t(($) => $.detail.gitlab_approved_by)}
              </span>{" "}
              {approvedBy.join(", ")}
            </p>
          ) : null}
          {approvalRules.length > 0 ? (
            <div className="space-y-0.5">
              <p className="font-medium text-foreground">
                {t(($) => $.detail.gitlab_approval_rules)}
              </p>
              {approvalRules.slice(0, 3).map((rule, index) => {
                const ruleApprovedBy =
                  rule.approved_by
                    ?.map(formatGitLabUser)
                    .filter((name): name is string => Boolean(name)) ?? [];
                return (
                  <div
                    key={`${rule.id ?? rule.name ?? index}`}
                    className="space-y-0.5"
                  >
                    <p className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
                      <span className="font-medium text-foreground">
                        {rule.name ||
                          t(($) => $.detail.gitlab_approval_rule_fallback)}
                      </span>
                      {typeof rule.approvals_required === "number" ? (
                        <span>
                          {t(($) => $.detail.gitlab_approval_rule_required, {
                            count: rule.approvals_required,
                          })}
                        </span>
                      ) : null}
                      {typeof rule.approved === "boolean" ? (
                        <span>
                          {rule.approved
                            ? t(($) => $.detail.gitlab_approval_rule_approved)
                            : t(($) => $.detail.gitlab_approval_rule_waiting)}
                        </span>
                      ) : null}
                    </p>
                    {ruleApprovedBy.length > 0 ? (
                      <p className="line-clamp-2">
                        <span className="font-medium text-foreground">
                          {t(($) => $.detail.gitlab_approved_by)}
                        </span>{" "}
                        {ruleApprovedBy.join(", ")}
                      </p>
                    ) : null}
                  </div>
                );
              })}
            </div>
          ) : null}
        </div>
      ) : null}

      {jobs.length > 0 ? (
        <div className="space-y-1">
          <p className="font-medium text-foreground">
            {failedJobs.length > 0
              ? t(($) => $.detail.gitlab_failed_jobs)
              : t(($) => $.detail.gitlab_jobs)}
          </p>
          {jobs.slice(0, 4).map((job) => {
            const loadedTrace = loadedTraces[job.id];
            const traceSummary =
              loadedTrace?.trace_summary ?? job.trace_summary;
            const loadingTrace =
              loadTrace.isPending && loadTrace.variables === job.id;
            return (
              <div key={job.id} className="space-y-0.5">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
                  <span className="font-medium text-foreground">
                    {job.name}
                  </span>
                  <span>{job.status}</span>
                  {job.status === "failed" ? (
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon"
                      className="h-5 w-5"
                      aria-label={t(($) => $.detail.gitlab_load_trace)}
                      disabled={loadingTrace}
                      onClick={() => loadTrace.mutate(job.id)}
                    >
                      <RefreshCw
                        className={`h-3 w-3 ${loadingTrace ? "animate-spin" : ""}`}
                      />
                    </Button>
                  ) : null}
                  {job.artifacts_file_name ? (
                    <a
                      href={api.gitLabJobArtifactsURL(issueId, job.id)}
                      className="inline-flex items-center gap-1 hover:text-foreground"
                    >
                      {job.artifacts_file_name}
                      <ExternalLink className="h-3 w-3" />
                    </a>
                  ) : null}
                </div>
                {traceSummary ? (
                  <pre className="max-h-24 overflow-auto whitespace-pre-wrap rounded bg-muted/50 p-2 font-mono text-[10px] leading-snug text-foreground">
                    {traceSummary}
                  </pre>
                ) : null}
              </div>
            );
          })}
        </div>
      ) : null}

      {unresolved.length > 0 ? (
        <div className="space-y-1">
          <p className="font-medium text-foreground">
            {t(($) => $.detail.gitlab_unresolved_discussions)}
          </p>
          {unresolved.slice(0, 3).map((discussion) => (
            <div key={discussion.id} className="space-y-0.5">
              {discussion.notes.slice(0, 2).map((note) => (
                <p key={note.id} className="line-clamp-2">
                  {note.author_username ? `@${note.author_username}: ` : null}
                  {note.body}
                </p>
              ))}
            </div>
          ))}
        </div>
      ) : null}
    </div>
  );
}

function formatGitLabUser(user: GitLabUserRef): string | null {
  const username = user.username ? `@${user.username}` : null;
  if (user.name && username && user.name !== user.username) {
    return `${user.name} (${username})`;
  }
  return user.name || username;
}

function hasVisibleApprovalRule(rule: GitLabApprovalRule): boolean {
  return Boolean(
    rule.name ||
      typeof rule.approved === "boolean" ||
      typeof rule.approvals_required === "number" ||
      (rule.approved_by && rule.approved_by.length > 0),
  );
}
