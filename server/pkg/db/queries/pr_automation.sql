-- =====================
-- PR auto-complete (MUL-7429)
-- =====================

-- name: GetIssuePRAutoCompleteDisabled :one
SELECT EXISTS (
    SELECT 1 FROM issue_pr_automation
    WHERE issue_id = $1 AND auto_complete_disabled
)::boolean AS disabled;

-- name: SetIssuePRAutoCompleteDisabled :exec
INSERT INTO issue_pr_automation (
    issue_id, workspace_id, auto_complete_disabled, updated_by_type, updated_by_id
) VALUES (
    $1, $2, $3, sqlc.narg('updated_by_type'), sqlc.narg('updated_by_id')
)
ON CONFLICT (issue_id) DO UPDATE SET
    auto_complete_disabled = EXCLUDED.auto_complete_disabled,
    updated_by_type = EXCLUDED.updated_by_type,
    updated_by_id = EXCLUDED.updated_by_id,
    updated_at = now();

-- name: IsPullRequestExcludedFromIssue :one
SELECT EXISTS (
    SELECT 1 FROM issue_pull_request_exclusion
    WHERE issue_id = $1 AND pull_request_id = $2
)::boolean AS excluded;

-- name: ExcludePullRequestFromIssue :exec
INSERT INTO issue_pull_request_exclusion (
    issue_id, pull_request_id, workspace_id, excluded_by_type, excluded_by_id
) VALUES (
    $1, $2, $3, sqlc.narg('excluded_by_type'), sqlc.narg('excluded_by_id')
)
ON CONFLICT (issue_id, pull_request_id) DO UPDATE SET
    excluded_by_type = EXCLUDED.excluded_by_type,
    excluded_by_id = EXCLUDED.excluded_by_id,
    created_at = now();

-- name: DeletePullRequestExclusion :exec
DELETE FROM issue_pull_request_exclusion
WHERE issue_id = $1 AND pull_request_id = $2;

-- name: ListIssueLinkedPullRequestStates :many
-- Every PR linked to the issue across GitHub and self-hosted providers, for
-- the merge decision. Ordered by number so reasons read stably.
WITH legacy_gitlab_issues AS (
    SELECT DISTINCT legacy_ipr.issue_id
    FROM issue_vcs_pull_request legacy_ipr
    JOIN issue legacy_issue ON legacy_issue.id = legacy_ipr.issue_id
    JOIN vcs_pull_request legacy_pr
      ON legacy_pr.id = legacy_ipr.pull_request_id
     AND legacy_pr.workspace_id = legacy_issue.workspace_id
    JOIN vcs_gitlab_project legacy_gp
      ON legacy_gp.workspace_id = legacy_pr.workspace_id
     AND legacy_gp.connection_id = legacy_pr.connection_id
     AND legacy_gp.path_with_namespace = legacy_pr.repo_owner || '/' || legacy_pr.repo_name
    WHERE legacy_pr.provider = 'gitlab'
      AND legacy_gp.legacy_link_policy
      AND legacy_ipr.gitlab_linked_at IS NOT NULL
)
SELECT pr.id, 'github'::text AS provider, pr.pr_number, pr.state,
       NOT EXISTS (
           SELECT 1 FROM legacy_gitlab_issues legacy
           WHERE legacy.issue_id = ipr.issue_id
       ) AS completion_source
FROM github_pull_request pr
JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
JOIN issue i ON i.id = ipr.issue_id AND i.workspace_id = pr.workspace_id
WHERE i.id = $1
UNION ALL
SELECT pr.id, pr.provider AS provider, pr.pr_number, pr.state,
       ipr.vcs_linked
       AND NOT EXISTS (
           SELECT 1 FROM legacy_gitlab_issues legacy
           WHERE legacy.issue_id = ipr.issue_id
       ) AS completion_source
FROM vcs_pull_request pr
JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
JOIN issue i ON i.id = ipr.issue_id AND i.workspace_id = pr.workspace_id
LEFT JOIN vcs_gitlab_project gp
  ON gp.workspace_id = pr.workspace_id
 AND gp.connection_id = pr.connection_id
 AND gp.path_with_namespace = pr.repo_owner || '/' || pr.repo_name
WHERE i.id = $1
  AND (
      ipr.vcs_linked
      OR (pr.provider = 'gitlab' AND gp.legacy_link_policy AND ipr.gitlab_linked_at IS NOT NULL)
  )
ORDER BY pr_number;

-- name: MoveIssueFromPullRequests :one
-- Conditional status write for the PR merge automation. It lands only if the
-- issue is still in the status the decision saw (two merges racing move it
-- once), is not already in the target, and the linked PRs are still all merged
-- when the write runs (a PR linked between the decision and this statement
-- keeps the issue where it is). Repositions and clears a duplicate mark like
-- UpdateIssueStatus does; the target is never cancelled.
UPDATE issue AS i SET
    status = sqlc.arg('target_status')::text,
    duplicate_of_issue_id = NULL,
    position = (
        SELECT COALESCE(MIN(target.position), 0) - 1
        FROM issue AS target
        WHERE target.workspace_id = i.workspace_id
          AND target.status = sqlc.arg('target_status')::text
    ),
    revision = i.revision + 1,
    last_activity_at = GREATEST(COALESCE(i.last_activity_at, i.updated_at), now()),
    updated_at = now()
WHERE i.id = $1
  AND i.workspace_id = $2
  AND i.status = sqlc.arg('expected_status')::text
  AND i.status <> sqlc.arg('target_status')::text
  AND EXISTS (
      SELECT 1
      FROM issue_pull_request ipr
      JOIN github_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
      WHERE ipr.issue_id = i.id
      UNION ALL
      SELECT 1
      FROM issue_vcs_pull_request ipr
      JOIN vcs_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
      LEFT JOIN vcs_gitlab_project gp
        ON gp.workspace_id = pr.workspace_id
       AND gp.connection_id = pr.connection_id
       AND gp.path_with_namespace = pr.repo_owner || '/' || pr.repo_name
      WHERE ipr.issue_id = i.id
        AND (ipr.vcs_linked OR (pr.provider = 'gitlab' AND gp.legacy_link_policy AND ipr.gitlab_linked_at IS NOT NULL))
  )
  AND EXISTS (
      SELECT 1
      FROM issue_pull_request ipr
      JOIN github_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
      WHERE ipr.issue_id = i.id
      UNION ALL
      SELECT 1
      FROM issue_vcs_pull_request ipr
      JOIN vcs_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
      LEFT JOIN vcs_gitlab_project gp
        ON gp.workspace_id = pr.workspace_id
       AND gp.connection_id = pr.connection_id
       AND gp.path_with_namespace = pr.repo_owner || '/' || pr.repo_name
      WHERE ipr.issue_id = i.id
        AND ipr.vcs_linked
        AND NOT (
            pr.provider = 'gitlab'
            AND COALESCE(gp.legacy_link_policy, FALSE)
            AND ipr.gitlab_linked_at IS NOT NULL
        )
  )
  AND NOT EXISTS (
      SELECT 1
      FROM issue_vcs_pull_request legacy_ipr
      JOIN vcs_pull_request legacy_pr
        ON legacy_pr.id = legacy_ipr.pull_request_id
       AND legacy_pr.workspace_id = i.workspace_id
      JOIN vcs_gitlab_project legacy_gp
        ON legacy_gp.workspace_id = legacy_pr.workspace_id
       AND legacy_gp.connection_id = legacy_pr.connection_id
       AND legacy_gp.path_with_namespace = legacy_pr.repo_owner || '/' || legacy_pr.repo_name
      WHERE legacy_ipr.issue_id = i.id
        AND legacy_pr.provider = 'gitlab'
        AND legacy_gp.legacy_link_policy
        AND legacy_ipr.gitlab_linked_at IS NOT NULL
  )
  AND NOT EXISTS (
      SELECT 1
      FROM issue_pull_request ipr
      JOIN github_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
      WHERE ipr.issue_id = i.id AND pr.state <> 'merged'
  )
  AND NOT EXISTS (
      SELECT 1
      FROM issue_vcs_pull_request ipr
      JOIN vcs_pull_request pr ON pr.id = ipr.pull_request_id AND pr.workspace_id = i.workspace_id
      LEFT JOIN vcs_gitlab_project gp
        ON gp.workspace_id = pr.workspace_id
       AND gp.connection_id = pr.connection_id
       AND gp.path_with_namespace = pr.repo_owner || '/' || pr.repo_name
      WHERE ipr.issue_id = i.id
        AND (ipr.vcs_linked OR (pr.provider = 'gitlab' AND gp.legacy_link_policy AND ipr.gitlab_linked_at IS NOT NULL))
        AND pr.state <> 'merged'
  )
RETURNING i.*;
