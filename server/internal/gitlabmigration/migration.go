package gitlabmigration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

var (
	errAmbiguousLink = errors.New("conflicting manual issue-link owners")
	errAmbiguousMR   = errors.New("same-time GitLab merge-request snapshots conflict")
)

type idMappings struct {
	Connections  map[string]string
	PullRequests map[string]string
}

// rewriteChatWakeupCondition changes only legacy GitLab IDs. Pipeline baseline
// versions and all client-visible condition fields are left intact.
func rewriteChatWakeupCondition(raw json.RawMessage, mappings idMappings) (json.RawMessage, bool, error) {
	var condition map[string]json.RawMessage
	if err := json.Unmarshal(raw, &condition); err != nil {
		return nil, false, fmt.Errorf("decode chat wakeup condition: %w", err)
	}
	var provider string
	if err := json.Unmarshal(condition["provider"], &provider); err != nil || provider != "gitlab" {
		return append(json.RawMessage(nil), raw...), false, nil
	}
	var connectionID string
	if err := json.Unmarshal(condition["connection_id"], &connectionID); err != nil || connectionID == "" {
		return nil, false, errors.New("GitLab chat wakeup has no connection ID")
	}
	var gitlabState map[string]json.RawMessage
	if err := json.Unmarshal(condition["gitlab"], &gitlabState); err != nil || gitlabState == nil {
		return nil, false, errors.New("GitLab chat wakeup has no persisted target identity")
	}
	var source string
	if err := json.Unmarshal(gitlabState["source"], &source); err != nil {
		return nil, false, errors.New("GitLab chat wakeup has no persisted source")
	}
	if source == "vcs_connection" {
		return append(json.RawMessage(nil), raw...), false, nil
	}
	if source != "gitlab_connection" {
		return nil, false, fmt.Errorf("unsupported legacy GitLab wakeup source %q", source)
	}
	var mergeRequestID string
	if err := json.Unmarshal(gitlabState["merge_request_id"], &mergeRequestID); err != nil || mergeRequestID == "" {
		return nil, false, errors.New("legacy GitLab chat wakeup has no merge-request ID")
	}
	newConnectionID := mappings.Connections[connectionID]
	newMergeRequestID := mappings.PullRequests[mergeRequestID]
	if newConnectionID == "" || newMergeRequestID == "" {
		return nil, false, fmt.Errorf("legacy GitLab chat wakeup IDs have no workspace-scoped mapping (connection=%s, merge_request=%s)", connectionID, mergeRequestID)
	}
	condition["connection_id"], _ = json.Marshal(newConnectionID)
	gitlabState["source"], _ = json.Marshal("vcs_connection")
	gitlabState["merge_request_id"], _ = json.Marshal(newMergeRequestID)
	condition["gitlab"], _ = json.Marshal(gitlabState)
	updated, err := json.Marshal(condition)
	if err != nil {
		return nil, false, fmt.Errorf("encode migrated chat wakeup condition: %w", err)
	}
	return updated, true, nil
}

func rewriteChatWakeupConditionToLegacy(raw json.RawMessage, mappings idMappings) (json.RawMessage, bool, error) {
	var condition map[string]json.RawMessage
	if err := json.Unmarshal(raw, &condition); err != nil {
		return nil, false, fmt.Errorf("decode chat wakeup condition: %w", err)
	}
	var provider string
	if err := json.Unmarshal(condition["provider"], &provider); err != nil || provider != "gitlab" {
		return append(json.RawMessage(nil), raw...), false, nil
	}
	var connectionID string
	if err := json.Unmarshal(condition["connection_id"], &connectionID); err != nil || connectionID == "" {
		return nil, false, errors.New("GitLab chat wakeup has no connection ID")
	}
	var gitlabState map[string]json.RawMessage
	if err := json.Unmarshal(condition["gitlab"], &gitlabState); err != nil || gitlabState == nil {
		return nil, false, errors.New("GitLab chat wakeup has no persisted target identity")
	}
	var source string
	if err := json.Unmarshal(gitlabState["source"], &source); err != nil {
		return nil, false, errors.New("GitLab chat wakeup has no persisted source")
	}
	if source == "gitlab_connection" {
		return append(json.RawMessage(nil), raw...), false, nil
	}
	if source != "vcs_connection" {
		return nil, false, fmt.Errorf("unsupported canonical GitLab wakeup source %q", source)
	}
	var mergeRequestID string
	if err := json.Unmarshal(gitlabState["merge_request_id"], &mergeRequestID); err != nil || mergeRequestID == "" {
		return nil, false, errors.New("canonical GitLab chat wakeup has no merge-request ID")
	}
	legacyConnectionID := mappings.Connections[connectionID]
	legacyMergeRequestID := mappings.PullRequests[mergeRequestID]
	if legacyConnectionID == "" || legacyMergeRequestID == "" {
		return nil, false, fmt.Errorf("canonical GitLab chat wakeup IDs have no workspace-scoped legacy mapping (connection=%s, merge_request=%s)", connectionID, mergeRequestID)
	}
	condition["connection_id"], _ = json.Marshal(legacyConnectionID)
	gitlabState["source"], _ = json.Marshal("gitlab_connection")
	gitlabState["merge_request_id"], _ = json.Marshal(legacyMergeRequestID)
	condition["gitlab"], _ = json.Marshal(gitlabState)
	updated, err := json.Marshal(condition)
	if err != nil {
		return nil, false, fmt.Errorf("encode restored chat wakeup condition: %w", err)
	}
	return updated, true, nil
}

func rewriteChatWakeupConditionState(raw, fromSource, toSource string, mappings idMappings) (string, error) {
	if raw == "" {
		return "", nil
	}
	parts := strings.SplitN(raw, ":", 5)
	if len(parts) != 5 || parts[0] != "gitlab" || parts[1] != fromSource || parts[4] == "" {
		return "", fmt.Errorf("GitLab chat wakeup condition state has an unsupported fingerprint %q", raw)
	}
	connectionID := mappings.Connections[parts[2]]
	mergeRequestID := mappings.PullRequests[parts[3]]
	if connectionID == "" || mergeRequestID == "" {
		return "", fmt.Errorf("GitLab chat wakeup fingerprint IDs have no mapping (connection=%s, merge_request=%s)", parts[2], parts[3])
	}
	return strings.Join([]string{parts[0], toSource, connectionID, mergeRequestID, parts[4]}, ":"), nil
}

type issueLinkState struct {
	CloseIntent        bool
	ReferenceOnly      bool
	LinkedByType       string
	LinkedByID         string
	LinkedAt           time.Time
	VCSLinked          bool
	GitLabCloseIntent  bool
	GitLabLinkedByType string
	GitLabLinkedByID   string
	GitLabLinkedAt     pgtype.Timestamptz
}

func mergeIssueLink(current, incoming issueLinkState) (issueLinkState, error) {
	currentGitLabType := normalizedLinkType(current.GitLabLinkedByType)
	incomingGitLabType := normalizedLinkType(incoming.GitLabLinkedByType)
	if currentGitLabType == "member" && incomingGitLabType == "member" &&
		current.GitLabLinkedByID != incoming.GitLabLinkedByID &&
		(!current.GitLabLinkedAt.Valid || !incoming.GitLabLinkedAt.Valid ||
			current.GitLabLinkedAt.Time.Equal(incoming.GitLabLinkedAt.Time)) {
		return issueLinkState{}, errAmbiguousLink
	}
	merged := current
	if incoming.VCSLinked && !current.VCSLinked {
		merged.CloseIntent = incoming.CloseIntent
		merged.ReferenceOnly = incoming.ReferenceOnly
		merged.LinkedByType = incoming.LinkedByType
		merged.LinkedByID = incoming.LinkedByID
		merged.LinkedAt = incoming.LinkedAt
	}
	merged.VCSLinked = current.VCSLinked || incoming.VCSLinked

	switch {
	case !current.GitLabLinkedAt.Valid && incoming.GitLabLinkedAt.Valid:
		merged.GitLabCloseIntent = incoming.GitLabCloseIntent
		merged.GitLabLinkedByType = incoming.GitLabLinkedByType
		merged.GitLabLinkedByID = incoming.GitLabLinkedByID
		merged.GitLabLinkedAt = incoming.GitLabLinkedAt
	case current.GitLabLinkedAt.Valid && incoming.GitLabLinkedAt.Valid:
		switch {
		case incoming.GitLabLinkedAt.Time.After(current.GitLabLinkedAt.Time):
			merged.GitLabCloseIntent = incoming.GitLabCloseIntent
			merged.GitLabLinkedByType = incoming.GitLabLinkedByType
			merged.GitLabLinkedByID = incoming.GitLabLinkedByID
			merged.GitLabLinkedAt = incoming.GitLabLinkedAt
		case current.GitLabLinkedAt.Time.Equal(incoming.GitLabLinkedAt.Time) &&
			(current.GitLabCloseIntent != incoming.GitLabCloseIntent ||
				current.GitLabLinkedByType != incoming.GitLabLinkedByType ||
				current.GitLabLinkedByID != incoming.GitLabLinkedByID):
			return issueLinkState{}, errAmbiguousLink
		}
	}
	return merged, nil
}

func normalizedLinkType(value string) string {
	if strings.TrimSpace(value) == "" {
		return "system"
	}
	return strings.TrimSpace(value)
}

type mrSnapshot struct {
	UpdatedAt                                        time.Time
	State, Title, HeadSHA, URL, Branch, TargetBranch string
	AuthorLogin, AuthorAvatar                        string
	MergedAt, ClosedAt                               pgtype.Timestamptz
	Additions, Deletions, ChangedFiles               int32
}

func mergeMRSnapshot(current, incoming mrSnapshot) (mrSnapshot, error) {
	latest, older := current, incoming
	switch {
	case incoming.UpdatedAt.After(current.UpdatedAt):
		latest, older = incoming, current
	case current.UpdatedAt.After(incoming.UpdatedAt):
		// Keep the current unified snapshot.
	default:
		if current.State != incoming.State || current.HeadSHA != incoming.HeadSHA ||
			(current.Title != "" && incoming.Title != "" && current.Title != incoming.Title) ||
			(current.URL != "" && incoming.URL != "" && current.URL != incoming.URL) ||
			(current.Branch != "" && incoming.Branch != "" && current.Branch != incoming.Branch) ||
			(current.TargetBranch != "" && incoming.TargetBranch != "" && current.TargetBranch != incoming.TargetBranch) ||
			(current.AuthorLogin != "" && incoming.AuthorLogin != "" && current.AuthorLogin != incoming.AuthorLogin) ||
			(current.AuthorAvatar != "" && incoming.AuthorAvatar != "" && current.AuthorAvatar != incoming.AuthorAvatar) ||
			!sameNullableTimestamp(current.MergedAt, incoming.MergedAt) ||
			!sameNullableTimestamp(current.ClosedAt, incoming.ClosedAt) ||
			current.Additions != incoming.Additions || current.Deletions != incoming.Deletions || current.ChangedFiles != incoming.ChangedFiles {
			return mrSnapshot{}, errAmbiguousMR
		}
	}
	if latest.Title == "" {
		latest.Title = older.Title
	}
	if latest.URL == "" {
		latest.URL = older.URL
	}
	if latest.Branch == "" {
		latest.Branch = older.Branch
	}
	if latest.TargetBranch == "" {
		latest.TargetBranch = older.TargetBranch
	}
	if latest.AuthorLogin == "" {
		latest.AuthorLogin = older.AuthorLogin
	}
	if latest.AuthorAvatar == "" {
		latest.AuthorAvatar = older.AuthorAvatar
	}
	return latest, nil
}

func sameNullableTimestamp(a, b pgtype.Timestamptz) bool {
	return a.Valid == b.Valid && (!a.Valid || a.Time.Equal(b.Time))
}

func newerSync(aAt pgtype.Timestamptz, aErr pgtype.Text, bAt pgtype.Timestamptz, bErr pgtype.Text) (pgtype.Timestamptz, pgtype.Text, error) {
	if aAt.Valid && bAt.Valid && aAt.Time.Equal(bAt.Time) &&
		(aErr.Valid != bErr.Valid || aErr.String != bErr.String) {
		return pgtype.Timestamptz{}, pgtype.Text{}, errAmbiguousMR
	}
	if !aAt.Valid && !bAt.Valid && (aErr.Valid != bErr.Valid || aErr.String != bErr.String) {
		return pgtype.Timestamptz{}, pgtype.Text{}, errAmbiguousMR
	}
	if bAt.Valid && (!aAt.Valid || bAt.Time.After(aAt.Time)) {
		return bAt, bErr, nil
	}
	return aAt, aErr, nil
}

func jsonEqual(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}
