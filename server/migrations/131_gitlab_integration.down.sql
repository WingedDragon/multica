DROP TABLE IF EXISTS issue_gitlab_merge_request;
DROP TABLE IF EXISTS gitlab_mr_pipeline;
DROP TABLE IF EXISTS gitlab_merge_request;
DROP TABLE IF EXISTS gitlab_project_binding;
DROP TABLE IF EXISTS gitlab_connection;
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_workspace_id_id_key;
