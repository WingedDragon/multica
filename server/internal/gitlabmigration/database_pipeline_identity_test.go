package gitlabmigration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestDatabasePipelineMigrationUsesCompleteSameInstanceProjectURLs(t *testing.T) {
	f := newDatabaseMigrationFixture(t)
	f.seedLegacyProjectAndMR(t)
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE gitlab_merge_request
		SET source_project_id=88, source_project_path='group/fork/project'
		WHERE workspace_id=$1 AND id=$2
	`, f.workspace, f.mergeReq); err != nil {
		t.Fatalf("set fork source identity: %v", err)
	}
	for _, pipeline := range []struct {
		id   int64
		path string
	}{
		{1903, "group/sub/project"},
		{1905, "group/fork/project"},
	} {
		webURL := f.server.URL + "/" + pipeline.path + "/-/pipelines/" + strconv.FormatInt(pipeline.id, 10)
		if _, err := f.pool.Exec(context.Background(), `
			INSERT INTO gitlab_mr_pipeline (
				merge_request_id,pipeline_id,pipeline_project_id,sha,ref,status,web_url,pipeline_updated_at
			) VALUES ($1,$2,0,'legacy-ci-head','agent/agent/347021b5','failed',$3,$4)
		`, f.mergeReq, pipeline.id, webURL, f.at); err != nil {
			t.Fatalf("insert legacy pipeline %d: %v", pipeline.id, err)
		}
	}
	if err := f.runner.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate pipelines with complete same-instance project URLs: %v", err)
	}
	for _, want := range []struct {
		id        int64
		projectID int64
		path      string
	}{
		{1903, 77, "group/sub/project"},
		{1905, 88, "group/fork/project"},
	} {
		var projectID int64
		var projectPath, ref string
		if err := f.pool.QueryRow(context.Background(), `
			SELECT pipeline_project_id,pipeline_project_path,ref
			FROM vcs_gitlab_pipeline WHERE pipeline_id=$1
		`, want.id).Scan(&projectID, &projectPath, &ref); err != nil {
			t.Fatalf("read migrated pipeline %d: %v", want.id, err)
		}
		if projectID != want.projectID || projectPath != want.path || ref != "agent/agent/347021b5" {
			t.Fatalf("pipeline %d identity=(%d,%q), ref=%q; want (%d,%q), ref preserved", want.id, projectID, projectPath, ref, want.projectID, want.path)
		}
	}
}

func TestDatabasePipelineMigrationUsesIDProjectAndSHAWithoutComparingRef(t *testing.T) {
	f := newDatabaseMigrationFixture(t)
	f.seedLegacyProjectAndMR(t)
	baseURL := setPipelineMigrationAPI(t, f, 1904, http.StatusOK,
		`{"id":1904,"project_id":77,"sha":"api-fallback-head","ref":"refs/merge-requests/81/head"}`)
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_pipeline (
			merge_request_id,pipeline_id,pipeline_project_id,sha,ref,status,web_url,pipeline_updated_at
		) VALUES ($1,1904,0,'api-fallback-head','agent/agent/347021b5','failed',NULL,$2)
	`, f.mergeReq, f.at); err != nil {
		t.Fatalf("insert legacy pipeline without project ID or URL: %v", err)
	}
	if err := f.runner.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate pipeline using API project identity despite normalized ref: %v", err)
	}
	var projectID int64
	var projectPath, sha, ref string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT pipeline_project_id,pipeline_project_path,sha,ref
		FROM vcs_gitlab_pipeline WHERE pipeline_id=1904
	`).Scan(&projectID, &projectPath, &sha, &ref); err != nil {
		t.Fatalf("read API-verified pipeline: %v", err)
	}
	if projectID != 77 || projectPath != "group/sub/project" || sha != "api-fallback-head" || ref != "agent/agent/347021b5" {
		t.Fatalf("migrated pipeline=(%d,%q,%q,%q), want API project and unchanged legacy SHA/ref; API=%s", projectID, projectPath, sha, ref, baseURL)
	}
}

func TestDatabasePipelineMigrationFailsClosedWhenURLlessPipelineIsForbidden(t *testing.T) {
	f := newDatabaseMigrationFixture(t)
	f.seedLegacyProjectAndMR(t)
	setPipelineMigrationAPI(t, f, 1906, http.StatusForbidden, `{"message":"forbidden"}`)
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO gitlab_mr_pipeline (
			merge_request_id,pipeline_id,pipeline_project_id,sha,ref,status,web_url,pipeline_updated_at
		) VALUES ($1,1906,0,'unverified-head','feature/not-authoritative','failed',NULL,$2)
	`, f.mergeReq, f.at); err != nil {
		t.Fatalf("insert URL-less pipeline: %v", err)
	}
	if err := f.runner.Migrate(context.Background()); err == nil {
		t.Fatal("migration accepted a URL-less pipeline whose API identity is forbidden")
	}
	for _, query := range []string{
		`SELECT count(*) FROM vcs_connection WHERE workspace_id=$1`,
		`SELECT count(*) FROM vcs_gitlab_project WHERE workspace_id=$1`,
		`SELECT count(*) FROM vcs_pull_request WHERE workspace_id=$1`,
		`SELECT count(*) FROM vcs_gitlab_pipeline WHERE workspace_id=$1`,
	} {
		var count int
		if err := f.pool.QueryRow(context.Background(), query, f.workspace).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("forbidden pipeline left partial migration state: %s returned %d", query, count)
		}
	}
}

func setPipelineMigrationAPI(t *testing.T, f *databaseMigrationFixture, pipelineID int64, pipelineStatus int, pipelineBody string) string {
	t.Helper()
	pipelinePath := "/api/v4/projects/77/pipelines/" + strconv.FormatInt(pipelineID, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/user":
			_, _ = w.Write([]byte(`{"username":"migration-user"}`))
		case "/api/v4/projects/77":
			_, _ = w.Write([]byte(`{"id":77,"path_with_namespace":"group/sub/project"}`))
		case "/api/v4/groups":
			_, _ = w.Write([]byte(`[{"id":7,"full_path":"group"},{"id":8,"full_path":"group/sub"}]`))
		case "/api/v4/projects/77/hooks", "/api/v4/groups/7/hooks", "/api/v4/groups/8/hooks", "/api/v4/hooks":
			_, _ = w.Write([]byte(`[]`))
		case pipelinePath:
			w.WriteHeader(pipelineStatus)
			if pipelineBody != "" {
				_, _ = w.Write([]byte(pipelineBody))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.env.GitLabBaseURL = server.URL
	f.runner = NewRunner(f.pool, f.env)
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE gitlab_connection SET base_url=$2 WHERE workspace_id=$1 AND id=$3
	`, f.workspace, server.URL, f.connection); err != nil {
		t.Fatalf("point legacy connection at test GitLab API: %v", err)
	}
	return server.URL
}
