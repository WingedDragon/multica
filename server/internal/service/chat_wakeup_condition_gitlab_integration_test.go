package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/util"
)

type chatGitLabWakeupSyncFunc func(context.Context, vcs.GitLabWakeupTarget) error

func (f chatGitLabWakeupSyncFunc) SyncGitLabWakeupTarget(ctx context.Context, target vcs.GitLabWakeupTarget) error {
	return f(ctx, target)
}

func newChatGitLabMirror(t *testing.T, f *chatWakeupFixture, repository string, number int, state, head string) (string, string) {
	t.Helper()
	fixture := principalFixture{Fixture: f.fx, q: f.q}
	issue := util.MustParseUUID(f.fx.Issue(t, "GitLab wakeup target"))
	connection := newConnection(t, fixture)
	instanceURL := "https://" + strings.ReplaceAll(connection, "-", "") + ".gitlab.example.test"
	pr := linkConditionVCSGitLabMR(t, fixture, issue, connection, instanceURL, repository, int64(number+1000), number, state, head)
	return connection, pr
}

func chatGitLabConditionJSON(t *testing.T, connection, repository string, number int, event string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "pull_request", "provider": "gitlab", "connection_id": connection,
		"repo": repository, "number": number, "event": event,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func installChatGitLabSyncer(f *chatWakeupFixture, syncer chatGitLabWakeupSyncFunc) {
	f.svc.Tasks.GitLabWakeupSyncer = syncer
}

func markChatGitLabPipelineSyncSuccess(t *testing.T, f *chatWakeupFixture, target vcs.GitLabWakeupTarget) {
	t.Helper()
	f.fx.Exec(t, `UPDATE vcs_pull_request
		SET status_sync_at=now(),status_sync_error=NULL,pipeline_sync_at=now(),pipeline_sync_error=NULL
		WHERE id=$1 AND workspace_id=$2`, target.MergeRequestID, target.WorkspaceID)
}

func setChatVCSGitLabPipeline(t *testing.T, f *chatWakeupFixture, mr string, id int64, sha, status string, updatedAt time.Time) {
	t.Helper()
	f.fx.Exec(t, `INSERT INTO vcs_gitlab_pipeline(
		workspace_id,connection_id,pull_request_id,pipeline_id,pipeline_project_id,pipeline_project_path,sha,ref,status,pipeline_updated_at
	)
	SELECT pr.workspace_id,pr.connection_id,pr.id,$2,COALESCE(pr.source_project_id,0),
		pr.repo_owner||'/'||pr.repo_name,$3,'refs/merge-requests/'||pr.pr_number||'/head',$4,$5
	FROM vcs_pull_request pr WHERE pr.id=$1
	ON CONFLICT(pull_request_id,pipeline_project_path,pipeline_id) DO UPDATE SET
		pipeline_project_id=EXCLUDED.pipeline_project_id,pipeline_project_path=EXCLUDED.pipeline_project_path,
		sha=EXCLUDED.sha,ref=EXCLUDED.ref,status=EXCLUDED.status,pipeline_updated_at=EXCLUDED.pipeline_updated_at`,
		mr, id, sha, status, updatedAt)
}

func setChatVCSGitLabPipelineIdentity(t *testing.T, f *chatWakeupFixture, mr string, id, projectID int64, projectPath, sha, ref, status string, updatedAt time.Time) {
	t.Helper()
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "skipped", "passed":
		status = "passed"
	case "failed", "failure", "canceled", "cancelled":
		status = "failed"
	default:
		status = "pending"
	}
	f.fx.Exec(t, `INSERT INTO vcs_gitlab_pipeline(
		workspace_id,connection_id,pull_request_id,pipeline_id,pipeline_project_id,pipeline_project_path,sha,ref,status,pipeline_updated_at
	)
	SELECT pr.workspace_id,pr.connection_id,pr.id,$2,$3,$4,$5,$6,$7,$8
	FROM vcs_pull_request pr WHERE pr.id=$1 AND pr.workspace_id=$9
	ON CONFLICT(pull_request_id,pipeline_project_path,pipeline_id) DO UPDATE SET
		pipeline_project_id=EXCLUDED.pipeline_project_id,sha=EXCLUDED.sha,ref=EXCLUDED.ref,
		status=EXCLUDED.status,pipeline_updated_at=EXCLUDED.pipeline_updated_at`,
		mr, id, projectID, projectPath, sha, ref, status, updatedAt, f.fx.WorkspaceID)
}

func TestChatWakeupGitLabMergeConditionUsesCanonicalConnectionAndProject(t *testing.T) {
	f := newChatWakeupFixture(t)
	const repository = "group/subgroup/project"
	connection, pr := newChatGitLabMirror(t, f, repository, 71, "merged", "head-a")
	otherConnection, _ := newChatGitLabMirror(t, f, repository, 71, "open", "head-other")
	// Official VCS connections may receive authorized project/group/System Hooks
	// without an explicit project binding; the workspace-scoped PR is still a
	// valid target.
	f.fx.Exec(t, "DELETE FROM vcs_gitlab_project WHERE workspace_id=$1 AND connection_id=$2", f.fx.WorkspaceID, connection)
	if otherConnection == connection {
		t.Fatal("test fixtures unexpectedly share a VCS connection")
	}

	var targets []vcs.GitLabWakeupTarget
	installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
		targets = append(targets, target)
		markChatGitLabPipelineSyncSuccess(t, f, target)
		return nil
	}))
	w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 71, "merged")})
	var condition ChatWakeupCondition
	if err := json.Unmarshal(w.Condition, &condition); err != nil {
		t.Fatal(err)
	}
	if condition.Provider != "gitlab" || condition.ConnectionID != connection || condition.Repo != repository ||
		condition.GitLab == nil || condition.GitLab.MergeRequestID != pr {
		t.Fatalf("normalized canonical GitLab identity = %+v", condition)
	}
	f.dueNow(t, w.ID)
	got := f.process(t, w.ID)
	if got.Status != "fired" || !got.LastTaskID.Valid ||
		f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 1 {
		t.Fatalf("merged condition did not fire from its exact canonical target: %+v", got)
	}
	f.turn(t, got)
	if len(targets) < 2 {
		t.Fatalf("expected baseline and due-evaluation refreshes, got %d", len(targets))
	}
	for _, target := range targets {
		if target.Source != vcs.GitLabWakeupSourceVCSConnection || target.ConnectionID != connection ||
			target.MergeRequestID != pr || target.Repository != repository || target.Number != 71 {
			t.Fatalf("sync lost canonical GitLab target identity: %+v", target)
		}
	}
}

func TestChatWakeupGitLabChecksKeepBaselineAndCollapseDuplicatePipeline(t *testing.T) {
	f := newChatWakeupFixture(t)
	const repository = "group/subgroup/project"
	connection, pr := newChatGitLabMirror(t, f, repository, 72, "open", "same-head")
	baselineAt := time.Now().Add(-time.Minute)
	setChatVCSGitLabPipeline(t, f, pr, 1101, "same-head", "passed", baselineAt)
	installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
		markChatGitLabPipelineSyncSuccess(t, f, target)
		return nil
	}))

	w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 72, "checks_finished")})
	var stored ChatWakeupCondition
	if err := json.Unmarshal(w.Condition, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.GitLab == nil || !stored.GitLab.BaselineAvailable ||
		stored.GitLab.BaselineHeadSHA != "same-head" || len(stored.GitLab.BaselinePipelines) != 1 {
		t.Fatalf("GitLab check baseline was not captured: %+v", stored.GitLab)
	}
	f.dueNow(t, w.ID)
	if got := f.process(t, w.ID); got.Status != "active" || got.LastTaskID.Valid ||
		f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 0 {
		t.Fatalf("baseline pipeline fired immediately: %+v", got)
	}

	updatedAt := w.CreatedAt.Time.Add(time.Second)
	setChatVCSGitLabPipeline(t, f, pr, 1102, "same-head", "passed", updatedAt)
	setChatVCSGitLabPipeline(t, f, pr, 1102, "same-head", "failed", updatedAt.Add(time.Second))
	if got := f.fx.Count(t, "SELECT count(*) FROM vcs_gitlab_pipeline WHERE pull_request_id=$1 AND pipeline_id=1102", pr); got != 1 {
		t.Fatalf("duplicate pipeline delivery created %d canonical rows, want one", got)
	}
	f.dueNow(t, w.ID)
	got := f.process(t, w.ID)
	if got.Status != "fired" || got.LastError.Valid {
		t.Fatalf("new same-head pipeline did not fire cleanly: %+v", got)
	}
	_, input := f.turn(t, got)
	for _, want := range []string{`"pipeline_id":"1102"`, `"pipeline_status":"failure"`} {
		if !strings.Contains(input.Content, want) {
			t.Fatalf("observed GitLab pipeline facts lack %s:\n%s", want, input.Content)
		}
	}
}

func TestChatWakeupGitLabChecksIgnoreOldPipelineAfterEnrichmentRetry(t *testing.T) {
	f := newChatWakeupFixture(t)
	const repository = "group/subgroup/project"
	connection, pr := newChatGitLabMirror(t, f, repository, 82, "open", "same-head")
	baselineAt := time.Now().Add(-time.Minute)
	setChatVCSGitLabPipeline(t, f, pr, 1101, "same-head", "passed", baselineAt)

	syncCalls := 0
	installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
		syncCalls++
		if syncCalls == 2 {
			return errors.New("injected MR enrichment failure")
		}
		markChatGitLabPipelineSyncSuccess(t, f, target)
		return nil
	}))
	w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 82, "checks_finished")})
	if syncCalls != 1 {
		t.Fatalf("baseline sync calls = %d, want 1", syncCalls)
	}

	f.dueNow(t, w.ID)
	failed := f.process(t, w.ID)
	if failed.Status != "active" || !failed.LastError.Valid || !strings.Contains(failed.LastError.String, "injected MR enrichment failure") ||
		failed.LastTaskID.Valid || f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 0 {
		t.Fatalf("failed MR enrichment fired or lost its retryable error: %+v", failed)
	}

	f.dueNow(t, w.ID)
	retried := f.process(t, w.ID)
	if retried.Status != "active" || retried.LastTaskID.Valid || retried.LastError.Valid ||
		f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 0 {
		t.Fatalf("retry of the old terminal pipeline fired or stayed pending: %+v", retried)
	}
	var condition ChatWakeupCondition
	if err := json.Unmarshal(retried.Condition, &condition); err != nil {
		t.Fatal(err)
	}
	if condition.GitLab == nil || !condition.GitLab.BaselineAvailable ||
		condition.GitLab.BaselineHeadSHA != "same-head" || len(condition.GitLab.BaselinePipelines) != 1 ||
		condition.GitLab.BaselinePipelines[0].PipelineID != "1101" {
		t.Fatalf("successful enrichment retry changed the stored baseline: %+v", condition.GitLab)
	}

	setChatVCSGitLabPipeline(t, f, pr, 1102, "same-head", "failed", w.CreatedAt.Time.Add(time.Second))
	f.dueNow(t, w.ID)
	fired := f.process(t, w.ID)
	if fired.Status != "fired" || !fired.LastTaskID.Valid || fired.LastError.Valid {
		t.Fatalf("new terminal pipeline for the same head did not fire after retry: %+v", fired)
	}
	_, input := f.turn(t, fired)
	for _, want := range []string{`"pipeline_id":"1102"`, `"pipeline_status":"failure"`} {
		if !strings.Contains(input.Content, want) {
			t.Fatalf("new terminal pipeline facts lack %s:\n%s", want, input.Content)
		}
	}
}

func TestChatWakeupGitLabMigrationAliasesAreWorkspaceScoped(t *testing.T) {
	ctx := context.Background()
	const repository = "group/subgroup/project"
	f := newChatWakeupFixture(t)
	canonicalConnection, canonicalMR := newChatGitLabMirror(t, f, repository, 73, "open", "same-head")
	setChatVCSGitLabPipeline(t, f, canonicalMR, 1101, "same-head", "passed", time.Now().Add(-time.Minute))
	var synced []vcs.GitLabWakeupTarget
	installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
		synced = append(synced, target)
		markChatGitLabPipelineSyncSuccess(t, f, target)
		return nil
	}))
	w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, canonicalConnection, repository, 73, "checks_finished")})
	var condition ChatWakeupCondition
	if err := json.Unmarshal(w.Condition, &condition); err != nil {
		t.Fatal(err)
	}
	if condition.GitLab == nil || !condition.GitLab.BaselineAvailable || len(condition.GitLab.BaselinePipelines) != 1 {
		t.Fatalf("CI baseline was not retained before alias resolution: %+v", condition.GitLab)
	}

	fixture := principalFixture{Fixture: f.fx, q: f.q}
	legacyConnection, legacyMR := newConnection(t, fixture), newConnection(t, fixture)
	f.fx.Exec(t, `INSERT INTO vcs_gitlab_migration_id_map(workspace_id,entity_kind,legacy_id,unified_id)
		VALUES($1,'connection',$2,$3),($1,'pull_request',$4,$5)`,
		f.fx.WorkspaceID, legacyConnection, canonicalConnection, legacyMR, canonicalMR)
	condition.ConnectionID = legacyConnection
	condition.GitLab.Source = vcs.GitLabWakeupSourceWorkspaceConnection
	condition.GitLab.MergeRequestID = legacyMR
	raw, err := json.Marshal(condition)
	if err != nil {
		t.Fatal(err)
	}
	f.fx.Exec(t, "UPDATE chat_wakeup SET condition=$2 WHERE id=$1", util.UUIDToString(w.ID), string(raw))
	setChatVCSGitLabPipeline(t, f, canonicalMR, 1102, "same-head", "failed", w.CreatedAt.Time.Add(time.Second))
	f.dueNow(t, w.ID)
	got := f.process(t, w.ID)
	if got.Status != "fired" || got.LastError.Valid {
		t.Fatalf("old connection and MR IDs did not resolve through aliases: %+v", got)
	}
	if !got.ExpiresAt.Time.Equal(w.ExpiresAt.Time) {
		t.Fatalf("resolving old IDs changed the deadline: %v -> %v", w.ExpiresAt.Time, got.ExpiresAt.Time)
	}
	if err := json.Unmarshal(got.Condition, &condition); err != nil {
		t.Fatal(err)
	}
	if condition.ConnectionID != legacyConnection || condition.GitLab == nil ||
		condition.GitLab.MergeRequestID != legacyMR || !condition.GitLab.BaselineAvailable ||
		condition.GitLab.BaselineHeadSHA != "same-head" || len(condition.GitLab.BaselinePipelines) != 1 ||
		condition.GitLab.BaselinePipelines[0].PipelineID != "1101" {
		t.Fatalf("stored target or CI baseline changed while resolving aliases: %+v", condition)
	}
	_, input := f.turn(t, got)
	for _, want := range []string{`"pipeline_id":"1102"`, `"pipeline_status":"failure"`} {
		if !strings.Contains(input.Content, want) {
			t.Fatalf("alias-resolved result lacks %s:\n%s", want, input.Content)
		}
	}
	if len(synced) < 2 {
		t.Fatalf("expected baseline and due-evaluation refreshes, got %d", len(synced))
	}
	for _, target := range synced {
		if target.Source != vcs.GitLabWakeupSourceVCSConnection || target.WorkspaceID != f.fx.WorkspaceID ||
			target.ConnectionID != canonicalConnection || target.MergeRequestID != canonicalMR {
			t.Fatalf("sync used a noncanonical or foreign target: %+v", target)
		}
	}

	other := newChatWakeupFixture(t)
	otherConnection, otherMR := newChatGitLabMirror(t, other, repository, 73, "open", "same-head")
	other.fx.Exec(t, `INSERT INTO vcs_gitlab_migration_id_map(workspace_id,entity_kind,legacy_id,unified_id)
		VALUES($1,'connection',$2,$3),($1,'pull_request',$4,$5)`,
		other.fx.WorkspaceID, legacyConnection, otherConnection, legacyMR, otherMR)
	t.Cleanup(func() {
		_, _ = f.fx.Pool.Exec(ctx, "DELETE FROM vcs_gitlab_migration_id_map WHERE workspace_id IN ($1,$2)",
			f.fx.WorkspaceID, other.fx.WorkspaceID)
	})
	resolve := func(f *chatWakeupFixture) vcs.GitLabWakeupTarget {
		t.Helper()
		tx, err := f.fx.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		target, err := canonicalGitLabWakeupTargetTx(ctx, tx, vcs.GitLabWakeupTarget{
			Source: vcs.GitLabWakeupSourceWorkspaceConnection, WorkspaceID: f.fx.WorkspaceID,
			ConnectionID: legacyConnection, MergeRequestID: legacyMR,
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return target
	}
	if target := resolve(f); target.ConnectionID != canonicalConnection || target.MergeRequestID != canonicalMR {
		t.Fatalf("another workspace's alias shadowed this workspace's mapping: %+v", target)
	}
	if target := resolve(other); target.ConnectionID != otherConnection || target.MergeRequestID != otherMR {
		t.Fatalf("legacy IDs did not resolve to the other workspace's own mapping: %+v", target)
	}
}

func TestChatWakeupGitLabMigratedFingerprintDoesNotRetriggerSameResult(t *testing.T) {
	const repository = "group/subgroup/project"
	f := newChatWakeupFixture(t)
	connection, mr := newChatGitLabMirror(t, f, repository, 77, "open", "head")
	installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
		markChatGitLabPipelineSyncSuccess(t, f, target)
		return nil
	}))
	w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 77, "merged")})
	legacyConnection := newConnection(t, principalFixture{Fixture: f.fx, q: f.q})
	legacyMR := newConnection(t, principalFixture{Fixture: f.fx, q: f.q})
	f.fx.Exec(t, `INSERT INTO vcs_gitlab_migration_id_map(workspace_id,entity_kind,legacy_id,unified_id)
		VALUES($1,'connection',$2,$3),($1,'pull_request',$4,$5)`,
		f.fx.WorkspaceID, legacyConnection, connection, legacyMR, mr)
	t.Cleanup(func() {
		_, _ = f.fx.Pool.Exec(context.Background(), "DELETE FROM vcs_gitlab_migration_id_map WHERE workspace_id=$1", f.fx.WorkspaceID)
	})
	migratedFingerprint := strings.Join([]string{
		"gitlab", string(vcs.GitLabWakeupSourceVCSConnection), connection, mr, "state", "merged",
	}, ":")
	f.fx.Exec(t, "UPDATE chat_wakeup SET condition_state=$2 WHERE id=$1", util.UUIDToString(w.ID), migratedFingerprint)
	f.fx.Exec(t, "UPDATE vcs_pull_request SET state='merged' WHERE id=$1 AND workspace_id=$2", mr, f.fx.WorkspaceID)
	f.dueNow(t, w.ID)
	alreadyObserved := f.process(t, w.ID)
	if alreadyObserved.Status != "active" || alreadyObserved.LastTaskID.Valid ||
		alreadyObserved.ConditionState != migratedFingerprint || alreadyObserved.LastError.Valid {
		t.Fatalf("migrated current-state fingerprint fired or was discarded: %+v", alreadyObserved)
	}
	if f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 0 {
		t.Fatal("the result observed before migration fired again")
	}

	f.fx.Exec(t, "UPDATE vcs_pull_request SET state='open' WHERE id=$1 AND workspace_id=$2", mr, f.fx.WorkspaceID)
	f.dueNow(t, w.ID)
	reset := f.process(t, w.ID)
	if reset.Status != "active" || reset.ConditionState != "" || reset.LastTaskID.Valid {
		t.Fatalf("condition did not reset after the observed result cleared: %+v", reset)
	}
	f.fx.Exec(t, "UPDATE vcs_pull_request SET state='merged' WHERE id=$1 AND workspace_id=$2", mr, f.fx.WorkspaceID)
	f.dueNow(t, w.ID)
	if fired := f.process(t, w.ID); fired.Status != "fired" || !fired.LastTaskID.Valid {
		t.Fatalf("new post-migration result did not trigger: %+v", fired)
	}
}

func TestChatWakeupGitLabChecksRejectUnknownPipelineIdentityUntilValidResult(t *testing.T) {
	const repository = "group/subgroup/project"
	f := newChatWakeupFixture(t)
	connection, mr := newChatGitLabMirror(t, f, repository, 75, "open", "same-head")
	installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
		markChatGitLabPipelineSyncSuccess(t, f, target)
		return nil
	}))
	w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 75, "checks_finished")})
	setChatVCSGitLabPipelineIdentity(t, f, mr, 1301, 99001, "other/group/project", "same-head",
		"refs/merge-requests/75/head", "passed", w.CreatedAt.Time.Add(time.Second))
	f.dueNow(t, w.ID)
	pending := f.process(t, w.ID)
	if pending.Status != "active" || pending.LastTaskID.Valid || !pending.LastError.Valid ||
		!strings.Contains(pending.LastError.String, "unknown project identity") {
		t.Fatalf("unknown pipeline identity was not held as pending: %+v", pending)
	}
	f.fx.Exec(t, "DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id=$1 AND pipeline_id=1301", mr)
	setChatVCSGitLabPipelineIdentity(t, f, mr, 1302, 1075, repository, "same-head",
		"refs/merge-requests/75/head", "failed", w.CreatedAt.Time.Add(time.Minute))
	f.dueNow(t, w.ID)
	if got := f.process(t, w.ID); got.Status != "fired" || !got.LastTaskID.Valid || got.LastError.Valid {
		t.Fatalf("valid current-project pipeline did not recover the wakeup: %+v", got)
	}
}

func TestChatWakeupGitLabForkPipelineMustMatchSourceBranch(t *testing.T) {
	const repository = "group/subgroup/project"
	const sourcePath = "fork/group/project"
	const sourceBranch = "feature/branch"
	f := newChatWakeupFixture(t)
	connection, mr := newChatGitLabMirror(t, f, repository, 76, "open", "same-head")
	f.fx.Exec(t, "UPDATE vcs_pull_request SET source_project_id=$2,source_project_path=$3,branch=$4 WHERE id=$1",
		mr, int64(8876), sourcePath, sourceBranch)
	installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
		markChatGitLabPipelineSyncSuccess(t, f, target)
		return nil
	}))
	w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 76, "checks_finished")})
	setChatVCSGitLabPipelineIdentity(t, f, mr, 1401, 8876, sourcePath, "same-head",
		"wrong/branch", "passed", w.CreatedAt.Time.Add(time.Second))
	f.dueNow(t, w.ID)
	pending := f.process(t, w.ID)
	if pending.Status != "active" || pending.LastTaskID.Valid || !pending.LastError.Valid ||
		!strings.Contains(pending.LastError.String, "no terminal pipeline") {
		t.Fatalf("fork pipeline from another branch was accepted: %+v", pending)
	}
	setChatVCSGitLabPipelineIdentity(t, f, mr, 1402, 8876, sourcePath, "same-head",
		sourceBranch, "failed", w.CreatedAt.Time.Add(time.Minute))
	f.dueNow(t, w.ID)
	got := f.process(t, w.ID)
	if got.Status != "fired" || !got.LastTaskID.Valid || got.LastError.Valid {
		t.Fatalf("current-head fork pipeline on the MR branch did not fire: %+v", got)
	}
	_, input := f.turn(t, got)
	for _, want := range []string{`"pipeline_project_id":"8876"`, `"pipeline_project_path":"fork/group/project"`,
		`"pipeline_ref":"feature/branch"`, `"pipeline_id":"1402"`} {
		if !strings.Contains(input.Content, want) {
			t.Fatalf("fork pipeline facts lack %s:\n%s", want, input.Content)
		}
	}
}

func TestChatWakeupGitLabUnavailableTargetDoesNotRedirectToSameName(t *testing.T) {
	const repository = "group/subgroup/project"
	f := newChatWakeupFixture(t)
	connection, mr := newChatGitLabMirror(t, f, repository, 74, "open", "target-head")
	otherConnection, _ := newChatGitLabMirror(t, f, repository, 74, "merged", "other-head")
	if connection == otherConnection {
		t.Fatal("test fixtures unexpectedly share a VCS connection")
	}
	targetUnavailable := false
	mergeTargetOnRecovery := false
	installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
		if target.ConnectionID != connection || target.MergeRequestID != mr {
			t.Fatalf("syncer received a different GitLab target: %+v", target)
		}
		if targetUnavailable {
			return errors.New("GitLab API unavailable")
		}
		markChatGitLabPipelineSyncSuccess(t, f, target)
		if mergeTargetOnRecovery {
			f.fx.Exec(t, `UPDATE vcs_pull_request
				SET state='merged'
				WHERE id=$1 AND workspace_id=$2 AND connection_id=$3`,
				target.MergeRequestID, target.WorkspaceID, target.ConnectionID)
		}
		return nil
	}))
	w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 74, "merged")})
	targetUnavailable = true
	f.dueNow(t, w.ID)
	pending := f.process(t, w.ID)
	if pending.Status != "active" || pending.LastTaskID.Valid || !pending.LastError.Valid ||
		!strings.Contains(pending.LastError.String, "GitLab API unavailable") {
		t.Fatalf("unavailable pinned target did not remain pending with the API failure reason: %+v", pending)
	}
	if f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 0 {
		t.Fatal("same-name MR from a different connection triggered the wakeup")
	}
	var stored ChatWakeupCondition
	if err := json.Unmarshal(pending.Condition, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ConnectionID != connection || stored.GitLab == nil || stored.GitLab.MergeRequestID != mr {
		t.Fatalf("unavailable wakeup target was rebound: %+v", stored)
	}

	targetUnavailable = false
	mergeTargetOnRecovery = true
	f.dueNow(t, w.ID)
	if recovered := f.process(t, w.ID); recovered.Status != "fired" || !recovered.LastTaskID.Valid {
		t.Fatalf("pinned target did not recover after GitLab became available: %+v", recovered)
	}

}

func TestChatWakeupGitLabRemovedLegacyConnectionDoesNotRedirect(t *testing.T) {
	const repository = "group/subgroup/project"
	f := newChatWakeupFixture(t)
	connection, mr := newChatGitLabMirror(t, f, repository, 79, "open", "target-head")
	otherConnection, _ := newChatGitLabMirror(t, f, repository, 79, "merged", "other-head")
	if connection == otherConnection {
		t.Fatal("test fixtures unexpectedly share a VCS connection")
	}
	installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
		markChatGitLabPipelineSyncSuccess(t, f, target)
		return nil
	}))
	w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 79, "merged")})
	var condition ChatWakeupCondition
	if err := json.Unmarshal(w.Condition, &condition); err != nil {
		t.Fatal(err)
	}
	fixture := principalFixture{Fixture: f.fx, q: f.q}
	legacyConnection, legacyMR := newConnection(t, fixture), newConnection(t, fixture)
	f.fx.Exec(t, `INSERT INTO vcs_gitlab_migration_id_map(workspace_id,entity_kind,legacy_id,unified_id)
		VALUES($1,'connection',$2,$3),($1,'pull_request',$4,$5)`,
		f.fx.WorkspaceID, legacyConnection, connection, legacyMR, mr)
	condition.ConnectionID = legacyConnection
	condition.GitLab.Source = vcs.GitLabWakeupSourceWorkspaceConnection
	condition.GitLab.MergeRequestID = legacyMR
	raw, err := json.Marshal(condition)
	if err != nil {
		t.Fatal(err)
	}
	f.fx.Exec(t, "UPDATE chat_wakeup SET condition=$2 WHERE id=$1", util.UUIDToString(w.ID), string(raw))

	// Connection deletion removes its aliases and mirrors. The historical ID
	// must remain unavailable rather than selecting another same-name target.
	f.fx.Exec(t, "DELETE FROM vcs_gitlab_migration_id_map WHERE workspace_id=$1 AND legacy_id IN ($2,$3)",
		f.fx.WorkspaceID, legacyConnection, legacyMR)
	f.fx.Exec(t, "DELETE FROM issue_vcs_pull_request WHERE pull_request_id=$1", mr)
	f.fx.Exec(t, "DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id=$1", mr)
	f.fx.Exec(t, "DELETE FROM vcs_gitlab_project WHERE workspace_id=$1 AND connection_id=$2", f.fx.WorkspaceID, connection)
	f.fx.Exec(t, "DELETE FROM vcs_pull_request WHERE id=$1 AND workspace_id=$2", mr, f.fx.WorkspaceID)
	f.fx.Exec(t, "DELETE FROM vcs_connection WHERE id=$1 AND workspace_id=$2", connection, f.fx.WorkspaceID)

	f.dueNow(t, w.ID)
	pending := f.process(t, w.ID)
	if pending.Status != "active" || pending.LastTaskID.Valid || !pending.LastError.Valid ||
		!strings.Contains(pending.LastError.String, "legacy connection alias is unavailable") {
		t.Fatalf("deleted connection alias did not fail closed with a reason: %+v", pending)
	}
	if f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 0 {
		t.Fatal("same-name MR from a replacement connection triggered the wakeup")
	}
	var stored ChatWakeupCondition
	if err := json.Unmarshal(pending.Condition, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ConnectionID != legacyConnection || stored.GitLab == nil || stored.GitLab.MergeRequestID != legacyMR {
		t.Fatalf("deleted wakeup target was silently rebound: %+v", stored)
	}
}

func TestChatWakeupGitLabConditionRejectsUnmirroredTarget(t *testing.T) {
	f := newChatWakeupFixture(t)
	const repository = "group/subgroup/project"
	connection, _ := newChatGitLabMirror(t, f, repository, 74, "open", "head")
	_, err := f.svc.Create(context.Background(), f.source, ChatWakeupInput{
		Title: "x", Instruction: "y",
		Condition: chatGitLabConditionJSON(t, connection, "other/subgroup/project", 74, "merged"),
	})
	if !errors.Is(err, ErrChatWakeupInput) || !strings.Contains(err.Error(), "not mirrored") {
		t.Fatalf("unmirrored exact target: error = %v, want an actionable input error", err)
	}
}
