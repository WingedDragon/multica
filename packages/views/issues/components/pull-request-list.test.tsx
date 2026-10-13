import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type {
  GitHubPullRequest,
  GitLabMergeRequest,
  GitLabMergeRequestDetailsResponse,
  PRAutoComplete,
} from "@multica/core/types";
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
      queryFn: async () => ({ pull_requests: mockPRs, auto_complete: mockAutoComplete }),
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

const apiMock = vi.hoisted(() => ({
  unlinkIssuePullRequest: vi.fn(),
  linkIssuePullRequest: vi.fn(),
  setIssuePRAutoComplete: vi.fn(),
  gitLabJobArtifactsURL: (issueId: string, jobId: string) =>
    `https://api.example.test/api/issues/${issueId}/gitlab/jobs/${jobId}/artifacts`,
}));
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: apiMock,
}));
const toastMock = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }));
vi.mock("sonner", () => ({ toast: toastMock }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useWorkspacePaths: () => ({ settings: () => "/acme/settings" }),
  useCurrentWorkspace: () => ({ id: "ws-1", slug: "acme", settings: {} }),
}));
vi.mock("../../navigation", () => ({
  AppLink: ({ href, children, className }: { href: string; children: React.ReactNode; className?: string }) => (
    <a href={href} className={className}>{children}</a>
  ),
}));

import { PullRequestList } from "./pull-request-list";

let mockPRs: GitHubPullRequest[] = [];
let mockAutoComplete: PRAutoComplete | null = null;
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

function renderList(issueId = "issue-1", includeGitHub = true) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const result = render(
    <QueryClientProvider client={qc}>
      <I18nProvider resources={TEST_RESOURCES} locale="en">
        <PullRequestList issueId={issueId} identifier="MUL-1" includeGitHub={includeGitHub} />
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
    mockAutoComplete = null;
    mockMRs = [];
    mockDetails = { merge_request: makeMR(), approval: null, discussions: [], jobs: [] };
  });

  const verdict = () => screen.getByTestId("pull-request-verdict");

  // --- One verdict per PR --------------------------------------------------

  it("leads a failed PR with the count and lists what failed under it", async () => {
    mockPRs = [
      makePR({
        checks_rollup: "failure",
        checks_total: 7,
        checks_failed: 2,
        failed_check_names: ["backend", "e2e"],
        merge_state_status: "unstable",
      }),
    ];
    renderList();
    await waitForRender();
    expect(verdict()).toHaveTextContent("2/7 failed");
    expect(screen.getByText("backend")).toBeInTheDocument();
    expect(screen.getByText("e2e")).toBeInTheDocument();
    // "Unstable" only restates the failing checks.
    expect(screen.queryByText("Unstable")).not.toBeInTheDocument();
  });

  it("still shows a conflict when the checks also failed", async () => {
    mockPRs = [
      makePR({
        checks_rollup: "failure",
        checks_total: 7,
        checks_failed: 1,
        failed_check_names: ["backend"],
        mergeable: "conflicting",
      }),
    ];
    renderList();
    await waitForRender();
    expect(verdict()).toHaveTextContent("1/7 failed");
    expect(screen.getByText("Has merge conflicts")).toBeInTheDocument();
    expect(screen.getByText("backend")).toBeInTheDocument();
  });

  it("folds problems past three into a +N more line", async () => {
    mockPRs = [
      makePR({
        checks_rollup: "failure",
        checks_total: 7,
        checks_failed: 5,
        failed_check_names: ["a", "b", "c", "d", "e"],
      }),
    ];
    renderList();
    await waitForRender();
    expect(screen.getByText("c")).toBeInTheDocument();
    expect(screen.queryByText("d")).not.toBeInTheDocument();
    expect(screen.getByText("+2 more")).toBeInTheDocument();
  });

  it("shows running checks as progress, with the full count on hover", async () => {
    mockPRs = [makePR({ checks_rollup: "pending", checks_total: 7, checks_passed: 5, checks_running: 2 })];
    renderList();
    await waitForRender();
    expect(verdict()).toHaveTextContent("5/7");
    expect(verdict()).toHaveAttribute("title", "5/7 · 2 running");
    expect(verdict().querySelector("svg")).toHaveClass("motion-safe:animate-spin");
  });

  it("stops the running spinner when the snapshot is stale", async () => {
    mockPRs = [
      makePR({
        checks_rollup: "pending",
        checks_total: 7,
        checks_passed: 5,
        checks_running: 2,
        snapshot_stale: true,
        snapshot_fetched_at: new Date(Date.now() - 2 * 3_600_000).toISOString(),
      }),
    ];
    renderList();
    await waitForRender();
    expect(verdict()).toHaveTextContent("5/7");
    expect(verdict().querySelector("svg")).not.toHaveClass("motion-safe:animate-spin");
  });

  it("calls a PR ready only when the merge state is clean", async () => {
    mockPRs = [makePR({ checks_rollup: "success", checks_total: 3, merge_state_status: "clean" })];
    renderList();
    await waitForRender();
    expect(verdict()).toHaveTextContent("Ready");
    expect(verdict()).toHaveAttribute("title", "Ready to merge");
  });

  it("never infers ready from mergeable alone", async () => {
    // Acceptance criterion 8: mergeable without a clean state is not "ready".
    mockPRs = [makePR({ checks_rollup: "success", checks_total: 3, mergeable: "mergeable", merge_state_status: null })];
    renderList();
    await waitForRender();
    expect(verdict()).toHaveTextContent("Checks passed");
    expect(verdict()).toHaveAttribute("title", "All checks passed (3/3)");
  });

  it("renders 'No checks yet' when the rollup is absent — never passed", async () => {
    // Acceptance criterion 5: absent snapshot must not read as a green build.
    mockPRs = [makePR({ checks_rollup: null, checks_passed: 5, checks_total: 5 })];
    renderList();
    await waitForRender();
    expect(verdict()).toHaveTextContent("No checks yet");
  });

  it("keeps a draft a draft even when GitHub calls it clean", async () => {
    mockPRs = [makePR({ state: "draft", checks_rollup: "success", merge_state_status: "clean" })];
    renderList();
    await waitForRender();
    expect(verdict()).toHaveTextContent("Draft");
  });

  it("says nothing about CI or merging when the snapshot is unavailable, even with old data", async () => {
    mockPRs = [
      makePR({
        snapshot_available: false,
        checks_rollup: "failure",
        checks_conclusion: "failed",
        checks_total: 2,
        checks_failed: 2,
        failed_check_names: ["backend"],
        mergeable: "conflicting",
        merge_state_status: "dirty",
      }),
    ];
    renderList();
    await waitForRender();
    expect(screen.queryByTestId("pull-request-verdict")).not.toBeInTheDocument();
    expect(screen.queryByText("backend")).not.toBeInTheDocument();
  });

  it.each([
    ["forgejo", "passed", "Checks passed"],
    ["gitea", "pending", "2/3"],
    ["gitlab", "failed", "1/3 failed"],
  ] as const)("preserves %s legacy %s check status", async (provider, conclusion, expected) => {
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
    expect(verdict()).toHaveTextContent(expected);
  });

  // --- Terminal PRs --------------------------------------------------------

  it("ends a merged PR at Merged, whatever its last snapshot said", async () => {
    mockPRs = [
      makePR({
        state: "merged",
        merged_at: new Date(Date.now() - 2 * 86_400_000).toISOString(),
        checks_rollup: "failure",
        checks_failed: 5,
        checks_total: 5,
        failed_check_names: ["backend"],
        mergeable: "conflicting",
      }),
    ];
    renderList();
    await waitForRender();
    expect(verdict()).toHaveTextContent("Merged");
    expect(screen.getByText("2d ago")).toBeInTheDocument();
    expect(screen.queryByText("backend")).not.toBeInTheDocument();
    expect(screen.queryByText("Has merge conflicts")).not.toBeInTheDocument();
  });

  it("ends a closed PR at Closed", async () => {
    mockPRs = [makePR({ state: "closed", checks_rollup: "success", merge_state_status: "clean" })];
    renderList();
    await waitForRender();
    expect(verdict()).toHaveTextContent("Closed");
  });

  // --- Stale snapshot ------------------------------------------------------

  it("greys out the verdict and shows the snapshot age when the snapshot is stale", async () => {
    mockPRs = [
      makePR({
        checks_rollup: "success",
        checks_total: 3,
        additions: 10,
        snapshot_stale: true,
        snapshot_fetched_at: new Date(Date.now() - 2 * 3_600_000).toISOString(),
      }),
    ];
    renderList();
    await waitForRender();
    expect(verdict()).toHaveClass("opacity-60");
    expect(verdict()).toHaveAttribute("title", "Updated 2h ago");
    expect(screen.getByText("2h ago")).toBeInTheDocument();
  });

  // --- Title and meta line -------------------------------------------------

  it("drops this issue's key from the PR title and keeps other keys", async () => {
    mockPRs = [
      makePR({ id: "a", number: 1, title: "MUL-1: regroup settings" }),
      makePR({ id: "b", number: 2, title: "MUL-2: unrelated work" }),
    ];
    renderList();
    await waitForRender();
    expect(screen.getByText("regroup settings")).toBeInTheDocument();
    expect(screen.getByText("MUL-2: unrelated work")).toBeInTheDocument();
  });

  it("shows only the number when every PR is in one repo", async () => {
    mockPRs = [makePR({ id: "a", number: 42 }), makePR({ id: "b", number: 43 })];
    renderList();
    await waitForRender();
    expect(screen.getByText("#42")).toBeInTheDocument();
    expect(screen.getByText("#43")).toBeInTheDocument();
    expect(screen.queryByText("widget#42")).not.toBeInTheDocument();
  });

  it("shows the repo and number without the owner when the PRs span repos", async () => {
    mockPRs = [
      makePR({ id: "a", number: 42 }),
      makePR({ id: "b", number: 7, repo_name: "gadget" }),
    ];
    renderList();
    await waitForRender();
    expect(screen.getByText("widget#42")).toBeInTheDocument();
    expect(screen.getByText("gadget#7")).toBeInTheDocument();
  });

  it("hides the diff size when all stats are 0 (legacy backend)", async () => {
    mockPRs = [makePR()];
    renderList();
    await waitForRender();
    expect(screen.queryByText(/^\+0/)).not.toBeInTheDocument();
  });

  it("shortens the diff size to thousands", async () => {
    mockPRs = [makePR({ additions: 6743, deletions: 6, changed_files: 119 })];
    renderList();
    await waitForRender();
    expect(screen.getByText("+6.7k")).toBeInTheDocument();
    expect(screen.getByText("−6")).toBeInTheDocument();
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

  // --- GitLab merge requests share the list --------------------------------

  it("uses the sidebar list-row surface instead of a card surface", async () => {
    mockPRs = [makePR({ title: "Visual row" })];
    renderList();
    await waitForRender();
    const row = screen.getByTestId("pull-request-row");
    expect(row).toHaveClass("rounded-md");
    expect(row).not.toHaveClass("rounded-lg", "border", "bg-card");
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

  it("lists only GitLab MRs when the workspace turns the GitHub PR sidebar off", async () => {
    mockPRs = [makePR({ title: "Hidden GitHub PR" })];
    mockMRs = [makeMR({ title: "Visible GitLab MR" })];
    renderList("issue-1", false);
    await screen.findByText("Visible GitLab MR");
    expect(screen.queryByText("Hidden GitHub PR")).not.toBeInTheDocument();
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
          <PullRequestList issueId="issue-b" identifier="MUL-1" />
        </I18nProvider>
      </QueryClientProvider>,
    );
    await screen.findByText("Issue B MR 1");
    expect(screen.getAllByTestId("pull-request-row")).toHaveLength(3);
    expect(screen.getByText("Show 1 more")).toBeInTheDocument();
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
});

// MUL-7429 / MUL-7726: the line under the list says what merging will do to the
// issue's status, straight from the server's decision, and each row can be
// removed.
describe("PullRequestList auto-complete", () => {
  beforeEach(() => {
    mockAutoComplete = null;
    mockMRs = [];
    apiMock.unlinkIssuePullRequest.mockReset();
    apiMock.linkIssuePullRequest.mockReset();
    apiMock.setIssuePRAutoComplete.mockReset();
    toastMock.success.mockReset();
  });

  const decision = (state: string, ids: string[] = [], extra: Partial<PRAutoComplete> = {}): PRAutoComplete => ({
    state,
    pull_request_ids: ids,
    issue_disabled: false,
    workspace_enabled: true,
    target_status: "done",
    ...extra,
  });

  it("names the PR the issue is waiting on and the status it moves to", async () => {
    mockPRs = [makePR({ id: "a", number: 12, state: "merged" }), makePR({ id: "b", number: 19 })];
    mockAutoComplete = decision("waiting", ["b"], { target_status: "in_review" });
    renderList();
    expect(await screen.findByTestId("pr-auto-complete-line")).toHaveTextContent("Moves to In Review when #19 merges");
  });

  it("falls back to Done on a backend that does not name the target", async () => {
    mockPRs = [makePR({ id: "b", number: 19 })];
    mockAutoComplete = decision("waiting", ["b"], { target_status: undefined });
    renderList();
    expect(await screen.findByTestId("pr-auto-complete-line")).toHaveTextContent("Moves to Done when #19 merges");
  });

  it("offers to remove a PR that closed without merging", async () => {
    mockPRs = [makePR({ id: "a", number: 12, state: "merged" }), makePR({ id: "b", number: 19, state: "closed" })];
    mockAutoComplete = decision("not_merged", ["b"]);
    apiMock.unlinkIssuePullRequest.mockResolvedValue({ pull_requests: [], auto_complete: decision("terminal") });
    renderList();
    const line = await screen.findByTestId("pr-auto-complete-line");
    expect(line).toHaveTextContent("#19 closed without merging");
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(apiMock.unlinkIssuePullRequest).toHaveBeenCalledWith("issue-1", "b"));
    // Removing the last unmerged PR moved the issue; the toast says so and
    // offers no undo.
    await waitFor(() =>
      expect(toastMock.success).toHaveBeenCalledWith("Removed #19. Every remaining PR is merged, so the issue moved to Done."),
    );
  });

  // The workspace chose to leave status alone: a team choice, not repeated on
  // every issue. An older backend's no_close_intent says nothing either.
  it.each(["workspace_disabled", "no_close_intent", "at_target", "terminal"])("says nothing for %s", async (state) => {
    mockPRs = [makePR({ id: "a", number: 12, state: "merged" })];
    mockAutoComplete = decision(state, [], state === "workspace_disabled" ? { workspace_enabled: false, target_status: "none" } : {});
    renderList();
    await waitForRender();
    expect(screen.queryByTestId("pr-auto-complete-line")).toBeNull();
  });

  it("turns the merge status change back on for the issue from the line", async () => {
    mockPRs = [makePR({ id: "a", number: 12 })];
    mockAutoComplete = decision("issue_disabled", [], { issue_disabled: true });
    apiMock.setIssuePRAutoComplete.mockResolvedValue({ pull_requests: mockPRs, auto_complete: decision("waiting", ["a"]) });
    renderList();
    expect(await screen.findByTestId("pr-auto-complete-line")).toHaveTextContent("PR merges won’t change this issue");
    fireEvent.click(screen.getByRole("button", { name: "Turn on" }));
    await waitFor(() => expect(apiMock.setIssuePRAutoComplete).toHaveBeenCalledWith("issue-1", false));
  });

  it("hides row actions and the line on a backend without auto-complete", async () => {
    mockPRs = [makePR({ id: "a", number: 12 })];
    mockAutoComplete = null;
    renderList();
    await waitForRender();
    expect(screen.queryByRole("button", { name: "Pull request actions" })).toBeNull();
    expect(screen.queryByTestId("pr-auto-complete-line")).toBeNull();
  });

  it("explains how a PR was linked in its menu", async () => {
    mockPRs = [makePR({ id: "a", number: 12, link_source: "title" })];
    mockAutoComplete = decision("waiting", ["a"]);
    renderList();
    fireEvent.click(await screen.findByRole("button", { name: "Pull request actions" }));
    expect(await screen.findByText("Linked by MUL-1 in the title")).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Remove from issue" })).toBeInTheDocument();
  });
});
