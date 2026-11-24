package main

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestAutopilotIssueListenerPausesAndResumesWithVCSGate(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	var oldPhase string
	var oldMaintenance bool
	if err := testPool.QueryRow(ctx, `
		SELECT phase, maintenance_mode
		FROM vcs_gitlab_migration_state
		WHERE id = 1
	`).Scan(&oldPhase, &oldMaintenance); err != nil {
		t.Fatalf("read VCS migration state: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `
			UPDATE vcs_gitlab_migration_state
			SET phase = $1, maintenance_mode = $2, updated_at = now()
			WHERE id = 1
		`, oldPhase, oldMaintenance); err != nil {
			t.Errorf("restore VCS migration state: %v", err)
		}
	})
	setState := func(phase string, maintenance bool) {
		t.Helper()
		if _, err := testPool.Exec(ctx, `
			UPDATE vcs_gitlab_migration_state
			SET phase = $1, maintenance_mode = $2, updated_at = now()
			WHERE id = 1
		`, phase, maintenance); err != nil {
			t.Fatalf("set VCS migration state: %v", err)
		}
	}

	seen := watchAutopilotIssueSync(t)
	bus := events.New()
	registerAutopilotListeners(bus, nil, db.New(testPool))
	setState("verification_pending", true)
	publishIssueUpdated(bus, map[string]any{
		"id": "maintenance-issue", "status": "done",
	}, true)
	if len(*seen) != 0 {
		t.Fatalf("autopilot sync ran during maintenance: %v", *seen)
	}

	setState("complete", false)
	publishIssueUpdated(bus, map[string]any{
		"id": "resumed-issue", "status": "done",
	}, true)
	if len(*seen) != 1 || (*seen)[0] != "resumed-issue" {
		t.Fatalf("autopilot sync after completion = %v, want [resumed-issue]", *seen)
	}
}
