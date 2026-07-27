import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiClient, setApiInstance } from "@multica/core/api";
import { I18nProvider } from "@multica/core/i18n/react";
import type { GitHubPullRequest, GitLabMergeRequest, GitLabMergeRequestDetailsResponse } from "@multica/core/types";
import enCommon from "../../locales/en/common.json";
import enIssues from "../../locales/en/issues.json";

const TEST_RESOURCES = { en: { common: enCommon, issues: enIssues } };

vi.mock("@multica/core/github/queries", async () => {
  const actual = await vi.importActual<typeof import("@multica/core/github/queries")>(
    "@multica/core/github/queries",
  );
  return {
    ...actual,
    issuePullRequestsOptions: (issueId: string) => ({
      queryKey: ["github", "pull-requests", issueId],
      queryFn: async () => ({ pull_requests: mockPRs }),
      enabled: !!issueId,
    }),
  };
});

vi.mock("@multica/core/gitlab/queries", async () => {
  const actual = await vi.importActual<typeof import("@multica/core/gitlab/queries")>(
    "@multica/core/gitlab/queries",
  );
  return {
    ...actual,
    issueGitLabMergeRequestsOptions: (issueId: string) => ({
      queryKey: ["gitlab", "merge-requests", issueId],
      queryFn: async () => ({ merge_requests: mockMRs }),
      enabled: !!issueId,
    }),
    gitlabMergeRequestDetailsOptions: (issueId: string, mrId: string) => ({
      queryKey: ["gitlab", "merge-request-details", issueId, mrId],
      queryFn: async () => mockDetails,
      enabled: !!issueId && !!mrId,
    }),
  };
});

import { PullRequestList } from "./pull-request-list";

let mockPRs: GitHubPullRequest[] = [];
let mockMRs: GitLabMergeRequest[] = [];
let mockDetails: GitLabMergeRequestDetailsResponse;

function makePR(overrides: Partial<GitHubPullRequest> = {}): GitHubPullRequest {
  return {
    id: "pr-1",
    provider: "github",
    workspace_id: "ws-1",
    repo_owner: "acme",
    repo_name: "widget",
    number: 1,
    title: "Test PR",
    state: "open",
    html_url: "https://example.test/pr/1",
    branch: "feat/x",
    author_login: "octocat",
    author_avatar_url: null,
    merged_at: null,
    closed_at: null,
    pr_created_at: "2026-01-01T00:00:00Z",
    pr_updated_at: "2026-01-01T00:00:00Z",
    mergeable: null,
    merge_state_status: null,
    snapshot_available: true,
    checks_rollup: null,
    checks_total: 0,
    checks_passed: 0,
    checks_failed: 0,
    checks_running: 0,
    failed_check_names: [],
    snapshot_stale: false,
    snapshot_fetched_at: null,
    additions: 0,
    deletions: 0,
    changed_files: 0,
    ...overrides,
  };
}

function makeMR(overrides: Partial<GitLabMergeRequest> = {}): GitLabMergeRequest {
  return {
    id: "mr-1",
    workspace_id: "ws-1",
    project_path: "group/repo",
    gitlab_project_id: 101,
    iid: 7,
    title: "Test MR",
    state: "open",
    web_url: "https://gitlab.example.test/group/repo/-/merge_requests/7",
    source_branch: "feat/y",
    target_branch: "main",
    author_username: "alice",
    author_avatar_url: null,
    sha: "abc123",
    detailed_merge_status: null,
    has_conflicts: null,
    pipeline_status: null,
    pipeline_url: null,
    additions: 0,
    deletions: 0,
    changed_files: 0,
    merged_at: null,
    closed_at: null,
    mr_created_at: "2026-01-02T00:00:00Z",
    mr_updated_at: "2026-01-02T00:00:00Z",
    ...overrides,
  };
}

function renderList(issueId = "issue-1") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const result = render(
    <QueryClientProvider client={qc}>
      <I18nProvider resources={TEST_RESOURCES} locale="en">
        <PullRequestList issueId={issueId} />
      </I18nProvider>
    </QueryClientProvider>,
  );
  return { ...result, queryClient: qc };
}

async function waitForRender() {
  return screen.findAllByRole("link");
}

describe("PullRequestList sidebar rows", () => {
  beforeEach(() => {
    setApiInstance(new ApiClient("https://api.example.test"));
    mockPRs = [];
    mockMRs = [];
    mockDetails = {
      merge_request: makeMR(),
      approval: null,
      discussions: [],
      jobs: [],
    };
  });

  it("uses the sidebar list-row surface instead of a card surface", async () => {
    mockPRs = [makePR({ title: "Visual row" })];
    renderList();
    await waitForRender();
    const row = screen.getByTestId("pull-request-row");
    expect(row).toHaveClass("rounded-md", "-mx-2", "hover:bg-accent/50");
    expect(row).not.toHaveClass("rounded-lg", "border", "bg-card");
  });

  // --- CI status element ---------------------------------------------------

  it("renders all-checks-passed only when the rollup is success", async () => {
    mockPRs = [makePR({ checks_rollup: "success", checks_total: 7 })];
    renderList();
    await waitForRender();
    expect(screen.getByText("All checks passed (7/7)")).toBeInTheDocument();
  });

  it("renders 'No checks yet' when the rollup is absent — never passed", async () => {
    // Acceptance criterion 5: absent snapshot must not read as a green build.
    mockPRs = [makePR({ checks_rollup: null, checks_passed: 5, checks_total: 5 })];
    renderList();
    await waitForRender();
    expect(screen.getByText("No checks yet")).toBeInTheDocument();
    expect(screen.queryByText(/All checks passed/)).not.toBeInTheDocument();
  });

  it("hides snapshot status when the GitHub App key is unavailable, even with old data", async () => {
    mockPRs = [
      makePR({
        snapshot_available: false,
        checks_rollup: "failure",
        checks_conclusion: "failed",
        checks_total: 2,
        checks_failed: 2,
        mergeable: "conflicting",
        merge_state_status: "dirty",
      }),
    ];
    renderList();
    await waitForRender();
    expect(screen.queryByText(/failed/)).not.toBeInTheDocument();
    expect(screen.queryByText("No checks yet")).not.toBeInTheDocument();
    expect(screen.queryByText("Has merge conflicts")).not.toBeInTheDocument();
  });

  it("renders failed count with the first failing check names", async () => {
    mockPRs = [
      makePR({
        checks_rollup: "failure",
        checks_total: 7,
        checks_failed: 2,
        failed_check_names: ["backend", "e2e"],
      }),
    ];
    renderList();
    await waitForRender();
    const badge = screen.getByText(/2\/7 failed/);
    expect(badge).toHaveTextContent("2/7 failed");
    expect(badge).toHaveTextContent("backend, e2e");
  });

  it("truncates the failing names to two and appends a +N more count", async () => {
    mockPRs = [
      makePR({
        checks_rollup: "failure",
        checks_total: 7,
        checks_failed: 4,
        failed_check_names: ["a", "b", "c", "d"],
      }),
    ];
    renderList();
    await waitForRender();
    const badge = screen.getByText(/4\/7 failed/);
    expect(badge).toHaveTextContent("4/7 failed");
    expect(badge).toHaveTextContent("a, b, +2 more");
  });

  it("renders the running count when the rollup is pending", async () => {
    mockPRs = [
      makePR({ checks_rollup: "pending", checks_total: 7, checks_passed: 5, checks_running: 2 }),
    ];
    renderList();
    await waitForRender();
    const badge = screen.getByText(/2 running/);
    expect(badge).toHaveTextContent("5/7");
    expect(badge).toHaveTextContent("2 running");
  });

  it.each([
    ["forgejo", "passed", "All checks passed (3/3)"],
    ["gitea", "pending", "2/3 · 1 running"],
    ["gitlab", "failed", "1/3 failed"],
  ] as const)(
    "preserves %s legacy %s check status",
    async (provider, conclusion, expected) => {
      mockPRs = [
        makePR({
          provider,
          snapshot_available: undefined,
          checks_rollup: undefined,
          checks_conclusion: conclusion,
          checks_total: 3,
          checks_passed: conclusion === "passed" ? 3 : 2,
          checks_failed: conclusion === "failed" ? 1 : 0,
          checks_running: conclusion === "pending" ? 1 : 0,
          checks_pending: conclusion === "pending" ? 1 : 0,
        }),
      ];
      renderList();
      await waitForRender();
      expect(screen.getByText(expected, { exact: false })).toBeInTheDocument();
    },
  );

  // --- Mergeability element ------------------------------------------------

  it("renders 'Ready to merge' only when the merge state is clean", async () => {
    mockPRs = [makePR({ merge_state_status: "clean" })];
    renderList();
    await waitForRender();
    expect(screen.getByText("Ready to merge")).toBeInTheDocument();
  });

  it("never infers 'Ready to merge' from mergeable alone", async () => {
    // Acceptance criterion 8: mergeable without a clean state shows neither.
    mockPRs = [makePR({ mergeable: "mergeable", merge_state_status: null })];
    renderList();
    await waitForRender();
    expect(screen.queryByText("Ready to merge")).not.toBeInTheDocument();
    expect(screen.queryByText("Has merge conflicts")).not.toBeInTheDocument();
  });

  it("renders 'Has merge conflicts' when mergeable is conflicting", async () => {
    mockPRs = [makePR({ mergeable: "conflicting" })];
    renderList();
    await waitForRender();
    expect(screen.getByText("Has merge conflicts")).toBeInTheDocument();
  });

  it("shows neither conflict nor ready when the merge verdict is unknown", async () => {
    // Acceptance criterion 5: unknown mergeability shows neither element.
    mockPRs = [makePR({ mergeable: "unknown", merge_state_status: "unknown" })];
    renderList();
    await waitForRender();
    expect(screen.queryByText("Has merge conflicts")).not.toBeInTheDocument();
    expect(screen.queryByText("Ready to merge")).not.toBeInTheDocument();
  });

  // --- The two elements are independent ------------------------------------

  it("shows a failed CI element and a conflict element together", async () => {
    mockPRs = [
      makePR({
        checks_rollup: "failure",
        checks_total: 7,
        checks_failed: 2,
        failed_check_names: ["backend"],
        mergeable: "conflicting",
      }),
    ];
    renderList();
    await waitForRender();
    expect(screen.getByText(/2\/7 failed/)).toHaveTextContent("2/7 failed");
    expect(screen.getByText("Has merge conflicts")).toBeInTheDocument();
  });

  // --- Terminal PRs suppress both elements ---------------------------------

  it("shows neither status element for merged PRs", async () => {
    mockPRs = [
      makePR({
        state: "merged",
        checks_rollup: "failure",
        checks_failed: 5,
        checks_total: 5,
        mergeable: "conflicting",
      }),
    ];
    renderList();
    await waitForRender();
    expect(screen.queryByText(/failed/)).not.toBeInTheDocument();
    expect(screen.queryByText("Has merge conflicts")).not.toBeInTheDocument();
    expect(screen.queryByText("No checks yet")).not.toBeInTheDocument();
  });

  it("shows neither status element for closed PRs", async () => {
    mockPRs = [
      makePR({
        state: "closed",
        checks_rollup: "success",
        checks_passed: 3,
        checks_total: 3,
        merge_state_status: "clean",
      }),
    ];
    renderList();
    await waitForRender();
    expect(screen.queryByText(/All checks passed/)).not.toBeInTheDocument();
    expect(screen.queryByText("Ready to merge")).not.toBeInTheDocument();
    expect(screen.queryByText("No checks yet")).not.toBeInTheDocument();
  });

  // --- Stale snapshot ------------------------------------------------------

  it("greys out the status elements and annotates the age when the snapshot is stale", async () => {
    mockPRs = [
      makePR({
        checks_rollup: "success",
        checks_total: 3,
        snapshot_stale: true,
        snapshot_fetched_at: "2026-01-01T00:00:00Z",
      }),
    ];
    renderList();
    await waitForRender();
    const badge = screen.getByText("All checks passed (3/3)");
    expect(badge).toHaveClass("opacity-60");
    expect(badge).toHaveAttribute("title");
    expect(badge.getAttribute("title")).toBeTruthy();
  });

  // --- Diff stats ----------------------------------------------------------

  it("hides stats row when all stats are 0 (legacy backend)", async () => {
    mockPRs = [makePR()];
    renderList();
    await waitForRender();
    expect(screen.queryByText(/files?$/)).not.toBeInTheDocument();
    expect(screen.queryByText(/^\+0/)).not.toBeInTheDocument();
  });

  it("shows stats row with additions / deletions / file count when present", async () => {
    mockPRs = [makePR({ additions: 437, deletions: 6, changed_files: 6 })];
    renderList();
    await waitForRender();
    expect(screen.getByText("+437")).toBeInTheDocument();
    expect(screen.getByText("−6")).toBeInTheDocument();
    expect(screen.getByText("6 files")).toBeInTheDocument();
  });

  it("uses singular file copy when changed_files=1", async () => {
    mockPRs = [makePR({ additions: 1, changed_files: 1 })];
    renderList();
    await waitForRender();
    expect(screen.getByText("1 file")).toBeInTheDocument();
  });

  it("renders GitHub PRs and GitLab MRs in the same sidebar list", async () => {
    mockPRs = [makePR({ title: "GitHub fix" })];
    mockMRs = [
      makeMR({
        title: "GitLab feature",
        source_branch: "feat/gitlab",
        target_branch: "main",
      }),
    ];
    renderList();
    await waitForRender();
    expect(screen.getByText("GitHub fix")).toBeInTheDocument();
    expect(screen.getByText("GitLab feature")).toBeInTheDocument();
    expect(screen.getByText(/group\/repo!7/)).toBeInTheDocument();
    const gitlabRow = screen.getByText("GitLab feature").closest('[data-testid="pull-request-row"]');
    expect(gitlabRow).toHaveTextContent("GitLab");
    expect(gitlabRow).toHaveTextContent("feat/gitlab -> main");
  });

  it("sorts mixed GitHub PR and GitLab MR rows by created time descending", async () => {
    mockPRs = [makePR({ title: "Older GitHub PR", pr_created_at: "2026-01-01T00:00:00Z" })];
    mockMRs = [makeMR({ title: "Newer GitLab MR", mr_created_at: "2026-01-03T00:00:00Z" })];
    renderList();
    const rows = await screen.findAllByTestId("pull-request-row");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("Newer GitLab MR");
    expect(rows[1]).toHaveTextContent("Older GitHub PR");
  });

  it("renders blocked GitLab merge status before a passed pipeline", async () => {
    mockMRs = [
      makeMR({
        title: "Blocked MR",
        detailed_merge_status: "not_approved",
        pipeline_status: "passed",
      }),
    ];
    renderList();
    await waitForRender();
    expect(screen.getByText("Merge blocked")).toBeInTheDocument();
    expect(screen.queryByText("All checks passed")).not.toBeInTheDocument();
  });

  it("renders unknown GitLab status safely instead of passed pipeline text", async () => {
    mockMRs = [
      makeMR({
        title: "Unknown MR",
        state: "locked" as GitLabMergeRequest["state"],
        detailed_merge_status: "unexpected_status",
        pipeline_status: "passed",
      }),
    ];
    renderList();
    await waitForRender();
    expect(screen.getByText("Merge status unknown")).toBeInTheDocument();
    expect(screen.queryByText("All checks passed")).not.toBeInTheDocument();
  });

  it("renders failed GitLab pipeline when merge status is missing", async () => {
    mockMRs = [
      makeMR({
        title: "Failed pipeline MR",
        detailed_merge_status: null,
        pipeline_status: "failed",
      }),
    ];
    renderList();
    await waitForRender();
    expect(screen.getByText("Some checks failed")).toBeInTheDocument();
    expect(screen.queryByText("Merge status unknown")).not.toBeInTheDocument();
  });

  it("expands GitLab merge request details", async () => {
    mockMRs = [makeMR({ title: "GitLab detail MR" })];
    mockDetails = {
      merge_request: mockMRs[0]!,
      approval: {
        approved: false,
        approvals_left: 1,
        approved_by: [],
        rules: [],
      },
      jobs: [{
        id: "job-1",
        pipeline_id: 77,
        job_id: 9001,
        name: "test",
        status: "failed",
        allow_failure: false,
        artifacts_file_name: "artifacts.zip",
        trace_summary: "assert failed",
      }],
      discussions: [{
        id: "discussion-1",
        gitlab_discussion_id: "abc",
        individual_note: false,
        resolved: false,
        notes: [{
          id: "note-1",
          gitlab_note_id: 501,
          body: "Please fix this",
          system: false,
          author_username: "grace",
        }],
      }],
    };
    renderList();

    await screen.findByText("GitLab detail MR");
    fireEvent.click(screen.getByRole("button", { name: "GitLab details" }));

    expect(await screen.findByText("1 approval remaining")).toBeInTheDocument();
    expect(screen.getByText("assert failed")).toBeInTheDocument();
    expect(screen.getByText("artifacts.zip")).toBeInTheDocument();
    expect(screen.getByText(/Please fix this/)).toBeInTheDocument();
  });

  it("renders pending GitLab pipeline when merge status is unknown", async () => {
    mockMRs = [
      makeMR({
        title: "Pending pipeline MR",
        detailed_merge_status: "future_status",
        pipeline_status: "pending",
      }),
    ];
    renderList();
    await waitForRender();
    expect(screen.getByText("Some checks haven't completed yet")).toBeInTheDocument();
    expect(screen.queryByText("Merge status unknown")).not.toBeInTheDocument();
  });

  // --- Collapse behaviour --------------------------------------------------

  it("collapses extra PR rows past the visible limit behind Show more toggle", async () => {
    mockPRs = [
      makePR({ id: "a", number: 1, title: "PR-A" }),
      makePR({ id: "b", number: 2, title: "PR-B" }),
      makePR({ id: "c", number: 3, title: "PR-C" }),
      makePR({ id: "d", number: 4, title: "PR-D" }),
      makePR({ id: "e", number: 5, title: "PR-E" }),
    ];
    renderList();
    await waitForRender();
    expect(screen.getByText("PR-A")).toBeInTheDocument();
    expect(screen.getByText("PR-B")).toBeInTheDocument();
    expect(screen.getByText("PR-C")).toBeInTheDocument();
    expect(screen.queryByText("PR-D")).not.toBeInTheDocument();
    expect(screen.queryByText("PR-E")).not.toBeInTheDocument();
    expect(screen.getByText("Show 2 more")).toBeInTheDocument();
  });

  it("collapses to 3 rows + hidden tail when count == threshold", async () => {
    mockPRs = [
      makePR({ id: "a", number: 1, title: "PR-A" }),
      makePR({ id: "b", number: 2, title: "PR-B" }),
      makePR({ id: "c", number: 3, title: "PR-C" }),
      makePR({ id: "d", number: 4, title: "PR-D" }),
    ];
    renderList();
    await waitForRender();
    expect(screen.getByText("PR-A")).toBeInTheDocument();
    expect(screen.getByText("PR-B")).toBeInTheDocument();
    expect(screen.getByText("PR-C")).toBeInTheDocument();
    expect(screen.queryByText("PR-D")).not.toBeInTheDocument();
    expect(screen.getByText("Show 1 more")).toBeInTheDocument();
  });

  it("resets expanded mixed rows when switching issues", async () => {
    mockPRs = [
      makePR({ id: "a-pr-1", number: 1, title: "Issue A PR 1" }),
      makePR({ id: "a-pr-2", number: 2, title: "Issue A PR 2" }),
    ];
    mockMRs = [
      makeMR({ id: "a-mr-1", iid: 1, title: "Issue A MR 1" }),
      makeMR({ id: "a-mr-2", iid: 2, title: "Issue A MR 2" }),
    ];
    const view = renderList("issue-a");
    await waitForRender();
    fireEvent.click(screen.getByText("Show 1 more"));
    expect(screen.getAllByTestId("pull-request-row")).toHaveLength(4);

    mockPRs = [
      makePR({ id: "b-pr-1", number: 1, title: "Issue B PR 1" }),
      makePR({ id: "b-pr-2", number: 2, title: "Issue B PR 2" }),
    ];
    mockMRs = [
      makeMR({ id: "b-mr-1", iid: 1, title: "Issue B MR 1" }),
      makeMR({ id: "b-mr-2", iid: 2, title: "Issue B MR 2" }),
    ];
    view.rerender(
      <QueryClientProvider client={view.queryClient}>
        <I18nProvider resources={TEST_RESOURCES} locale="en">
          <PullRequestList issueId="issue-b" />
        </I18nProvider>
      </QueryClientProvider>,
    );
    await screen.findByText("Issue B MR 1");
    expect(screen.getAllByTestId("pull-request-row")).toHaveLength(3);
    expect(screen.getByText("Show 1 more")).toBeInTheDocument();
  });
});
