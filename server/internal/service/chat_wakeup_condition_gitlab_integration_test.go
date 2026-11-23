package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/util"
)

type chatGitLabWakeupSyncFunc func(context.Context, vcs.GitLabWakeupTarget) error

func (f chatGitLabWakeupSyncFunc) SyncGitLabWakeupTarget(ctx context.Context, target vcs.GitLabWakeupTarget) error {
	return f(ctx, target)
}

func newChatGitLabMirror(t *testing.T, f *chatWakeupFixture, source, repository string, number int, state, head string) (string, string) {
	t.Helper()
	fixture := principalFixture{Fixture: f.fx, q: f.q}
	issue := util.MustParseUUID(f.fx.Issue(t, "GitLab wakeup target"))
	projectID := int64(number + 1000)
	if source == "gitlab_connection" {
		connection, _ := newConditionGitLabConnection(t, fixture)
		mr := linkConditionGitLabMR(t, fixture, issue, connection, repository, projectID, number, state, head)
		return connection, mr
	}
	connection := newConnection(t, fixture)
	instanceURL := "https://" + strings.ReplaceAll(connection, "-", "") + ".gitlab.example.test"
	mr := linkConditionVCSGitLabMR(t, fixture, issue, connection, instanceURL, repository, projectID, number, state, head)
	return connection, mr
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
	if target.Source == vcs.GitLabWakeupSourceWorkspaceConnection {
		f.fx.Exec(t, "UPDATE gitlab_merge_request SET pipeline_sync_at=now(),pipeline_sync_error=NULL WHERE id=$1", target.MergeRequestID)
	} else {
		f.fx.Exec(t, "UPDATE vcs_pull_request SET pipeline_sync_at=now(),pipeline_sync_error=NULL WHERE id=$1", target.MergeRequestID)
	}
}

func setChatLegacyGitLabPipeline(t *testing.T, f *chatWakeupFixture, mr string, id int64, sha, status string, updatedAt time.Time) {
	t.Helper()
	f.fx.Exec(t, `INSERT INTO gitlab_mr_pipeline(
		merge_request_id,pipeline_id,pipeline_project_id,sha,ref,status,pipeline_updated_at
	)
	SELECT mr.id,$2,mr.gitlab_project_id,$3,
		'refs/merge-requests/'||mr.mr_iid||'/head',$4,$5
	FROM gitlab_merge_request mr WHERE mr.id=$1
	ON CONFLICT(merge_request_id,pipeline_id) DO UPDATE SET
		pipeline_project_id=EXCLUDED.pipeline_project_id,sha=EXCLUDED.sha,ref=EXCLUDED.ref,
		status=EXCLUDED.status,pipeline_updated_at=EXCLUDED.pipeline_updated_at`,
		mr, id, sha, status, updatedAt)
}

func setChatLegacyGitLabPipelineIdentity(t *testing.T, f *chatWakeupFixture, mr string, id, projectID int64, sha, ref, status string, updatedAt time.Time) {
	t.Helper()
	f.fx.Exec(t, `INSERT INTO gitlab_mr_pipeline(
		merge_request_id,pipeline_id,pipeline_project_id,sha,ref,status,pipeline_updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7)
	ON CONFLICT(merge_request_id,pipeline_id) DO UPDATE SET
		pipeline_project_id=EXCLUDED.pipeline_project_id,sha=EXCLUDED.sha,ref=EXCLUDED.ref,
		status=EXCLUDED.status,pipeline_updated_at=EXCLUDED.pipeline_updated_at`,
		mr, id, projectID, sha, ref, status, updatedAt)
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
	f.fx.Exec(t, `INSERT INTO vcs_gitlab_pipeline(
		workspace_id,connection_id,pull_request_id,pipeline_id,pipeline_project_id,pipeline_project_path,sha,ref,status,pipeline_updated_at
	)
	SELECT pr.workspace_id,pr.connection_id,pr.id,$2,$3,$4,$5,$6,$7,$8
	FROM vcs_pull_request pr WHERE pr.id=$1
	ON CONFLICT(pull_request_id,pipeline_project_path,pipeline_id) DO UPDATE SET
		pipeline_project_id=EXCLUDED.pipeline_project_id,pipeline_project_path=EXCLUDED.pipeline_project_path,
		sha=EXCLUDED.sha,ref=EXCLUDED.ref,status=EXCLUDED.status,pipeline_updated_at=EXCLUDED.pipeline_updated_at`,
		mr, id, projectID, projectPath, sha, ref, status, updatedAt)
}

func TestChatWakeupGitLabMergeConditionsUseExactConnectionAndSubgroupPath(t *testing.T) {
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		for _, tc := range []struct {
			event string
			state string
		}{
			{event: "merged", state: "merged"},
			{event: "closed", state: "closed"},
		} {
			t.Run(source+"/"+tc.event, func(t *testing.T) {
				f := newChatWakeupFixture(t)
				repository := "group/subgroup/project"
				connection, mr := newChatGitLabMirror(t, f, source, repository, 71, tc.state, "head-a")
				otherSource := "gitlab_connection"
				if source == otherSource {
					otherSource = "vcs_connection"
				}
				newChatGitLabMirror(t, f, otherSource, repository, 71, "open", "head-other")

				var wakeupID pgtype.UUID
				var targets []vcs.GitLabWakeupTarget
				installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(ctx context.Context, target vcs.GitLabWakeupTarget) error {
					targets = append(targets, target)
					if wakeupID.Valid {
						tx, err := f.fx.Pool.Begin(ctx)
						if err != nil {
							return err
						}
						var locked string
						if err = tx.QueryRow(ctx, "SELECT id::text FROM chat_wakeup WHERE id=$1 FOR UPDATE NOWAIT", wakeupID).Scan(&locked); err != nil {
							_ = tx.Rollback(ctx)
							return err
						}
						if err = tx.Commit(ctx); err != nil {
							return err
						}
					}
					switch target.Source {
					case vcs.GitLabWakeupSourceWorkspaceConnection:
						f.fx.Exec(t, "UPDATE gitlab_merge_request SET last_refreshed_at=now(),last_refresh_error=NULL WHERE id=$1 AND workspace_id=$2", target.MergeRequestID, target.WorkspaceID)
					case vcs.GitLabWakeupSourceVCSConnection:
						f.fx.Exec(t, "UPDATE vcs_pull_request SET status_sync_at=now(),status_sync_error=NULL WHERE id=$1 AND workspace_id=$2", target.MergeRequestID, target.WorkspaceID)
					}
					return nil
				}))

				w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 71, tc.event)})
				wakeupID = w.ID
				var condition ChatWakeupCondition
				if err := json.Unmarshal(w.Condition, &condition); err != nil {
					t.Fatal(err)
				}
				if condition.Provider != "gitlab" || condition.ConnectionID != connection || condition.Repo != repository ||
					condition.GitLab == nil || condition.GitLab.MergeRequestID != mr {
					t.Fatalf("normalized GitLab identity = %+v", condition)
				}
				f.dueNow(t, w.ID)
				got := f.process(t, w.ID)
				if got.Status != "fired" || !got.LastTaskID.Valid ||
					f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 1 {
					t.Fatalf("%s condition did not fire from its exact mirrored target: %+v", tc.event, got)
				}
				f.turn(t, got)
				for _, target := range targets {
					if target.ConnectionID != connection || target.MergeRequestID != mr || target.Repository != repository || target.Number != 71 {
						t.Fatalf("sync lost exact GitLab target identity: %+v", target)
					}
				}
				if len(targets) != 2 {
					t.Fatalf("sync calls = %d, want create baseline and due evaluation: %+v", len(targets), targets)
				}
			})
		}
	}
}

func TestChatWakeupGitLabChecksUseCurrentHeadPipelineBaselineOnBothSources(t *testing.T) {
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		t.Run(source, func(t *testing.T) {
			f := newChatWakeupFixture(t)
			repository := "group/subgroup/project"
			connection, mr := newChatGitLabMirror(t, f, source, repository, 72, "open", "same-head")
			baselineAt := time.Now().Add(-time.Minute)
			if source == "gitlab_connection" {
				setChatLegacyGitLabPipeline(t, f, mr, 1101, "same-head", "success", baselineAt)
			} else {
				setChatVCSGitLabPipeline(t, f, mr, 1101, "same-head", "passed", baselineAt)
			}
			installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
				if target.Source == vcs.GitLabWakeupSourceWorkspaceConnection {
					f.fx.Exec(t, "UPDATE gitlab_merge_request SET last_refreshed_at=now(),last_refresh_error=NULL WHERE id=$1", target.MergeRequestID)
				} else {
					f.fx.Exec(t, "UPDATE vcs_pull_request SET status_sync_at=now(),status_sync_error=NULL WHERE id=$1", target.MergeRequestID)
				}
				markChatGitLabPipelineSyncSuccess(t, f, target)
				return nil
			}))

			w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 72, "checks_finished")})
			var stored ChatWakeupCondition
			if err := json.Unmarshal(w.Condition, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.GitLab == nil || !stored.GitLab.BaselineAvailable || stored.GitLab.BaselineHeadSHA != "same-head" || len(stored.GitLab.BaselinePipelines) != 1 {
				t.Fatalf("GitLab check baseline was not captured: %+v", stored.GitLab)
			}

			f.dueNow(t, w.ID)
			if got := f.process(t, w.ID); got.Status != "active" || got.LastTaskID.Valid ||
				f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 0 {
				t.Fatalf("baseline pipeline fired immediately: %+v", got)
			}
			newPipelineAt := w.CreatedAt.Time.Add(time.Second)
			if source == "gitlab_connection" {
				setChatLegacyGitLabPipeline(t, f, mr, 1102, "same-head", "success", newPipelineAt)
			} else {
				setChatVCSGitLabPipeline(t, f, mr, 1102, "same-head", "passed", newPipelineAt)
			}
			f.dueNow(t, w.ID)
			got := f.process(t, w.ID)
			if got.Status != "fired" || got.LastError.Valid {
				t.Fatalf("new same-head pipeline did not fire cleanly: %+v", got)
			}
			_, input := f.turn(t, got)
			for _, want := range []string{`"pipeline_project_id"`, `"pipeline_project_path":"group/subgroup/project"`, `"pipeline_ref":"refs/merge-requests/72/head"`, `"pipeline_id":"1102"`, `"pipeline_status":"success"`} {
				if !strings.Contains(input.Content, want) {
					t.Fatalf("observed GitLab pipeline facts lack %s:\n%s", want, input.Content)
				}
			}
		})
	}
}
func TestChatWakeupGitLabFailedPipelineRerunFiresOnBothSources(t *testing.T) {
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		t.Run(source, func(t *testing.T) {
			f := newChatWakeupFixture(t)
			const repository = "group/subgroup/project"
			connection, mr := newChatGitLabMirror(t, f, source, repository, 82, "open", "same-head")
			baselineAt := f.clock.Add(-time.Minute)
			if source == "gitlab_connection" {
				setChatLegacyGitLabPipeline(t, f, mr, 1901, "same-head", "success", baselineAt)
			} else {
				setChatVCSGitLabPipeline(t, f, mr, 1901, "same-head", "passed", baselineAt)
			}
			installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
				if target.Source == vcs.GitLabWakeupSourceWorkspaceConnection {
					f.fx.Exec(t, "UPDATE gitlab_merge_request SET last_refreshed_at=now(),last_refresh_error=NULL WHERE id=$1", target.MergeRequestID)
				} else {
					f.fx.Exec(t, "UPDATE vcs_pull_request SET status_sync_at=now(),status_sync_error=NULL WHERE id=$1", target.MergeRequestID)
				}
				markChatGitLabPipelineSyncSuccess(t, f, target)
				return nil
			}))

			w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 82, "checks_finished")})
			var stored ChatWakeupCondition
			if err := json.Unmarshal(w.Condition, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.GitLab == nil || !stored.GitLab.BaselineAvailable || len(stored.GitLab.BaselinePipelines) != 1 {
				t.Fatalf("GitLab check baseline was not captured: %+v", stored.GitLab)
			}

			f.dueNow(t, w.ID)
			if got := f.process(t, w.ID); got.Status != "active" || got.LastTaskID.Valid {
				t.Fatalf("baseline success fired before a rerun: %+v", got)
			}
			rerunAt := w.CreatedAt.Time.Add(time.Second)
			if source == "gitlab_connection" {
				setChatLegacyGitLabPipeline(t, f, mr, 1901, "same-head", "failed", rerunAt)
			} else {
				setChatVCSGitLabPipeline(t, f, mr, 1901, "same-head", "failed", rerunAt)
			}
			f.dueNow(t, w.ID)
			got := f.process(t, w.ID)
			if got.Status != "fired" || got.LastError.Valid || !got.LastTaskID.Valid {
				t.Fatalf("same pipeline's failed rerun did not fire: %+v", got)
			}
			_, input := f.turn(t, got)
			for _, want := range []string{`"pipeline_id":"1901"`, `"pipeline_status":"failure"`} {
				if !strings.Contains(input.Content, want) {
					t.Fatalf("failed rerun facts lack %s:\n%s", want, input.Content)
				}
			}
		})
	}
}

func TestChatWakeupGitLabChecksUseMatchingSourceBranchAndRejectUnknownPipelineIdentity(t *testing.T) {
	const (
		number         = 74
		repository     = "group/subgroup/project"
		sourcePath     = "fork/subgroup/project"
		sourceBranch   = "feature/current"
		wrongSourceRef = "feature/other"
	)
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		t.Run(source, func(t *testing.T) {
			f := newChatWakeupFixture(t)
			connection, mr := newChatGitLabMirror(t, f, source, repository, number, "open", "same-head")
			sourceProjectID, targetProjectID := int64(number+2000), int64(number+1000)
			baselineAt := f.clock.Add(-time.Minute)
			if source == "gitlab_connection" {
				f.fx.Exec(t, "UPDATE gitlab_merge_request SET source_project_id=$2,source_project_path=$3,source_branch=$4 WHERE id=$1", mr, sourceProjectID, sourcePath, sourceBranch)
				setChatLegacyGitLabPipelineIdentity(t, f, mr, 1299, targetProjectID, "same-head", "refs/merge-requests/74/head", "success", baselineAt)
			} else {
				f.fx.Exec(t, "UPDATE vcs_pull_request SET source_project_id=$2,source_project_path=$3,branch=$4 WHERE id=$1", mr, sourceProjectID, sourcePath, sourceBranch)
				setChatVCSGitLabPipelineIdentity(t, f, mr, 1299, targetProjectID, repository, "same-head", "refs/merge-requests/74/head", "passed", baselineAt)
			}
			installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
				if target.Source == vcs.GitLabWakeupSourceWorkspaceConnection {
					f.fx.Exec(t, "UPDATE gitlab_merge_request SET last_refreshed_at=now(),last_refresh_error=NULL WHERE id=$1", target.MergeRequestID)
				} else {
					f.fx.Exec(t, "UPDATE vcs_pull_request SET status_sync_at=now(),status_sync_error=NULL WHERE id=$1", target.MergeRequestID)
				}
				markChatGitLabPipelineSyncSuccess(t, f, target)
				return nil
			}))
			w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, number, "checks_finished")})
			var stored ChatWakeupCondition
			if err := json.Unmarshal(w.Condition, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.GitLab == nil || !stored.GitLab.BaselineAvailable || len(stored.GitLab.BaselinePipelines) != 1 ||
				stored.GitLab.BaselinePipelines[0].PipelineProjectPath != repository ||
				stored.GitLab.BaselinePipelines[0].Ref != "refs/merge-requests/74/head" {
				t.Fatalf("fork MR target-project Pipeline was not captured as the baseline: %+v", stored.GitLab)
			}
			if source == "gitlab_connection" {
				f.fx.Exec(t, "DELETE FROM gitlab_mr_pipeline WHERE merge_request_id=$1 AND pipeline_id=1299", mr)
			} else {
				f.fx.Exec(t, "DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id=$1 AND pipeline_id=1299", mr)
			}
			pipelineAt := w.CreatedAt.Time.Add(time.Second)

			if source == "gitlab_connection" {
				setChatLegacyGitLabPipelineIdentity(t, f, mr, 1301, sourceProjectID, "same-head", wrongSourceRef, "success", pipelineAt)
			} else {
				setChatVCSGitLabPipelineIdentity(t, f, mr, 1301, sourceProjectID, sourcePath, "same-head", wrongSourceRef, "passed", pipelineAt)
			}
			f.dueNow(t, w.ID)
			pending := f.process(t, w.ID)
			if pending.Status != "active" || !pending.LastError.Valid || !strings.Contains(pending.LastError.String, "no terminal pipeline") {
				t.Fatalf("source pipeline with a mismatched ref was counted or not diagnosed: %+v", pending)
			}

			if source == "gitlab_connection" {
				f.fx.Exec(t, "DELETE FROM gitlab_mr_pipeline WHERE merge_request_id=$1 AND pipeline_id=1301", mr)
				setChatLegacyGitLabPipelineIdentity(t, f, mr, 1302, 99001, "same-head", "refs/merge-requests/74/head", "success", pipelineAt)
			} else {
				f.fx.Exec(t, "DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id=$1 AND pipeline_id=1301", mr)
				setChatVCSGitLabPipelineIdentity(t, f, mr, 1302, 99001, "unknown/subgroup/project", "same-head", "refs/merge-requests/74/head", "passed", pipelineAt)
			}
			f.dueNow(t, w.ID)
			pending = f.process(t, w.ID)
			if pending.Status != "active" || !pending.LastError.Valid || !strings.Contains(pending.LastError.String, "unknown project identity") {
				t.Fatalf("unknown pipeline project was not diagnosed as pending: %+v", pending)
			}

			if source == "gitlab_connection" {
				f.fx.Exec(t, "DELETE FROM gitlab_mr_pipeline WHERE merge_request_id=$1 AND pipeline_id=1302", mr)
				setChatLegacyGitLabPipelineIdentity(t, f, mr, 1303, sourceProjectID, "same-head", sourceBranch, "success", w.CreatedAt.Time.Add(time.Minute))
			} else {
				f.fx.Exec(t, "DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id=$1 AND pipeline_id=1302", mr)
				setChatVCSGitLabPipelineIdentity(t, f, mr, 1303, sourceProjectID, sourcePath, "same-head", sourceBranch, "passed", w.CreatedAt.Time.Add(time.Minute))
			}
			f.dueNow(t, w.ID)
			got := f.process(t, w.ID)
			if got.Status != "fired" || got.LastError.Valid {
				t.Fatalf("matching source-branch pipeline did not fire after recovery: %+v", got)
			}
			_, input := f.turn(t, got)
			for _, want := range []string{`"pipeline_project_path":"` + sourcePath + `"`, `"pipeline_ref":"` + sourceBranch + `"`, `"pipeline_id":"1303"`} {
				if !strings.Contains(input.Content, want) {
					t.Fatalf("source pipeline facts lack %s:\n%s", want, input.Content)
				}
			}
		})
	}
}

func TestChatWakeupGitLabPendingStatusKeepsCIActivationBaseline(t *testing.T) {
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		t.Run(source, func(t *testing.T) {
			f := newChatWakeupFixture(t)
			repository := "group/subgroup/project"
			connection, mr := newChatGitLabMirror(t, f, source, repository, 73, "open", "same-head")
			oldPipelineAt := f.clock.Add(-time.Minute)
			if source == "gitlab_connection" {
				f.fx.Exec(t, "UPDATE gitlab_merge_request SET last_refreshed_at=NULL,last_refresh_error='temporarily unavailable' WHERE id=$1", mr)
				setChatLegacyGitLabPipeline(t, f, mr, 1201, "same-head", "failed", oldPipelineAt)
			} else {
				f.fx.Exec(t, "UPDATE vcs_pull_request SET status_sync_at=NULL,status_sync_error='temporarily unavailable' WHERE id=$1", mr)
				setChatVCSGitLabPipeline(t, f, mr, 1201, "same-head", "failed", oldPipelineAt)
			}
			failSync, refresh := true, false
			installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
				if failSync {
					return errors.New("temporary GitLab API failure")
				}
				if refresh {
					if target.Source == vcs.GitLabWakeupSourceWorkspaceConnection {
						f.fx.Exec(t, "UPDATE gitlab_merge_request SET last_refreshed_at=now(),last_refresh_error=NULL WHERE id=$1", target.MergeRequestID)
					} else {
						f.fx.Exec(t, "UPDATE vcs_pull_request SET status_sync_at=now(),status_sync_error=NULL WHERE id=$1", target.MergeRequestID)
					}
					markChatGitLabPipelineSyncSuccess(t, f, target)
				}
				return nil
			}))

			w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 73, "checks_finished")})
			if !w.LastError.Valid || !strings.Contains(w.LastError.String, "temporary GitLab API failure") {
				t.Fatalf("sync failure did not expose a diagnostic: %+v", w.LastError)
			}
			f.fx.Exec(t, "UPDATE chat_wakeup SET condition_state='keep-baseline' WHERE id=$1", w.ID)
			f.dueNow(t, w.ID)
			pending := f.process(t, w.ID)
			if pending.LastTaskID.Valid || f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 0 ||
				!pending.LastError.Valid || pending.ConditionState != "keep-baseline" {
				t.Fatalf("pending sync consumed stale state or hid its reason: state=%q error=%+v", pending.ConditionState, pending.LastError)
			}

			failSync = false // The sync succeeds, but the mirror still reports unavailable status.
			f.dueNow(t, w.ID)
			pending = f.process(t, w.ID)
			if pending.LastTaskID.Valid || f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 0 ||
				!pending.LastError.Valid || !strings.Contains(pending.LastError.String, "temporarily unavailable") || pending.ConditionState != "keep-baseline" {
				t.Fatalf("unavailable mirror status was not diagnosed/preserved: state=%q error=%+v", pending.ConditionState, pending.LastError)
			}

			refresh = true
			f.dueNow(t, w.ID)
			recovered := f.process(t, w.ID)
			if recovered.LastTaskID.Valid || f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 0 || recovered.LastError.Valid {
				t.Fatalf("pre-activation pipeline fired or recovery diagnostic remained: error=%+v", recovered.LastError)
			}
			if source == "gitlab_connection" {
				setChatLegacyGitLabPipeline(t, f, mr, 1202, "same-head", "success", w.CreatedAt.Time.Add(time.Minute))
			} else {
				setChatVCSGitLabPipeline(t, f, mr, 1202, "same-head", "passed", w.CreatedAt.Time.Add(time.Minute))
			}
			f.dueNow(t, w.ID)
			if got := f.process(t, w.ID); got.Status != "fired" {
				t.Fatalf("post-activation pipeline did not fire after recovery: %+v", got)
			}
		})
	}
}

func TestChatWakeupGitLabPipelineSyncErrorsBlockOnlyChecksFinished(t *testing.T) {
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		for _, tc := range []struct {
			event string
			state string
		}{
			{event: "merged", state: "merged"},
			{event: "closed", state: "closed"},
			{event: "checks_finished", state: "open"},
		} {
			t.Run(source+"/"+tc.event, func(t *testing.T) {
				f := newChatWakeupFixture(t)
				repository := "group/subgroup/project"
				connection, mr := newChatGitLabMirror(t, f, source, repository, 81, tc.state, "same-head")
				const pipelineError = "source project CI API unavailable"
				if source == "gitlab_connection" {
					f.fx.Exec(t, "UPDATE gitlab_merge_request SET pipeline_sync_at=NULL,pipeline_sync_error=$2 WHERE id=$1", mr, pipelineError)
				} else {
					f.fx.Exec(t, "UPDATE vcs_pull_request SET pipeline_sync_at=NULL,pipeline_sync_error=$2 WHERE id=$1", mr, pipelineError)
				}
				installChatGitLabSyncer(f, chatGitLabWakeupSyncFunc(func(_ context.Context, target vcs.GitLabWakeupTarget) error {
					if target.Source == vcs.GitLabWakeupSourceWorkspaceConnection {
						f.fx.Exec(t, "UPDATE gitlab_merge_request SET last_refreshed_at=now(),last_refresh_error=NULL WHERE id=$1", target.MergeRequestID)
					} else {
						f.fx.Exec(t, "UPDATE vcs_pull_request SET status_sync_at=now(),status_sync_error=NULL WHERE id=$1", target.MergeRequestID)
					}
					return nil
				}))
				w := f.create(t, ChatWakeupInput{Condition: chatGitLabConditionJSON(t, connection, repository, 81, tc.event)})
				if tc.event != "checks_finished" {
					f.dueNow(t, w.ID)
					got := f.process(t, w.ID)
					if got.Status != "fired" || got.LastError.Valid {
						t.Fatalf("Pipeline API error blocked %s condition: %+v", tc.event, got)
					}
					return
				}
				if !w.LastError.Valid || !strings.Contains(w.LastError.String, pipelineError) {
					t.Fatalf("checks condition hid Pipeline API error: %+v", w.LastError)
				}
				f.fx.Exec(t, "UPDATE chat_wakeup SET condition_state='keep-ci-state' WHERE id=$1", w.ID)
				if source == "gitlab_connection" {
					setChatLegacyGitLabPipeline(t, f, mr, 1801, "same-head", "success", w.CreatedAt.Time.Add(time.Second))
				} else {
					setChatVCSGitLabPipeline(t, f, mr, 1801, "same-head", "passed", w.CreatedAt.Time.Add(time.Second))
				}
				f.dueNow(t, w.ID)
				got := f.process(t, w.ID)
				if got.Status != "active" || !got.LastError.Valid || !strings.Contains(got.LastError.String, pipelineError) ||
					got.ConditionState != "keep-ci-state" ||
					f.fx.Count(t, "SELECT count(*) FROM chat_message WHERE wakeup_id=$1", util.UUIDToString(w.ID)) != 0 {
					t.Fatalf("checks fired or lost its pending CI diagnostic/state: %+v", got)
				}
			})
		}
	}
}

func TestChatWakeupGitLabConditionRejectsUnmirroredTarget(t *testing.T) {
	f := newChatWakeupFixture(t)
	repository := "group/subgroup/project"
	connection, _ := newChatGitLabMirror(t, f, "gitlab_connection", repository, 74, "open", "head")
	_, err := f.svc.Create(context.Background(), f.source, ChatWakeupInput{
		Title: "x", Instruction: "y", Condition: chatGitLabConditionJSON(t, connection, "other/subgroup/project", 74, "merged"),
	})
	if !errors.Is(err, ErrChatWakeupInput) || !strings.Contains(err.Error(), "not mirrored") {
		t.Fatalf("unmirrored exact target: error = %v, want an actionable input error", err)
	}
}
