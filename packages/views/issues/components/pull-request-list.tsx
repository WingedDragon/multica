"use client";

import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  CheckCircle2,
  CircleDashed,
  GitMerge,
  GitPullRequest,
  GitPullRequestArrow,
  GitPullRequestClosed,
  GitPullRequestDraft,
  TriangleAlert,
  XCircle,
} from "lucide-react";
import {
  issuePullRequestsOptions,
  derivePullRequestStatusKind,
  derivePullRequestProgressSegments,
  shouldShowPullRequestStats,
  type PullRequestStatusKind,
  type PullRequestProgressSegment,
} from "@multica/core/github";
import { issueGitLabMergeRequestsOptions } from "@multica/core/gitlab";
import type {
  GitHubPullRequest,
  GitHubPullRequestChecksConclusion,
  GitLabMergeRequest,
  GitLabPipelineStatus,
} from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

type IssuesT = ReturnType<typeof useT<"issues">>["t"];

// Keep the existing sidebar density: show the first 3 PR rows inline, then
// collapse the rest once the section reaches 4 rows.
const PR_LIMIT_BEFORE_COLLAPSE = 4;

const STATE_ICON: Record<
  string,
  { icon: React.ComponentType<{ className?: string }>; className: string }
> = {
  open: { icon: GitPullRequestArrow, className: "text-emerald-600 dark:text-emerald-400" },
  draft: { icon: GitPullRequestDraft, className: "text-muted-foreground" },
  merged: { icon: GitMerge, className: "text-violet-600 dark:text-violet-400" },
  closed: { icon: GitPullRequestClosed, className: "text-rose-600 dark:text-rose-400" },
};

const CHECKS_ICON: Record<
  GitHubPullRequestChecksConclusion,
  { icon: React.ComponentType<{ className?: string }>; className: string }
> = {
  passed: { icon: CheckCircle2, className: "text-emerald-600 dark:text-emerald-400" },
  failed: { icon: XCircle, className: "text-rose-600 dark:text-rose-400" },
  pending: { icon: CircleDashed, className: "text-amber-600 dark:text-amber-400" },
};

type PullRequestRowEntry =
  | { provider: "github"; item: GitHubPullRequest }
  | { provider: "gitlab"; item: GitLabMergeRequest };

type GitLabMergeRequestStatusKind =
  | PullRequestStatusKind
  | "gitlab_blocked"
  | "gitlab_checking"
  | "gitlab_unknown";

const GITLAB_CONFLICT_STATUSES = new Set(["conflict", "need_rebase"]);

const GITLAB_BLOCKED_STATUSES = new Set([
  "ci_must_pass",
  "commits_status",
  "discussions_not_resolved",
  "draft_status",
  "jira_association_missing",
  "locked_lfs_files",
  "locked_paths",
  "merge_request_blocked",
  "merge_time",
  "not_approved",
  "not_open",
  "requested_changes",
  "security_policy_pipeline_check",
  "security_policy_violations",
  "status_checks_must_pass",
  "title_regex",
]);

const GITLAB_CHECKING_STATUSES = new Set([
  "approvals_syncing",
  "checking",
  "ci_still_running",
  "preparing",
  "unchecked",
]);

export function PullRequestList({
  issueId,
  includeGitHub = true,
}: {
  issueId: string;
  includeGitHub?: boolean;
}) {
  const { t } = useT("issues");
  const [expanded, setExpanded] = useState(false);
  const githubQuery = useQuery({
    ...issuePullRequestsOptions(issueId),
    enabled: includeGitHub && !!issueId,
  });
  const gitlabQuery = useQuery(issueGitLabMergeRequestsOptions(issueId));
  const rows: PullRequestRowEntry[] = [
    ...(includeGitHub
      ? (githubQuery.data?.pull_requests ?? []).map((pr) => ({ provider: "github" as const, item: pr }))
      : []),
    ...(gitlabQuery.data?.merge_requests ?? []).map((mr) => ({ provider: "gitlab" as const, item: mr })),
  ].sort((a, b) => createdAt(b).localeCompare(createdAt(a)));

  useEffect(() => {
    setExpanded(false);
  }, [issueId]);

  if ((includeGitHub && githubQuery.isLoading) || gitlabQuery.isLoading) {
    return <p className="text-xs text-muted-foreground px-2">{t(($) => $.detail.pull_requests_loading)}</p>;
  }
  if (rows.length === 0) {
    return (
      <p className="text-xs text-muted-foreground px-2">
        {t(($) => $.detail.pull_requests_empty)}
      </p>
    );
  }

  // Render rule:
  //   - <  PR_LIMIT_BEFORE_COLLAPSE: every PR row is visible.
  //   - >= PR_LIMIT_BEFORE_COLLAPSE: first (LIMIT - 1) rows are visible and
  //     the remainder sits behind a toggle.
  const useCollapse = rows.length >= PR_LIMIT_BEFORE_COLLAPSE;
  const expandedHead = useCollapse ? rows.slice(0, PR_LIMIT_BEFORE_COLLAPSE - 1) : rows;
  const collapsedTail = useCollapse ? rows.slice(PR_LIMIT_BEFORE_COLLAPSE - 1) : [];

  return (
    <div className="space-y-1">
      {expandedHead.map((row) => (
        <ProviderPullRequestRow key={`${row.provider}:${row.item.id}`} row={row} />
      ))}
      {useCollapse ? (
        <div className="space-y-1">
          {expanded
            ? collapsedTail.map((row) => <ProviderPullRequestRow key={`${row.provider}:${row.item.id}`} row={row} />)
            : null}
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="block w-[calc(100%+1rem)] -mx-2 rounded-md px-2 py-1.5 text-left text-[11px] text-muted-foreground hover:bg-accent/50 hover:text-foreground transition-colors"
          >
            {expanded
              ? t(($) => $.detail.pull_request_card_show_less)
              : t(($) => $.detail.pull_request_card_show_more, { count: collapsedTail.length })}
          </button>
        </div>
      ) : null}
    </div>
  );
}

function ProviderPullRequestRow({ row }: { row: PullRequestRowEntry }) {
  return row.provider === "github" ? (
    <PullRequestRow pr={row.item} />
  ) : (
    <GitLabMergeRequestRow mr={row.item} />
  );
}

function createdAt(row: PullRequestRowEntry): string {
  return row.provider === "github" ? row.item.pr_created_at : row.item.mr_created_at;
}

function PullRequestRow({ pr }: { pr: GitHubPullRequest }) {
  const { t } = useT("issues");
  const cfg = STATE_ICON[pr.state] ?? { icon: GitPullRequest, className: "" };
  const StateIcon = cfg.icon;
  const kind = derivePullRequestStatusKind({
    state: pr.state,
    mergeable_state: pr.mergeable_state,
    checks_failed: pr.checks_failed,
    checks_pending: pr.checks_pending,
    checks_passed: pr.checks_passed,
  });
  const segments = derivePullRequestProgressSegments({
    state: pr.state,
    checks_failed: pr.checks_failed,
    checks_pending: pr.checks_pending,
    checks_passed: pr.checks_passed,
  });
  const showStats = shouldShowPullRequestStats({
    additions: pr.additions,
    deletions: pr.deletions,
    changed_files: pr.changed_files,
  });
  const statusText = useStatusText(kind);
  const draftPrefix = pr.state === "draft";
  const stateLabel = getStateLabel(pr.state, t);

  return (
    <a
      data-testid="pull-request-row"
      href={pr.html_url}
      target="_blank"
      rel="noreferrer noopener"
      className={cn(
        "flex items-start gap-2 rounded-md px-2 py-1.5 -mx-2 hover:bg-accent/50 transition-colors group",
        draftPrefix ? "opacity-80" : null,
      )}
    >
      <StateIcon className={cn("h-3.5 w-3.5 mt-0.5 shrink-0", cfg.className)} />
      <div className="min-w-0 flex-1">
        <p className="text-xs font-medium leading-snug truncate group-hover:text-foreground">
          {pr.title}
        </p>
        <p className="text-[11px] text-muted-foreground truncate">
          {pr.repo_owner}/{pr.repo_name}#{pr.number} · {stateLabel}
          {pr.author_login ? ` · @${pr.author_login}` : null}
        </p>
        <PullRequestRowDetails
          pr={pr}
          segments={segments}
          showStats={showStats}
          statusText={
            draftPrefix
              ? t(($) => $.detail.pull_request_card_draft_prefix, { status: statusText })
              : statusText
          }
          statusKind={kind}
        />
      </div>
    </a>
  );
}

function PullRequestRowDetails({
  pr,
  segments,
  showStats,
  statusText,
  statusKind,
}: {
  pr: GitHubPullRequest;
  segments: PullRequestProgressSegment[] | null;
  showStats: boolean;
  statusText: string;
  statusKind: PullRequestStatusKind;
}) {
  const { t } = useT("issues");
  const checksBadge = getChecksBadge(pr, t);
  const conflictsBadge = getConflictsBadge(pr, t);
  const isTerminal = statusKind === "closed" || statusKind === "merged";
  const showChecksBadge =
    !isTerminal &&
    !!checksBadge &&
    statusKind !== "checks_failed" &&
    statusKind !== "checks_pending" &&
    statusKind !== "checks_passed";
  const showConflictsBadge =
    !isTerminal && !!conflictsBadge && statusKind !== "conflicts" && statusKind !== "ready";

  return (
    <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground">
      {showStats ? <PullRequestStats stats={pr} /> : null}
      <PullRequestProgressStrip segments={segments} />
      <span className="truncate">{statusText}</span>
      {showChecksBadge ? <PullRequestBadge badge={checksBadge} /> : null}
      {showConflictsBadge ? <PullRequestBadge badge={conflictsBadge} /> : null}
    </div>
  );
}

function GitLabMergeRequestRow({ mr }: { mr: GitLabMergeRequest }) {
  const { t } = useT("issues");
  const cfg = STATE_ICON[mr.state] ?? { icon: GitPullRequest, className: "" };
  const StateIcon = cfg.icon;
  const counts = getGitLabPipelineCounts(mr.pipeline_status);
  const kind = deriveGitLabMergeRequestStatusKind(mr, counts);
  const segments = deriveGitLabMergeRequestProgressSegments(mr.state, counts);
  const showStats = shouldShowPullRequestStats({
    additions: mr.additions,
    deletions: mr.deletions,
    changed_files: mr.changed_files,
  });
  const statusText = getGitLabStatusText(kind, t);
  const draftPrefix = mr.state === "draft";
  const stateLabel = getStateLabel(mr.state, t);
  const branchLabel = getGitLabBranchLabel(mr);

  return (
    <a
      data-testid="pull-request-row"
      href={mr.web_url}
      target="_blank"
      rel="noreferrer noopener"
      className={cn(
        "flex items-start gap-2 rounded-md px-2 py-1.5 -mx-2 hover:bg-accent/50 transition-colors group",
        draftPrefix ? "opacity-80" : null,
      )}
    >
      <StateIcon className={cn("h-3.5 w-3.5 mt-0.5 shrink-0", cfg.className)} />
      <div className="min-w-0 flex-1">
        <p className="text-xs font-medium leading-snug truncate group-hover:text-foreground">
          {mr.title}
        </p>
        <p className="text-[11px] text-muted-foreground truncate">
          {t(($) => $.detail.merge_request_provider_gitlab)} · {mr.project_path}!{mr.iid}
          {branchLabel ? ` · ${branchLabel}` : null} · {stateLabel}
          {mr.author_username ? ` · @${mr.author_username}` : null}
        </p>
        <GitLabMergeRequestRowDetails
          mr={mr}
          segments={segments}
          showStats={showStats}
          statusText={
            draftPrefix
              ? t(($) => $.detail.pull_request_card_draft_prefix, { status: statusText })
              : statusText
          }
        />
      </div>
    </a>
  );
}

function GitLabMergeRequestRowDetails({
  mr,
  segments,
  showStats,
  statusText,
}: {
  mr: GitLabMergeRequest;
  segments: PullRequestProgressSegment[] | null;
  showStats: boolean;
  statusText: string;
}) {
  return (
    <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground">
      {showStats ? <PullRequestStats stats={mr} /> : null}
      <PullRequestProgressStrip segments={segments} />
      <span className="truncate">{statusText}</span>
    </div>
  );
}

function PullRequestStats({ stats }: { stats: { additions?: number; deletions?: number; changed_files?: number } }) {
  const { t } = useT("issues");
  return (
    <span className="inline-flex items-center gap-1.5 tabular-nums">
      <span className="text-emerald-600 dark:text-emerald-400">+{stats.additions ?? 0}</span>
      <span className="text-rose-600 dark:text-rose-400">−{stats.deletions ?? 0}</span>
      <span aria-hidden="true">·</span>
      <span>
        {t(($) => $.detail.pull_request_card_files_count, {
          count: stats.changed_files ?? 0,
        })}
      </span>
    </span>
  );
}

function PullRequestProgressStrip({
  segments,
}: {
  segments: PullRequestProgressSegment[] | null;
}) {
  if (!segments) return null;
  return (
    <span className="flex h-1 w-12 shrink-0 overflow-hidden rounded-full bg-muted" aria-hidden="true">
      {segments.map((seg) => (
        <span
          key={seg.kind}
          className={cn(
            "h-full block",
            seg.kind === "failed" && "bg-rose-500 dark:bg-rose-400",
            seg.kind === "pending" && "bg-amber-500 dark:bg-amber-400",
            seg.kind === "passed" && "bg-emerald-500 dark:bg-emerald-400",
          )}
          style={{ width: `${seg.ratio * 100}%` }}
        />
      ))}
    </span>
  );
}

interface PullRequestBadgeConfig {
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  className: string;
}

function PullRequestBadge({ badge }: { badge: PullRequestBadgeConfig }) {
  const Icon = badge.icon;
  return (
    <span className="inline-flex items-center gap-1">
      <Icon className={cn("h-3 w-3", badge.className)} />
      {badge.label}
    </span>
  );
}

function getConflictsBadge(
  pr: GitHubPullRequest,
  t: IssuesT,
): PullRequestBadgeConfig | null {
  const mergeable = pr.mergeable_state ?? null;
  return mergeable === "dirty"
    ? {
        icon: TriangleAlert,
        label: t(($) => $.detail.pull_request_conflicts_dirty),
        className: "text-rose-600 dark:text-rose-400",
      }
    : mergeable === "clean"
      ? {
          icon: CheckCircle2,
          label: t(($) => $.detail.pull_request_conflicts_clean),
          className: "text-emerald-600 dark:text-emerald-400",
        }
      : null;
}

function getChecksBadge(
  pr: GitHubPullRequest,
  t: IssuesT,
): PullRequestBadgeConfig | null {
  const checks = pr.checks_conclusion ?? null;
  return checks && CHECKS_ICON[checks]
    ? {
        icon: CHECKS_ICON[checks].icon,
        className: CHECKS_ICON[checks].className,
        label:
          checks === "passed"
            ? t(($) => $.detail.pull_request_checks_passed)
            : checks === "failed"
              ? t(($) => $.detail.pull_request_checks_failed)
              : t(($) => $.detail.pull_request_checks_pending),
      }
    : null;
}

function getStateLabel(
  state: string,
  t: IssuesT,
): string {
  return state === "open"
    ? t(($) => $.detail.pull_request_state_open)
    : state === "draft"
      ? t(($) => $.detail.pull_request_state_draft)
      : state === "merged"
        ? t(($) => $.detail.pull_request_state_merged)
        : state === "closed"
          ? t(($) => $.detail.pull_request_state_closed)
          : state;
}

function getGitLabPipelineCounts(status: GitLabPipelineStatus | string | null): {
  failed: number;
  pending: number;
  passed: number;
} {
  return {
    failed: status === "failed" ? 1 : 0,
    pending: status === "pending" ? 1 : 0,
    passed: status === "passed" ? 1 : 0,
  };
}

function deriveGitLabMergeRequestStatusKind(
  mr: GitLabMergeRequest,
  counts: { failed: number; pending: number; passed: number },
): GitLabMergeRequestStatusKind {
  if (mr.state === "closed") return "closed";
  if (mr.state === "merged") return "merged";

  const detail = mr.detailed_merge_status ?? null;
  if (mr.has_conflicts === true || (detail && GITLAB_CONFLICT_STATUSES.has(detail))) {
    return "conflicts";
  }
  if (mr.state !== "open" && mr.state !== "draft") {
    return "gitlab_unknown";
  }
  if (detail && GITLAB_BLOCKED_STATUSES.has(detail)) {
    return "gitlab_blocked";
  }
  if (detail && GITLAB_CHECKING_STATUSES.has(detail)) {
    return "gitlab_checking";
  }
  if (counts.failed > 0) return "checks_failed";
  if (counts.pending > 0) return "checks_pending";
  if (detail !== "mergeable") {
    return "gitlab_unknown";
  }
  if (counts.passed > 0) return "checks_passed";
  return "ready";
}

function getGitLabBranchLabel(mr: GitLabMergeRequest): string | null {
  if (mr.source_branch && mr.target_branch) {
    return `${mr.source_branch} -> ${mr.target_branch}`;
  }
  return mr.source_branch ?? mr.target_branch ?? null;
}

function deriveGitLabMergeRequestProgressSegments(
  state: string,
  counts: { failed: number; pending: number; passed: number },
): PullRequestProgressSegment[] | null {
  if (state === "closed" || state === "merged") return null;
  const total = counts.failed + counts.pending + counts.passed;
  if (total === 0) return null;
  const segments: PullRequestProgressSegment[] = [];
  if (counts.failed > 0) segments.push({ kind: "failed", ratio: counts.failed / total });
  if (counts.pending > 0) segments.push({ kind: "pending", ratio: counts.pending / total });
  if (counts.passed > 0) segments.push({ kind: "passed", ratio: counts.passed / total });
  return segments;
}

function getGitLabStatusText(kind: GitLabMergeRequestStatusKind, t: IssuesT): string {
  switch (kind) {
    case "gitlab_blocked":
      return t(($) => $.detail.merge_request_card_status_blocked);
    case "gitlab_checking":
      return t(($) => $.detail.merge_request_card_status_checking);
    case "gitlab_unknown":
      return t(($) => $.detail.merge_request_card_status_unknown);
    case "closed":
      return t(($) => $.detail.pull_request_card_status_closed);
    case "merged":
      return t(($) => $.detail.pull_request_card_status_merged);
    case "conflicts":
      return t(($) => $.detail.pull_request_card_status_conflicts);
    case "checks_failed":
      return t(($) => $.detail.pull_request_card_status_checks_failed);
    case "checks_pending":
      return t(($) => $.detail.pull_request_card_status_checks_pending);
    case "checks_passed":
      return t(($) => $.detail.pull_request_card_status_checks_passed);
    case "ready":
      return t(($) => $.detail.pull_request_card_status_ready);
    case "unknown":
      return t(($) => $.detail.pull_request_card_status_unknown);
  }
}

function useStatusText(kind: PullRequestStatusKind): string {
  const { t } = useT("issues");
  switch (kind) {
    case "closed":
      return t(($) => $.detail.pull_request_card_status_closed);
    case "merged":
      return t(($) => $.detail.pull_request_card_status_merged);
    case "conflicts":
      return t(($) => $.detail.pull_request_card_status_conflicts);
    case "checks_failed":
      return t(($) => $.detail.pull_request_card_status_checks_failed);
    case "checks_pending":
      return t(($) => $.detail.pull_request_card_status_checks_pending);
    case "checks_passed":
      return t(($) => $.detail.pull_request_card_status_checks_passed);
    case "ready":
      return t(($) => $.detail.pull_request_card_status_ready);
    case "unknown":
      return t(($) => $.detail.pull_request_card_status_unknown);
  }
}
