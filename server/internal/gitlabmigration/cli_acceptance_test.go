package gitlabmigration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestGitLabCutoverCLICommandsAcceptance(t *testing.T) {
	ctx := context.Background()
	fixture := newCutoverAcceptanceFixture(t)
	moduleRoot := cutoverCLIModuleRoot(t)
	binary := buildCutoverCLIBinary(t, moduleRoot)
	source, _ := seedCutoverCLIInstance(t, fixture, "cli-instance", 711)
	cli := newCutoverCLI(t, fixture, source, binary, moduleRoot)

	initial := runCutoverCLIState(t, ctx, cli, "status")
	if initial.Phase != "maintenance" || !initial.MaintenanceMode || initial.Generation != 0 {
		t.Fatalf("initial CLI status was unexpected: phase=%q maintenance=%v generation=%d", initial.Phase, initial.MaintenanceMode, initial.Generation)
	}
	preflightBytes := runCutoverCLI(t, ctx, cli, "preflight")
	var preflight Report
	if err := json.Unmarshal(preflightBytes, &preflight); err != nil {
		t.Fatalf("decode CLI preflight JSON: %v", err)
	}
	var preflightFields map[string]json.RawMessage
	if err := json.Unmarshal(preflightBytes, &preflightFields); err != nil {
		t.Fatalf("decode CLI preflight JSON fields: %v", err)
	}
	if _, ok := preflightFields["legacy_pipelines"]; !ok {
		t.Fatal("CLI preflight JSON omitted legacy_pipelines")
	}
	if preflight.NoOp || preflight.LegacyConnections != 1 || preflight.LegacyProjects != 1 || preflight.LegacyMergeRequests != 1 ||
		preflight.LegacyPipelines != 0 || preflight.ManagedHooks != 1 {
		t.Fatalf("unexpected CLI preflight inventory: no_op=%v connections=%d projects=%d MRs=%d pipelines=%d Hooks=%d",
			preflight.NoOp, preflight.LegacyConnections, preflight.LegacyProjects, preflight.LegacyMergeRequests, preflight.LegacyPipelines, preflight.ManagedHooks)
	}

	began := runCutoverCLIState(t, ctx, cli, "begin")
	if began.Phase != "maintenance" || !began.MaintenanceMode || began.Generation != 1 {
		t.Fatalf("CLI begin state was unexpected: phase=%q maintenance=%v generation=%d", began.Phase, began.MaintenanceMode, began.Generation)
	}
	migrated := runCutoverCLIState(t, ctx, cli, "migrate")
	if migrated.Phase != "database_migrated" || !migrated.MaintenanceMode {
		t.Fatalf("CLI migrate state was unexpected: phase=%q maintenance=%v", migrated.Phase, migrated.MaintenanceMode)
	}
	connectionID := cutoverAcceptanceAlias(t, fixture.pool, source.workspaceID, "connection", source.legacyConnectionID)
	source.unifiedConnectionID = connectionID

	switched := runCutoverCLIState(t, ctx, cli, "switch-hooks")
	if switched.Phase != "verification_pending" || !switched.MaintenanceMode {
		t.Fatalf("CLI switch-hooks state was unexpected: phase=%q maintenance=%v", switched.Phase, switched.MaintenanceMode)
	}
	remoteHook, ok := fixture.remoteHook(source.instance, source.projectID, source.hookID)
	if !ok || remoteHook.Token != source.unifiedSecret {
		t.Fatalf("CLI switch-hooks did not rotate the remote Hook credential: found=%v rotated=%v", ok, remoteHook.Token == source.unifiedSecret)
	}
	parsedHookURL, err := url.Parse(remoteHook.URL)
	if err != nil {
		t.Fatal("CLI switch-hooks produced an invalid webhook URL")
	}
	if parsedHookURL.Path != "/api/webhooks/vcs/"+connectionID {
		t.Fatalf("CLI switch-hooks did not target the workspace VCS endpoint: path=%q", parsedHookURL.Path)
	}

	since := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	events := []string{"Merge Request Hook", "Pipeline Hook", "Job Hook", "Note Hook"}
	deliveryIDs := make([]string, 0, len(events))
	for _, event := range events {
		deliveryID := "cli-" + strings.ReplaceAll(event, " ", "-")
		if status := cutoverAcceptancePost(t, remoteHook.URL, source.unifiedSecret, deliveryID, event,
			cutoverAcceptanceEvent(event, source.projectID, source.path)); status != 503 {
			t.Fatalf("maintenance %q webhook response = %d, want 503", event, status)
		}
		deliveryIDs = append(deliveryIDs, deliveryID)
	}
	wantReceipts := int64(len(events))
	assertCutoverReceiptCounts(t, fixture.pool, wantReceipts, wantReceipts, 0)
	replayed := runCutoverCLIState(t, ctx, cli, "replay", "--since", since.Format(time.RFC3339))
	assertCLIRecoveryIDs(t, replayed.RecoveryDeliveryIDs, deliveryIDs...)
	assertCutoverReceiptCounts(t, fixture.pool, wantReceipts, wantReceipts, wantReceipts)
	verified := runCutoverCLIState(t, ctx, cli, "verify-hooks", "--since", since.Format(time.RFC3339))
	if verified.Phase != "ready" || !verified.MaintenanceMode {
		t.Fatalf("CLI verify-hooks state was unexpected: phase=%q maintenance=%v", verified.Phase, verified.MaintenanceMode)
	}
	assertCLIRecoveryIDs(t, verified.RecoveryDeliveryIDs, deliveryIDs...)
	statusAfterReplay := runCutoverCLIState(t, ctx, cli, "status")
	assertCLIRecoveryIDs(t, statusAfterReplay.RecoveryDeliveryIDs, deliveryIDs...)

	complete := runCutoverCLIState(t, ctx, cli, "finalize")
	if complete.Phase != "complete" || complete.MaintenanceMode {
		t.Fatalf("CLI finalize state was unexpected: phase=%q maintenance=%v", complete.Phase, complete.MaintenanceMode)
	}
	assertCLIRecoveryIDs(t, complete.RecoveryDeliveryIDs)
	assertCutoverReceiptCounts(t, fixture.pool, wantReceipts, 0, wantReceipts)
	finalStatus := runCutoverCLIState(t, ctx, cli, "status")
	if finalStatus.Phase != "complete" || finalStatus.MaintenanceMode {
		t.Fatalf("final CLI status was unexpected: phase=%q maintenance=%v", finalStatus.Phase, finalStatus.MaintenanceMode)
	}
	assertCLIRecoveryIDs(t, finalStatus.RecoveryDeliveryIDs)

	// The real legacy pipeline table requires an MR FK and its project/
	// connection ancestry; add only that required chain plus one pipeline row.
	pipelineFixture := newCutoverAcceptanceFixture(t)
	pipelineSource, _ := seedCutoverCLIInstance(t, pipelineFixture, "cli-pipeline-only", 713)
	if _, err := pipelineFixture.pool.Exec(ctx, `UPDATE vcs_gitlab_migration_state SET phase='complete',maintenance_mode=false WHERE id=1`); err != nil {
		t.Fatalf("prepare completed pipeline-only CLI state: %v", err)
	}
	if _, err := pipelineFixture.pool.Exec(ctx, `
		INSERT INTO gitlab_mr_pipeline (
			merge_request_id,pipeline_id,sha,ref,status,web_url,pipeline_updated_at
		) VALUES ($1,$2,'pipeline-only-sha','main','success','https://gitlab.example.test/pipelines/901',$3)
	`, pipelineSource.legacyMRID, acceptancePipelineID, pipelineFixture.at); err != nil {
		t.Fatalf("insert pipeline-only CLI regression row: %v", err)
	}
	pipelineCLI := cutoverCLI{
		binary: binary, moduleRoot: moduleRoot,
		databaseURL: cutoverCLIIsolatedDatabaseDSN(t, pipelineFixture.pool),
		publicURL:   pipelineFixture.appHTTP.URL,
		baseURL:     pipelineFixture.remoteHTTP.URL + "/pipeline-only",
		token:       "unused-test-token", webhookSecret: "unused-test-secret",
		secretKey: pipelineFixture.env.VCSSecretKey,
	}
	pipelineStatus := runCutoverCLIState(t, ctx, pipelineCLI, "status")
	if pipelineStatus.Phase != "complete" || pipelineStatus.MaintenanceMode {
		t.Fatalf("pipeline-only CLI status was unexpected: phase=%q maintenance=%v", pipelineStatus.Phase, pipelineStatus.MaintenanceMode)
	}
	pipelineReportBytes := runCutoverCLIExpectedExit(t, ctx, pipelineCLI, 1, "preflight")
	var pipelineReport Report
	if err := json.Unmarshal(pipelineReportBytes, &pipelineReport); err != nil {
		t.Fatalf("decode pipeline-only CLI preflight JSON: %v", err)
	}
	if pipelineReport.NoOp || pipelineReport.LegacyConnections != 1 || pipelineReport.LegacyProjects != 1 ||
		pipelineReport.LegacyMergeRequests != 1 || pipelineReport.LegacyPipelines != 1 ||
		len(pipelineReport.Findings) != 1 || pipelineReport.Findings[0].Code != "legacy_data_after_complete" {
		t.Fatalf("pipeline-only CLI preflight was not fail-closed: no_op=%v connections=%d projects=%d MRs=%d pipelines=%d findings=%d",
			pipelineReport.NoOp, pipelineReport.LegacyConnections, pipelineReport.LegacyProjects,
			pipelineReport.LegacyMergeRequests, pipelineReport.LegacyPipelines, len(pipelineReport.Findings))
	}

	// Exercise the CLI rollback command in its own database after a real remote
	// Hook update failure, retaining the authenticated 503 receipt for recovery.
	restoreFixture := newCutoverAcceptanceFixture(t)
	restoreSource, legacyHook := seedCutoverCLIInstance(t, restoreFixture, "cli-restore-instance", 711)
	failedProjectID, failedHookID := int64(78), int64(712)
	failedProjectPath := "group/sub/restore-failure"
	restoreFixture.addInstance(restoreSource.instance, restoreSource.token, "cli-gitlab-user", map[int64]string{
		restoreSource.projectID: restoreSource.path,
		failedProjectID:         failedProjectPath,
	})
	failedHook := cutoverAcceptanceHook{
		ID: failedHookID, URL: cutoverAcceptanceLegacyHookURL(restoreSource.legacyConnectionID),
		Token: restoreSource.legacySecret, MergeRequestsEvents: true, PipelineEvents: true,
		JobEvents: true, NoteEvents: true, EnableSSLVerification: true,
	}
	restoreFixture.addHook(restoreSource.instance, failedProjectID, failedHook)
	if _, err := restoreFixture.pool.Exec(ctx, `
		INSERT INTO gitlab_project_binding (
			id,workspace_id,connection_id,gitlab_project_id,path_with_namespace,web_url,hook_id,hook_enabled,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,true,$8,$8)
	`, uuid.NewString(), restoreSource.workspaceID, restoreSource.legacyConnectionID, failedProjectID,
		failedProjectPath, "https://gitlab.example.test/"+failedProjectPath, failedHookID, restoreFixture.at); err != nil {
		t.Fatalf("insert second restore-failure project binding: %v", err)
	}
	restoreCLI := newCutoverCLI(t, restoreFixture, restoreSource, binary, moduleRoot)
	if state := runCutoverCLIState(t, ctx, restoreCLI, "begin"); state.Phase != "maintenance" || !state.MaintenanceMode {
		t.Fatalf("restore fixture begin state was unexpected: phase=%q maintenance=%v", state.Phase, state.MaintenanceMode)
	}
	if state := runCutoverCLIState(t, ctx, restoreCLI, "migrate"); state.Phase != "database_migrated" || !state.MaintenanceMode {
		t.Fatalf("restore fixture migrate state was unexpected: phase=%q maintenance=%v", state.Phase, state.MaintenanceMode)
	}
	restoreFixture.failNextHookUpdate(restoreSource.instance, failedProjectID, failedHookID)
	runCutoverCLIExpectedExit(t, ctx, restoreCLI, 1, "switch-hooks")
	failedStatus := runCutoverCLIState(t, ctx, restoreCLI, "status")
	if failedStatus.Phase != "hooks_pending" && failedStatus.Phase != "verification_pending" {
		t.Fatalf("failed CLI switch-hooks phase was unexpected: %q", failedStatus.Phase)
	}
	pendingID := "cli-restore-pending-delivery"
	activeHook, ok := restoreFixture.remoteHook(restoreSource.instance, restoreSource.projectID, restoreSource.hookID)
	if !ok || activeHook.Token != restoreSource.unifiedSecret {
		t.Fatalf("first GitLab Hook did not switch before the later failure: found=%v rotated=%v", ok, activeHook.Token == restoreSource.unifiedSecret)
	}
	if status := cutoverAcceptancePost(t, activeHook.URL, restoreSource.unifiedSecret, pendingID, "Merge Request Hook",
		cutoverAcceptanceEvent("Merge Request Hook", restoreSource.projectID, restoreSource.path)); status != 503 {
		t.Fatalf("restore fixture maintenance webhook response = %d, want 503", status)
	}
	assertCutoverReceiptCounts(t, restoreFixture.pool, 1, 1, 0)
	beforeRestore := runCutoverCLIState(t, ctx, restoreCLI, "status")
	assertCLIRecoveryIDs(t, beforeRestore.RecoveryDeliveryIDs, pendingID)

	restored := runCutoverCLIState(t, ctx, restoreCLI, "restore")
	if restored.Phase != "restored" || !restored.MaintenanceMode {
		t.Fatalf("CLI restore state was unexpected: phase=%q maintenance=%v", restored.Phase, restored.MaintenanceMode)
	}
	assertCLIRecoveryIDs(t, restored.RecoveryDeliveryIDs, pendingID)
	statusAfterRestore := runCutoverCLIState(t, ctx, restoreCLI, "status")
	assertCLIRecoveryIDs(t, statusAfterRestore.RecoveryDeliveryIDs, pendingID)
	assertCutoverReceiptCounts(t, restoreFixture.pool, 1, 1, 0)
	for _, want := range []struct {
		projectID int64
		hookID    int64
		hook      cutoverAcceptanceHook
	}{
		{restoreSource.projectID, restoreSource.hookID, legacyHook},
		{failedProjectID, failedHookID, failedHook},
	} {
		got, found := restoreFixture.remoteHook(restoreSource.instance, want.projectID, want.hookID)
		if !found || got != want.hook {
			t.Fatalf("CLI restore did not restore the original remote Hook %d: found=%v", want.hookID, found)
		}
	}
}

type cutoverCLI struct {
	binary, moduleRoot, databaseURL                     string
	publicURL, baseURL, token, webhookSecret, secretKey string
}

func newCutoverCLI(t *testing.T, fixture *cutoverAcceptanceFixture, source *cutoverAcceptanceSource, binary, moduleRoot string) cutoverCLI {
	t.Helper()
	return cutoverCLI{
		binary:        binary,
		moduleRoot:    moduleRoot,
		databaseURL:   cutoverCLIIsolatedDatabaseDSN(t, fixture.pool),
		publicURL:     fixture.appHTTP.URL,
		baseURL:       source.baseURL,
		token:         source.token,
		webhookSecret: source.legacySecret,
		secretKey:     fixture.env.VCSSecretKey,
	}
}

func (cli cutoverCLI) run(ctx context.Context, command string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	commandArgs := append([]string{command}, args...)
	cmd := exec.CommandContext(commandCtx, cli.binary, commandArgs...)
	cmd.Dir = cli.moduleRoot
	cmd.Env = cli.environment()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("gitlab-vcs-migrate %s: %w; stderr=%q; stdout=%q",
			command, err, cli.redactDiagnostics(stderr.String()), cli.redactDiagnostics(stdout.String()))
	}
	return stdout.Bytes(), nil
}
func (cli cutoverCLI) redactDiagnostics(output string) string {
	for _, credential := range []string{cli.databaseURL, cli.token, cli.webhookSecret, cli.secretKey} {
		if credential != "" {
			output = strings.ReplaceAll(output, credential, "[REDACTED]")
		}
	}
	output = strings.TrimSpace(output)
	if len(output) > 4096 {
		output = output[:4096] + "…"
	}
	return output
}

func (cli cutoverCLI) environment() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + os.TempDir(),
		"TZ=UTC",
		"DATABASE_URL=" + cli.databaseURL,
		"GITLAB_BASE_URL=" + cli.baseURL,
		"GITLAB_TOKEN=" + cli.token,
		"GITLAB_WEBHOOK_SECRET=" + cli.webhookSecret,
		"GITLAB_PROXY_URL=",
		"MULTICA_VCS_INTEGRATION_ENABLED=true",
		"MULTICA_VCS_SECRET_KEY=" + cli.secretKey,
		"MULTICA_PUBLIC_URL=" + cli.publicURL,
		"HTTP_PROXY=",
		"HTTPS_PROXY=",
		"ALL_PROXY=",
		"NO_PROXY=*",
	}
}

func runCutoverCLI(t *testing.T, ctx context.Context, cli cutoverCLI, command string, args ...string) []byte {
	t.Helper()
	output, err := cli.run(ctx, command, args...)
	if err != nil {
		t.Fatalf("GitLab migration CLI command %q failed: %v", command, err)
	}
	return output
}
func runCutoverCLIExpectedExit(t *testing.T, ctx context.Context, cli cutoverCLI, exitCode int, command string, args ...string) []byte {
	t.Helper()
	output, err := cli.run(ctx, command, args...)
	var processErr *exec.ExitError
	if !errors.As(err, &processErr) || processErr.ExitCode() != exitCode {
		t.Fatalf("GitLab migration CLI command %q did not exit with status %d (process exit error present=%v)", command, exitCode, processErr != nil)
	}
	return output
}

func runCutoverCLIState(t *testing.T, ctx context.Context, cli cutoverCLI, command string, args ...string) MigrationState {
	t.Helper()
	var state MigrationState
	if err := json.Unmarshal(runCutoverCLI(t, ctx, cli, command, args...), &state); err != nil {
		t.Fatalf("decode CLI %s state JSON: %v", command, err)
	}
	return state
}

func assertCLIRecoveryIDs(t *testing.T, actual []string, want ...string) {
	t.Helper()
	actual = append([]string(nil), actual...)
	want = append([]string(nil), want...)
	sort.Strings(actual)
	sort.Strings(want)
	if len(actual) != len(want) {
		t.Fatalf("CLI recovery delivery ID count = %d, want %d", len(actual), len(want))
	}
	for i := range want {
		if actual[i] != want[i] {
			t.Fatalf("CLI recovery delivery IDs differ at index %d", i)
		}
	}
}

func seedCutoverCLIInstance(t *testing.T, fixture *cutoverAcceptanceFixture, instance string, hookID int64) (*cutoverAcceptanceSource, cutoverAcceptanceHook) {
	t.Helper()
	seed := testutil.New(fixture.pool, "", "")
	workspace := seed.Workspace(t, "CLI GitLab workspace", "gitlab-cli-"+uuid.NewString())
	user := seed.User(t, "CLI GitLab user", "gitlab-cli-"+uuid.NewString()+"@example.test")
	source := &cutoverAcceptanceSource{
		workspaceID:        workspace,
		userID:             user,
		instance:           instance,
		legacyConnectionID: uuid.NewString(),
		legacyBindingID:    uuid.NewString(),
		legacyMRID:         uuid.NewString(),
		projectID:          acceptanceProjectID,
		hookID:             hookID,
		token:              "cli-api-token-" + instance,
		legacySecret:       "cli-hook-secret-" + instance,
		unifiedSecret:      "cli-hook-secret-" + instance,
	}
	source.baseURL = fixture.remoteHTTP.URL + "/" + instance
	source.path = acceptanceProjectPath
	fixture.addInstance(instance, source.token, "cli-gitlab-user", map[int64]string{source.projectID: source.path})
	hook := cutoverAcceptanceHook{
		ID:                    hookID,
		URL:                   cutoverAcceptanceLegacyHookURL(source.legacyConnectionID),
		Token:                 source.legacySecret,
		MergeRequestsEvents:   true,
		PipelineEvents:        true,
		JobEvents:             true,
		NoteEvents:            true,
		EnableSSLVerification: true,
	}
	fixture.addHook(instance, source.projectID, hook)
	seedCutoverLegacySource(t, fixture, source)
	return source, hook
}

func buildCutoverCLIBinary(t *testing.T, moduleRoot string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "gitlab-vcs-migrate")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/gitlab-vcs-migrate")
	cmd.Dir = moduleRoot
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("build isolated GitLab migration CLI: %v", err)
	}
	return binary
}

func cutoverCLIModuleRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for directory := cwd; ; directory = filepath.Dir(directory) {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	t.Fatalf("could not locate server module from %q", cwd)
	return ""
}

func cutoverCLIIsolatedDatabaseDSN(t *testing.T, fixturePool *pgxpool.Pool) string {
	t.Helper()
	var database string
	if err := fixturePool.QueryRow(context.Background(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal("could not read the isolated database name")
	}
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("could not parse the test database URL")
		}
		parsed.Path, parsed.RawPath = "/"+database, ""
		query := parsed.Query()
		query.Del("database")
		query.Del("dbname")
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	return dsn + " dbname=" + database
}
