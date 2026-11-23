package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestGitLabConnectionIndexSupportsUpDownUp(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	schema := fmt.Sprintf("gitlab_connection_flow_%d", time.Now().UnixNano())
	schemaIdent := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schemaIdent); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaIdent+" CASCADE"); err != nil {
			t.Logf("drop schema: %v", err)
		}
	})

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET search_path TO "+schemaIdent); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE gitlab_connection (
		workspace_id UUID NOT NULL,
		base_url TEXT NOT NULL,
		host TEXT NOT NULL,
		CONSTRAINT gitlab_connection_workspace_id_host_key UNIQUE (workspace_id, host)
	)`); err != nil {
		t.Fatalf("create GitLab connection fixture: %v", err)
	}
	const workspaceID = "00000000-0000-0000-0000-000000000001"
	const host = "gitlab.example"
	if _, err := conn.Exec(ctx, `INSERT INTO gitlab_connection (workspace_id, base_url, host)
		VALUES ($1, 'https://gitlab.example/instance-a', $2)`, workspaceID, host); err != nil {
		t.Fatalf("seed existing GitLab connection: %v", err)
	}

	execMigration := func(name string) error {
		sql, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			return err
		}
		_, err = conn.Exec(ctx, string(sql))
		return err
	}
	apply := func(name string) {
		t.Helper()
		if err := execMigration(name); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	apply("742_gitlab_connection_base_url_unique_index.up.sql")
	apply("743_gitlab_connection_drop_host_unique.up.sql")
	apply("744_gitlab_connection_remove_rollback_host_index.up.sql")
	if _, err := conn.Exec(ctx, `INSERT INTO gitlab_connection (workspace_id, base_url, host)
		VALUES ($1, 'https://gitlab.example/instance-b', $2)`, workspaceID, host); err != nil {
		t.Fatalf("multi-instance up did not permit same-host URL: %v", err)
	}

	apply("744_gitlab_connection_remove_rollback_host_index.down.sql")
	if err := execMigration("743_gitlab_connection_drop_host_unique.down.sql"); err == nil {
		t.Fatal("rollback unexpectedly restored host uniqueness with duplicate hosts present")
	}
	if got := concurrentDownIndexCleanups["743_gitlab_connection_drop_host_unique"]; got != "gitlab_connection_workspace_host_uidx" {
		t.Fatalf("rollback invalid-index cleanup = %q", got)
	}
	if _, err := conn.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS gitlab_connection_workspace_host_uidx"); err != nil {
		t.Fatalf("remove invalid failed-build index: %v", err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM gitlab_connection
		WHERE workspace_id = $1 AND base_url = 'https://gitlab.example/instance-b'`, workspaceID); err != nil {
		t.Fatalf("remove duplicate host before rollback retry: %v", err)
	}
	apply("743_gitlab_connection_drop_host_unique.down.sql")
	if _, err := conn.Exec(ctx, `INSERT INTO gitlab_connection (workspace_id, base_url, host)
		VALUES ($1, 'https://gitlab.example/instance-c', $2)`, workspaceID, host); err == nil {
		t.Fatal("rollback did not restore the legacy same-host uniqueness")
	}
	apply("742_gitlab_connection_base_url_unique_index.down.sql")

	apply("742_gitlab_connection_base_url_unique_index.up.sql")
	apply("743_gitlab_connection_drop_host_unique.up.sql")
	apply("744_gitlab_connection_remove_rollback_host_index.up.sql")
	if _, err := conn.Exec(ctx, `INSERT INTO gitlab_connection (workspace_id, base_url, host)
		VALUES ($1, 'https://gitlab.example/instance-c', $2)`, workspaceID, host); err != nil {
		t.Fatalf("re-up left the rollback host index in place: %v", err)
	}
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM gitlab_connection WHERE workspace_id = $1 AND host = $2`, workspaceID, host).Scan(&count); err != nil {
		t.Fatalf("count same-host connections after re-up: %v", err)
	}
	if count != 2 {
		t.Fatalf("same-host connection count after re-up = %d, want 2", count)
	}
}
