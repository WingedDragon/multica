package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const ompCatalogFixture = `{
  "models": [
    {
      "provider": "openai",
      "id": "gpt-5",
      "selector": "openai/gpt-5",
      "name": "GPT 5",
      "thinking": ["low", "high"]
    },
    {
      "provider": "anthropic",
      "id": "claude-opus",
      "name": "Claude Opus",
      "thinking": []
    },
    {
      "provider": "openai",
      "id": "gpt-5-alias",
      "selector": "openai/gpt-5",
      "name": "Duplicate selector"
    },
    {
      "provider": "google",
      "id": "gemini",
      "selector": "google/gemini",
      "thinking": ["medium"]
    },
    {
      "selector": "custom/provider-model",
      "name": "Custom model"
    },
    {
      "provider": "broken",
      "name": "Malformed"
    }
  ]
}`

func TestParseOMPModels_MapsSelectorsAndThinking(t *testing.T) {
	models, err := parseOMPModels([]byte(ompCatalogFixture))
	if err != nil {
		t.Fatalf("parseOMPModels: %v", err)
	}
	if len(models) != 4 {
		t.Fatalf("len(models) = %d, want 4: %+v", len(models), models)
	}

	got := models[0]
	if got.ID != "openai/gpt-5" || got.Label != "GPT 5" || got.Provider != "openai" {
		t.Fatalf("first model = %+v", got)
	}
	if got.Thinking == nil || got.Thinking.DefaultLevel != "" {
		t.Fatalf("first model thinking = %+v, want levels with no default", got.Thinking)
	}
	wantLevels := []ThinkingLevel{{Value: "low", Label: "Low"}, {Value: "high", Label: "High"}}
	if len(got.Thinking.SupportedLevels) != len(wantLevels) {
		t.Fatalf("thinking levels = %+v, want %+v", got.Thinking.SupportedLevels, wantLevels)
	}
	for i, want := range wantLevels {
		if got.Thinking.SupportedLevels[i] != want {
			t.Errorf("thinking level %d = %+v, want %+v", i, got.Thinking.SupportedLevels[i], want)
		}
	}
	if models[1].ID != "anthropic/claude-opus" || models[1].Thinking != nil {
		t.Errorf("fallback model = %+v, want fallback ID and nil thinking", models[1])
	}
	if models[2].ID != "google/gemini" || models[2].Label != "google/gemini" {
		t.Errorf("missing-name model = %+v, want selector label fallback", models[2])
	}
}

func TestParseOMPModels_DeduplicatesSelectors(t *testing.T) {
	models, err := parseOMPModels([]byte(ompCatalogFixture))
	if err != nil {
		t.Fatalf("parseOMPModels: %v", err)
	}
	wantIDs := []string{"openai/gpt-5", "anthropic/claude-opus", "google/gemini", "custom/provider-model"}
	if len(models) != len(wantIDs) {
		t.Fatalf("len(models) = %d, want %d: %+v", len(models), len(wantIDs), models)
	}
	for i, want := range wantIDs {
		if models[i].ID != want {
			t.Errorf("models[%d].ID = %q, want %q", i, models[i].ID, want)
		}
	}
}

func TestParseOMPModels_FallsBackToProviderAndID(t *testing.T) {
	models, err := parseOMPModels([]byte(`{"models":[{"provider":"vendor","id":"model","name":"Model"}]}`))
	if err != nil {
		t.Fatalf("parseOMPModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("len(models) = %d, want 1: %+v", len(models), models)
	}
	if got := models[0]; got.ID != "vendor/model" || got.Label != "Model" || got.Provider != "vendor" {
		t.Errorf("model = %+v, want provider/id fallback", got)
	}
}

func TestParseOMPModels_SkipsTypeMalformedEntries(t *testing.T) {
	models, err := parseOMPModels([]byte(`{"models":[{"selector":"valid/model","name":"Valid"},{"selector":42}]}`))
	if err != nil {
		t.Fatalf("parseOMPModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "valid/model" {
		t.Fatalf("models = %+v, want only valid/model", models)
	}
}

func TestParseOMPModels_RejectsMalformedTopLevelJSON(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"models":`),
		[]byte(`[{"selector":"vendor/model"}]`),
		[]byte(`{}`),
		[]byte(`{"models":null}`),
	} {
		if _, err := parseOMPModels(raw); err == nil {
			t.Errorf("parseOMPModels(%s) succeeded, want top-level error", raw)
		}
	}
}

func TestDiscoverOMPModels_ReturnsCommandErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	fake := filepath.Join(t.TempDir(), "omp")
	writeTestExecutable(t, fake, []byte("#!/bin/sh\necho 'catalog unavailable' >&2\nexit 3\n"))

	_, err := discoverOMPModels(context.Background(), fake)
	if err == nil {
		t.Fatal("discoverOMPModels succeeded, want command error")
	}
	if !strings.Contains(err.Error(), "catalog unavailable") {
		t.Errorf("error = %q, want stderr diagnostic", err)
	}
}

func TestDiscoverOMPModels_DisablesExtensions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	fake := filepath.Join(dir, "omp")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$OMP_ARGV_FILE\"\n" +
		"printf '%s\\n' '{\"models\":[{\"selector\":\"vendor/model\"}]}'\n"
	writeTestExecutable(t, fake, []byte(script))
	t.Setenv("OMP_ARGV_FILE", argvPath)

	models, err := discoverOMPModels(context.Background(), fake)
	if err != nil {
		t.Fatalf("discoverOMPModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "vendor/model" {
		t.Fatalf("models = %+v, want vendor/model", models)
	}
	args, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	if got, want := strings.Fields(string(args)), []string{"models", "--no-extensions", "--json"}; !slices.Equal(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestListModels_OMPIsolatedByExecutablePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	dir := t.TempDir()
	first := filepath.Join(dir, "omp-first")
	second := filepath.Join(dir, "omp-second")
	writeTestExecutable(t, first, []byte("#!/bin/sh\nprintf '%s\\n' '{\"models\":[{\"selector\":\"first/model\",\"name\":\"First\"}]}'\n"))
	writeTestExecutable(t, second, []byte("#!/bin/sh\nprintf '%s\\n' '{\"models\":[{\"selector\":\"second/model\",\"name\":\"Second\"}]}'\n"))

	resetCache := func() {
		modelCacheMu.Lock()
		delete(modelCache, discoveryCacheKey("omp", first))
		delete(modelCache, discoveryCacheKey("omp", second))
		delete(modelCache, "pi")
		modelCacheMu.Unlock()
	}
	resetCache()
	modelCacheMu.Lock()
	modelCache["pi"] = modelCacheEntry{
		models:    []Model{{ID: "pi/sentinel"}},
		expiresAt: time.Now().Add(modelCacheTTL),
	}
	modelCacheMu.Unlock()
	t.Cleanup(resetCache)

	firstModels, err := ListModels(context.Background(), "omp", first)
	if err != nil {
		t.Fatalf("ListModels first: %v", err)
	}
	secondModels, err := ListModels(context.Background(), "omp", second)
	if err != nil {
		t.Fatalf("ListModels second: %v", err)
	}
	if len(firstModels.Models) != 1 || firstModels.Models[0].ID != "first/model" {
		t.Errorf("first models = %+v, want first/model", firstModels.Models)
	}
	if len(secondModels.Models) != 1 || secondModels.Models[0].ID != "second/model" {
		t.Errorf("second models = %+v, want second/model", secondModels.Models)
	}
}
