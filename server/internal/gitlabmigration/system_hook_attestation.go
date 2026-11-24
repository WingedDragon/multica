package gitlabmigration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	systemHookAttestationReason = "no_legacy_multica_system_hook"
	systemHookAttestationTable  = "vcs_gitlab_migration_system_hook_attestation"
)

type SystemHookInventoryAttestation struct {
	Generation  int64     `json:"generation"`
	InstanceURL string    `json:"instance_url"`
	Operator    string    `json:"operator"`
	AttestedAt  time.Time `json:"attested_at,omitempty"`
	Source      string    `json:"source,omitempty"`
}

type systemHookInventoryAttestationInput struct {
	instanceURL string
	operator    string
}

// CanonicalSystemHookInventoryAttestationOrigin accepts only a complete HTTPS
// origin. The exact host and port are retained so an attestation cannot cover a
// different GitLab instance.
func CanonicalSystemHookInventoryAttestationOrigin(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil ||
		origin == "" ||
		strings.TrimSpace(origin) != origin ||
		u == nil ||
		u.Scheme != "https" ||
		u.Host == "" ||
		u.Hostname() == "" ||
		strings.HasSuffix(u.Host, ":") ||
		u.User != nil ||
		u.Opaque != "" ||
		u.Path != "" ||
		u.RawPath != "" ||
		u.RawQuery != "" ||
		u.ForceQuery ||
		u.Fragment != "" ||
		u.RawFragment != "" ||
		u.String() != origin {
		return "", errors.New("System Hook inventory attestation must be a complete HTTPS origin without a path, user info, query, or fragment")
	}
	return normalizeInstanceURL(origin), nil
}

func (r *Runner) SetSystemHookInventoryAttestation(instanceOrigin, operator string) error {
	origin, err := CanonicalSystemHookInventoryAttestationOrigin(instanceOrigin)
	if err != nil {
		return err
	}
	operator = strings.TrimSpace(operator)
	if operator == "" {
		operator = "unknown"
	}
	r.systemHookAttestation = &systemHookInventoryAttestationInput{instanceURL: origin, operator: operator}
	return nil
}

func systemHookAttestationMatchesInstance(attestation *SystemHookInventoryAttestation, instanceURL string) bool {
	if attestation == nil {
		return false
	}
	origin, err := CanonicalSystemHookInventoryAttestationOrigin(attestation.InstanceURL)
	return err == nil && origin == attestation.InstanceURL && normalizeInstanceURL(instanceURL) == origin
}

func systemHookAttestationGeneration(state MigrationState) int64 {
	if state.Phase == "restored" || state.Generation <= 0 {
		return state.Generation + 1
	}
	return state.Generation
}

func (r *Runner) preflightSystemHookAttestation(ctx context.Context, state MigrationState, instanceURL string) (*SystemHookInventoryAttestation, error) {
	generation := systemHookAttestationGeneration(state)
	if r.systemHookAttestation != nil && normalizeInstanceURL(instanceURL) == r.systemHookAttestation.instanceURL {
		return &SystemHookInventoryAttestation{
			Generation:  generation,
			InstanceURL: r.systemHookAttestation.instanceURL,
			Operator:    r.systemHookAttestation.operator,
			Source:      "operator_input",
		}, nil
	}
	return r.systemHookAttestationForGeneration(ctx, generation, instanceURL)
}

func (r *Runner) activeSystemHookAttestation(ctx context.Context, instanceURL string) (*SystemHookInventoryAttestation, error) {
	var state MigrationState
	if err := r.pool.QueryRow(ctx, `
		SELECT phase, maintenance_mode, generation FROM vcs_gitlab_migration_state WHERE id=1
	`).Scan(&state.Phase, &state.MaintenanceMode, &state.Generation); err != nil {
		return nil, fmt.Errorf("read migration generation for System Hook inventory attestation: %w", err)
	}
	if !state.MaintenanceMode ||
		(state.Phase != "database_migrated" && state.Phase != "hooks_pending" && state.Phase != "verification_pending") {
		return nil, nil
	}
	return r.systemHookAttestationForGeneration(ctx, state.Generation, instanceURL)
}

func (r *Runner) systemHookAttestationForGeneration(ctx context.Context, generation int64, instanceURL string) (*SystemHookInventoryAttestation, error) {
	if generation <= 0 {
		return nil, nil
	}
	origin := normalizeInstanceURL(instanceURL)
	if origin == "" {
		return nil, nil
	}
	var tableExists bool
	if err := r.pool.QueryRow(ctx, `
		SELECT to_regclass(format('%I.%I', current_schema(), $1::text)) IS NOT NULL
	`, systemHookAttestationTable).Scan(&tableExists); err != nil {
		return nil, fmt.Errorf("inspect System Hook attestation audit table: %w", err)
	}
	if !tableExists {
		return nil, nil
	}
	var attestation SystemHookInventoryAttestation
	if err := r.pool.QueryRow(ctx, `
		SELECT generation, instance_url, operator_identity, attested_at
		FROM vcs_gitlab_migration_system_hook_attestation
		WHERE generation=$1 AND instance_url=$2
	`, generation, origin).Scan(
		&attestation.Generation, &attestation.InstanceURL, &attestation.Operator, &attestation.AttestedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read System Hook inventory attestation: %w", err)
	}
	attestation.Source = "persisted"
	return &attestation, nil
}

func (r *Runner) loadSystemHookInventoryAttestations(ctx context.Context) ([]SystemHookInventoryAttestation, error) {
	var tableExists bool
	if err := r.pool.QueryRow(ctx, `
		SELECT to_regclass(format('%I.%I', current_schema(), $1::text)) IS NOT NULL
	`, systemHookAttestationTable).Scan(&tableExists); err != nil {
		return nil, fmt.Errorf("inspect System Hook attestation audit table: %w", err)
	}
	if !tableExists {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT generation, instance_url, operator_identity, attested_at
		FROM vcs_gitlab_migration_system_hook_attestation
		ORDER BY generation, instance_url
	`)
	if err != nil {
		return nil, fmt.Errorf("list System Hook inventory attestations: %w", err)
	}
	defer rows.Close()
	var attestations []SystemHookInventoryAttestation
	for rows.Next() {
		var attestation SystemHookInventoryAttestation
		if err := rows.Scan(&attestation.Generation, &attestation.InstanceURL, &attestation.Operator, &attestation.AttestedAt); err != nil {
			return nil, fmt.Errorf("read System Hook inventory attestation: %w", err)
		}
		attestation.Source = "persisted"
		attestations = append(attestations, attestation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate System Hook inventory attestations: %w", err)
	}
	return attestations, nil
}

func (r *Runner) persistSystemHookAttestation(ctx context.Context, tx pgx.Tx, generation int64) error {
	if r.systemHookAttestation == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO vcs_gitlab_migration_system_hook_attestation
			(generation, instance_url, operator_identity, attestation)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (generation, instance_url) DO NOTHING
	`, generation, r.systemHookAttestation.instanceURL, r.systemHookAttestation.operator, systemHookAttestationReason); err != nil {
		return fmt.Errorf("record System Hook inventory operator attestation: %w", err)
	}
	return nil
}

func shouldSkipSystemHookInventory(instanceURL string, err error, attestation *SystemHookInventoryAttestation) bool {
	return systemHookAttestationMatchesInstance(attestation, instanceURL) &&
		gitLabHTTPStatus(err) == http.StatusForbidden
}

func addSystemHookInventoryAttestation(report *Report, attestation SystemHookInventoryAttestation) {
	for _, existing := range report.SystemHookInventoryAttestations {
		if existing.Generation == attestation.Generation && existing.InstanceURL == attestation.InstanceURL {
			return
		}
	}
	report.SystemHookInventoryAttestations = append(report.SystemHookInventoryAttestations, attestation)
}
