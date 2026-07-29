package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func stageFakeOMP(t *testing.T, binDir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture is unavailable on Windows")
	}

	path := filepath.Join(binDir, "omp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake omp: %v", err)
	}
	return path
}

func loadConfigWithOMP(t *testing.T) Config {
	t.Helper()
	binDir := stageFakeAgent(t)
	stageFakeOMP(t, binDir)
	t.Setenv("SHELL", "/usr/bin/fish")
	t.Setenv("MULTICA_OMP_PATH", "")

	cfg, err := LoadConfig(Overrides{
		ServerURL:      "http://localhost:0",
		WorkspacesRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func TestLoadConfig_DiscoversOMP(t *testing.T) {
	cfg := loadConfigWithOMP(t)

	entry, ok := cfg.Agents["omp"]
	if !ok {
		t.Fatalf("omp was not discovered: %v", cfg.Agents)
	}
	if entry.Command != "omp" {
		t.Errorf("OMP command = %q, want %q", entry.Command, "omp")
	}
	if entry.Path == "" || !filepath.IsAbs(entry.Path) {
		t.Errorf("OMP path = %q, want absolute executable path", entry.Path)
	}
}

func TestLoadConfig_OMPPathOverrideWins(t *testing.T) {
	binDir := stageFakeAgent(t)
	override := stageFakeOMP(t, t.TempDir())
	t.Setenv("PATH", binDir)
	t.Setenv("SHELL", "/usr/bin/fish")
	t.Setenv("MULTICA_OMP_PATH", override)

	cfg, err := LoadConfig(Overrides{
		ServerURL:      "http://localhost:0",
		WorkspacesRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	entry, ok := cfg.Agents["omp"]
	if !ok {
		t.Fatalf("OMP override was not discovered: %v", cfg.Agents)
	}
	if entry.Path != override || entry.Command != override {
		t.Fatalf("OMP entry = %+v, want path=%q command=%q", entry, override, override)
	}
}

func TestLoadConfig_OMPModelOverrideIsStored(t *testing.T) {
	t.Setenv("MULTICA_OMP_MODEL", "openai-codex/gpt-5.6-luna")
	cfg := loadConfigWithOMP(t)

	if got := cfg.Agents["omp"].Model; got != "openai-codex/gpt-5.6-luna" {
		t.Errorf("OMP model = %q, want %q", got, "openai-codex/gpt-5.6-luna")
	}
}

func TestDefaultAgentCommandNamesIncludesOMP(t *testing.T) {
	for _, name := range defaultAgentCommandNames {
		if name == "omp" {
			return
		}
	}
	t.Errorf("defaultAgentCommandNames = %v, want omp", defaultAgentCommandNames)
}
