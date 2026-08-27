package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestSupportedTypesLockstepWithNew guards the iron-rule whitelist: every type
// in SupportedTypes must be constructable by New, and New must reject anything
// not in SupportedTypes. This is the single source of truth the custom runtime
// profile protocol_family validation (handler) and the runtime_profile
// protocol_family CHECK (migration 120 plus later tightening migrations) are aligned to. If a backend is added
// to New, it must be added here too — and to the migration CHECK.
func TestSupportedTypesLockstepWithNew(t *testing.T) {
	cfg := Config{Logger: slog.Default()}

	for _, typ := range SupportedTypes {
		if !IsSupportedType(typ) {
			t.Errorf("IsSupportedType(%q) = false, but it is in SupportedTypes", typ)
		}
		if _, err := New(typ, cfg); err != nil {
			t.Errorf("New(%q) returned error for a SupportedTypes entry: %v", typ, err)
		}
	}

	// A type outside the whitelist must be rejected by both.
	const bogus = "definitely-not-a-real-backend"
	if IsSupportedType(bogus) {
		t.Errorf("IsSupportedType(%q) = true, want false", bogus)
	}
	if _, err := New(bogus, cfg); err == nil {
		t.Errorf("New(%q) succeeded, want error for an unsupported type", bogus)
	}
}

// TestSupportedTypesMatchesMigrationWhitelist reads the newest migration that
// rebuilds the runtime_profile.protocol_family CHECK and requires it to match
// SupportedTypes exactly.
//
// It parses the SQL on purpose. The previous version compared SupportedTypes
// against a hand-copied map, so it kept passing while upstream migrations 370
// and 403 rebuilt the real constraint from upstream's whitelist and silently
// dropped this fork's `omp` entry — the same regression migration 362 had
// already repaired once. Deriving the expected set from the migration itself
// makes any future upstream rebuild that drops a fork-local family fail here
// instead of in production, where the insert is rejected by Postgres.
func TestSupportedTypesMatchesMigrationWhitelist(t *testing.T) {
	path, families := latestProtocolFamilyCheck(t)

	want := make(map[string]bool, len(families))
	for _, family := range families {
		want[family] = true
	}
	have := make(map[string]bool, len(SupportedTypes))
	for _, typ := range SupportedTypes {
		have[typ] = true
	}

	for _, typ := range SupportedTypes {
		if !want[typ] {
			t.Errorf("SupportedTypes contains %q, missing from the protocol_family CHECK in %s; add a migration that rebuilds the constraint with it", typ, filepath.Base(path))
		}
	}
	for _, family := range families {
		if !have[family] {
			t.Errorf("protocol_family CHECK in %s allows %q, missing from SupportedTypes; add the backend or drop it from the constraint", filepath.Base(path), family)
		}
	}
}

var (
	protocolFamilyCheckRe = regexp.MustCompile(`(?s)ADD CONSTRAINT runtime_profile_protocol_family_check\s*CHECK \(protocol_family IN \((.*?)\)\)`)
	sqlStringRe           = regexp.MustCompile(`'([^']*)'`)
)

// latestProtocolFamilyCheck returns the highest-numbered up migration that adds
// the runtime_profile protocol_family CHECK, plus the families it whitelists.
// Migration numbers are not unique in this fork (314 exists twice), so ties
// break on the full filename, matching golang-migrate's ordering.
func latestProtocolFamilyCheck(t *testing.T) (string, []string) {
	t.Helper()

	dir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}

	type candidate struct {
		version int
		name    string
		body    string
	}
	var candidates []candidate
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		match := protocolFamilyCheckRe.FindStringSubmatch(string(raw))
		if match == nil {
			continue
		}
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			t.Fatalf("migration %s has no numeric version prefix: %v", name, err)
		}
		candidates = append(candidates, candidate{version: version, name: name, body: match[1]})
	}
	if len(candidates) == 0 {
		t.Fatal("no migration adds runtime_profile_protocol_family_check; the whitelist guard cannot run")
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].version != candidates[j].version {
			return candidates[i].version < candidates[j].version
		}
		return candidates[i].name < candidates[j].name
	})

	latest := candidates[len(candidates)-1]
	var families []string
	for _, match := range sqlStringRe.FindAllStringSubmatch(latest.body, -1) {
		families = append(families, match[1])
	}
	if len(families) == 0 {
		t.Fatalf("migration %s declares an empty protocol_family whitelist", latest.name)
	}
	return filepath.Join(dir, latest.name), families
}
