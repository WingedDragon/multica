package agent

import (
	"context"
	"fmt"
)

// BuiltinRuntime describes a runtime the daemon probes and registers
// automatically. A descriptor may name an independent provider such as OMP or
// a future protocol-compatible derivative; in either case it centralizes the
// provider-specific registration, discovery, display, and skill metadata.
//
// The descriptor is the single declaration site for a runtime identity:
// agents_probe.go, config.go, daemon.go display-name overrides, execenv
// skill/config paths, local_skills.go, ListModels, and the frontend display
// maps all derive from this.
type BuiltinRuntime struct {
	// ID is the provider key the daemon registers under (e.g. "omp"). It may
	// also be a supported protocol family when that provider owns independent
	// execution and runtime-profile semantics.
	ID string

	// ProtocolFamily is the execution provider passed to New. It MUST be in
	// SupportedTypes and may equal ID for a native provider such as OMP.
	ProtocolFamily string

	// DefaultCommand is the bare CLI name the probe looks up on PATH
	// when MULTICA_<ID>_PATH is not set (e.g. "omp").
	DefaultCommand string

	// EnvPrefix is the MULTICA_ prefix for *_PATH and *_MODEL env overrides
	// (e.g. "MULTICA_OMP" → MULTICA_OMP_PATH / MULTICA_OMP_MODEL).
	EnvPrefix string

	// DisplayName is the human-facing runtime name. The daemon and frontend
	// both use this so they never drift apart.
	DisplayName string

	// SkillsDir is the project-level skills directory relative to the
	// workdir (e.g. ".omp/skills").
	SkillsDir string

	// UserSkillsDir is the user-level skills directory relative to $HOME
	// (e.g. ".omp/agent/skills").
	UserSkillsDir string

	// LaunchHeader is the user-visible launch skeleton shown in the UI
	// (e.g. "omp (json mode)").
	LaunchHeader string

	// DefaultExecutable is the binary name the backend falls back to when
	// cfg.ExecutablePath is empty (passed to piBackend.defaultExecutable).
	DefaultExecutable string

	// ProviderLabel is the label used in log/error messages (passed to
	// piBackend.providerLabel).
	ProviderLabel string

	// ModelDiscovery is the strategy for discovering available models. OMP
	// uses `omp models --no-extensions --json`, whose catalog and invocation
	// differ from Pi's `--list-models`; a nil strategy returns an empty
	// catalog instead of attempting an incompatible family command.
	ModelDiscovery ModelDiscoveryFunc
}

// ModelDiscoveryFunc discovers available models for a runtime identity. It
// receives the context and resolved executable path; ListModels propagates an
// error so callers can retain their manual model-selector fallback.
type ModelDiscoveryFunc func(ctx context.Context, executablePath string) ([]Model, error)

// BuiltinRuntimes is the registry of automatically detected runtime
// identities. Each entry is probed independently so a host with both Pi and
// OMP installed registers two distinct runtimes.
var BuiltinRuntimes = []BuiltinRuntime{
	{
		ID:                "omp",
		ProtocolFamily:    "omp",
		DefaultCommand:    "omp",
		EnvPrefix:         "MULTICA_OMP",
		DisplayName:       "OMP",
		SkillsDir:         ".omp/skills",
		UserSkillsDir:     ".omp/agent/skills",
		LaunchHeader:      "omp (json mode)",
		DefaultExecutable: "omp",
		ProviderLabel:     "omp",
		ModelDiscovery:    discoverOMPModels,
	},
}

// BuiltinRuntimeByID returns the descriptor for the given runtime identity,
// or false if no such built-in runtime exists.
func BuiltinRuntimeByID(id string) (BuiltinRuntime, bool) {
	for _, r := range BuiltinRuntimes {
		if r.ID == id {
			return r, true
		}
	}
	return BuiltinRuntime{}, false
}

// IsBuiltinRuntime reports whether id is a registered built-in runtime
// identity (as opposed to a protocol family in SupportedTypes).
func IsBuiltinRuntime(id string) bool {
	_, ok := BuiltinRuntimeByID(id)
	return ok
}

// BuiltinRuntimeCommands returns the default CLI command names for all
// built-in runtimes, for the daemon's defaultAgentCommandNames list.
func BuiltinRuntimeCommands() []string {
	cmds := make([]string, len(BuiltinRuntimes))
	for i, r := range BuiltinRuntimes {
		cmds[i] = r.DefaultCommand
	}
	return cmds
}

// backendOverrideApplicator lets a descriptor apply per-runtime executable and
// label defaults to its backend. A backend that cannot honor those defaults
// cannot host the registered runtime, so NewRuntime fails closed.
type backendOverrideApplicator interface {
	applyBuiltinRuntimeOverrides(desc BuiltinRuntime)
}

// piBackend already implements this via its defaultExecutable and
// providerLabel fields. The method is defined here (not in pi.go) to keep
// the descriptor→backend contract in one file.
func (b *piBackend) applyBuiltinRuntimeOverrides(desc BuiltinRuntime) {
	b.defaultExecutable = desc.DefaultExecutable
	b.providerLabel = desc.ProviderLabel
}

// ResolveBackend is the single production entry point the daemon uses to
// construct a backend from a provider key. Registered runtime metadata routes
// through NewRuntime; all other supported provider keys route directly through
// New.
func ResolveBackend(provider string, cfg Config) (Backend, error) {
	if IsBuiltinRuntime(provider) {
		return NewRuntime(provider, cfg)
	}
	return New(provider, cfg)
}

// NewRuntime creates a Backend for a registered runtime identity (e.g. "omp").
// It builds the descriptor's execution provider via New(), then applies the
// per-runtime executable and label defaults. New remains available for direct
// protocol-family construction, including OMP runtime profiles.

// Fails closed: if the execution backend does not implement
// backendOverrideApplicator, the descriptor's defaults cannot be applied and
// NewRuntime returns an error rather than returning an unmodified backend.
func NewRuntime(runtimeID string, cfg Config) (Backend, error) {
	desc, ok := BuiltinRuntimeByID(runtimeID)
	if !ok {
		return nil, fmt.Errorf("unknown runtime identity: %q", runtimeID)
	}
	backend, err := New(desc.ProtocolFamily, cfg)
	if err != nil {
		return nil, fmt.Errorf("runtime %q (family %q): %w", runtimeID, desc.ProtocolFamily, err)
	}
	applicator, ok := backend.(backendOverrideApplicator)
	if !ok {
		return nil, fmt.Errorf("runtime %q: protocol family %q backend %T does not support runtime overrides", runtimeID, desc.ProtocolFamily, backend)
	}
	applicator.applyBuiltinRuntimeOverrides(desc)
	return backend, nil
}
