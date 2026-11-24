package service

import (
	"context"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func vcsMigrationAllowsBusinessWrites(ctx context.Context, q *db.Queries) (bool, error) {
	state, err := q.GetVCSGitLabMigrationState(ctx)
	if err != nil {
		return false, err
	}
	return state.Phase == "complete" && !state.MaintenanceMode, nil
}
