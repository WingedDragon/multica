package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const ompModelDiscoveryTimeout = 15 * time.Second

type ompModelCatalog struct {
	Models json.RawMessage `json:"models"`
}

type ompCatalogModel struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Selector string `json:"selector"`
	Name     string `json:"name"`
	// Reasoning gates whether the model exposes an effort picker at all, and
	// Thinking carries the efforts it advertises. The two are independent: a
	// reasoning model may advertise none.
	Reasoning bool            `json:"reasoning"`
	Thinking  json.RawMessage `json:"thinking"`
}

// discoverOMPModels runs `omp models --no-extensions --json` through the
// shared launch boundary (so a custom profile's prefix is honoured) and
// converts the runtime catalog into dropdown models. Disabling extensions
// keeps discovery independent of extension startup cost and failures;
// selectors are passed back to OMP verbatim. Unlike the protocol-family
// discovery fallbacks, errors propagate so callers can retain the manual
// model-selector fallback.
func discoverOMPModels(ctx context.Context, runtimeCmd Command) ([]Model, error) {
	if runtimeCmd.Path == "" {
		runtimeCmd.Path = "omp"
	}
	if _, err := exec.LookPath(runtimeCmd.Path); err != nil {
		return nil, fmt.Errorf("find OMP executable %q: %w", runtimeCmd.Path, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, ompModelDiscoveryTimeout)
	defer cancel()

	cmd := runtimeCmd.exec(runCtx, "models", "--no-extensions", "--json")
	hideAgentWindow(cmd)
	var stderr tailBuffer
	stderr.max = probeStderrSampleBytes
	cmd.Stderr = &stderr
	stdout, err := outputOwned(cmd, runtimeCmd.logger)
	if err != nil {
		return nil, fmt.Errorf("run OMP model discovery with %q: %w%s", runtimeCmd.Path, err, ompDiscoveryStderr(string(stderr.Bytes())))
	}

	models, err := parseOMPModels(stdout)
	if err != nil {
		return nil, fmt.Errorf("parse OMP model catalog: %w%s", err, ompDiscoveryStderr(string(stderr.Bytes())))
	}
	return models, nil
}

func ompDiscoveryStderr(stderr string) string {
	if stderr = strings.TrimSpace(stderr); stderr != "" {
		return "; stderr: " + stderr
	}
	return ""
}

// parseOMPModels accepts only OMP's {"models": [...]} JSON catalog. Individual
// malformed entries are skipped so one stale record cannot hide the catalog.
func parseOMPModels(raw []byte) ([]Model, error) {
	var catalog ompModelCatalog
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, err
	}
	modelsJSON := bytes.TrimSpace(catalog.Models)
	if len(modelsJSON) == 0 || modelsJSON[0] != '[' {
		return nil, fmt.Errorf("missing models array")
	}

	var entries []json.RawMessage
	if err := json.Unmarshal(modelsJSON, &entries); err != nil {
		return nil, fmt.Errorf("decode models array: %w", err)
	}

	models := make([]Model, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, rawEntry := range entries {
		var entry ompCatalogModel
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			continue
		}
		selector := entry.Selector
		if selector == "" {
			if entry.Provider == "" || entry.ID == "" {
				continue
			}
			selector = entry.Provider + "/" + entry.ID
		}
		if _, ok := seen[selector]; ok {
			continue
		}
		seen[selector] = struct{}{}

		label := entry.Name
		if label == "" {
			label = selector
		}
		model := Model{ID: selector, Label: label, Provider: entry.Provider}
		if entry.Reasoning {
			model.Thinking = ompThinkingFromCatalogEntry(entry.Reasoning, entry.Thinking)
		}
		models = append(models, model)
	}
	return models, nil
}

// ompThinkingFromCatalogEntry maps one `omp models --json` entry's reasoning
// metadata onto Multica's per-model effort catalog.
//
// The rules are omp's own, verified against can1357/oh-my-pi v18.2.0:
//
//   - `models --json` emits `thinking` as a concrete effort array or `null`,
//     never an object — see ModelJson/toModelJson in cli/models-cli.ts.
//   - omp's Effort vocabulary is minimal|low|medium|high|xhigh|max. `off` is
//     NOT an effort and so never appears in that array (catalog/src/effort.ts).
//   - `reasoning: true` with `thinking: null` means "reasons, but exposes no
//     controllable effort dial": getSupportedEfforts documents that exact case.
//     Inferring efforts there would offer levels omp then clamps away, which is
//     the silent mismatch MUL-7412 exists to remove.
//   - `off` is honoured for any model whatever its effort array, because
//     resolveThinkingLevelForModel returns it before clamping runs
//     (coding-agent/src/thinking.ts).
//
// So a reasoning model gets `off` plus exactly the efforts the catalog
// advertised, and nothing inferred. A non-reasoning model gets no picker at
// all, matching how Multica hides pi's inert `off`-only control.
//
// `auto` is deliberately excluded: omp keeps AUTO_THINKING as a session-level
// sentinel that is explicitly never an Effort or ThinkingLevel and is resolved
// per turn, so it is not a per-model capability, and providerThinkingEnums
// rejects it (MUL-7412).
func ompThinkingFromCatalogEntry(reasoning bool, raw json.RawMessage) *ModelThinking {
	if !reasoning {
		return nil
	}
	// `off` is always available; the catalog only ever adds efforts on top.
	advertised := map[string]bool{"off": true}
	for _, effort := range parseOmpEfforts(raw) {
		advertised[strings.TrimSpace(effort)] = true
	}
	levels := make([]ThinkingLevel, 0, len(piThinkingLevelOrder))
	for _, value := range piThinkingLevelOrder {
		if advertised[value] {
			levels = append(levels, ThinkingLevel{Value: value, Label: piThinkingLevelLabels[value]})
		}
	}
	return &ModelThinking{SupportedLevels: levels}
}

// parseOmpEfforts reads the effort array out of a catalog entry's `thinking`
// field. An absent field, `null`, and any shape that is not an array of strings
// all yield no efforts: a payload we cannot read is not evidence that the model
// supports anything.
func parseOmpEfforts(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var efforts []string
	if err := json.Unmarshal(raw, &efforts); err != nil {
		return nil
	}
	return efforts
}
