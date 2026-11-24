package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/util"
)

func init() { chatCmd.AddCommand(newChatWakeupCommand()) }

const chatWakeupLong = `Come back to this conversation later, on your own, to report.

Create one when you act toward someone else for the user and they want the
outcome back (a decision, an answer, a review, work done), and whenever you
tell the user you will report back: at a time they named, or while you keep an
eye on something. Skip it when the user only wanted someone told. The wakeup
starts a new turn in THIS conversation (and its IM
thread, when it came from one), so your reply lands where the user asked.

Kinds (pick exactly one):
  --until-*            the platform checks a fact and wakes you once it holds
  --at / --after       a single reminder at a time
  --every              a periodic check; quiet check-ins send nothing

Every wakeup ends. Conditions and checks run until --expires-in/--expires-at
(default 24h, at most 7 days); if the deadline comes first you are woken once
more to tell the user where things stand. Tell the user what you are waiting
for and until when. A conversation holds at most 10 active wakeups.

For GitHub, use owner/repo#N[:merged|closed|checks]. For GitLab, use the full
group/subgroup/project#N path and pass --gitlab-connection. Run
'multica chat wakeup connections' to list IDs for both GitLab integrations.

Recurring work (every morning, every Friday) is not a chat wakeup: suggest an
Autopilot instead.`

func newChatWakeupCommand() *cobra.Command {
	wake := &cobra.Command{Use: "wakeup", Short: "Come back to this conversation when something happens", Long: chatWakeupLong}

	create := &cobra.Command{Use: "create", Args: cobra.NoArgs, Short: "Create a wakeup for this conversation", Long: chatWakeupLong,
		RunE: func(cmd *cobra.Command, _ []string) error { return runChatWakeupWrite(cmd, "") }}
	update := &cobra.Command{Use: "update <wakeup-id>", Args: cobra.ExactArgs(1), Short: "Replace a wakeup's configuration (deadlines restart from now)",
		RunE: func(cmd *cobra.Command, args []string) error { return runChatWakeupWrite(cmd, args[0]) }}
	for _, c := range []*cobra.Command{create, update} {
		f := c.Flags()
		f.String("title", "", "What you are waiting for, in the user's words; shown to them (required)")
		f.String("instruction", "", "What to do when you wake up (required)")
		f.String("instruction-file", "", "Read the instruction from a UTF-8 file")
		f.String("until-issue", "", "Condition: issue ISSUE reaches --state (or --status)")
		f.String("state", "", "With --until-issue: done (default), ended (done or cancelled), in_review or blocked")
		f.String("status", "", "With --until-issue: one exact status key instead of --state")
		f.String("until-children-done", "", "Condition: every sub-issue of ISSUE is closed (or those up to --stage)")
		f.Int32("stage", 0, "With --until-children-done: this stage and earlier ones")
		f.String("until-pr", "", "Condition: owner/repo#N[:merged|closed|checks] (GitHub) or full/group/project#N with --gitlab-connection (GitLab)")
		f.String("gitlab-connection", "", "GitLab connection UUID from multica chat wakeup connections (selects GitLab for --until-pr)")
		f.String("until-run", "", "Condition: this agent run finishes (task/run ID)")
		f.String("until-reply", "", "Condition: a member comments on issue ISSUE")
		f.String("from", "", "With --until-reply: only this member (user id)")
		f.String("at", "", "Reminder at this RFC3339 time")
		f.String("after", "", "Reminder after this delay, e.g. 2h")
		f.String("every", "", "Check at this interval, e.g. 30m (at least 10m)")
		f.String("expires-in", "", "Stop waiting after, e.g. 48h (default 24h, at most 168h)")
		f.String("expires-at", "", "Stop waiting at this RFC3339 time")
		f.String("output", "json", "Output format (json or table)")
	}

	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "List this conversation's wakeups",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, ctx, cancel, err := chatWakeupClient(cmd)
			if err != nil {
				return err
			}
			defer cancel()
			var rows []map[string]any
			if err := client.GetJSON(ctx, "/api/chat/wakeups", &rows); err != nil {
				return err
			}
			return printChatWakeups(cmd, rows)
		}}
	get := &cobra.Command{Use: "get <wakeup-id>", Args: cobra.ExactArgs(1), Short: "Show one wakeup",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, ctx, cancel, err := chatWakeupClient(cmd)
			if err != nil {
				return err
			}
			defer cancel()
			var rows []map[string]any
			if err := client.GetJSON(ctx, "/api/chat/wakeups", &rows); err != nil {
				return err
			}
			for _, row := range rows {
				if strVal(row, "id") == args[0] {
					return printChatWakeups(cmd, row)
				}
			}
			return fmt.Errorf("wakeup not found in this conversation")
		}}
	cancelCmd := &cobra.Command{Use: "cancel <wakeup-id>", Args: cobra.ExactArgs(1), Short: "Stop a wakeup (when the user no longer needs it)",
		RunE: func(cmd *cobra.Command, args []string) error { return runChatWakeupAction(cmd, args[0], "cancel", nil) }}
	finish := &cobra.Command{Use: "finish <wakeup-id>", Args: cobra.ExactArgs(1), Short: "End a check (--every) because what it watched for happened",
		RunE: func(cmd *cobra.Command, args []string) error { return runChatWakeupAction(cmd, args[0], "finish", nil) }}
	checkin := &cobra.Command{Use: "checkin <wakeup-id>", Args: cobra.ExactArgs(1), Short: "End a check turn quietly: nothing is sent to the user",
		Long: "Run this from a turn a check (--every) started, when nothing needs the user's attention yet. The note is shown on the wakeup, this turn's reply is not delivered, and the next check runs on schedule.",
		RunE: func(cmd *cobra.Command, args []string) error {
			note, _ := cmd.Flags().GetString("note")
			if strings.TrimSpace(note) == "" {
				return fmt.Errorf("--note is required")
			}
			if err := runChatWakeupAction(cmd, args[0], "checkin", map[string]any{"note": note}); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "Checked in; this turn's reply will not be sent.")
			return nil
		}}
	checkin.Flags().String("note", "", "What the check saw, e.g. \"CI still running\" (up to 500 characters)")
	connections := &cobra.Command{
		Use:   "connections",
		Args:  cobra.NoArgs,
		Short: "List GitLab connection IDs for --gitlab-connection",
		Long:  "List GitLab connection IDs and hosts from this workspace.",
		RunE:  runChatWakeupConnections,
	}
	connections.Flags().String("output", "table", "Output format (json or table)")
	for _, c := range []*cobra.Command{list, get, cancelCmd, finish, checkin} {
		c.Flags().String("output", "json", "Output format (json or table)")
	}
	wake.AddCommand(create, list, get, update, cancelCmd, finish, checkin, connections)
	return wake
}

func chatWakeupClient(cmd *cobra.Command) (*cli.APIClient, context.Context, context.CancelFunc, error) {
	client, err := newAPIClient(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := cli.APIContext(context.Background())
	return client, ctx, cancel, nil
}

func runChatWakeupAction(cmd *cobra.Command, id, action string, body map[string]any) error {
	client, ctx, cancel, err := chatWakeupClient(cmd)
	if err != nil {
		return err
	}
	defer cancel()
	if body == nil {
		body = map[string]any{}
	}
	var row map[string]any
	if err := client.PostJSON(ctx, "/api/chat/wakeups/"+url.PathEscape(id)+"/"+action, body, &row); err != nil {
		return err
	}
	if action == "checkin" {
		return nil
	}
	return printChatWakeups(cmd, row)
}

func runChatWakeupWrite(cmd *cobra.Command, id string) error {
	client, ctx, cancel, err := chatWakeupClient(cmd)
	if err != nil {
		return err
	}
	defer cancel()
	title, _ := cmd.Flags().GetString("title")
	instruction, _, err := resolveTextFlag(cmd, "instruction")
	if err != nil {
		return err
	}
	body := map[string]any{"title": title, "instruction": instruction}
	condition, err := chatWakeupConditionFromFlags(ctx, cmd, client)
	if err != nil {
		return err
	}
	if condition != nil {
		body["condition"] = condition
	}
	for flag, key := range map[string]string{"at": "at", "expires-at": "expires_at"} {
		if v, _ := cmd.Flags().GetString(flag); v != "" {
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				return fmt.Errorf("--%s takes an RFC3339 time, e.g. 2026-05-01T09:00:00+08:00", flag)
			}
			body[key] = v
		}
	}
	for flag, key := range map[string]string{"after": "after_seconds", "every": "interval_seconds", "expires-in": "expires_in_seconds"} {
		if v, _ := cmd.Flags().GetString(flag); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 || d%time.Second != 0 {
				return fmt.Errorf("--%s requires a positive whole-second duration, e.g. 30m", flag)
			}
			body[key] = int64(d / time.Second)
		}
	}
	var row map[string]any
	if id == "" {
		err = client.PostJSON(ctx, "/api/chat/wakeups", body, &row)
	} else {
		err = client.PutJSON(ctx, "/api/chat/wakeups/"+url.PathEscape(id), body, &row)
	}
	if err != nil {
		return err
	}
	return printChatWakeups(cmd, row)
}

// chatWakeupConditionFromFlags builds the one condition the --until-* flags
// describe, or nil when none is set.
func chatWakeupConditionFromFlags(ctx context.Context, cmd *cobra.Command, client *cli.APIClient) (map[string]any, error) {
	var conditions []map[string]any
	issueID := func(ref string) (string, error) {
		r, err := resolveIssueRef(ctx, client, ref)
		if err != nil {
			return "", err
		}
		return r.ID, nil
	}
	state, _ := cmd.Flags().GetString("state")
	status, _ := cmd.Flags().GetString("status")
	if v, _ := cmd.Flags().GetString("until-issue"); v != "" {
		id, err := issueID(v)
		if err != nil {
			return nil, err
		}
		c := map[string]any{"type": "issue", "issue_id": id}
		switch {
		case state != "" && status != "":
			return nil, fmt.Errorf("use --state or --status, not both")
		case status != "":
			c["status"] = status
		case state != "":
			c["state"] = state
		default:
			c["state"] = "done"
		}
		conditions = append(conditions, c)
	} else if state != "" || status != "" {
		return nil, fmt.Errorf("--state and --status require --until-issue")
	}
	stage, _ := cmd.Flags().GetInt32("stage")
	if v, _ := cmd.Flags().GetString("until-children-done"); v != "" {
		id, err := issueID(v)
		if err != nil {
			return nil, err
		}
		c := map[string]any{"type": "children_done", "issue_id": id}
		if stage != 0 {
			c["stage"] = stage
		}
		conditions = append(conditions, c)
	} else if stage != 0 {
		return nil, fmt.Errorf("--stage requires --until-children-done")
	}
	gitlabConnection, _ := cmd.Flags().GetString("gitlab-connection")
	if v, _ := cmd.Flags().GetString("until-pr"); v != "" {
		c, err := parseChatWakeupPR(v, gitlabConnection)
		if err != nil {
			return nil, err
		}
		conditions = append(conditions, c)
	} else if gitlabConnection != "" {
		return nil, fmt.Errorf("--gitlab-connection requires --until-pr")
	}
	if v, _ := cmd.Flags().GetString("until-run"); v != "" {
		conditions = append(conditions, map[string]any{"type": "run", "task_id": v})
	}
	from, _ := cmd.Flags().GetString("from")
	if v, _ := cmd.Flags().GetString("until-reply"); v != "" {
		id, err := issueID(v)
		if err != nil {
			return nil, err
		}
		c := map[string]any{"type": "issue_reply", "issue_id": id}
		if from != "" {
			c["author_id"] = from
		}
		conditions = append(conditions, c)
	} else if from != "" {
		return nil, fmt.Errorf("--from requires --until-reply")
	}
	switch len(conditions) {
	case 0:
		return nil, nil
	case 1:
		return conditions[0], nil
	default:
		return nil, fmt.Errorf("use one --until-* condition per wakeup")
	}
}

// parseChatWakeupPR reads owner/repo#N[:merged|closed|checks], or a full
// GitLab namespace/project path when a connection UUID is supplied.
func parseChatWakeupPR(v, gitlabConnection string) (map[string]any, error) {
	ref, event, _ := strings.Cut(v, ":")
	repo, num, ok := strings.Cut(ref, "#")
	n, err := strconv.Atoi(num)
	parts := strings.Split(repo, "/")
	if !ok || err != nil || n <= 0 || (gitlabConnection == "" && len(parts) != 2) || (gitlabConnection != "" && len(parts) < 2) {
		return nil, fmt.Errorf("--until-pr takes owner/repo#N[:merged|closed|checks] for GitHub, or full/group/path#N[:merged|closed|checks] with --gitlab-connection")
	}
	for _, part := range parts {
		if strings.TrimSpace(part) != part || part == "" {
			return nil, fmt.Errorf("--until-pr requires a non-empty repository path without surrounding whitespace")
		}
	}
	events := map[string]string{"": "merged", "merged": "merged", "closed": "closed", "checks": "checks_finished"}
	e, ok := events[event]
	if !ok {
		return nil, fmt.Errorf("--until-pr event must be merged, closed or checks")
	}
	condition := map[string]any{"type": "pull_request", "repo": repo, "number": n, "event": e}
	if gitlabConnection == "" {
		return condition, nil
	}
	id, err := util.ParseUUID(gitlabConnection)
	if err != nil {
		return nil, fmt.Errorf("--gitlab-connection must be a connection UUID")
	}
	condition["provider"] = "gitlab"
	condition["connection_id"] = util.UUIDToString(id)
	return condition, nil
}

func printChatWakeups(cmd *cobra.Command, result any) error {
	output, _ := cmd.Flags().GetString("output")
	if output != "table" {
		return cli.PrintJSON(os.Stdout, result)
	}
	var rows []map[string]any
	switch r := result.(type) {
	case []map[string]any:
		rows = r
	case map[string]any:
		rows = []map[string]any{r}
	}
	cells := [][]string{}
	for _, r := range rows {
		next := strVal(r, "next_eval_at")
		if strVal(r, "status") != "active" {
			next = ""
		}
		cells = append(cells, []string{strVal(r, "id"), strVal(r, "kind"), strVal(r, "status"), strVal(r, "title"), chatWakeupTarget(r), next, strVal(r, "expires_at")})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "KIND", "STATUS", "TITLE", "TARGET", "NEXT", "EXPIRES"}, cells)
	return nil
}
func chatWakeupTarget(row map[string]any) string {
	condition, ok := row["condition"].(map[string]any)
	if !ok || strVal(condition, "type") != "pull_request" {
		return ""
	}
	repo, number := strVal(condition, "repo"), fmt.Sprint(condition["number"])
	if repo == "" || number == "<nil>" {
		return ""
	}
	event := strVal(condition, "event")
	if strVal(condition, "provider") == "gitlab" {
		return fmt.Sprintf("GitLab %s#%s (%s, connection %s)", repo, number, event, strVal(condition, "connection_id"))
	}
	return fmt.Sprintf("GitHub %s#%s (%s)", repo, number, event)
}

func runChatWakeupConnections(cmd *cobra.Command, _ []string) error {
	client, ctx, cancel, err := chatWakeupClient(cmd)
	if err != nil {
		return err
	}
	defer cancel()
	rows, err := listChatWakeupGitLabConnections(ctx, client)
	if err != nil {
		return err
	}
	return printChatWakeupConnections(cmd, rows)
}

func listChatWakeupGitLabConnections(ctx context.Context, client *cli.APIClient) ([]map[string]any, error) {
	if strings.TrimSpace(client.WorkspaceID) == "" {
		return nil, fmt.Errorf("select a workspace before listing GitLab connections")
	}
	path := "/api/workspaces/" + url.PathEscape(client.WorkspaceID) + "/vcs/connections"
	var response struct {
		Connections []struct {
			ID          string `json:"id"`
			WorkspaceID string `json:"workspace_id"`
			Provider    string `json:"provider"`
			InstanceURL string `json:"instance_url"`
		} `json:"connections"`
	}
	if err := client.GetJSON(ctx, path, &response); err != nil {
		return nil, fmt.Errorf("list VCS connections: %w", err)
	}
	rows := make([]map[string]any, 0, len(response.Connections))
	for _, connection := range response.Connections {
		if connection.Provider != "gitlab" || connection.ID == "" || connection.WorkspaceID != client.WorkspaceID {
			continue
		}
		rows = append(rows, map[string]any{
			"id": connection.ID, "provider": "gitlab", "source": "vcs_connection",
			"instance_url": connection.InstanceURL,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return strVal(rows[i], "id") < strVal(rows[j], "id")
	})
	return rows, nil
}

func printChatWakeupConnections(cmd *cobra.Command, rows []map[string]any) error {
	output, _ := cmd.Flags().GetString("output")
	if output != "table" {
		return cli.PrintJSON(os.Stdout, rows)
	}
	cells := make([][]string, 0, len(rows))
	for _, row := range rows {
		cells = append(cells, []string{strVal(row, "id"), strVal(row, "source"), strVal(row, "instance_url")})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "SOURCE", "INSTANCE"}, cells)
	return nil
}
