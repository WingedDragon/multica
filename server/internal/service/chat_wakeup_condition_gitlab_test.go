package service

import (
	"testing"
	"time"
)

func TestChatWakeupGitLabPipelineStatusesArePipelineTerminal(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
	}{
		{status: "success", want: "success"},
		{status: "skipped", want: "success"},
		{status: "passed", want: "success"},
		{status: "failed", want: "failure"},
		{status: "failure", want: "failure"},
		{status: "canceled", want: "failure"},
		{status: "cancelled", want: "failure"},
		{status: "running"},
		{status: "pending"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			if got := normalizeGitLabPipelineStatus(tc.status); got != tc.want {
				t.Fatalf("normalizeGitLabPipelineStatus(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestNormalizeChatPRIdentityPreservesGitHubAndAcceptsGitLab(t *testing.T) {
	for _, tc := range []struct {
		name             string
		condition        ChatWakeupCondition
		wantRepo         string
		wantProvider     string
		wantConnectionID string
	}{
		{
			name: "legacy GitHub identity",
			condition: ChatWakeupCondition{
				Type: "pull_request", Repo: "octo/widgets", Number: 17, Event: "merged",
			},
			wantRepo: "octo/widgets",
		},
		{
			name: "connection-scoped GitLab identity",
			condition: ChatWakeupCondition{
				Type: "pull_request", Provider: "gitlab", ConnectionID: "44B522CA-5FE6-4EA4-B686-263946524F14",
				Repo: "group/subgroup/project", Number: 17, Event: "checks_finished",
			},
			wantRepo: "group/subgroup/project", wantProvider: "gitlab",
			wantConnectionID: "44b522ca-5fe6-4ea4-b686-263946524f14",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := normalizeChatPRIdentity(&tc.condition); err != nil {
				t.Fatal(err)
			}
			if tc.condition.Repo != tc.wantRepo || tc.condition.Provider != tc.wantProvider || tc.condition.ConnectionID != tc.wantConnectionID {
				t.Fatalf("normalized identity = %+v", tc.condition)
			}
		})
	}
}

func TestChatWakeupGitLabPipelineBaselineDistinguishesReruns(t *testing.T) {
	activatedAt := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	baselineUpdatedAt := activatedAt.Add(-time.Minute)
	baseline := []chatGitLabPipelineVersion{{
		PipelineID: "51", PipelineProjectID: "100", PipelineProjectPath: "group/source",
		SHA: "current-head", Status: "success", UpdatedAt: baselineUpdatedAt.Format(time.RFC3339Nano),
	}}
	cases := []struct {
		name              string
		pipeline          chatGitLabPipelineVersion
		baselineAvailable bool
		want              bool
	}{
		{
			name: "same version as baseline is old",
			pipeline: chatGitLabPipelineVersion{
				PipelineID: "51", PipelineProjectID: "100", PipelineProjectPath: "group/source",
				SHA: "current-head", Status: "success", UpdatedAt: baselineUpdatedAt.Format(time.RFC3339Nano),
			},
			baselineAvailable: true,
			want:              false,
		},
		{
			name: "same pipeline identity on another source project is new",
			pipeline: chatGitLabPipelineVersion{
				PipelineID: "51", PipelineProjectID: "101", PipelineProjectPath: "group/fork",
				SHA: "current-head", Status: "success", UpdatedAt: activatedAt.Add(time.Minute).Format(time.RFC3339Nano),
			},
			baselineAvailable: true,
			want:              true,
		},
		{
			name: "unbaselined historical pipeline delivered late stays old",
			pipeline: chatGitLabPipelineVersion{
				PipelineID: "99", PipelineProjectID: "100", PipelineProjectPath: "group/source",
				SHA: "current-head", Status: "success", UpdatedAt: baselineUpdatedAt.Format(time.RFC3339Nano),
			},
			baselineAvailable: true,
			want:              false,
		},
		{
			name: "new pipeline with same outcome is new",
			pipeline: chatGitLabPipelineVersion{
				PipelineID: "52", PipelineProjectID: "100", PipelineProjectPath: "group/source",
				SHA: "current-head", Status: "success", UpdatedAt: activatedAt.Add(time.Minute).Format(time.RFC3339Nano),
			},
			baselineAvailable: true,
			want:              true,
		},
		{
			name: "same pipeline rerun with same outcome is new",
			pipeline: chatGitLabPipelineVersion{
				PipelineID: "51", PipelineProjectID: "100", PipelineProjectPath: "group/source",
				SHA: "current-head", Status: "success", UpdatedAt: activatedAt.Add(time.Minute).Format(time.RFC3339Nano),
			},
			baselineAvailable: true,
			want:              true,
		},
		{
			name: "same pipeline rerun with changed outcome is new",
			pipeline: chatGitLabPipelineVersion{
				PipelineID: "51", PipelineProjectID: "100", PipelineProjectPath: "group/source",
				SHA: "current-head", Status: "failure", UpdatedAt: activatedAt.Add(time.Minute).Format(time.RFC3339Nano),
			},
			baselineAvailable: true,
			want:              true,
		},
		{
			name: "another head is not the current result",
			pipeline: chatGitLabPipelineVersion{
				PipelineID: "53", SHA: "old-head", Status: "success",
				UpdatedAt: activatedAt.Add(time.Minute).Format(time.RFC3339Nano),
			},
			baselineAvailable: true,
			want:              false,
		},
		{
			name: "nonterminal pipeline does not fire",
			pipeline: chatGitLabPipelineVersion{
				PipelineID: "54", SHA: "current-head", Status: "running",
				UpdatedAt: activatedAt.Add(time.Minute).Format(time.RFC3339Nano),
			},
			baselineAvailable: true,
			want:              false,
		},
		{
			name: "old pipeline after missing baseline recovery stays old",
			pipeline: chatGitLabPipelineVersion{
				PipelineID: "55", SHA: "current-head", Status: "failure",
				UpdatedAt: activatedAt.Add(-time.Second).Format(time.RFC3339Nano),
			},
			baselineAvailable: false,
			want:              false,
		},
		{
			name: "pipeline updated after missing baseline recovery fires",
			pipeline: chatGitLabPipelineVersion{
				PipelineID: "56", SHA: "current-head", Status: "failure",
				UpdatedAt: activatedAt.Add(time.Second).Format(time.RFC3339Nano),
			},
			baselineAvailable: false,
			want:              true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chatGitLabPipelineAfterBaseline(tc.pipeline, "current-head", tc.baselineAvailable, baseline, activatedAt)
			if got != tc.want {
				t.Fatalf("pipeline after baseline = %t, want %t", got, tc.want)
			}
		})
	}
}
