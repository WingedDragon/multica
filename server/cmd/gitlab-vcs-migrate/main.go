package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/gitlabmigration"
	"github.com/multica-ai/multica/server/internal/handler"
)

type options struct {
	command                     string
	publicURL                   string
	since                       time.Time
	systemHookAttestationOrigin string
	configurationOnly           bool
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	parsed, err := parseOptions(args)
	if err != nil {
		return err
	}
	env := gitlabmigration.EnvironmentFromOS()
	if parsed.publicURL != "" {
		env.PublicURL = strings.TrimRight(strings.TrimSpace(parsed.publicURL), "/")
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect migration database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping migration database: %w", err)
	}
	runner := gitlabmigration.NewRunner(pool, env)
	if parsed.systemHookAttestationOrigin != "" {
		if err := runner.SetSystemHookInventoryAttestation(parsed.systemHookAttestationOrigin, migrationOperatorIdentity()); err != nil {
			return err
		}
	}
	switch parsed.command {
	case "preflight":
		report, preflightErr := runner.Preflight(ctx)
		if err := writeJSON(output, report); err != nil {
			return err
		}
		return preflightErr
	case "begin":
		err = runner.Begin(ctx)
	case "migrate":
		err = runner.Migrate(ctx)
	case "switch-hooks":
		err = runner.SwitchHooks(ctx)
	case "replay":
		box, boxErr := env.SecretBox()
		if boxErr != nil {
			return boxErr
		}
		replayHandler, handlerErr := handler.NewGitLabMigrationReplayHandler(pool, box, handler.Config{PublicURL: env.PublicURL})
		if handlerErr != nil {
			return fmt.Errorf("initialize authoritative GitLab replay: %w", handlerErr)
		}
		err = runner.Replay(ctx, parsed.since, replayHandler.ReplayGitLabMigrationDelivery)
	case "verify-hooks":
		if parsed.configurationOnly {
			err = runner.VerifyConfiguredHooks(ctx, parsed.since)
		} else {
			err = runner.VerifyHooks(ctx, parsed.since)
		}
	case "finalize":
		err = runner.Finalize(ctx)
	case "restore":
		err = runner.Restore(ctx)
	case "status":
		state, stateErr := runner.State(ctx)
		if stateErr != nil {
			return stateErr
		}
		return writeJSON(output, state)
	default:
		return fmt.Errorf("unsupported command %q", parsed.command)
	}
	if err != nil {
		return err
	}
	state, err := runner.State(ctx)
	if err != nil {
		return err
	}
	return writeJSON(output, state)
}

func parseOptions(args []string) (options, error) {
	if len(args) == 0 {
		return options{}, errors.New("usage: gitlab-vcs-migrate <preflight|begin|migrate|switch-hooks|replay|verify-hooks|finalize|restore|status> [--public-url URL] [--since RFC3339-UTC] [--configuration-only] [--attest-no-legacy-system-hooks-for=https://gitlab.example.com]")
	}
	command := args[0]
	switch command {
	case "preflight", "begin", "migrate", "switch-hooks", "replay", "verify-hooks", "finalize", "restore", "status":
	default:
		return options{}, fmt.Errorf("unknown GitLab migration command %q", command)
	}
	flags := flag.NewFlagSet("gitlab-vcs-migrate "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	publicURL := flags.String("public-url", "", "public API origin used for Hook URLs")
	sinceRaw := flags.String("since", "", "UTC RFC3339 lower bound for delivery replay/verification")
	attestationOrigin := flags.String("attest-no-legacy-system-hooks-for", "", "one-time operator confirmation for an exact HTTPS GitLab origin")
	configurationOnly := flags.Bool("configuration-only", false, "verify Hook configuration without requiring event deliveries")
	if err := flags.Parse(args[1:]); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	needsSince := command == "replay" || command == "verify-hooks"
	if needsSince && *sinceRaw == "" {
		return options{}, fmt.Errorf("%s requires --since RFC3339-UTC", command)
	}
	if !needsSince && *sinceRaw != "" {
		return options{}, fmt.Errorf("--since is only valid with replay and verify-hooks")
	}
	if *configurationOnly && command != "verify-hooks" {
		return options{}, errors.New("--configuration-only is only valid with verify-hooks")
	}
	if *attestationOrigin != "" {
		if command != "preflight" && command != "begin" {
			return options{}, errors.New("--attest-no-legacy-system-hooks-for is only valid with preflight and begin")
		}
		canonicalOrigin, err := gitlabmigration.CanonicalSystemHookInventoryAttestationOrigin(*attestationOrigin)
		if err != nil {
			return options{}, err
		}
		*attestationOrigin = canonicalOrigin
	}
	var since time.Time
	if needsSince {
		if !strings.HasSuffix(*sinceRaw, "Z") {
			return options{}, errors.New("--since must be a UTC RFC3339 timestamp ending in Z")
		}
		parsed, err := time.Parse(time.RFC3339, *sinceRaw)
		if err != nil || parsed.Location() != time.UTC {
			return options{}, errors.New("--since must be a valid UTC RFC3339 timestamp")
		}
		since = parsed.UTC()
	}
	return options{command: command, publicURL: *publicURL, since: since, systemHookAttestationOrigin: *attestationOrigin, configurationOnly: *configurationOnly}, nil
}
func migrationOperatorIdentity() string {
	for _, key := range []string{"SUDO_USER", "USER", "LOGNAME"} {
		if identity := strings.TrimSpace(os.Getenv(key)); identity != "" {
			return identity
		}
	}
	return "unknown"
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write GitLab migration status: %w", err)
	}
	return nil
}
