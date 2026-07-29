package execenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteContextFilesOMPNativeSkills(t *testing.T) {
	workDir := t.TempDir()
	fallback := skillsDirPath(workDir, "unknown-provider")
	wantSkillsDir := filepath.Join(workDir, ".pi", "skills")
	if got := skillsDirPath(workDir, "omp"); got != wantSkillsDir {
		t.Fatalf("skillsDirPath(omp) = %q, want %q", got, wantSkillsDir)
	}
	if fallback == wantSkillsDir {
		t.Fatalf("OMP skills path %q must not use fallback path", wantSkillsDir)
	}

	ctx := TaskContextForEnv{
		IssueID: "omp-skill-test",
		AgentSkills: []SkillContextForEnv{{
			Name:    "OMP Helper",
			Content: "Use the OMP native skill path.",
		}},
	}
	if err := writeContextFiles(workDir, "omp", ctx, nil); err != nil {
		t.Fatalf("writeContextFiles: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(wantSkillsDir, "omp-helper", "SKILL.md"))
	if err != nil {
		t.Fatalf("read OMP SKILL.md: %v", err)
	}
	if !strings.Contains(string(content), "Use the OMP native skill path.") {
		t.Fatalf("OMP SKILL.md = %q, want skill content", content)
	}
	if _, err := os.Stat(filepath.Join(fallback, "omp-helper", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("fallback skill file exists for OMP: %v", err)
	}
}

func TestOMPSkillCleanupPreservesExistingContent(t *testing.T) {
	workDir := t.TempDir()
	envRoot := t.TempDir()
	userSkill := filepath.Join(workDir, ".pi", "skills", "user-owned", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(userSkill), 0o755); err != nil {
		t.Fatalf("create user skill directory: %v", err)
	}
	if err := os.WriteFile(userSkill, []byte("user-owned skill"), 0o644); err != nil {
		t.Fatalf("write user skill: %v", err)
	}

	manifest := &sidecarManifest{}
	ctx := TaskContextForEnv{
		IssueID: "omp-cleanup-test",
		AgentSkills: []SkillContextForEnv{{
			Name:    "Issue Review",
			Content: "Multica-managed OMP skill",
		}},
	}
	if err := writeContextFiles(workDir, "omp", ctx, manifest); err != nil {
		t.Fatalf("writeContextFiles: %v", err)
	}
	if err := writeSidecarManifest(envRoot, manifest); err != nil {
		t.Fatalf("writeSidecarManifest: %v", err)
	}

	managedSkill := filepath.Join(workDir, ".pi", "skills", "issue-review", "SKILL.md")
	if _, err := os.Stat(managedSkill); err != nil {
		t.Fatalf("managed OMP skill missing before cleanup: %v", err)
	}
	if err := CleanupSidecars(envRoot); err != nil {
		t.Fatalf("CleanupSidecars: %v", err)
	}
	if _, err := os.Stat(managedSkill); !os.IsNotExist(err) {
		t.Fatalf("managed OMP skill remains after cleanup: %v", err)
	}
	content, err := os.ReadFile(userSkill)
	if err != nil {
		t.Fatalf("read preserved user skill: %v", err)
	}
	if string(content) != "user-owned skill" {
		t.Fatalf("user skill content = %q, want unchanged content", content)
	}
}
