package gitlab

import "strings"

type Project struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	HTTPURLToRepo     string `json:"http_url_to_repo"`
	SSHURLToRepo      string `json:"ssh_url_to_repo"`
}

type ProjectHook struct {
	ID int64 `json:"id"`
}

type MergeRequest struct {
	IID                 int32     `json:"iid"`
	Title               string    `json:"title"`
	Description         string    `json:"description"`
	State               string    `json:"state"`
	Draft               bool      `json:"draft"`
	WebURL              string    `json:"web_url"`
	SourceBranch        string    `json:"source_branch"`
	TargetBranch        string    `json:"target_branch"`
	SHA                 string    `json:"sha"`
	MergeCommitSHA      string    `json:"merge_commit_sha"`
	DetailedMergeStatus string    `json:"detailed_merge_status"`
	HasConflicts        bool      `json:"has_conflicts"`
	ChangesCount        string    `json:"changes_count"`
	CreatedAt           string    `json:"created_at"`
	UpdatedAt           string    `json:"updated_at"`
	MergedAt            string    `json:"merged_at"`
	ClosedAt            string    `json:"closed_at"`
	Reviewers           []UserRef `json:"reviewers"`
	Assignees           []UserRef `json:"assignees"`
	Labels              []string  `json:"labels"`
	Author              struct {
		Username  string `json:"username"`
		AvatarURL string `json:"avatar_url"`
	} `json:"author"`
}

type MergeRequestMergeOptions struct {
	ShouldRemoveSourceBranch bool `json:"should_remove_source_branch"`
}

type Pipeline struct {
	ID        int64  `json:"id"`
	SHA       string `json:"sha"`
	Ref       string `json:"ref"`
	Status    string `json:"status"`
	WebURL    string `json:"web_url"`
	UpdatedAt string `json:"updated_at"`
}

type PipelineFilters struct {
	SHA string
	Ref string
}

type UserRef struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	WebURL    string `json:"web_url"`
}

type MergeRequestChange struct {
	OldPath string `json:"old_path"`
	NewPath string `json:"new_path"`
	Diff    string `json:"diff"`
}

type MergeRequestChanges struct {
	ChangesCount string               `json:"changes_count"`
	Changes      []MergeRequestChange `json:"changes"`
}

func (c MergeRequestChanges) ChangedFiles() int32 {
	return int32(len(c.Changes))
}

func (c MergeRequestChanges) Additions() int32 {
	var n int32
	for _, change := range c.Changes {
		for _, line := range strings.Split(change.Diff, "\n") {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				n++
			}
		}
	}
	return n
}

func (c MergeRequestChanges) Deletions() int32 {
	var n int32
	for _, change := range c.Changes {
		for _, line := range strings.Split(change.Diff, "\n") {
			if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				n++
			}
		}
	}
	return n
}

type ApprovalState struct {
	Approved          bool            `json:"approved"`
	ApprovalsRequired *int32          `json:"approvals_required"`
	ApprovalsLeft     *int32          `json:"approvals_left"`
	ApprovedBy        []ApprovalEntry `json:"approved_by"`
	Rules             []ApprovalRule  `json:"rules"`
}

type ApprovalEntry struct {
	User UserRef `json:"user"`
}

type ApprovalRule struct {
	ID                int64     `json:"id"`
	Name              string    `json:"name"`
	Approved          bool      `json:"approved"`
	ApprovalsRequired int32     `json:"approvals_required"`
	ApprovedBy        []UserRef `json:"approved_by"`
}

type Discussion struct {
	ID             string `json:"id"`
	IndividualNote bool   `json:"individual_note"`
	Resolved       *bool  `json:"resolved"`
	Notes          []Note `json:"notes"`
}

type Note struct {
	ID         int64   `json:"id"`
	Body       string  `json:"body"`
	System     bool    `json:"system"`
	Resolved   *bool   `json:"resolved"`
	Resolvable *bool   `json:"resolvable"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
	Author     UserRef `json:"author"`
}

type Job struct {
	ID                int64            `json:"id"`
	Name              string           `json:"name"`
	Stage             string           `json:"stage"`
	Status            string           `json:"status"`
	Ref               string           `json:"ref"`
	SHA               string           `json:"sha"`
	WebURL            string           `json:"web_url"`
	StartedAt         string           `json:"started_at"`
	FinishedAt        string           `json:"finished_at"`
	Duration          *float64         `json:"duration"`
	QueuedDuration    *float64         `json:"queued_duration"`
	FailureReason     string           `json:"failure_reason"`
	AllowFailure      bool             `json:"allow_failure"`
	ArtifactsFile     *JobArtifactFile `json:"artifacts_file"`
	ArtifactsExpireAt string           `json:"artifacts_expire_at"`
}

type JobArtifactFile struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

type JobTrace struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}
