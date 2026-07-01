export type GitLabMergeRequestState = "open" | "closed" | "merged" | "draft";
export type GitLabPipelineStatus = "passed" | "failed" | "pending";

export interface GitLabProjectBinding {
  id: string;
  workspace_id: string;
  gitlab_project_id: number;
  path_with_namespace: string;
  web_url: string;
  hook_id?: number;
  hook_enabled: boolean;
  last_sync_error?: string | null;
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
  merged_at: string | null;
  closed_at: string | null;
  mr_created_at: string;
  mr_updated_at: string;
}

export interface ListGitLabMergeRequestsResponse {
  merge_requests: GitLabMergeRequest[];
}
