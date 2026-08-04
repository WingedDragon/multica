package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOMPBackend_UsesOMPDefaultExecutableAndJSONProtocol(t *testing.T) {
	backend, argvPath := newOMPTestBackend(t, true)

	result, _ := executeOMPTestBackend(t, backend, "omp-prompt", ExecOptions{Timeout: 5 * time.Second})
	if result.Status != "completed" {
		t.Fatalf("status = %q, want completed (error=%q)", result.Status, result.Error)
	}
	if result.SessionID != "omp-session-1" {
		t.Fatalf("SessionID = %q, want omp-session-1", result.SessionID)
	}

	args := readOMPTestArgv(t, argvPath)
	sessionDir, err := ompSessionDir()
	if err != nil {
		t.Fatalf("omp session dir: %v", err)
	}
	want := []string{"-p", "--mode", "json", "--session-dir", sessionDir, "omp-prompt"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", args, want)
	}
}

func TestOMPBackend_PassesSelectorThinkingAndBlockedArgs(t *testing.T) {
	backend, argvPath := newOMPTestBackend(t, true)
	const selector = "openai-codex/gpt-5.6-luna"

	_, _ = executeOMPTestBackend(t, backend, "omp-prompt", ExecOptions{
		Timeout:         5 * time.Second,
		ResumeSessionID: "prior-1",
		Model:           selector,
		ThinkingLevel:   "high",
		CustomArgs: []string{
			"-p", "--print", "--mode", "wrong-mode", "--session", "wrong-session",
			"--session-dir", "wrong-dir", "--continue", "--resume", "wrong-resume",
			"--model", "wrong-model", "--thinking", "low", "--provider", "wrong-provider",
			"--verbose",
		},
	})

	args := readOMPTestArgv(t, argvPath)
	sessionDir, err := ompSessionDir()
	if err != nil {
		t.Fatalf("omp session dir: %v", err)
	}
	want := []string{
		"-p", "--mode", "json", "--session-dir", sessionDir,
		"--resume", "prior-1", "--model", selector, "--thinking", "high",
		"--verbose", "omp-prompt",
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", args, want)
	}
}

func TestOMPBackend_MapsStreamSessionIDAndResume(t *testing.T) {
	backend, _ := newOMPTestBackend(t, true)

	result, messages := executeOMPTestBackend(t, backend, "first", ExecOptions{Timeout: 5 * time.Second})
	if result.SessionID != "omp-session-1" {
		t.Fatalf("SessionID = %q, want omp-session-1", result.SessionID)
	}
	if !containsOMPStatus(messages, "omp-session-1") {
		t.Fatalf("messages = %#v, want running status with stream session id", messages)
	}

	resumedBackend, _ := newOMPTestBackend(t, false)
	resumed, _ := executeOMPTestBackend(t, resumedBackend, "second", ExecOptions{
		Timeout:         5 * time.Second,
		ResumeSessionID: "prior-1",
	})
	if resumed.SessionID != "prior-1" {
		t.Fatalf("resumed SessionID = %q, want prior-1", resumed.SessionID)
	}
}

func TestOMPBackend_ClassifiesMissingSessionDirectoryAsResumeRejection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' 'Error: Session \"stale-session\" belongs to a directory that no longer exists (/tmp/removed); run interactively to move it into the current project.' >&2\n" +
		"exit 1\n"
	writeTestExecutable(t, filepath.Join(dir, "omp"), []byte(script))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend, err := New("omp", Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New(omp): %v", err)
	}
	result, _ := executeOMPTestBackend(t, backend, "prompt", ExecOptions{
		Timeout:         5 * time.Second,
		ResumeSessionID: "stale-session",
	})
	if !result.ResumeRejected {
		t.Fatalf("ResumeRejected = false, want true (error=%q)", result.Error)
	}
	if !strings.Contains(result.Error, "belongs to a directory that no longer exists") {
		t.Fatalf("error = %q, want OMP stderr diagnostic", result.Error)
	}
}

func TestOMPProviderResumeRejectionIsDetectable(t *testing.T) {
	if ResumeRejectionUndetectable("omp") {
		t.Fatal("ResumeRejectionUndetectable(\"omp\") = true, want false")
	}
}

func newOMPTestBackend(t *testing.T, emitSession bool) (Backend, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$OMP_ARGV_FILE\"\n"
	if emitSession {
		script += "printf '%s\\n' '{\"type\":\"session\",\"id\":\"omp-session-1\"}'\n"
	}
	script += "printf '%s\\n' '{\"type\":\"agent_start\"}'\n" +
		"printf '%s\\n' '{\"type\":\"turn_start\"}'\n" +
		"printf '%s\\n' '{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"delta\":\"done\"}}'\n" +
		"printf '%s\\n' '{\"type\":\"turn_end\",\"message\":{\"role\":\"assistant\",\"model\":\"test\",\"usage\":{\"input\":1,\"output\":1}}}'\n"
	writeTestExecutable(t, filepath.Join(dir, "omp"), []byte(script))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend, err := New("omp", Config{Env: map[string]string{"OMP_ARGV_FILE": argvPath}, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New(omp): %v", err)
	}
	return backend, argvPath
}

func executeOMPTestBackend(t *testing.T, backend Backend, prompt string, opts ExecOptions) (Result, []Message) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, prompt, opts)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var messages []Message
	for message := range session.Messages {
		messages = append(messages, message)
	}
	result, ok := <-session.Result
	if !ok {
		t.Fatal("result channel closed without a value")
	}
	return result, messages
}

func readOMPTestArgv(t *testing.T, path string) []string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n")
}

func containsOMPStatus(messages []Message, sessionID string) bool {
	for _, message := range messages {
		if message.Type == MessageStatus && message.Status == "running" && message.SessionID == sessionID {
			return true
		}
	}
	return false
}
