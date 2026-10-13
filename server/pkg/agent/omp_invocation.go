package agent

import (
	"log/slog"
	"os"
	"path/filepath"
)

// ompBlockedArgs are the flags a custom profile may not supply: the daemon owns
// OMP's print-mode protocol, its session selection, and the model/thinking
// selectors, so a custom arg that restated one would silently win over the
// invocation the backend builds.
var ompBlockedArgs = map[string]blockedArgMode{
	"-p":            blockedStandalone,
	"--print":       blockedStandalone,
	"--mode":        blockedWithValue,
	"--session":     blockedWithValue,
	"--session-dir": blockedWithValue,
	"--continue":    blockedStandalone,
	"--resume":      blockedWithValue,
	"--model":       blockedWithValue,
	"--thinking":    blockedWithValue,
	"--provider":    blockedWithValue,
}

// buildOMPArgs assembles the argv for native OMP. OMP's JSON event stream is
// wire-compatible with Pi's, but its session protocol is not: the daemon names
// the shared session directory on every run, and a resume is the opaque id the
// stream reported rather than a transcript path.
func buildOMPArgs(sessionDir string, opts ExecOptions, logger *slog.Logger) []string {
	args := []string{
		"-p",
		"--mode", "json",
		"--session-dir", sessionDir,
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ThinkingLevel != "" {
		args = append(args, "--thinking", opts.ThinkingLevel)
	}
	args = append(args, filterCustomArgs(opts.CustomArgs, ompBlockedArgs, logger)...)
	return args
}

// ompSessionDir is the directory OMP stores its transcripts in. Unlike Pi's
// per-session files, every OMP run shares it and resumes by opaque id, so the
// directory survives workdir changes and outlives any single task.
func ompSessionDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".multica", "omp-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
