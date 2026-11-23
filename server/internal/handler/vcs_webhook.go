package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ── Response mappers ────────────────────────────────────────────────────────

// vcsPullRequestToResponse maps a stored VCS PR onto the shared PR response
// shape for single-PR webhook broadcasts (no aggregated check counts; the
// frontend re-queries the issue's PR list for fresh counts).
func vcsPullRequestToResponse(p db.VcsPullRequest) GitHubPullRequestResponse {
	return GitHubPullRequestResponse{
		ID:               uuidToString(p.ID),
		Provider:         p.Provider,
		WorkspaceID:      uuidToString(p.WorkspaceID),
		RepoOwner:        p.RepoOwner,
		RepoName:         p.RepoName,
		Number:           p.PrNumber,
		Title:            p.Title,
		State:            p.State,
		HtmlURL:          p.HtmlUrl,
		Branch:           textToPtr(p.Branch),
		AuthorLogin:      textToPtr(p.AuthorLogin),
		AuthorAvatarURL:  textToPtr(p.AuthorAvatarUrl),
		MergedAt:         timestampToPtr(p.MergedAt),
		ClosedAt:         timestampToPtr(p.ClosedAt),
		PRCreatedAt:      timestampToString(p.PrCreatedAt),
		PRUpdatedAt:      timestampToString(p.PrUpdatedAt),
		MergeableState:   nil,
		ChecksConclusion: nil,
		Additions:        p.Additions,
		Deletions:        p.Deletions,
		ChangedFiles:     p.ChangedFiles,
	}
}

// vcsPullRequestRowToResponse maps an issue's PR-list row, which carries the
// aggregated commit-status counts, onto the shared response shape.
func vcsPullRequestRowToResponse(p db.ListVCSPullRequestsByIssueRow) GitHubPullRequestResponse {
	return GitHubPullRequestResponse{
		ID:               uuidToString(p.ID),
		Provider:         p.Provider,
		WorkspaceID:      uuidToString(p.WorkspaceID),
		RepoOwner:        p.RepoOwner,
		RepoName:         p.RepoName,
		Number:           p.PrNumber,
		Title:            p.Title,
		State:            p.State,
		HtmlURL:          p.HtmlUrl,
		Branch:           textToPtr(p.Branch),
		AuthorLogin:      textToPtr(p.AuthorLogin),
		AuthorAvatarURL:  textToPtr(p.AuthorAvatarUrl),
		MergedAt:         timestampToPtr(p.MergedAt),
		ClosedAt:         timestampToPtr(p.ClosedAt),
		PRCreatedAt:      timestampToString(p.PrCreatedAt),
		PRUpdatedAt:      timestampToString(p.PrUpdatedAt),
		MergeableState:   nil,
		ChecksConclusion: aggregateChecksConclusion(p.ChecksFailed, p.ChecksPassed, p.ChecksPending, p.ChecksTotal),
		ChecksTotal:      p.ChecksTotal,
		ChecksPassed:     p.ChecksPassed,
		ChecksFailed:     p.ChecksFailed,
		ChecksPending:    p.ChecksPending,
		ChecksRunning:    p.ChecksPending,
		FailedCheckNames: []string{},
		Additions:        p.Additions,
		Deletions:        p.Deletions,
		ChangedFiles:     p.ChangedFiles,
	}
}

// ── Webhook ─────────────────────────────────────────────────────────────────

// HandleVCSWebhook (POST /api/webhooks/vcs/{connectionId}) authenticates and
// mirrors webhooks from any token-based Git provider. The connection id in the path
// selects the workspace, the provider, and the decryption secret; the provider
// adapter handles the provider-specific signature scheme, event header, and
// payload shape, returning normalized events to the shared mirror logic below.
func (h *Handler) HandleVCSWebhook(w http.ResponseWriter, r *http.Request) {
	// Where the integration is off (the managed cloud) the endpoint behaves as
	// if it does not exist — a bare 404 that reveals nothing about config, the
	// same response a genuinely unknown connection id gets below.
	if !h.isVCSAvailable() {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	if !h.isVCSConfigured() {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	connUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "connectionId"), "connection id")
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MiB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}

	conn, err := h.Queries.GetVCSConnectionByID(r.Context(), connUUID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("vcs: lookup connection failed", "err", err)
		}
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	provider, ok := vcs.For(conn.Provider)
	if !ok {
		slog.Error("vcs: connection has unknown provider", "provider", conn.Provider)
		writeError(w, http.StatusInternalServerError, "unknown provider")
		return
	}

	secret, err := h.openVCSSecret(conn.WebhookSecretEncrypted)
	if err != nil {
		slog.Error("vcs: decrypt webhook secret failed", "err", err)
		writeError(w, http.StatusInternalServerError, "secret error")
		return
	}
	if !provider.VerifySignature(secret, r.Header, body) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	systemHook := conn.Provider == string(vcs.KindGitLab) && r.Header.Get("X-Gitlab-Event") == "System Hook"
	switch provider.EventKind(r.Header, body) {
	case vcs.EventPullRequest:
		pr, err := provider.ParsePullRequest(body)
		if err != nil {
			slog.Warn("vcs: bad pull_request payload", "provider", conn.Provider, "err", err)
			if conn.Provider == string(vcs.KindGitLab) {
				writeError(w, http.StatusBadRequest, "invalid merge request payload")
				return
			}
			break
		}
		if pr.RepoOwner == "" || pr.RepoName == "" || pr.Number <= 0 {
			if conn.Provider == string(vcs.KindGitLab) {
				writeError(w, http.StatusBadRequest, "merge request identity missing")
				return
			}
			break
		}
		if systemHook {
			allowed, err := h.authorizeGitLabSystemHook(r.Context(), conn, provider, pr.RepoOwner, pr.RepoName, nil)
			if err != nil {
				slog.Warn("vcs: system-hook repository validation failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, gitLabSystemHookFailureStatus(err), "system hook validation failed")
				return
			}
			if !allowed {
				break
			}
		}
		if err := h.mirrorVCSPullRequest(r.Context(), conn, pr); err != nil {
			if conn.Provider == string(vcs.KindGitLab) {
				slog.Warn("vcs: merge request mirror failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, http.StatusServiceUnavailable, "merge request sync failed")
				return
			}
			slog.Warn("vcs: upsert pr failed", "err", err)
		}
	case vcs.EventCIStatus:
		status, err := provider.ParseCIStatus(body)
		if err != nil {
			slog.Warn("vcs: bad status payload", "provider", conn.Provider, "err", err)
			if conn.Provider == string(vcs.KindGitLab) {
				writeError(w, http.StatusBadRequest, "invalid pipeline payload")
				return
			}
			break
		}
		if conn.Provider == string(vcs.KindGitLab) && (status.SHA == "" || status.PipelineID <= 0 || status.ProjectID <= 0 || status.RepoOwner == "" || status.RepoName == "") {
			writeError(w, http.StatusBadRequest, "pipeline identity missing")
			return
		}
		if systemHook {
			allowed, err := h.authorizeGitLabSystemHook(r.Context(), conn, provider, status.RepoOwner, status.RepoName, &status)
			if err != nil {
				slog.Warn("vcs: system-hook pipeline validation failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, gitLabSystemHookFailureStatus(err), "system hook validation failed")
				return
			}
			if !allowed {
				break
			}
		}
		if err := h.mirrorVCSCIStatus(r.Context(), conn, status); err != nil {
			if conn.Provider == string(vcs.KindGitLab) {
				slog.Warn("vcs: pipeline mirror failed", "reason", gitLabWakeupFailureMessage(err))
				writeError(w, http.StatusServiceUnavailable, "pipeline sync failed")
				return
			}
			slog.Warn("vcs: upsert commit status failed", "err", err)
		}
	default:
		// Acknowledge unmodelled events so the provider doesn't flag the hook.
	}
	w.WriteHeader(http.StatusAccepted)
}

var errGitLabSystemHookCredential = errors.New("GitLab system-hook credentials are unavailable")

func gitLabSystemHookFailureStatus(err error) int {
	if errors.Is(err, errGitLabSystemHookCredential) {
		return http.StatusInternalServerError
	}
	return http.StatusServiceUnavailable
}

// A system hook covers the entire GitLab instance. Only repositories explicitly
// configured in this workspace may affect its issues; a similarly named repo
// on another host or under another namespace must not match.
func gitLabSystemHookRepoAllowed(raw []byte, instanceURL, owner, name string) bool {
	var repos []workspaceRepoRef
	if json.Unmarshal(raw, &repos) != nil || owner == "" || name == "" {
		return false
	}
	instance, err := url.Parse(instanceURL)
	if err != nil || instance.Host == "" {
		return false
	}
	want := strings.Trim(instance.Path, "/")
	if want != "" {
		want += "/"
	}
	want += owner + "/" + name
	for _, repo := range repos {
		u, err := parseGitLabRepoURL(strings.TrimSpace(repo.URL))
		if err != nil || !gitLabRepoURLMatchesInstance(u, instance) {
			continue
		}
		path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
		if path == want {
			return true
		}
	}
	return false
}

func parseGitLabRepoURL(raw string) (*url.URL, error) {
	if strings.Contains(raw, "://") {
		return url.Parse(raw)
	}
	at := strings.Index(raw, "@")
	colon := strings.Index(raw, ":")
	if at <= 0 || colon <= at+1 || colon == len(raw)-1 {
		return nil, errors.New("invalid scp-style repository URL")
	}
	return &url.URL{Scheme: "ssh", Host: raw[at+1 : colon], Path: "/" + raw[colon+1:], User: url.User(raw[:at])}, nil
}
func gitLabRepoURLMatchesInstance(repo, instance *url.URL) bool {
	if repo.RawQuery != "" || repo.Fragment != "" {
		return false
	}
	if repo.Scheme == "ssh" {
		return strings.EqualFold(repo.Hostname(), instance.Hostname()) && repo.User != nil && repo.User.Username() == "git"
	}
	if repo.Scheme == "https" || repo.Scheme == "http" {
		return repo.Scheme == instance.Scheme && strings.EqualFold(repo.Host, instance.Host) && repo.User == nil
	}
	return false
}

func (h *Handler) authorizeGitLabSystemHook(ctx context.Context, conn db.VcsConnection, provider vcs.Provider, owner, name string, pipeline *vcs.CIStatusEvent) (bool, error) {
	workspace, err := h.Queries.GetWorkspace(ctx, conn.WorkspaceID)
	if err != nil {
		return false, fmt.Errorf("load workspace for GitLab system hook: %w", err)
	}
	candidates := make(map[string]db.VcsPullRequest)
	if pipeline != nil && pipeline.ProjectID > 0 && pipeline.SHA != "" {
		byProject, err := h.Queries.ListVCSGitLabPullRequestsByProjectHead(ctx, db.ListVCSGitLabPullRequestsByProjectHeadParams{
			WorkspaceID:  conn.WorkspaceID,
			ConnectionID: conn.ID,
			RepoOwner:    owner,
			RepoName:     name,
			HeadSha:      pipeline.SHA,
		})
		if err != nil {
			return false, fmt.Errorf("find configured GitLab target pipelines: %w", err)
		}
		for _, candidate := range byProject {
			candidates[uuidToString(candidate.ID)] = candidate
		}
		bySource, err := h.Queries.ListVCSGitLabPullRequestsBySourceProjectHead(ctx, db.ListVCSGitLabPullRequestsBySourceProjectHeadParams{
			WorkspaceID:     conn.WorkspaceID,
			ConnectionID:    conn.ID,
			SourceProjectID: pgtype.Int8{Int64: pipeline.ProjectID, Valid: true},
			HeadSha:         pipeline.SHA,
		})
		if err != nil {
			return false, fmt.Errorf("find configured GitLab source pipelines: %w", err)
		}
		for _, candidate := range bySource {
			candidates[uuidToString(candidate.ID)] = candidate
		}
	}
	allowed := gitLabSystemHookRepoAllowed(workspace.Repos, conn.InstanceUrl, owner, name)
	for _, candidate := range candidates {
		if gitLabSystemHookRepoAllowed(workspace.Repos, conn.InstanceUrl, candidate.RepoOwner, candidate.RepoName) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false, nil
	}
	checker, ok := provider.(vcs.RepositoryAccessChecker)
	if !ok {
		return true, nil
	}
	token, err := h.openVCSSecret(conn.AccessTokenEncrypted)
	if err != nil || token == "" {
		if err == nil {
			err = errors.New("empty GitLab access token")
		}
		return false, errors.Join(errGitLabSystemHookCredential, fmt.Errorf("open GitLab access token: %w", err))
	}
	readable, err := checker.RepositoryReadable(ctx, conn.InstanceUrl, owner, name, token)
	if err == nil && !readable {
		return false, nil
	}
	if err != nil {
		if pipeline != nil {
			for _, candidate := range candidates {
				if gitLabSystemHookRepoAllowed(workspace.Repos, conn.InstanceUrl, candidate.RepoOwner, candidate.RepoName) {
					_, _ = h.Queries.MarkGitLabVCSPullRequestPipelineSync(ctx, db.MarkGitLabVCSPullRequestPipelineSyncParams{
						ID:                   candidate.ID,
						WorkspaceID:          conn.WorkspaceID,
						PipelineSyncError:    strToText(gitLabWakeupFailureMessage(err)),
						WakeupSyncGeneration: candidate.WakeupSyncGeneration,
						HeadSha:              candidate.HeadSha,
					})
				}
			}
		}
		return false, err
	}
	return true, nil
}

func (h *Handler) mirrorVCSPullRequest(ctx context.Context, conn db.VcsConnection, ev vcs.PullRequestEvent) error {
	if ev.RepoOwner == "" || ev.RepoName == "" || ev.Number <= 0 {
		return errors.New("vcs: pull_request missing repository identity")
	}

	prevState := ""
	prev, err := h.Queries.GetVCSPullRequestByKey(ctx, db.GetVCSPullRequestByKeyParams{
		ConnectionID: conn.ID,
		RepoOwner:    ev.RepoOwner,
		RepoName:     ev.RepoName,
		PrNumber:     ev.Number,
	})
	if err == nil {
		prevState = prev.State
	} else if conn.Provider == string(vcs.KindGitLab) && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	conflictState := ev.HasConflicts
	conflictHeadSHA := ""
	if conflictState != nil && ev.HeadSHA != "" {
		conflictHeadSHA = ev.HeadSHA
	} else {
		conflictState = nil
	}
	sourceProjectID := pgtype.Int8{}
	if ev.SourceProjectID > 0 {
		sourceProjectID = pgtype.Int8{Int64: ev.SourceProjectID, Valid: true}
	}
	pr, err := h.Queries.UpsertVCSPullRequest(ctx, db.UpsertVCSPullRequestParams{
		WorkspaceID:       conn.WorkspaceID,
		ConnectionID:      conn.ID,
		Provider:          conn.Provider,
		RepoOwner:         ev.RepoOwner,
		RepoName:          ev.RepoName,
		PrNumber:          ev.Number,
		Title:             ev.Title,
		State:             ev.State,
		HtmlUrl:           ev.HTMLURL,
		Branch:            ptrToText(strPtrOrNil(ev.Branch)),
		AuthorLogin:       ptrToText(strPtrOrNil(ev.AuthorLogin)),
		AuthorAvatarUrl:   ptrToText(strPtrOrNil(ev.AuthorAvatarURL)),
		MergedAt:          parseGHTime(ev.MergedAt),
		ClosedAt:          parseGHTime(ev.ClosedAt),
		PrCreatedAt:       parseGHTimeRequired(ev.CreatedAt),
		PrUpdatedAt:       parseGHTimeRequired(ev.UpdatedAt),
		Additions:         ev.Additions,
		Deletions:         ev.Deletions,
		ChangedFiles:      ev.ChangedFiles,
		HeadSha:           ev.HeadSHA,
		HasConflicts:      boolToPg(conflictState),
		ConflictHeadSha:   conflictHeadSHA,
		SourceProjectID:   sourceProjectID,
		SourceProjectPath: strToText(ev.SourceProjectPath),
	})
	if err != nil {
		return err
	}

	evUpdatedAt := parseGHTimeRequired(ev.UpdatedAt)
	if pr.PrUpdatedAt.Valid && evUpdatedAt.Valid && pr.PrUpdatedAt.Time.After(evUpdatedAt.Time) {
		return nil
	}

	var pendingGitLabPipelineSync *vcsGitLabWakeupPipelineSync
	if conn.Provider == string(vcs.KindGitLab) {
		pendingGitLabPipelineSync, err = h.beginGitLabWakeupTargetForMergeRequestWebhook(ctx, vcs.GitLabWakeupTarget{
			Source:         vcs.GitLabWakeupSourceVCSConnection,
			WorkspaceID:    uuidToString(conn.WorkspaceID),
			ConnectionID:   uuidToString(conn.ID),
			MergeRequestID: uuidToString(pr.ID),
			Repository:     ev.RepoOwner + "/" + ev.RepoName,
			Number:         ev.Number,
		})
		if err != nil {
			return err
		}
	}

	workspaceID := uuidToString(conn.WorkspaceID)
	resp := vcsPullRequestToResponse(pr)
	linkedIssueIDs := make([]string, 0)
	ws, err := h.Queries.GetWorkspace(ctx, conn.WorkspaceID)
	if err != nil {
		if conn.Provider == string(vcs.KindGitLab) {
			return err
		}
		slog.Warn("vcs: load workspace failed", "err", err)
	} else {
		var touched map[pgtype.UUID]struct{}
		idents := prClaimedIdentifiers(ev.Title, ev.Body, ev.Branch)
		linkedIssueIDs, touched = h.reconcileAutoLinks(ctx, ws, pr.ID, ev.State, prAutoLinkInput{
			idents:    idents,
			permits:   func(string) bool { return true },
			ambiguous: func(string) bool { return false },
			link: func(issueID pgtype.UUID) (int64, error) {
				return h.Queries.LinkIssueToVCSPullRequest(ctx, db.LinkIssueToVCSPullRequestParams{IssueID: issueID, PullRequestID: pr.ID})
			},
			unlink: func(issueID pgtype.UUID) (int64, error) {
				return h.Queries.UnlinkIssueFromVCSPullRequest(ctx, db.UnlinkIssueFromVCSPullRequestParams{IssueID: issueID, PullRequestID: pr.ID})
			},
			listAuto: func() ([]pgtype.UUID, error) {
				return h.Queries.ListAutoLinkedIssueIDsForVCSPullRequest(ctx, pr.ID)
			},
		})
		if ev.State == "merged" && prevState != "merged" {
			issueIDs, err := h.Queries.ListIssueIDsForVCSPullRequest(ctx, pr.ID)
			if err != nil {
				if conn.Provider == string(vcs.KindGitLab) {
					return err
				}
				slog.Warn("vcs: list linked issues failed", "err", err)
			}
			for _, id := range issueIDs {
				touched[id] = struct{}{}
			}
		}
		resolver := issuestatus.NewResolver(conn.WorkspaceID)
		for issueID := range touched {
			h.maybeAutoCompleteIssue(ctx, conn.WorkspaceID, issueID, resolver)
		}
	}

	h.publish(protocol.EventPullRequestUpdated, workspaceID, "system", "", map[string]any{
		"pull_request":     resp,
		"linked_issue_ids": linkedIssueIDs,
	})
	if pendingGitLabPipelineSync != nil {
		if err := h.syncVCSGitLabWakeupPipelines(ctx, pendingGitLabPipelineSync); err != nil {
			if isGitLabRefreshPersistenceError(err) {
				return err
			}
			slog.Warn("vcs: merge request Pipeline refresh failed", "reason", gitLabWakeupFailureMessage(err), "pr_id", uuidToString(pr.ID))
		}
	}
	return nil
}

func (h *Handler) mirrorVCSCIStatus(ctx context.Context, conn db.VcsConnection, ev vcs.CIStatusEvent) error {
	if ev.SHA == "" || ev.State == "" {
		return nil
	}
	if conn.Provider == string(vcs.KindGitLab) {
		return h.syncGitLabPipelineEvent(ctx, conn, ev)
	}
	if err := h.Queries.UpsertVCSCommitStatus(ctx, db.UpsertVCSCommitStatusParams{
		ConnectionID: conn.ID,
		Sha:          ev.SHA,
		Context:      ev.Context,
		State:        ev.State,
		TargetUrl:    ptrToText(strPtrOrNil(ev.TargetURL)),
		Description:  ptrToText(strPtrOrNil(ev.Description)),
		UpdatedAt:    parseGHTimeRequired(ev.UpdatedAt),
	}); err != nil {
		return err
	}

	issueIDs, err := h.Queries.ListIssueIDsForVCSPRHead(ctx, db.ListIssueIDsForVCSPRHeadParams{
		ConnectionID: conn.ID,
		HeadSha:      ev.SHA,
	})
	if err != nil {
		return err
	}
	workspaceID := uuidToString(conn.WorkspaceID)
	for _, issueID := range issueIDs {
		h.publish(protocol.EventPullRequestUpdated, workspaceID, "system", "", map[string]any{
			"issue_id": uuidToString(issueID),
		})
	}
	return nil
}

func (h *Handler) syncGitLabPipelineEvent(ctx context.Context, conn db.VcsConnection, ev vcs.CIStatusEvent) error {
	candidates := make(map[string]db.VcsPullRequest)
	byProject, err := h.Queries.ListVCSGitLabPullRequestsByProjectHead(ctx, db.ListVCSGitLabPullRequestsByProjectHeadParams{
		WorkspaceID:  conn.WorkspaceID,
		ConnectionID: conn.ID,
		RepoOwner:    ev.RepoOwner,
		RepoName:     ev.RepoName,
		HeadSha:      ev.SHA,
	})
	if err != nil {
		return err
	}
	for _, pr := range byProject {
		if vcsGitLabPipelineRefMayMatchMR(ev.Ref, pr) {
			candidates[uuidToString(pr.ID)] = pr
		}
	}
	bySource, err := h.Queries.ListVCSGitLabPullRequestsBySourceProjectHead(ctx, db.ListVCSGitLabPullRequestsBySourceProjectHeadParams{
		WorkspaceID:     conn.WorkspaceID,
		ConnectionID:    conn.ID,
		SourceProjectID: pgtype.Int8{Int64: ev.ProjectID, Valid: ev.ProjectID > 0},
		HeadSha:         ev.SHA,
	})
	if err != nil {
		return err
	}
	for _, pr := range bySource {
		if vcsGitLabPipelineRefMayMatchMR(ev.Ref, pr) {
			candidates[uuidToString(pr.ID)] = pr
		}
	}
	for _, pr := range candidates {
		if err := h.syncGitLabWakeupTargetForWebhook(ctx, vcs.GitLabWakeupTarget{
			Source:         vcs.GitLabWakeupSourceVCSConnection,
			WorkspaceID:    uuidToString(conn.WorkspaceID),
			ConnectionID:   uuidToString(conn.ID),
			MergeRequestID: uuidToString(pr.ID),
			Repository:     pr.RepoOwner + "/" + pr.RepoName,
			Number:         pr.PrNumber,
		}); err != nil {
			return err
		}
	}
	return nil
}

func vcsGitLabPipelineRefMayMatchMR(ref string, pr db.VcsPullRequest) bool {
	if ref == "" {
		return true
	}
	if pr.Branch.Valid && strings.TrimPrefix(ref, "refs/heads/") == pr.Branch.String {
		return true
	}
	parts := strings.Split(ref, "/")
	if len(parts) == 4 && parts[0] == "refs" && parts[1] == "merge-requests" && (parts[3] == "head" || parts[3] == "merge") {
		number, err := strconv.ParseInt(parts[2], 10, 32)
		return err == nil && int32(number) == pr.PrNumber
	}
	return false
}
