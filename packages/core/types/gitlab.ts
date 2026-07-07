export type GitLabMergeRequestState = "open" | "closed" | "merged" | "draft";
export type GitLabPipelineStatus = string;

export interface GitLabProjectBinding {
  id: string;
  workspace_id: string;
  gitlab_project_id: number;
  path_with_namespace: string;
  web_url: string;
  hook_id?: number;
  hook_enabled: boolean;
  last_sync_error?: string | null;
  last_refresh_at?: string | null;
  last_refresh_error?: string | null;
  last_event_at?: string | null;
  last_event_type?: string | null;
  refresh_in_progress_at?: string | null;
  created_at: string;
}

export interface GitLabConfigResponse {
  configured: boolean;
  can_manage?: boolean;
  base_url?: string;
  manual_webhook_url?: string;
  projects: GitLabProjectBinding[];
}

export interface CreateGitLabProjectRequest {
  project: string;
}

export interface CreateGitLabProjectResponse {
  project: GitLabProjectBinding;
  manual_webhook_url?: string;
}

export interface GitLabMergeRequest {
  id: string;
  workspace_id: string;
  project_path: string;
  gitlab_project_id: number;
  iid: number;
  title: string;
  state: GitLabMergeRequestState;
  web_url: string;
  source_branch: string | null;
  target_branch: string | null;
  author_username: string | null;
  author_avatar_url: string | null;
  sha: string;
  detailed_merge_status: string | null;
  has_conflicts: boolean | null;
  pipeline_status: GitLabPipelineStatus | null;
  pipeline_url: string | null;
  additions?: number;
  deletions?: number;
  changed_files?: number;
  reviewers?: GitLabUserRef[];
  assignees?: GitLabUserRef[];
  labels?: string[];
  last_refreshed_at?: string | null;
  last_refresh_error?: string | null;
  merged_at: string | null;
  closed_at: string | null;
  mr_created_at: string;
  mr_updated_at: string;
}

export interface ListGitLabMergeRequestsResponse {
  merge_requests: GitLabMergeRequest[];
}

export interface GitLabUserRef {
  id?: number;
  username?: string;
  name?: string;
  avatar_url?: string;
  web_url?: string;
}

export interface GitLabApprovalRule {
  id?: number;
  name?: string;
  approved?: boolean;
  approvals_required?: number | null;
  approved_by?: GitLabUserRef[];
}

export interface GitLabMRApproval {
  approved: boolean;
  approvals_required?: number | null;
  approvals_left?: number | null;
  approved_by: GitLabUserRef[];
  rules: GitLabApprovalRule[];
  fetched_at?: string;
}

export interface GitLabMRNote {
  id: string;
  gitlab_note_id: number;
  author_username?: string | null;
  author_avatar_url?: string | null;
  body: string;
  system: boolean;
  resolved?: boolean | null;
  resolvable?: boolean | null;
  created_at?: string | null;
  updated_at?: string | null;
}

export interface GitLabMRDiscussion {
  id: string;
  gitlab_discussion_id: string;
  individual_note: boolean;
  resolved?: boolean | null;
  created_at?: string | null;
  updated_at?: string | null;
  notes: GitLabMRNote[];
}

export interface GitLabPipelineJob {
  id: string;
  pipeline_id: number;
  job_id: number;
  name: string;
  stage?: string | null;
  status: string;
  ref?: string | null;
  sha?: string | null;
  web_url?: string | null;
  started_at?: string | null;
  finished_at?: string | null;
  duration_seconds?: number | null;
  queued_duration_seconds?: number | null;
  failure_reason?: string | null;
  allow_failure: boolean;
  artifacts_file_name?: string | null;
  artifacts_file_size?: number | null;
  artifacts_expire_at?: string | null;
  trace_summary?: string | null;
  trace_truncated?: boolean;
  trace_fetched_at?: string | null;
}

export interface GitLabMergeRequestDetailsResponse {
  merge_request: GitLabMergeRequest;
  approval: GitLabMRApproval | null;
  discussions: GitLabMRDiscussion[];
  jobs: GitLabPipelineJob[];
}

export interface GitLabJobTraceResponse {
  trace_summary: string;
  trace_truncated: boolean;
}

export interface GitLabProjectRefreshResponse {
  updated_mrs: number;
  updated_jobs: number;
  updated_discussions: number;
  errors?: string[];
}
