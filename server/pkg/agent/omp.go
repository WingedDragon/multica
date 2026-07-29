package agent

import (
	"log/slog"
	"os"
	"path/filepath"
)

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

// buildOMPArgs assembles the argv for OMP's Pi-compatible JSON event stream.
func buildOMPArgs(prompt, sessionDir string, opts ExecOptions, logger *slog.Logger) []string {
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
	return append(args, prompt)
}

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
