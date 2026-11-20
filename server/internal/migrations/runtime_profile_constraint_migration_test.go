package migrations

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestRuntimeProfileSupportedTypesConstraintMigration(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire Postgres connection: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE runtime_profile (protocol_family TEXT NOT NULL)`); err != nil {
		t.Fatalf("create temporary runtime_profile table: %v", err)
	}
	applyMigrationFile(t, ctx, conn.Conn(), "656_runtime_profile_add_amp.up.sql")
	applyMigrationFile(t, ctx, conn.Conn(), "710_runtime_profile_restore_supported_types.up.sql")

	for _, family := range agent.SupportedTypes {
		if _, err := conn.Exec(ctx, `INSERT INTO runtime_profile (protocol_family) VALUES ($1)`, family); err != nil {
			t.Errorf("insert supported runtime %q: %v", family, err)
		}
	}
	assertInsertCheckViolation(t, ctx, conn.Conn(),
		`INSERT INTO runtime_profile (protocol_family) VALUES ($1)`, "not-a-supported-runtime")

	// Rolling back 710 restores the 656 whitelist: Amp remains allowed, while
	// OMP and Museapp are rejected for new writes. NOT VALID preserves the
	// historical rows inserted above.
	applyMigrationFile(t, ctx, conn.Conn(), "710_runtime_profile_restore_supported_types.down.sql")
	if _, err := conn.Exec(ctx, `INSERT INTO runtime_profile (protocol_family) VALUES ('amp')`); err != nil {
		t.Fatalf("insert Amp after rollback to migration 656: %v", err)
	}
	for _, family := range []string{"omp", "museapp"} {
		assertInsertCheckViolation(t, ctx, conn.Conn(),
			`INSERT INTO runtime_profile (protocol_family) VALUES ($1)`, family)
	}
}
