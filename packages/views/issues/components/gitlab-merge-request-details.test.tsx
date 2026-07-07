import { useState, type ReactNode } from "react";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient, setApiInstance } from "@multica/core/api";
import { I18nProvider } from "@multica/core/i18n/react";
import type { GitLabMergeRequestDetailsResponse } from "@multica/core/types";
import enCommon from "../../locales/en/common.json";
import enIssues from "../../locales/en/issues.json";
import { GitLabMergeRequestDetails } from "./gitlab-merge-request-details";

const TEST_RESOURCES = { en: { common: enCommon, issues: enIssues } };

function TestWrapper({ children }: { children: ReactNode }) {
  const [queryClient] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: { retry: false },
          mutations: { retry: false },
        },
      }),
  );

  return (
    <QueryClientProvider client={queryClient}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        {children}
      </I18nProvider>
    </QueryClientProvider>
  );
}

function makeDetails(): GitLabMergeRequestDetailsResponse {
  return {
    merge_request: {
      id: "mr-1",
      workspace_id: "ws-1",
      project_path: "group/repo",
      gitlab_project_id: 42,
      iid: 7,
      title: "Fix issue",
      state: "open",
      web_url: "https://gitlab.example.test/group/repo/-/merge_requests/7",
      source_branch: "fix",
      target_branch: "main",
      author_username: "alice",
      author_avatar_url: null,
      sha: "abc",
      detailed_merge_status: "mergeable",
      has_conflicts: false,
      pipeline_status: "failed",
      pipeline_url: null,
      additions: 2,
      deletions: 1,
      changed_files: 1,
      merged_at: null,
      closed_at: null,
      mr_created_at: "2026-07-06T00:00:00Z",
      mr_updated_at: "2026-07-06T00:01:00Z",
    },
    approval: {
      approved: false,
      approvals_required: 2,
      approvals_left: 1,
      approved_by: [{ username: "grace", name: "Grace Hopper" }],
      rules: [
        {
          id: 1,
          name: "Maintainers",
          approved: false,
          approvals_required: 2,
          approved_by: [{ username: "grace", name: "Grace Hopper" }],
        },
      ],
      fetched_at: "2026-07-06T00:02:00Z",
    },
    jobs: [
      {
        id: "job-1",
        job_id: 9,
        pipeline_id: 77,
        name: "test",
        status: "failed",
        allow_failure: false,
        trace_summary: "assert failed",
        artifacts_file_name: "artifacts.zip",
      },
    ],
    discussions: [
      {
        id: "d-1",
        gitlab_discussion_id: "abc",
        resolved: false,
        individual_note: false,
        notes: [
          {
            id: "n-1",
            gitlab_note_id: 1,
            body: "Please fix",
            system: false,
          },
        ],
      },
    ],
  };
}

describe("GitLabMergeRequestDetails", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders approvals, failed jobs, artifacts, trace, and unresolved discussions", () => {
    setApiInstance(new ApiClient("https://api.example.test"));

    render(
      <GitLabMergeRequestDetails issueId="issue-1" details={makeDetails()} />,
      {
        wrapper: TestWrapper,
      },
    );

    expect(screen.getByText("1 approval remaining")).toBeInTheDocument();
    expect(screen.getAllByText("Approved by")).toHaveLength(2);
    expect(screen.getAllByText("Grace Hopper (@grace)")).toHaveLength(2);
    expect(screen.getByText("Approval rules")).toBeInTheDocument();
    expect(screen.getByText("Maintainers")).toBeInTheDocument();
    expect(screen.getByText("2 required")).toBeInTheDocument();
    expect(screen.getByText("test")).toBeInTheDocument();
    expect(screen.getByText("assert failed")).toBeInTheDocument();
    expect(screen.getByText("Please fix")).toBeInTheDocument();

    const artifact = screen.getByText("artifacts.zip").closest("a");
    expect(artifact).toHaveAttribute(
      "href",
      "https://api.example.test/api/issues/issue-1/gitlab/jobs/job-1/artifacts",
    );
  });

  it("loads a job trace summary on demand", async () => {
    const user = userEvent.setup();
    setApiInstance(new ApiClient("https://api.example.test"));
    const details = makeDetails();
    details.jobs = [
      {
        ...details.jobs[0]!,
        trace_summary: null,
      },
    ];
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          trace_summary: "fresh trace failure",
          trace_truncated: false,
        }),
        {
          status: 200,
          headers: { "Content-Type": "application/json" },
        },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(<GitLabMergeRequestDetails issueId="issue-1" details={details} />, {
      wrapper: TestWrapper,
    });

    await user.click(screen.getByRole("button", { name: "Load trace" }));

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        "https://api.example.test/api/issues/issue-1/gitlab/jobs/job-1/trace",
        expect.any(Object),
      );
    });
    expect(await screen.findByText("fresh trace failure")).toBeInTheDocument();
  });

  it("hides GitLab system notes from unresolved discussions", () => {
    setApiInstance(new ApiClient("https://api.example.test"));
    const details = makeDetails();
    details.discussions = [
      {
        id: "system-1",
        gitlab_discussion_id: "system-1",
        resolved: null,
        individual_note: true,
        notes: [
          {
            id: "system-note-1",
            gitlab_note_id: 10,
            body: "mentioned in commit abc123",
            system: true,
          },
          {
            id: "system-note-2",
            gitlab_note_id: 11,
            body: "approved this merge request",
            system: true,
          },
        ],
      },
      {
        id: "review-1",
        gitlab_discussion_id: "review-1",
        resolved: false,
        individual_note: false,
        notes: [
          {
            id: "review-note-1",
            gitlab_note_id: 12,
            body: "Please fix the failing assertion",
            system: false,
            resolvable: true,
            resolved: false,
          },
        ],
      },
    ];

    render(<GitLabMergeRequestDetails issueId="issue-1" details={details} />, {
      wrapper: TestWrapper,
    });

    expect(screen.getByText("Unresolved discussions")).toBeInTheDocument();
    expect(
      screen.getByText("Please fix the failing assertion"),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("mentioned in commit abc123"),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText("approved this merge request"),
    ).not.toBeInTheDocument();
  });

  it("merges an open merge request from the details panel", async () => {
    const user = userEvent.setup();
    setApiInstance(new ApiClient("https://api.example.test"));
    const mergedDetails = makeDetails();
    mergedDetails.merge_request.state = "merged";
    mergedDetails.merge_request.merged_at = "2026-07-06T00:03:00Z";
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(mergedDetails), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(
      <GitLabMergeRequestDetails issueId="issue-1" details={makeDetails()} />,
      {
        wrapper: TestWrapper,
      },
    );

    await user.click(screen.getByRole("button", { name: "Merge" }));

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        "https://api.example.test/api/issues/issue-1/gitlab/merge-requests/mr-1/merge",
        expect.objectContaining({ method: "POST" }),
      );
    });
  });
});
