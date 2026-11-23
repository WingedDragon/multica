package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// conditionFixture is wakeFixture plus cleanup for the timeline entries rules
// write.
func conditionFixture(t *testing.T) (principalFixture, *IssueWakeupService, pgtype.UUID, string) {
	t.Helper()
	f, s, issue, agent := wakeFixture(t)
	f.Cleanup(t, "DELETE FROM activity_log WHERE issue_id=$1", issue)
	return f, s, issue, agent
}

// wakeTick makes the rule due and dispatches it, as the scheduler would.
func wakeTick(t *testing.T, f principalFixture, s *IssueWakeupService, id pgtype.UUID) db.IssueWakeup {
	t.Helper()
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1 AND next_fire_at IS NOT NULL", id)
	w, err := f.q.LocklessWakeup(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if errs := s.dispatchFromTick(context.Background(), w); len(errs) != 0 {
		t.Fatal(errors.Join(errs...))
	}
	w, err = f.q.LocklessWakeup(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func wakeRuns(t *testing.T, f principalFixture, id pgtype.UUID) int {
	t.Helper()
	return f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1", util.UUIDToString(id))
}

func condition(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWakeupConditionFieldValueWakesOnceWhenItHolds(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Review when ready",
		Condition: condition(t, map[string]any{"type": "issue_field", "field": "status", "value": "in_review"})})
	if len(w.EventTypes) != 1 || w.EventTypes[0] != "issue.status_changed" || w.Mode != "once" {
		t.Fatalf("condition rule = %+v", w)
	}
	// Not yet true: no run, no matter how often it is evaluated.
	wakeTick(t, f, s, w.ID)
	wakeSetStatus(t, f, issue, "in_progress")
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("unmet condition started %d runs", n)
	}
	// The matching status change is a hint; the platform evaluates it.
	wakeSetStatus(t, f, issue, "in_review")
	got := wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("met condition started %d runs, want 1", n)
	}
	if got.Enabled {
		t.Fatal("a once condition must be consumed")
	}
	task, err := f.q.GetAgentTask(context.Background(), got.LastTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task.HandoffNote.String, "condition.met") || !strings.Contains(task.HandoffNote.String, `"status":"in_review"`) {
		t.Fatalf("trigger facts missing the observation: %s", task.HandoffNote.String)
	}
	// Status-change hints never reach the agent as separate facts.
	if strings.Contains(task.HandoffNote.String, "issue.status_changed") {
		t.Fatalf("hint leaked into the run: %s", task.HandoffNote.String)
	}
	if n := f.Count(t, "SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_triggered'", issue); n != 1 {
		t.Fatalf("trigger timeline entries = %d, want 1", n)
	}
	if n := f.Count(t, "SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_created'", issue); n != 1 {
		t.Fatalf("created timeline entries = %d, want 1", n)
	}
}

// Cross-issue conditions have no event hints. Their stored predicates must be
// checked through ListReadyWakeups, including after an unmet scheduled check.
func TestWakeupConditionOtherIssueUsesScheduledChecks(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	ctx := context.Background()
	workspace := parseTestUUID(t, f.WorkspaceID)
	other := f.Issue(t, "Waiting for review", testutil.Cols{"status": "in_progress"})
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Continue after review",
		Condition: condition(t, map[string]any{"type": "other_issue", "issue_id": other, "state": "in_review"})})
	if len(w.EventTypes) != 0 || !w.NextFireAt.Valid {
		t.Fatalf("poll-only rule: events=%v next_fire_at=%+v", w.EventTypes, w.NextFireAt)
	}
	tick := func() db.IssueWakeup {
		t.Helper()
		if err := s.TickWorkspaces(ctx, workspace); err != nil {
			t.Fatal(err)
		}
		got, err := f.q.LocklessWakeup(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := tick()
	if !got.Enabled || got.FireCount != 0 || wakeRuns(t, f, w.ID) != 0 {
		t.Fatalf("unmet condition: enabled=%t fire_count=%d", got.Enabled, got.FireCount)
	}
	if !got.NextFireAt.Valid || !got.NextFireAt.Time.After(w.NextFireAt.Time) {
		t.Fatalf("unmet condition was not scheduled for another check: %+v", got.NextFireAt)
	}
	// Hold the next check in the future without sleeping or relying on the
	// wall-clock poll interval. Changing the watched issue emits no hint here.
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()+interval '1 hour' WHERE id=$1", w.ID)
	wakeSetStatus(t, f, parseTestUUID(t, other), "in_review")
	if n := f.Count(t, "SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL", w.ID); n != 0 {
		t.Fatalf("cross-issue change created %d event hints", n)
	}
	if active, err := f.q.HasActiveTaskForIssue(ctx, issue); err != nil || active {
		t.Fatalf("waiting issue has active work: active=%t err=%v", active, err)
	}
	got = tick()
	if got.FireCount != 0 || wakeRuns(t, f, w.ID) != 0 {
		t.Fatal("condition fired before its scheduled check")
	}
	// Only the persisted due time can make this rule a scheduler candidate.
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", w.ID)
	got = tick()
	if got.Enabled || got.FireCount != 1 || got.LastError.Valid || wakeRuns(t, f, w.ID) != 1 {
		t.Fatalf("due condition: enabled=%t fire_count=%d last_error=%+v", got.Enabled, got.FireCount, got.LastError)
	}
	task, err := f.q.GetAgentTask(ctx, got.LastTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task.HandoffNote.String, "condition.met") || !strings.Contains(task.HandoffNote.String, other) || !strings.Contains(task.HandoffNote.String, `"status":"in_review"`) {
		t.Fatalf("cross-issue observation missing from the run: %s", task.HandoffNote.String)
	}
	got = tick()
	if got.FireCount != 1 || wakeRuns(t, f, w.ID) != 1 {
		t.Fatal("once condition fired again on a repeated scheduler pass")
	}
}

// A coordinator can wait for a child's blocker handoff: state=blocked matches
// only the blocked status, so a review handoff on the same child does not fire it.
func TestWakeupConditionOtherIssueBlocked(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	ctx := context.Background()
	workspace := parseTestUUID(t, f.WorkspaceID)
	other := f.Issue(t, "Child that may block", testutil.Cols{"status": "in_progress"})
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Resolve the child's blocker",
		Condition: condition(t, map[string]any{"type": "other_issue", "issue_id": other, "state": "blocked"})})
	tick := func() db.IssueWakeup {
		t.Helper()
		f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1 AND next_fire_at IS NOT NULL", w.ID)
		if err := s.TickWorkspaces(ctx, workspace); err != nil {
			t.Fatal(err)
		}
		got, err := f.q.LocklessWakeup(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	wakeSetStatus(t, f, parseTestUUID(t, other), "in_review")
	if got := tick(); !got.Enabled || got.FireCount != 0 || wakeRuns(t, f, w.ID) != 0 {
		t.Fatalf("in_review fired a blocked rule: enabled=%t fire_count=%d", got.Enabled, got.FireCount)
	}
	wakeSetStatus(t, f, parseTestUUID(t, other), "blocked")
	got := tick()
	if got.Enabled || got.FireCount != 1 || wakeRuns(t, f, w.ID) != 1 {
		t.Fatalf("blocked child: enabled=%t fire_count=%d last_error=%+v", got.Enabled, got.FireCount, got.LastError)
	}
	task, err := f.q.GetAgentTask(ctx, got.LastTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task.HandoffNote.String, other) || !strings.Contains(task.HandoffNote.String, `"status":"blocked"`) {
		t.Fatalf("blocked observation missing from the run: %s", task.HandoffNote.String)
	}
}

func TestWakeupConditionOtherIssueRejectsUnknownState(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	other := f.Issue(t, "Watched", testutil.Cols{"status": "todo"})
	_, err := s.Create(context.Background(), issue, parseTestUUID(t, f.UserID), pgtype.UUID{}, WakeupInput{AgentID: agent, Kind: "event", Instruction: "x",
		Condition: condition(t, map[string]any{"type": "other_issue", "issue_id": other, "state": "in_progress"})})
	if err == nil || !strings.Contains(err.Error(), "state must be done, ended, in_review or blocked") {
		t.Fatalf("unknown state error = %v", err)
	}
}

func TestWakeupConditionRepeatsOnlyWhenItBecomesTrueAgain(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	label := f.Insert(t, "issue_label", testutil.Cols{"workspace_id": f.WorkspaceID, "name": "approved", "color": "#123456"})
	f.Cleanup(t, "DELETE FROM issue_to_label WHERE label_id=$1", label)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Ship it",
		Condition: condition(t, map[string]any{"type": "issue_field", "field": "label", "label_id": label})})
	if !w.MaxFires.Valid || w.MaxFires.Int32 != wakeupDefaultMaxFires {
		t.Fatalf("repeating rule cap = %+v, want default", w.MaxFires)
	}
	f.Exec(t, "INSERT INTO issue_to_label(issue_id,label_id) VALUES($1,$2)", issue, label)
	wakeTick(t, f, s, w.ID)
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("a condition that stays true fired %d times, want 1", n)
	}
	f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE context->>'wakeup_id'=$1", util.UUIDToString(w.ID))
	f.Exec(t, "DELETE FROM issue_to_label WHERE issue_id=$1 AND label_id=$2", issue, label)
	wakeTick(t, f, s, w.ID)
	f.Exec(t, "INSERT INTO issue_to_label(issue_id,label_id) VALUES($1,$2)", issue, label)
	got := wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 2 {
		t.Fatalf("re-satisfied condition fired %d times total, want 2", n)
	}
	if !got.Enabled || got.FireCount != 2 {
		t.Fatalf("continuous rule after two fires: enabled=%t fire_count=%d", got.Enabled, got.FireCount)
	}
}

func TestWakeupConditionPRConflictsWakesOnEachNewConflict(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	s.Tasks.PRSnapshotsEnabled = true
	pr := f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 1,
		"repo_owner": "fixture", "repo_name": "conflicts", "pr_number": 7,
		"title": "PR", "state": "open", "html_url": "https://example.test/pr/7",
		"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
		"head_sha": "a", "snapshot_head_sha": "a", "api_mergeable": "MERGEABLE",
	})
	f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
	f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Resolve the conflict",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "conflicts"})})
	finishRuns := func() {
		f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE context->>'wakeup_id'=$1 AND status<>'completed'", util.UUIDToString(w.ID))
	}
	snapshot := func(head, snapshotHead, mergeable string) db.IssueWakeup {
		t.Helper()
		f.Exec(t, "UPDATE github_pull_request SET head_sha=$2,snapshot_head_sha=$3,api_mergeable=$4 WHERE id=$1", pr, head, snapshotHead, mergeable)
		return wakeTick(t, f, s, w.ID)
	}
	expectRuns := func(want int, step string) {
		t.Helper()
		if n := wakeRuns(t, f, w.ID); n != want {
			t.Fatalf("%s: %d runs, want %d", step, n, want)
		}
	}

	snapshot("a", "a", "MERGEABLE")
	expectRuns(0, "mergeable")
	got := snapshot("a", "a", "CONFLICTING")
	expectRuns(1, "conflict")
	task, err := f.q.GetAgentTask(context.Background(), got.LastTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task.HandoffNote.String, `"mergeable":"conflicting"`) || !strings.Contains(task.HandoffNote.String, `"head_sha":"a"`) {
		t.Fatalf("conflict facts missing from the run: %s", task.HandoffNote.String)
	}
	finishRuns()
	// GitHub recomputing after another base move is not a new conflict.
	snapshot("a", "a", "UNKNOWN")
	snapshot("a", "a", "CONFLICTING")
	expectRuns(1, "recompute")
	// The agent pushed a fix; until the new head is fetched the old verdict
	// does not count, and the pending snapshot is visible on the rule.
	got = snapshot("b", "a", "CONFLICTING")
	expectRuns(1, "pushed head awaiting a snapshot")
	if !got.LastError.Valid || got.LastError.String != conditionDiagnosticPrefix+errPRSnapshotPending.Error() {
		t.Fatalf("pending snapshot diagnostic = %+v", got.LastError)
	}
	snapshot("b", "b", "MERGEABLE")
	expectRuns(1, "resolved")
	got = snapshot("b", "b", "CONFLICTING")
	expectRuns(2, "conflict again")
	if !got.Enabled || got.FireCount != 2 || got.LastError.Valid {
		t.Fatalf("continuous rule after two conflicts: enabled=%t fire_count=%d error=%+v", got.Enabled, got.FireCount, got.LastError)
	}
	finishRuns()
	f.Exec(t, "UPDATE github_pull_request SET state='merged' WHERE id=$1", pr)
	wakeTick(t, f, s, w.ID)
	expectRuns(2, "merged PR")
}

func TestWakeupConditionPRConflictsAcrossSeveralPRs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sameNumber bool
		wantRuns   int
	}{
		{"resolving one PR does not re-wake for the other", false, 1},
		{"PRs in different repositories keep distinct identities", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			s.Tasks.PRSnapshotsEnabled = true
			insertPR := func(repo string, number int, mergeable string) string {
				pr := f.Insert(t, "github_pull_request", testutil.Cols{
					"workspace_id": f.WorkspaceID, "installation_id": 1,
					"repo_owner": "fixture", "repo_name": repo, "pr_number": number,
					"title": "PR", "state": "open", "html_url": "https://example.test/" + repo,
					"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
					"head_sha": "shared-head", "snapshot_head_sha": "shared-head", "api_mergeable": mergeable,
				})
				f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
				f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
				return pr
			}
			a := insertPR("repo-a", 1, "CONFLICTING")
			bNumber, bMergeable := 2, "CONFLICTING"
			if tc.sameNumber {
				// A fork's PR can share both the number and the head commit.
				bNumber, bMergeable = 1, "MERGEABLE"
			}
			b := insertPR("repo-b", bNumber, bMergeable)
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Resolve conflicts",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": "conflicts"})})
			wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 1 {
				t.Fatalf("initial conflict runs = %d, want 1", n)
			}
			f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE context->>'wakeup_id'=$1", util.UUIDToString(w.ID))
			f.Exec(t, "UPDATE github_pull_request SET api_mergeable='MERGEABLE' WHERE id=$1", a)
			if tc.sameNumber {
				f.Exec(t, "UPDATE github_pull_request SET api_mergeable='CONFLICTING' WHERE id=$1", b)
			}
			wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != tc.wantRuns {
				t.Fatalf("after the conflict moved runs = %d, want %d", n, tc.wantRuns)
			}
		})
	}
}

// Checks and merged keys carry only the PR number, so same-numbered PRs from
// two repositories share a key. The second one satisfying still wakes.
func TestWakeupConditionPRSharedNumberWakesForEachPR(t *testing.T) {
	for _, event := range []string{"merged", "checks_finished"} {
		t.Run(event, func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			s.Tasks.PRSnapshotsEnabled = true
			insertPR := func(repo string) string {
				pr := f.Insert(t, "github_pull_request", testutil.Cols{
					"workspace_id": f.WorkspaceID, "installation_id": 1,
					"repo_owner": "fixture", "repo_name": repo, "pr_number": 7,
					"title": "PR", "state": "open", "html_url": "https://example.test/" + repo,
					"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
					"head_sha": "shared-head", "snapshot_head_sha": "shared-head", "checks_rollup_state": "PENDING",
				})
				f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
				f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
				return pr
			}
			a, b := insertPR("repo-a"), insertPR("repo-b")
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Follow each pull request",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": event})})
			wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 0 {
				t.Fatalf("before either PR satisfies: %d runs, want 0", n)
			}
			satisfy := func(pr string) {
				if event == "merged" {
					f.Exec(t, "UPDATE github_pull_request SET state='merged' WHERE id=$1", pr)
				} else {
					f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='SUCCESS' WHERE id=$1", pr)
				}
				wakeTick(t, f, s, w.ID)
			}
			satisfy(a)
			if n := wakeRuns(t, f, w.ID); n != 1 {
				t.Fatalf("after the first PR: %d runs, want 1", n)
			}
			f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE context->>'wakeup_id'=$1", util.UUIDToString(w.ID))
			satisfy(b)
			if n := wakeRuns(t, f, w.ID); n != 2 {
				t.Fatalf("after the second PR: %d runs, want 2", n)
			}
		})
	}
}

func TestWakeupConditionChildrenAndOtherIssue(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	first := f.Issue(t, "Stage 1 child", testutil.Cols{"parent_issue_id": issue, "status": "todo", "stage": 1})
	f.Issue(t, "Stage 2 child", testutil.Cols{"parent_issue_id": issue, "status": "todo", "stage": 2})
	other := f.Issue(t, "Blocking issue", testutil.Cols{"status": "in_progress"})
	stage := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Start stage 2",
		Condition: condition(t, map[string]any{"type": "children_done", "stage": 1})})
	watch := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Continue after the blocker",
		Condition: condition(t, map[string]any{"type": "other_issue", "issue_id": other, "state": "done"})})
	wakeTick(t, f, s, stage.ID)
	wakeTick(t, f, s, watch.ID)
	if wakeRuns(t, f, stage.ID)+wakeRuns(t, f, watch.ID) != 0 {
		t.Fatal("unmet conditions started runs")
	}
	var watched WakeupCondition
	_ = json.Unmarshal(watch.Condition, &watched)
	if !strings.Contains(watched.Identifier, "-") {
		t.Fatalf("watched issue identifier not recorded: %s", watch.Condition)
	}
	f.Exec(t, "UPDATE issue SET status='cancelled' WHERE id=$1", first)
	f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", other)
	wakeTick(t, f, s, stage.ID)
	wakeTick(t, f, s, watch.ID)
	// Both hold for the same agent: the second waits for the first one's run
	// and joins it when it is claimed, instead of queuing another behind it.
	if wakeRuns(t, f, stage.ID) != 1 || wakeRuns(t, f, watch.ID) != 0 {
		t.Fatalf("stage runs=%d, other-issue runs=%d, want one shared run", wakeRuns(t, f, stage.ID), wakeRuns(t, f, watch.ID))
	}
	var shared string
	f.QueryRow(t, "SELECT id FROM agent_task_queue WHERE context->>'wakeup_id'=$1", util.UUIDToString(stage.ID)).Scan(&shared)
	if notes := wakeClaim(t, f, s, shared); !strings.Contains(notes, "Continue after the blocker") || !strings.Contains(notes, `"state":"done"`) {
		t.Fatalf("shared run lacks the joined rule: %q", notes)
	}
}

func TestWakeupConditionPullRequestChecksWaitForNewResult(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	s.Tasks.PRSnapshotsEnabled = true
	ctx := context.Background()
	var pr string
	if err := f.Pool.QueryRow(ctx, `INSERT INTO github_pull_request(workspace_id,installation_id,repo_owner,repo_name,pr_number,title,state,html_url,pr_created_at,pr_updated_at,snapshot_head_sha,checks_rollup_state)
		VALUES($1,1,'multica-ai','wakeup-test',(extract(epoch from clock_timestamp())*1000)::bigint % 100000,'PR','open','https://example.test/pr',now(),now(),'aaa','SUCCESS') RETURNING id`, f.WorkspaceID).Scan(&pr); err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM github_pull_request WHERE id=$1", pr)
	f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
	f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Look at CI",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
	// Finished checks from before registration belong to an older push.
	wakeTick(t, f, s, w.ID)
	f.Exec(t, "UPDATE github_pull_request SET snapshot_head_sha='bbb',checks_rollup_state='PENDING' WHERE id=$1", pr)
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("stale or pending checks started %d runs", n)
	}
	f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='FAILURE' WHERE id=$1", pr)
	got := wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("finished checks started %d runs, want 1", n)
	}
	task, err := f.q.GetAgentTask(ctx, got.LastTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task.HandoffNote.String, `"checks":"failure"`) || !strings.Contains(task.HandoffNote.String, `"head_sha":"bbb"`) {
		t.Fatalf("PR facts missing: %s", task.HandoffNote.String)
	}
}

func TestWakeupConditionValidation(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	ctx := context.Background()
	member := parseTestUUID(t, f.UserID)
	for name, in := range map[string]WakeupInput{
		"unknown status":    {Condition: condition(t, map[string]any{"type": "issue_field", "field": "status", "value": "nope"})},
		"this issue":        {Condition: condition(t, map[string]any{"type": "other_issue", "issue_id": util.UUIDToString(issue), "state": "done"})},
		"unknown field":     {Condition: condition(t, map[string]any{"type": "issue_field", "field": "title", "value": "x"})},
		"extra key":         {Condition: condition(t, map[string]any{"type": "children_done", "when": "soon"})},
		"with events":       {EventTypes: []string{"comment.created"}, Condition: condition(t, map[string]any{"type": "children_done"})},
		"time kind":         {Kind: "at", AfterSeconds: 60, Condition: condition(t, map[string]any{"type": "children_done"})},
		"cap on once":       {MaxFires: 3, Condition: condition(t, map[string]any{"type": "children_done"})},
		"foreign assignee":  {Condition: condition(t, map[string]any{"type": "issue_field", "field": "assignee", "assignee_type": "agent", "assignee_id": util.UUIDToString(parseTestUUID(t, "00000000-0000-0000-0000-000000000001"))})},
		"property archived": {Condition: condition(t, map[string]any{"type": "issue_field", "field": "property", "property_id": "00000000-0000-0000-0000-000000000002", "value": "x"})},
	} {
		in.AgentID, in.Instruction = agent, "x"
		if in.Kind == "" {
			in.Kind = "event"
		}
		if _, err := s.Create(ctx, issue, member, pgtype.UUID{}, in); !errors.Is(err, ErrWakeupInput) {
			t.Errorf("%s: err = %v, want ErrWakeupInput", name, err)
		}
	}
}

// A check that loses its lock wait records an error; the next unmet check that commits clears it.
func TestWakeupConditionCheckClearsAnEarlierFailure(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	ctx := context.Background()
	workspace := parseTestUUID(t, f.WorkspaceID)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Review when ready",
		Condition: condition(t, map[string]any{"type": "issue_field", "field": "status", "value": "in_review"})})
	due := func() {
		f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", w.ID)
	}

	due()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "SELECT 1 FROM issue WHERE id=$1 FOR UPDATE", issue); err != nil {
		t.Fatal(err)
	}
	tickErr := s.TickWorkspaces(ctx, workspace)
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if tickErr == nil {
		t.Fatal("a check blocked on its issue row reported no error")
	}
	failed, err := f.q.LocklessWakeup(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !failed.LastError.Valid {
		t.Fatal("the failed check recorded no error")
	}

	due()
	if err = s.TickWorkspaces(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	got, err := f.q.LocklessWakeup(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError.Valid {
		t.Fatalf("the check that committed kept the earlier error %q", got.LastError.String)
	}
}

type wakeupCommitFailureStarter struct {
	TxStarter
	err error
}

func (s wakeupCommitFailureStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return wakeupCommitFailureTx{Tx: tx, err: s.err}, nil
}

type wakeupCommitFailureTx struct {
	pgx.Tx
	err error
}

func (tx wakeupCommitFailureTx) Commit(context.Context) error { return tx.err }

func TestWakeupConditionFailedCommitPreservesFailure(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	ctx := context.Background()
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Review when ready",
		Condition: condition(t, map[string]any{"type": "issue_field", "field": "status", "value": "in_review"})})
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second',last_error='earlier dispatch failure' WHERE id=$1", w.ID)
	commitErr := errors.New("dispatch commit failed")
	s.Tasks.TxStarter = wakeupCommitFailureStarter{TxStarter: f.Pool, err: commitErr}
	if err := s.dispatch(ctx, w); !errors.Is(err, commitErr) {
		t.Fatalf("dispatch error=%v, want %v", err, commitErr)
	}
	got, err := f.q.LocklessWakeup(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastError.Valid || got.LastError.String != "earlier dispatch failure" {
		t.Fatalf("failed commit cleared the earlier failure: %+v", got.LastError)
	}
	if err := s.TickWorkspaces(ctx, parseTestUUID(t, f.WorkspaceID)); !errors.Is(err, commitErr) {
		t.Fatalf("tick error=%v, want %v", err, commitErr)
	}
	got, err = f.q.LocklessWakeup(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastError.Valid || got.LastError.String != commitErr.Error() {
		t.Fatalf("failed pass did not record the current failure: %+v", got.LastError)
	}
	s.Tasks.TxStarter = f.Pool
	if got := wakeTick(t, f, s, w.ID); got.LastError.Valid {
		t.Fatalf("successful retry kept the commit failure: %+v", got.LastError)
	}
}

func TestWakeupConditionPRDiagnosticsRecover(t *testing.T) {
	for _, event := range []string{"checks_finished", "merged"} {
		t.Run(event, func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			ctx := context.Background()
			parent := f.Issue(t, "PR owner")
			f.Exec(t, "UPDATE issue SET parent_issue_id=$2 WHERE id=$1", issue, parent)
			pr := f.Insert(t, "github_pull_request", testutil.Cols{
				"workspace_id": f.WorkspaceID, "installation_id": 1,
				"repo_owner": "fixture", "repo_name": "wakeup", "pr_number": 1,
				"title": "PR", "state": "open", "html_url": "https://example.test/pr/1",
				"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
			})
			f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
			f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", parent, pr)
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Review PR",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": event})})
			assertWaiting := func(w db.IssueWakeup, diagnostic error) {
				t.Helper()
				if !w.Enabled || !w.LastError.Valid || w.LastError.String != conditionDiagnosticPrefix+diagnostic.Error() || w.FireCount != 0 || wakeRuns(t, f, w.ID) != 0 {
					t.Fatalf("expected enabled, unfired rule with %q; got enabled=%t fires=%d error=%+v", diagnostic, w.Enabled, w.FireCount, w.LastError)
				}
				if !w.NextFireAt.Valid {
					t.Fatal("diagnostic stopped condition polling")
				}
			}
			assertRecoveredFailure := func(diagnostic error) {
				t.Helper()
				f.Exec(t, "UPDATE issue_wakeup SET last_error='earlier dispatch failure' WHERE id=$1", w.ID)
				assertWaiting(wakeTick(t, f, s, w.ID), diagnostic)
			}
			if event == "checks_finished" {
				// A disabled snapshot client is visible even in the create response.
				assertWaiting(w, errPRSnapshotsUnavailable)
				assertWaiting(wakeTick(t, f, s, w.ID), errPRSnapshotsUnavailable)
				assertRecoveredFailure(errPRSnapshotsUnavailable)
				s.Tasks.PRSnapshotsEnabled = true
			}
			// Linking to the parent must never satisfy a child issue's rule.
			assertWaiting(wakeTick(t, f, s, w.ID), errPRNotLinked)
			assertRecoveredFailure(errPRNotLinked)
			f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
			if event == "checks_finished" {
				assertWaiting(wakeTick(t, f, s, w.ID), errPRSnapshotPending)
				assertRecoveredFailure(errPRSnapshotPending)
				// Re-enabling also accepts a transiently missing snapshot.
				disabled, err := s.Disable(ctx, issue, w.ID, parseTestUUID(t, f.UserID))
				if err != nil {
					t.Fatal(err)
				}
				w, err = s.Enable(ctx, issue, parseTestUUID(t, f.UserID), pgtype.UUID{}, w.ID, WakeupEnableInput{Revision: disabled.Revision})
				if err != nil {
					t.Fatal(err)
				}
				assertWaiting(w, errPRSnapshotPending)
				f.Exec(t, "UPDATE github_pull_request SET snapshot_head_sha='new',checks_rollup_state='PENDING' WHERE id=$1", pr)
			}
			got := wakeTick(t, f, s, w.ID)
			if got.LastError.Valid || !got.Enabled || wakeRuns(t, f, w.ID) != 0 {
				t.Fatalf("normal pending condition did not clear diagnostic: %+v", got.LastError)
			}
			f.Exec(t, "UPDATE github_pull_request SET state='merged',checks_rollup_state='SUCCESS' WHERE id=$1", pr)
			got = wakeTick(t, f, s, w.ID)
			if got.Enabled || got.LastError.Valid || wakeRuns(t, f, w.ID) != 1 {
				t.Fatalf("recovered condition failed to fire once: enabled=%t error=%+v", got.Enabled, got.LastError)
			}
		})
	}
}

func TestWakeupConditionPullRequestChecksCoversVCSProviders(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	ctx := context.Background()
	var pr, connection string
	if err := f.Pool.QueryRow(ctx, `INSERT INTO vcs_pull_request(workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,pr_created_at,pr_updated_at,head_sha)
		VALUES($1,gen_random_uuid(),'forgejo','selfhosted','wakeup-test',(extract(epoch from clock_timestamp())*1000)::bigint % 100000,'PR','open','https://forgejo.example.test/pr',now(),now(),'aaa') RETURNING id, connection_id`, f.WorkspaceID).Scan(&pr, &connection); err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM vcs_pull_request WHERE id=$1", pr)
	f.Cleanup(t, "DELETE FROM issue_vcs_pull_request WHERE pull_request_id=$1", pr)
	f.Exec(t, "INSERT INTO issue_vcs_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	f.Exec(t, `INSERT INTO vcs_commit_status(connection_id,sha,context,state) VALUES($1,'aaa','ci/test','pending')`, connection)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Look at CI",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
	// Pending statuses from before registration are not a finished check.
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("pending statuses started %d runs", n)
	}
	f.Exec(t, "UPDATE vcs_pull_request SET head_sha='bbb' WHERE id=$1", pr)
	f.Exec(t, `INSERT INTO vcs_commit_status(connection_id,sha,context,state) VALUES($1,'bbb','ci/test','failed')`, connection)
	got := wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("failed statuses on the new head started %d runs, want 1", n)
	}
	task, err := f.q.GetAgentTask(ctx, got.LastTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task.HandoffNote.String, `"checks":"failure"`) || !strings.Contains(task.HandoffNote.String, `"head_sha":"bbb"`) {
		t.Fatalf("VCS PR facts missing: %s", task.HandoffNote.String)
	}
	if !strings.Contains(task.HandoffNote.String, `"provider":"forgejo"`) {
		t.Fatalf("provider missing from PR facts: %s", task.HandoffNote.String)
	}
}

func TestWakeupConditionPullRequestMergedCoversVCSProviders(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	ctx := context.Background()
	var pr string
	if err := f.Pool.QueryRow(ctx, `INSERT INTO vcs_pull_request(workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,pr_created_at,pr_updated_at,head_sha)
		VALUES($1,gen_random_uuid(),'gitea','selfhosted','wakeup-test',(extract(epoch from clock_timestamp())*1000)::bigint % 100000,'PR','open','https://gitea.example.test/pr',now(),now(),'ccc') RETURNING id`, f.WorkspaceID).Scan(&pr); err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM vcs_pull_request WHERE id=$1", pr)
	f.Cleanup(t, "DELETE FROM issue_vcs_pull_request WHERE pull_request_id=$1", pr)
	f.Exec(t, "INSERT INTO issue_vcs_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Note the merge",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "merged"})})
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("open PR started %d runs", n)
	}
	f.Exec(t, "UPDATE vcs_pull_request SET state='merged' WHERE id=$1", pr)
	wakeTick(t, f, s, w.ID)
	if wakeRuns(t, f, w.ID) != 1 {
		t.Fatalf("merged VCS PR did not start exactly 1 run")
	}
}

// linkVCSPR links a self-hosted VCS pull request to issue and returns its id.
func linkVCSPR(t *testing.T, f principalFixture, issue pgtype.UUID, connection, provider string, number int, head string) string {
	t.Helper()
	if provider == "gitlab" {
		instanceURL := "https://" + strings.ReplaceAll(connection, "-", "") + ".gitlab.example.test"
		f.Exec(t, `INSERT INTO vcs_connection(id,workspace_id,provider,instance_url,account_login,access_token_encrypted,webhook_secret_encrypted)
			VALUES($1,$2,'gitlab',$3,'fixture','encrypted-token','encrypted-secret')`,
			connection, f.WorkspaceID, instanceURL)
		f.Cleanup(t, "DELETE FROM vcs_connection WHERE id=$1", connection)
	}
	var pr string
	if err := f.Pool.QueryRow(context.Background(), `INSERT INTO vcs_pull_request(
			workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,pr_created_at,pr_updated_at,
			head_sha,source_project_id,source_project_path,status_sync_at)
		VALUES($1,$2,$3,'selfhosted','wakeup-test',$4,'PR','open','https://vcs.example.test/pr',now(),now(),$5,
			CASE WHEN $3='gitlab' THEN 1::bigint END,CASE WHEN $3='gitlab' THEN 'selfhosted/wakeup-test' END,
			CASE WHEN $3='gitlab' THEN now() END) RETURNING id`,
		f.WorkspaceID, connection, provider, number, head).Scan(&pr); err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM issue_vcs_pull_request WHERE pull_request_id=$1", pr)
	f.Cleanup(t, "DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id=$1", pr)
	f.Cleanup(t, "DELETE FROM vcs_commit_status WHERE connection_id=$1", connection)
	f.Cleanup(t, "DELETE FROM vcs_pull_request WHERE id=$1", pr)
	f.Exec(t, "INSERT INTO issue_vcs_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	return pr
}

type conditionGitLabWakeupSyncFunc func(context.Context, vcs.GitLabWakeupTarget) error

func (f conditionGitLabWakeupSyncFunc) SyncGitLabWakeupTarget(ctx context.Context, target vcs.GitLabWakeupTarget) error {
	return f(ctx, target)
}

func installConditionGitLabSyncer(t *testing.T, s *IssueWakeupService, f principalFixture, observe func(vcs.GitLabWakeupTarget)) {
	t.Helper()
	s.Tasks.GitLabWakeupSyncer = conditionGitLabWakeupSyncFunc(func(ctx context.Context, target vcs.GitLabWakeupTarget) error {
		if observe != nil {
			observe(target)
		}
		switch target.Source {
		case vcs.GitLabWakeupSourceWorkspaceConnection:
			f.Exec(t, "UPDATE gitlab_merge_request SET last_refreshed_at=now(),last_refresh_error=NULL,pipeline_sync_at=now(),pipeline_sync_error=NULL WHERE id=$1 AND workspace_id=$2", target.MergeRequestID, target.WorkspaceID)
		case vcs.GitLabWakeupSourceVCSConnection:
			f.Exec(t, "UPDATE vcs_pull_request SET status_sync_at=now(),status_sync_error=NULL,pipeline_sync_at=now(),pipeline_sync_error=NULL WHERE id=$1 AND workspace_id=$2", target.MergeRequestID, target.WorkspaceID)
		}
		return nil
	})
}

func setVCSStatus(t *testing.T, f principalFixture, connection, sha, state string) {
	t.Helper()
	f.Exec(t, `INSERT INTO vcs_commit_status(connection_id,sha,context,state) VALUES($1,$2,'ci/test',$3)
		ON CONFLICT (connection_id,sha,context) DO UPDATE SET state=EXCLUDED.state`, connection, sha, state)
}

func newConnection(t *testing.T, f principalFixture) string {
	t.Helper()
	var id string
	if err := f.Pool.QueryRow(context.Background(), "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func newConditionGitLabConnection(t *testing.T, f principalFixture) (string, string) {
	t.Helper()
	id := newConnection(t, f)
	host := strings.ReplaceAll(id, "-", "") + ".gitlab.example.test"
	_, err := f.q.UpsertGitLabConnection(context.Background(), db.UpsertGitLabConnectionParams{
		WorkspaceID:   parseTestUUID(t, f.WorkspaceID),
		BaseUrl:       "https://" + host,
		Host:          host,
		ConnectedByID: parseTestUUID(t, f.UserID),
	})
	if err != nil {
		t.Fatal(err)
	}
	var connection string
	if err := f.Pool.QueryRow(context.Background(), "SELECT id::text FROM gitlab_connection WHERE workspace_id=$1 AND host=$2", f.WorkspaceID, host).Scan(&connection); err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM gitlab_connection WHERE id=$1", connection)
	return connection, host
}

func linkConditionGitLabMR(t *testing.T, f principalFixture, issue pgtype.UUID, connection, repository string, projectID int64, number int, state, head string) string {
	t.Helper()
	ctx := context.Background()
	var binding string
	if err := f.Pool.QueryRow(ctx, `INSERT INTO gitlab_project_binding(workspace_id,connection_id,gitlab_project_id,path_with_namespace,web_url,hook_enabled)
		VALUES($1,$2,$3,$4,$5,false) RETURNING id::text`,
		f.WorkspaceID, connection, projectID, repository, "https://gitlab.example.test/"+repository).Scan(&binding); err != nil {
		t.Fatal(err)
	}
	var mr string
	if err := f.Pool.QueryRow(ctx, `INSERT INTO gitlab_merge_request(
		workspace_id,project_binding_id,gitlab_project_id,mr_iid,title,state,web_url,sha,mr_created_at,mr_updated_at,last_refreshed_at,source_project_id,source_project_path
	) VALUES($1,$2,$3,$4,'MR',$5,$6,$7,now(),now(),now(),$3,$8) RETURNING id::text`,
		f.WorkspaceID, binding, projectID, number, state, "https://gitlab.example.test/"+repository+"/-/merge_requests/1", head, repository).Scan(&mr); err != nil {
		t.Fatal(err)
	}
	f.Exec(t, "INSERT INTO issue_gitlab_merge_request(workspace_id,issue_id,merge_request_id) VALUES($1,$2,$3)", f.WorkspaceID, issue, mr)
	return mr
}

func linkConditionVCSGitLabMR(t *testing.T, f principalFixture, issue pgtype.UUID, connection, instanceURL, repository string, projectID int64, number int, state, head string) string {
	t.Helper()
	ctx := context.Background()
	f.Exec(t, `INSERT INTO vcs_connection(id,workspace_id,provider,instance_url,account_login,access_token_encrypted,webhook_secret_encrypted)
		VALUES($1,$2,'gitlab',$3,'fixture','encrypted-token','encrypted-secret')`, connection, f.WorkspaceID, instanceURL)
	f.Cleanup(t, "DELETE FROM vcs_connection WHERE id=$1", connection)
	slash := strings.LastIndex(repository, "/")
	if slash < 1 || slash == len(repository)-1 {
		t.Fatalf("repository path must include group and project: %q", repository)
	}
	owner, name := repository[:slash], repository[slash+1:]
	var pr string
	if err := f.Pool.QueryRow(ctx, `INSERT INTO vcs_pull_request(
		workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,pr_created_at,pr_updated_at,head_sha,status_sync_at,source_project_id,source_project_path
	) VALUES($1,$2,'gitlab',$3,$4,$5,'MR',$6,$7,now(),now(),$8,now(),$9,$10) RETURNING id::text`,
		f.WorkspaceID, connection, owner, name, number, state, instanceURL+"/"+repository+"/-/merge_requests/1", head, projectID, repository).Scan(&pr); err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM issue_vcs_pull_request WHERE pull_request_id=$1", pr)
	f.Cleanup(t, "DELETE FROM vcs_gitlab_pipeline WHERE pull_request_id=$1", pr)
	f.Cleanup(t, "DELETE FROM vcs_pull_request WHERE id=$1", pr)
	f.Exec(t, "INSERT INTO issue_vcs_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	return pr
}

func setGitLabMRConflict(t *testing.T, f principalFixture, mr, head string, hasConflicts *bool, conflictHead string) {
	t.Helper()
	f.Exec(t, `UPDATE gitlab_merge_request
		SET sha=$2,has_conflicts=$3,conflict_sha=$4,last_refreshed_at=now(),last_refresh_error=NULL
		WHERE id=$1`, mr, head, hasConflicts, conflictHead)
}

func setGitLabPipeline(t *testing.T, f principalFixture, mr string, id int64, sha, status string, updatedAt time.Time) {
	t.Helper()
	f.Exec(t, `INSERT INTO gitlab_mr_pipeline(merge_request_id,pipeline_id,pipeline_project_id,ref,sha,status,pipeline_updated_at)
		SELECT mr.id,$2,mr.source_project_id,COALESCE(mr.source_branch,''),$3,$4,$5
		FROM gitlab_merge_request mr WHERE mr.id=$1
		ON CONFLICT(merge_request_id,pipeline_id) DO UPDATE SET
			pipeline_project_id=EXCLUDED.pipeline_project_id,ref=EXCLUDED.ref,sha=EXCLUDED.sha,
			status=EXCLUDED.status,pipeline_updated_at=EXCLUDED.pipeline_updated_at`,
		mr, id, sha, status, updatedAt)
}

func setVCSGitLabPipeline(t *testing.T, f principalFixture, pr string, id int64, sha, status string, updatedAt time.Time) {
	t.Helper()
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "skipped", "passed":
		status = "passed"
	case "failed", "canceled", "cancelled":
		status = "failed"
	default:
		status = "pending"
	}
	f.Exec(t, `INSERT INTO vcs_gitlab_pipeline(
		workspace_id,connection_id,pull_request_id,pipeline_id,pipeline_project_id,pipeline_project_path,sha,ref,status,web_url,pipeline_updated_at
	)
	SELECT workspace_id,connection_id,id,$2,COALESCE(source_project_id,0),
		COALESCE(source_project_path,repo_owner||'/'||repo_name),$3,COALESCE(branch,''),$4,NULL,$5
	FROM vcs_pull_request WHERE id=$1
	ON CONFLICT(pull_request_id,pipeline_project_path,pipeline_id) DO UPDATE SET
		pipeline_project_id=EXCLUDED.pipeline_project_id,sha=EXCLUDED.sha,ref=EXCLUDED.ref,
		status=EXCLUDED.status,web_url=EXCLUDED.web_url,pipeline_updated_at=EXCLUDED.pipeline_updated_at`,
		pr, id, sha, status, updatedAt)
}

func TestWakeupConditionPullRequestCoversEveryVCSProvider(t *testing.T) {
	for _, provider := range []string{"forgejo", "gitea", "gitlab"} {
		t.Run(provider+"/checks_finished", func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			s.Tasks.PRSnapshotsEnabled = true
			connection := newConnection(t, f)
			pr := linkVCSPR(t, f, issue, connection, provider, 7, "old")
			if provider == "gitlab" {
				installConditionGitLabSyncer(t, s, f, nil)
				setVCSGitLabPipeline(t, f, pr, 101, "old", "success", time.Now().Add(-time.Minute))
			} else {
				setVCSStatus(t, f, connection, "old", "passed")
			}
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Look at CI",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
			wakeTick(t, f, s, w.ID)
			f.Exec(t, "UPDATE vcs_pull_request SET head_sha='new' WHERE id=$1", pr)
			wakeTick(t, f, s, w.ID)
			if provider == "gitlab" {
				setVCSGitLabPipeline(t, f, pr, 102, "old", "failed", time.Now())
				setVCSGitLabPipeline(t, f, pr, 103, "new", "running", time.Now())
			} else {
				setVCSStatus(t, f, connection, "old", "failed")
				setVCSStatus(t, f, connection, "new", "pending")
			}
			wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 0 {
				t.Fatalf("unfinished current head started %d runs", n)
			}
			if provider == "gitlab" {
				setVCSGitLabPipeline(t, f, pr, 103, "new", "failed", time.Now().Add(time.Second))
			} else {
				setVCSStatus(t, f, connection, "new", "passed")
			}
			got := wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 1 || got.LastError.Valid {
				t.Fatalf("finished checks started %d runs (error %+v), want 1", n, got.LastError)
			}
			task, err := f.q.GetAgentTask(context.Background(), got.LastTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if provider == "gitlab" && (!strings.Contains(task.HandoffNote.String, `"checks":"failure"`) || !strings.Contains(task.HandoffNote.String, `"head_sha":"new"`)) {
				t.Fatalf("GitLab pipeline facts missing: %s", task.HandoffNote.String)
			}
		})
		t.Run(provider+"/merged", func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			connection := newConnection(t, f)
			pr := linkVCSPR(t, f, issue, connection, provider, 7, "head")
			if provider == "gitlab" {
				installConditionGitLabSyncer(t, s, f, nil)
			}
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Note the merge",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": "merged"})})
			wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 0 {
				t.Fatalf("open PR started %d runs", n)
			}
			f.Exec(t, "UPDATE vcs_pull_request SET state='merged' WHERE id=$1", pr)
			wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 1 {
				t.Fatalf("merged PR started %d runs, want 1", n)
			}
		})
	}
}

// A server without the GitHub snapshot client still evaluates VCS PRs linked
// next to a GitHub PR; the GitHub diagnostic only explains the wait.
func TestWakeupConditionVCSChecksNotBlockedByGitHubSnapshotGate(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	if s.Tasks.PRSnapshotsEnabled {
		t.Fatal("fixture must start without the snapshot client")
	}
	gh := f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 1,
		"repo_owner": "fixture", "repo_name": "wakeup", "pr_number": 1,
		"title": "PR", "state": "open", "html_url": "https://example.test/pr/1",
		"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
	})
	f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", gh)
	f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, gh)
	connection := newConnection(t, f)
	linkVCSPR(t, f, issue, connection, "forgejo", 2, "sha")
	setVCSStatus(t, f, connection, "sha", "pending")
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Look at CI",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
	got := wakeTick(t, f, s, w.ID)
	if wakeRuns(t, f, w.ID) != 0 || !got.LastError.Valid || got.LastError.String != conditionDiagnosticPrefix+errPRSnapshotsUnavailable.Error() {
		t.Fatalf("pending VCS checks: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
	}
	setVCSStatus(t, f, connection, "sha", "passed")
	got = wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 || got.LastError.Valid {
		t.Fatalf("passed VCS checks started %d runs (error %+v), want 1 and a cleared diagnostic", n, got.LastError)
	}
}

// PR numbers and shas repeat across connections: each PR rolls up its own
// statuses and has its own fingerprint.
func TestWakeupConditionVCSChecksKeyedByPullRequest(t *testing.T) {
	setup := func(t *testing.T) (principalFixture, *IssueWakeupService, pgtype.UUID, string, string, string) {
		f, s, issue, agent := conditionFixture(t)
		s.Tasks.PRSnapshotsEnabled = true
		a, b := newConnection(t, f), newConnection(t, f)
		linkVCSPR(t, f, issue, a, "forgejo", 5, "same")
		linkVCSPR(t, f, issue, b, "forgejo", 5, "same")
		return f, s, issue, agent, a, b
	}
	create := func(t *testing.T, f principalFixture, s *IssueWakeupService, issue pgtype.UUID, agent string) db.IssueWakeup {
		return wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Look at CI",
			Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
	}
	t.Run("rollup", func(t *testing.T) {
		f, s, issue, agent, a, b := setup(t)
		setVCSStatus(t, f, a, "same", "pending")
		setVCSStatus(t, f, b, "same", "pending")
		w := create(t, f, s, issue, agent)
		// One PR finishing satisfies the rule while the other still runs.
		setVCSStatus(t, f, a, "same", "passed")
		wakeTick(t, f, s, w.ID)
		if n := wakeRuns(t, f, w.ID); n != 1 {
			t.Fatalf("one finished PR started %d runs, want 1", n)
		}
	})
	t.Run("fingerprint", func(t *testing.T) {
		f, s, issue, agent, a, b := setup(t)
		setVCSStatus(t, f, a, "same", "passed")
		setVCSStatus(t, f, b, "same", "pending")
		w := create(t, f, s, issue, agent)
		// A reruns while B finishes before the next evaluation: a new result.
		setVCSStatus(t, f, a, "same", "pending")
		setVCSStatus(t, f, b, "same", "passed")
		wakeTick(t, f, s, w.ID)
		if n := wakeRuns(t, f, w.ID); n != 1 {
			t.Fatalf("a different PR finishing started %d runs, want 1", n)
		}
	})
}
func setVCSGitLabConflict(t *testing.T, f principalFixture, pr, head string, hasConflicts *bool, conflictHead string) {
	t.Helper()
	f.Exec(t, `UPDATE vcs_pull_request
		SET head_sha=$2,has_conflicts=$3,conflict_head_sha=$4,status_sync_at=now(),status_sync_error=NULL
		WHERE id=$1`, pr, head, hasConflicts, conflictHead)
}

func finishConditionWakeupRuns(t *testing.T, f principalFixture, wakeupID pgtype.UUID) {
	t.Helper()
	f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE context->>'wakeup_id'=$1 AND status<>'completed'", util.UUIDToString(wakeupID))
}

func TestWakeupConditionGitLabMergedDeduplicatesMirrorsAndSyncsOutsideTransaction(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	repository := "group/subgroup/project"
	gitlabConnection, host := newConditionGitLabConnection(t, f)
	gitlabMergeRequest := linkConditionGitLabMR(t, f, issue, gitlabConnection, repository, 901, 7, "merged", "head")
	vcsConnection := newConnection(t, f)
	vcsMergeRequest := linkConditionVCSGitLabMR(t, f, issue, vcsConnection, "https://"+host, repository, 901, 7, "merged", "head")

	f.Exec(t, "UPDATE gitlab_merge_request SET pipeline_sync_at=now(),pipeline_sync_error='source project CI API unavailable' WHERE id=$1", gitlabMergeRequest)
	f.Exec(t, "UPDATE vcs_pull_request SET pipeline_sync_at=now(),pipeline_sync_error='source project CI API unavailable' WHERE id=$1", vcsMergeRequest)

	targets := make(chan vcs.GitLabWakeupTarget, 16)
	targetLock := make(chan struct{}, 1)
	s.Tasks.GitLabWakeupSyncer = conditionGitLabWakeupSyncFunc(func(ctx context.Context, target vcs.GitLabWakeupTarget) error {
		targets <- target
		targetLock <- struct{}{}
		defer func() { <-targetLock }()
		tx, err := f.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		var lockedID string
		if err = tx.QueryRow(ctx, "SELECT id::text FROM issue WHERE id=$1 FOR UPDATE NOWAIT", issue).Scan(&lockedID); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		switch target.Source {
		case vcs.GitLabWakeupSourceWorkspaceConnection:
			f.Exec(t, "UPDATE gitlab_merge_request SET last_refreshed_at=now(),last_refresh_error=NULL WHERE id=$1", target.MergeRequestID)
		case vcs.GitLabWakeupSourceVCSConnection:
			f.Exec(t, "UPDATE vcs_pull_request SET status_sync_at=now(),status_sync_error=NULL WHERE id=$1", target.MergeRequestID)
		}
		return nil
	})
	assertTargets := func(phase string) {
		observed := make([]vcs.GitLabWakeupTarget, 0, 2)
		for len(targets) > 0 {
			observed = append(observed, <-targets)
		}
		sources := map[vcs.GitLabWakeupSource]bool{}
		for _, target := range observed {
			if target.Repository != repository || target.Number != 7 {
				t.Fatalf("%s sync target lost full GitLab MR identity: %+v", phase, target)
			}
			sources[target.Source] = true
		}
		if len(observed) != 2 || !sources[vcs.GitLabWakeupSourceWorkspaceConnection] || !sources[vcs.GitLabWakeupSourceVCSConnection] {
			t.Fatalf("%s did not sync both linked mirrors exactly once: %+v", phase, observed)
		}
	}
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Note the merge",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "merged"})})
	assertTargets("post-save")
	got := wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("a mirrored merged MR started %d runs, want one", n)
	}
	if !got.Enabled || got.LastError.Valid {
		t.Fatalf("merged mirror condition: enabled=%t error=%+v", got.Enabled, got.LastError)
	}
	assertTargets("scheduler")
	finishConditionWakeupRuns(t, f, w.ID)
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("duplicate mirrors re-fired the same merge, runs=%d", n)
	}
}

func TestWakeupConditionGitLabConflictRequiresCurrentHeadAndKeepsFingerprintAcrossUnknown(t *testing.T) {
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		t.Run(source, func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			var mr string
			if source == "gitlab_connection" {
				connection, _ := newConditionGitLabConnection(t, f)
				mr = linkConditionGitLabMR(t, f, issue, connection, "group/subgroup/project", 902, 8, "open", "head-a")
			} else {
				connection := newConnection(t, f)
				instanceURL := "https://" + strings.ReplaceAll(connection, "-", "") + ".gitlab.example.test"
				mr = linkConditionVCSGitLabMR(t, f, issue, connection, instanceURL, "group/subgroup/project", 902, 8, "open", "head-a")
			}
			setConflict := func(head string, known *bool, conflictHead string) {
				if source == "gitlab_connection" {
					setGitLabMRConflict(t, f, mr, head, known, conflictHead)
				} else {
					setVCSGitLabConflict(t, f, mr, head, known, conflictHead)
				}
			}
			s.Tasks.GitLabWakeupSyncer = conditionGitLabWakeupSyncFunc(func(context.Context, vcs.GitLabWakeupTarget) error { return nil })
			if source == "gitlab_connection" {
				f.Exec(t, "UPDATE gitlab_merge_request SET pipeline_sync_at=now(),pipeline_sync_error='source project CI API unavailable' WHERE id=$1", mr)
			} else {
				f.Exec(t, "UPDATE vcs_pull_request SET pipeline_sync_at=now(),pipeline_sync_error='source project CI API unavailable' WHERE id=$1", mr)
			}
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Resolve conflicts",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": "conflicts"})})
			got := wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 0 || !got.LastError.Valid {
				t.Fatalf("unknown conflict result started a run or hid its reason: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
			}
			yes := true
			setConflict("head-a", &yes, "old-head")
			got = wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 0 || !got.LastError.Valid {
				t.Fatalf("old-head conflict result was accepted: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
			}
			setConflict("head-a", &yes, "head-a")
			got = wakeTick(t, f, s, w.ID)
			satisfiedState := got.ConditionState
			if wakeRuns(t, f, w.ID) != 1 || satisfiedState == "" {
				t.Fatalf("current-head conflict started %d runs with state %q, want one run and a retained fingerprint", wakeRuns(t, f, w.ID), satisfiedState)
			}
			finishConditionWakeupRuns(t, f, w.ID)
			setConflict("head-a", nil, "")
			got = wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 1 || !got.LastError.Valid || got.ConditionState != satisfiedState {
				t.Fatalf("temporary unknown conflict lost its wait reason or fingerprint: runs=%d error=%+v state=%q want %q", wakeRuns(t, f, w.ID), got.LastError, got.ConditionState, satisfiedState)
			}
			setConflict("head-a", &yes, "head-a")
			got = wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 1 || got.LastError.Valid || got.ConditionState != satisfiedState {
				t.Fatalf("recovered same conflict fired again or kept a stale diagnostic: runs=%d error=%+v state=%q want %q", wakeRuns(t, f, w.ID), got.LastError, got.ConditionState, satisfiedState)
			}
			no := false
			setConflict("head-a", &no, "head-a")
			wakeTick(t, f, s, w.ID)
			setConflict("head-a", &yes, "head-a")
			wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 2 {
				t.Fatalf("resolved then reappearing conflict started %d runs, want two", wakeRuns(t, f, w.ID))
			}
			finishConditionWakeupRuns(t, f, w.ID)
			setConflict("head-b", &yes, "head-a")
			got = wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 2 || !got.LastError.Valid {
				t.Fatalf("old-head verdict leaked onto a new head: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
			}
			setConflict("head-b", &yes, "head-b")
			wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 3 {
				t.Fatalf("new-head conflict started %d runs, want three", wakeRuns(t, f, w.ID))
			}
		})
	}
}

func TestWakeupConditionGitLabUnknownMRKeepsFingerprintWhileAnotherMRTriggers(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	connection, _ := newConditionGitLabConnection(t, f)
	mrA := linkConditionGitLabMR(t, f, issue, connection, "group/subgroup/project-a", 930, 20, "open", "head-a")
	mrB := linkConditionGitLabMR(t, f, issue, connection, "group/subgroup/project-b", 931, 21, "open", "head-b")
	yes, no := true, false
	setGitLabMRConflict(t, f, mrA, "head-a", &no, "head-a")
	setGitLabMRConflict(t, f, mrB, "head-b", &no, "head-b")
	s.Tasks.GitLabWakeupSyncer = conditionGitLabWakeupSyncFunc(func(context.Context, vcs.GitLabWakeupTarget) error { return nil })
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Resolve conflicts",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "conflicts"})})

	setGitLabMRConflict(t, f, mrA, "head-a", &yes, "head-a")
	got := wakeTick(t, f, s, w.ID)
	if wakeRuns(t, f, w.ID) != 1 {
		t.Fatalf("first conflict started %d runs, want one", wakeRuns(t, f, w.ID))
	}
	finishConditionWakeupRuns(t, f, w.ID)

	setGitLabMRConflict(t, f, mrA, "head-a", nil, "")
	setGitLabMRConflict(t, f, mrB, "head-b", &yes, "head-b")
	got = wakeTick(t, f, s, w.ID)
	if wakeRuns(t, f, w.ID) != 2 {
		t.Fatalf("new known conflict was blocked by an unknown MR: runs=%d", wakeRuns(t, f, w.ID))
	}
	if !strings.Contains(got.ConditionState, "head-a") || !strings.Contains(got.ConditionState, "head-b") {
		t.Fatalf("mixed fingerprint lost an unknown MR or new conflict: %q", got.ConditionState)
	}
	finishConditionWakeupRuns(t, f, w.ID)

	setGitLabMRConflict(t, f, mrA, "head-a", &yes, "head-a")
	got = wakeTick(t, f, s, w.ID)
	if wakeRuns(t, f, w.ID) != 2 {
		t.Fatalf("recovered old conflict re-fired after another MR triggered: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
	}
}

func TestWakeupConditionGitLabChecksFinishedUsesPipelineIdentityAndVersionOnBothSources(t *testing.T) {
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		t.Run(source, func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			var mr string
			if source == "gitlab_connection" {
				connection, _ := newConditionGitLabConnection(t, f)
				mr = linkConditionGitLabMR(t, f, issue, connection, "group/subgroup/project", 903, 9, "open", "same-head")
				setGitLabPipeline(t, f, mr, 1101, "same-head", "success", time.Now().Add(-time.Minute))
			} else {
				connection := newConnection(t, f)
				instanceURL := "https://" + strings.ReplaceAll(connection, "-", "") + ".gitlab.example.test"
				mr = linkConditionVCSGitLabMR(t, f, issue, connection, instanceURL, "group/subgroup/project", 903, 9, "open", "same-head")
				setVCSGitLabPipeline(t, f, mr, 1101, "same-head", "success", time.Now().Add(-time.Minute))
			}
			installConditionGitLabSyncer(t, s, f, nil)
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Review CI",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
			wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 0 {
				t.Fatalf("baseline pipeline fired immediately: %d runs", n)
			}
			if source == "gitlab_connection" {
				setGitLabPipeline(t, f, mr, 1102, "same-head", "success", time.Now().Add(time.Second))
			} else {
				setVCSGitLabPipeline(t, f, mr, 1102, "same-head", "success", time.Now().Add(time.Second))
			}
			got := wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 1 || got.LastError.Valid {
				t.Fatalf("same-head rerun started %d runs (error %+v), want one", n, got.LastError)
			}
			task, err := f.q.GetAgentTask(context.Background(), got.LastTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(task.HandoffNote.String, `"pipeline_id":1102`) || !strings.Contains(task.HandoffNote.String, `"checks":"success"`) {
				t.Fatalf("pipeline identity/version absent from observed facts: %s", task.HandoffNote.String)
			}
			finishConditionWakeupRuns(t, f, w.ID)
			if source == "gitlab_connection" {
				setGitLabPipeline(t, f, mr, 1102, "same-head", "success", time.Now().Add(2*time.Second))
			} else {
				setVCSGitLabPipeline(t, f, mr, 1102, "same-head", "success", time.Now().Add(2*time.Second))
			}
			wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 2 {
				t.Fatalf("same pipeline's newer terminal version started %d runs, want two", n)
			}
		})
	}
}

func TestWakeupConditionGitLabChecksWaitForIndependentPipelineRefresh(t *testing.T) {
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		t.Run(source, func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			var mr string
			if source == "gitlab_connection" {
				connection, _ := newConditionGitLabConnection(t, f)
				mr = linkConditionGitLabMR(t, f, issue, connection, "group/subgroup/project", 909, 14, "open", "head")
				setGitLabPipeline(t, f, mr, 1801, "head", "success", time.Now().Add(-time.Minute))
				f.Exec(t, "UPDATE gitlab_merge_request SET pipeline_sync_at=now(),pipeline_sync_error='source project CI API unavailable' WHERE id=$1", mr)
			} else {
				connection := newConnection(t, f)
				instanceURL := "https://" + strings.ReplaceAll(connection, "-", "") + ".gitlab.example.test"
				mr = linkConditionVCSGitLabMR(t, f, issue, connection, instanceURL, "group/subgroup/project", 909, 14, "open", "head")
				setVCSGitLabPipeline(t, f, mr, 1801, "head", "success", time.Now().Add(-time.Minute))
				f.Exec(t, "UPDATE vcs_pull_request SET pipeline_sync_at=now(),pipeline_sync_error='source project CI API unavailable' WHERE id=$1", mr)
			}
			s.Tasks.GitLabWakeupSyncer = conditionGitLabWakeupSyncFunc(func(context.Context, vcs.GitLabWakeupTarget) error { return nil })
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Wait for current CI",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
			got := wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 0 || !got.LastError.Valid {
				t.Fatalf("failed pipeline refresh fired stale CI or hid its reason: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
			}
			if source == "gitlab_connection" {
				f.Exec(t, "UPDATE gitlab_merge_request SET pipeline_sync_at=now(),pipeline_sync_error=NULL WHERE id=$1", mr)
			} else {
				f.Exec(t, "UPDATE vcs_pull_request SET pipeline_sync_at=now(),pipeline_sync_error=NULL WHERE id=$1", mr)
			}
			got = wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 0 || got.LastError.Valid {
				t.Fatalf("old pipeline triggered after CI sync recovered: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
			}
			newVersion := w.CreatedAt.Time.Add(time.Minute)
			if source == "gitlab_connection" {
				setGitLabPipeline(t, f, mr, 1802, "head", "success", newVersion)
			} else {
				setVCSGitLabPipeline(t, f, mr, 1802, "head", "success", newVersion)
			}
			got = wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 1 || got.LastError.Valid {
				t.Fatalf("new CI result did not trigger after recovery: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
			}
		})
	}
}

func TestWakeupConditionGitLabMirroredPipelinesShareFingerprint(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	repository := "group/subgroup/project"
	gitlabConnection, host := newConditionGitLabConnection(t, f)
	legacyMR := linkConditionGitLabMR(t, f, issue, gitlabConnection, repository, 906, 11, "open", "same-head")
	vcsConnection := newConnection(t, f)
	vcsMR := linkConditionVCSGitLabMR(t, f, issue, vcsConnection, "https://"+host, repository, 906, 11, "open", "same-head")
	baselineAt := time.Now().Add(-time.Minute)
	setGitLabPipeline(t, f, legacyMR, 1401, "same-head", "success", baselineAt)
	setVCSGitLabPipeline(t, f, vcsMR, 1401, "same-head", "success", baselineAt)
	installConditionGitLabSyncer(t, s, f, nil)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Review mirrored CI",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("mirrored baseline pipeline fired immediately: %d runs", n)
	}
	updatedAt := time.Now().Add(time.Second)
	setGitLabPipeline(t, f, legacyMR, 1402, "same-head", "success", updatedAt)
	setVCSGitLabPipeline(t, f, vcsMR, 1402, "same-head", "success", updatedAt)
	got := wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 || got.LastError.Valid {
		t.Fatalf("mirrored pipeline started %d runs (error %+v), want one", n, got.LastError)
	}
	pipelineFacts := 0
	for key, count := range prConditionKeys(got.ConditionState) {
		if strings.HasPrefix(key, gitLabPipelineConditionPrefix) {
			pipelineFacts += count
		}
	}
	if pipelineFacts != 2 {
		t.Fatalf("mirrored pipelines produced %d fingerprint facts, want one per pipeline: %s", pipelineFacts, got.ConditionState)
	}
}

func TestWakeupConditionGitLabSourcePipelineMustMatchMRSourceBranch(t *testing.T) {
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		t.Run(source, func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			const targetProjectID, sourceProjectID = 9200, 9201
			targetRepository := "target/subgroup/project"
			sourceRepository := "fork/subgroup/project"
			branch := "feature/wakeup"
			var mr string
			if source == "gitlab_connection" {
				connection, _ := newConditionGitLabConnection(t, f)
				mr = linkConditionGitLabMR(t, f, issue, connection, targetRepository, targetProjectID, 13, "open", "head")
				f.Exec(t, `UPDATE gitlab_merge_request SET source_project_id=$2,source_project_path=$3,source_branch=$4 WHERE id=$1`,
					mr, sourceProjectID, sourceRepository, branch)
			} else {
				connection := newConnection(t, f)
				instanceURL := "https://" + strings.ReplaceAll(connection, "-", "") + ".gitlab.example.test"
				mr = linkConditionVCSGitLabMR(t, f, issue, connection, instanceURL, targetRepository, targetProjectID, 13, "open", "head")
				f.Exec(t, `UPDATE vcs_pull_request SET source_project_id=$2,source_project_path=$3,branch=$4 WHERE id=$1`,
					mr, sourceProjectID, sourceRepository, branch)
			}
			installConditionGitLabSyncer(t, s, f, nil)
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Review source branch CI",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
			updatedAt := w.CreatedAt.Time.Add(time.Minute)
			if source == "gitlab_connection" {
				setGitLabPipeline(t, f, mr, 1601, "head", "success", updatedAt)
				f.Exec(t, "UPDATE gitlab_mr_pipeline SET ref='wrong-branch' WHERE merge_request_id=$1 AND pipeline_id=1601", mr)
			} else {
				setVCSGitLabPipeline(t, f, mr, 1601, "head", "success", updatedAt)
				f.Exec(t, "UPDATE vcs_gitlab_pipeline SET ref='wrong-branch' WHERE pull_request_id=$1 AND pipeline_id=1601", mr)
			}
			got := wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 0 || !got.LastError.Valid {
				t.Fatalf("unrelated source branch pipeline fired or hid pending status: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
			}
			if source == "gitlab_connection" {
				f.Exec(t, "UPDATE gitlab_mr_pipeline SET ref=$3 WHERE merge_request_id=$1 AND pipeline_id=$2", mr, 1601, branch)
			} else {
				f.Exec(t, "UPDATE vcs_gitlab_pipeline SET ref=$3 WHERE pull_request_id=$1 AND pipeline_id=$2", mr, 1601, branch)
			}
			got = wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 1 || got.LastError.Valid {
				t.Fatalf("authorized source branch pipeline did not recover: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
			}
		})
	}
}

func TestWakeupConditionGitLabLegacyUnknownPipelineProjectIsPending(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	connection, _ := newConditionGitLabConnection(t, f)
	mr := linkConditionGitLabMR(t, f, issue, connection, "group/subgroup/project", 907, 12, "open", "head")
	installConditionGitLabSyncer(t, s, f, nil)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Review CI",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
	f.Exec(t, `INSERT INTO gitlab_mr_pipeline(merge_request_id,pipeline_id,pipeline_project_id,sha,status,pipeline_updated_at)
		VALUES($1,1501,999999,'head','success',now())`, mr)
	got := wakeTick(t, f, s, w.ID)
	if wakeRuns(t, f, w.ID) != 0 || !got.LastError.Valid || !strings.Contains(got.LastError.String, "no terminal pipeline") {
		t.Fatalf("unknown pipeline project was not held as pending: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
	}
	setGitLabPipeline(t, f, mr, 1502, "head", "success", time.Now().Add(time.Second))
	got = wakeTick(t, f, s, w.ID)
	if wakeRuns(t, f, w.ID) != 1 || got.LastError.Valid {
		t.Fatalf("known current project did not recover: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
	}
}

func TestWakeupConditionGitLabChecksUnavailableBaselineUsesActivationTime(t *testing.T) {
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		t.Run(source, func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			var mr string
			if source == "gitlab_connection" {
				connection, _ := newConditionGitLabConnection(t, f)
				mr = linkConditionGitLabMR(t, f, issue, connection, "group/subgroup/project", 904, 10, "open", "same-head")
				f.Exec(t, "UPDATE gitlab_merge_request SET last_refreshed_at=NULL,last_refresh_error='temporarily unavailable' WHERE id=$1", mr)
			} else {
				connection := newConnection(t, f)
				instanceURL := "https://" + strings.ReplaceAll(connection, "-", "") + ".gitlab.example.test"
				mr = linkConditionVCSGitLabMR(t, f, issue, connection, instanceURL, "group/subgroup/project", 904, 10, "open", "same-head")
				f.Exec(t, "UPDATE vcs_pull_request SET status_sync_at=NULL,status_sync_error='temporarily unavailable' WHERE id=$1", mr)
			}
			installConditionGitLabSyncer(t, s, f, nil)
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Wait for a new pipeline",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
			if !w.LastError.Valid {
				t.Fatal("unavailable baseline did not expose its reason")
			}
			oldVersion := w.CreatedAt.Time.Add(-time.Minute)
			if source == "gitlab_connection" {
				setGitLabPipeline(t, f, mr, 1201, "same-head", "failed", oldVersion)
			} else {
				setVCSGitLabPipeline(t, f, mr, 1201, "same-head", "failed", oldVersion)
			}
			got := wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 0 || got.LastError.Valid {
				t.Fatalf("pre-activation pipeline fired or recovered diagnostic remained: runs=%d error=%+v", wakeRuns(t, f, w.ID), got.LastError)
			}
			if source == "gitlab_connection" {
				setGitLabPipeline(t, f, mr, 1202, "same-head", "success", w.CreatedAt.Time.Add(time.Minute))
			} else {
				setVCSGitLabPipeline(t, f, mr, 1202, "same-head", "success", w.CreatedAt.Time.Add(time.Minute))
			}
			wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 1 {
				t.Fatalf("post-activation pipeline started %d runs, want one", n)
			}
		})
	}
}

func TestWakeupConditionGitLabPipelineUpdatedAfterActivationIsNotAbsorbedByBaseline(t *testing.T) {
	for _, source := range []string{"gitlab_connection", "vcs_connection"} {
		t.Run(source, func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			var mr string
			if source == "gitlab_connection" {
				connection, _ := newConditionGitLabConnection(t, f)
				mr = linkConditionGitLabMR(t, f, issue, connection, "group/subgroup/project", 908, 13, "open", "head")
				setGitLabPipeline(t, f, mr, 1701, "head", "success", time.Now().Add(time.Minute))
			} else {
				connection := newConnection(t, f)
				instanceURL := "https://" + strings.ReplaceAll(connection, "-", "") + ".gitlab.example.test"
				mr = linkConditionVCSGitLabMR(t, f, issue, connection, instanceURL, "group/subgroup/project", 908, 13, "open", "head")
				setVCSGitLabPipeline(t, f, mr, 1701, "head", "success", time.Now().Add(time.Minute))
			}
			installConditionGitLabSyncer(t, s, f, nil)
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Wait for pipeline completion",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
			got := wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 1 || got.LastError.Valid {
				t.Fatalf("pipeline updated after activation started %d runs (error %+v), want one", n, got.LastError)
			}
		})
	}
}

func TestWakeupConditionGitLabPipelineTerminalStatesAreAtPipelineLevel(t *testing.T) {
	for _, tc := range []struct {
		status, checks string
	}{
		{"success", "success"},
		{"skipped", "success"},
		{"failed", "failure"},
		{"canceled", "failure"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			f, s, issue, agent := conditionFixture(t)
			connection, _ := newConditionGitLabConnection(t, f)
			mr := linkConditionGitLabMR(t, f, issue, connection, "group/subgroup/project", 905, 11, "open", "head")
			installConditionGitLabSyncer(t, s, f, nil)
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Wait for CI",
				Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
			setGitLabPipeline(t, f, mr, 1301, "head", tc.status, w.CreatedAt.Time.Add(time.Second))
			got := wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 1 || got.LastError.Valid {
				t.Fatalf("terminal pipeline %q started %d runs (error %+v), want one", tc.status, n, got.LastError)
			}
			task, err := f.q.GetAgentTask(context.Background(), got.LastTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(task.HandoffNote.String, `"checks":"`+tc.checks+`"`) {
				t.Fatalf("terminal pipeline %q was not normalized to %q: %s", tc.status, tc.checks, task.HandoffNote.String)
			}
		})
	}
}

func TestWakeupConditionGitLabDoesNotSyncForUnrelatedCondition(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	connection, _ := newConditionGitLabConnection(t, f)
	linkConditionGitLabMR(t, f, issue, connection, "group/subgroup/project", 906, 12, "open", "head")
	calls := 0
	installConditionGitLabSyncer(t, s, f, func(vcs.GitLabWakeupTarget) {
		calls++
	})
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Wait for children",
		Condition: condition(t, map[string]any{"type": "children_done"})})
	wakeTick(t, f, s, w.ID)
	if calls != 0 {
		t.Fatalf("unrelated condition caused %d GitLab refreshes", calls)
	}
}
