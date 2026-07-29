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
	Provider string   `json:"provider"`
	ID       string   `json:"id"`
	Selector string   `json:"selector"`
	Name     string   `json:"name"`
	Thinking []string `json:"thinking"`
}

// discoverOMPModels runs `omp models --json` and converts the runtime catalog
// into dropdown models. OMP's selectors are passed back to its CLI verbatim.
func discoverOMPModels(ctx context.Context, executablePath string) ([]Model, error) {
	if executablePath == "" {
		executablePath = "omp"
	}
	if _, err := exec.LookPath(executablePath); err != nil {
		return nil, fmt.Errorf("find OMP executable %q: %w", executablePath, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, ompModelDiscoveryTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, executablePath, "models", "--json")
	hideAgentWindow(cmd)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("run OMP model discovery with %q: %w%s", executablePath, err, ompDiscoveryStderr(stderr.String()))
	}

	models, err := parseOMPModels([]byte(stdout.String()))
	if err != nil {
		return nil, fmt.Errorf("parse OMP model catalog: %w%s", err, ompDiscoveryStderr(stderr.String()))
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

	var entries []ompCatalogModel
	if err := json.Unmarshal(modelsJSON, &entries); err != nil {
		return nil, fmt.Errorf("decode models array: %w", err)
	}

	models := make([]Model, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
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
		if len(entry.Thinking) > 0 {
			levels := make([]ThinkingLevel, 0, len(entry.Thinking))
			for _, value := range entry.Thinking {
				levels = append(levels, ThinkingLevel{Value: value, Label: strings.Title(value)}) //nolint:staticcheck
			}
			model.Thinking = &ModelThinking{SupportedLevels: levels}
		}
		models = append(models, model)
	}
	return models, nil
}
