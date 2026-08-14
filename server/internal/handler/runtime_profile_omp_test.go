package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeProfileOMPMigrationSQL(t *testing.T) {
	up := readRuntimeProfileOMPMigration(t, "314_runtime_profile_add_omp.up.sql")
	down := readRuntimeProfileOMPMigration(t, "314_runtime_profile_add_omp.down.sql")

	for _, statement := range []string{
		"ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;",
		"ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check",
		"NOT VALID;",
	} {
		if !strings.Contains(up, statement) {
			t.Errorf("up migration missing %q", statement)
		}
		if !strings.Contains(down, statement) {
			t.Errorf("down migration missing %q", statement)
		}
	}

	for _, family := range []string{
		"claude", "codebuddy", "codex", "copilot", "opencode", "openclaw",
		"hermes", "pi", "omp", "cursor", "kimi", "kiro", "antigravity",
		"qoder", "traecli", "deveco", "grok", "qwen",
	} {
		if !strings.Contains(up, "'"+family+"'") {
			t.Errorf("up migration does not accept %q", family)
		}
	}
	if strings.Contains(down, "'omp'") {
		t.Error("down migration must not accept omp")
	}
	for _, family := range []string{
		"claude", "codebuddy", "codex", "copilot", "opencode", "openclaw",
		"hermes", "pi", "cursor", "kimi", "kiro", "antigravity", "qoder",
		"traecli", "deveco", "grok", "qwen",
	} {
		if !strings.Contains(down, "'"+family+"'") {
			t.Errorf("down migration does not restore %q", family)
		}
	}
}


func TestRuntimeProfile_OMPPersistsThroughCreateReadAndUpdate(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	create := httptest.NewRecorder()
	createReq := newRequest("POST", "/api/workspaces/"+testWorkspaceID+"/runtime-profiles", map[string]any{
		"display_name":    "OMP Profile Persistence",
		"protocol_family": "omp",
		"command_name":    "omp",
	})
	createReq = withURLParam(createReq, "id", testWorkspaceID)
	testHandler.CreateRuntimeProfile(create, createReq)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d: %s", create.Code, http.StatusCreated, create.Body.String())
	}

	var created RuntimeProfileResponse
	if err := json.NewDecoder(create.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM runtime_profile WHERE id = $1`, created.ID)
	})
	if created.ProtocolFamily != "omp" {
		t.Fatalf("created protocol_family = %q, want omp", created.ProtocolFamily)
	}

	get := httptest.NewRecorder()
	getReq := newRequest("GET", "/api/workspaces/"+testWorkspaceID+"/runtime-profiles/"+created.ID, nil)
	getReq = withURLParams(getReq, "id", testWorkspaceID, "profileId", created.ID)
	testHandler.GetRuntimeProfile(get, getReq)
	if get.Code != http.StatusOK {
		t.Fatalf("read status = %d, want %d: %s", get.Code, http.StatusOK, get.Body.String())
	}
	var read RuntimeProfileResponse
	if err := json.NewDecoder(get.Body).Decode(&read); err != nil {
		t.Fatalf("decode read response: %v", err)
	}
	if read.ProtocolFamily != "omp" {
		t.Fatalf("read protocol_family = %q, want omp", read.ProtocolFamily)
	}

	update := httptest.NewRecorder()
	updateReq := newRequest("PATCH", "/api/workspaces/"+testWorkspaceID+"/runtime-profiles/"+created.ID, map[string]any{
		"display_name": "OMP Profile Persistence Updated",
	})
	updateReq = withURLParams(updateReq, "id", testWorkspaceID, "profileId", created.ID)
	testHandler.UpdateRuntimeProfile(update, updateReq)
	if update.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d: %s", update.Code, http.StatusOK, update.Body.String())
	}
	var updated RuntimeProfileResponse
	if err := json.NewDecoder(update.Body).Decode(&updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updated.ProtocolFamily != "omp" {
		t.Fatalf("updated protocol_family = %q, want omp", updated.ProtocolFamily)
	}
}

func readRuntimeProfileOMPMigration(t *testing.T, filename string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("..", "..", "migrations", filename))
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	return string(contents)
}
