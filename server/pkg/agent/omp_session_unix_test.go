//go:build unix

package agent

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOmpFreshSessionThenResume covers OMP's native session protocol through the
// backend: a fresh run gets `--session-dir <dir>` and reports the opaque id its
// stream advertised, and the follow-up run resumes that id with `--resume`. The
// id is what the CLI keys its stored transcript by, so a run that reports one id
// and resumes another would silently start a new conversation every turn.
func TestOmpFreshSessionThenResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := filepath.Join(t.TempDir(), "omp")
	writeTestExecutable(t, fake, []byte(`#!/bin/sh
while IFS= read -r _; do :; done
resume=""
session_dir=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --resume) resume="$2"; shift ;;
    --session-dir) session_dir="$2"; shift ;;
  esac
  shift
done
if [ -n "$resume" ]; then
  [ -f "$session_dir/$resume.jsonl" ] || exit 1
else
  [ -d "$session_dir" ] || exit 2
  id="$(basename "$session_dir")-turn"
  printf '%s\n' "{\"type\":\"session\",\"id\":\"$id\"}"
  printf '{}\n' > "$session_dir/$id.jsonl"
fi
printf '%s\n' '{"type":"agent_end"}'
`))
	backend, err := ResolveBackend("omp", Config{ExecutablePath: fake, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	execute := func(prompt, resume string) Result {
		t.Helper()
		session, err := backend.Execute(t.Context(), prompt, ExecOptions{ResumeSessionID: resume, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		for range session.Messages {
		}
		return <-session.Result
	}
	for _, prompt := range []string{"fresh chat", "quick-create task"} {
		first := execute(prompt, "")
		if first.Status != "completed" || first.SessionID == "" {
			t.Fatalf("fresh result: %+v", first)
		}
		// The resumed run reports the id it was asked to resume, so the daemon
		// keeps storing the same session across turns.
		second := execute("follow-up", first.SessionID)
		if second.Status != "completed" || second.SessionID != first.SessionID {
			t.Fatalf("resume result: %+v", second)
		}
	}
	// A resume the CLI cannot resolve fails the run instead of silently
	// starting a fresh conversation under the requested id.
	if result := execute("follow-up", "missing-session"); result.Status != "failed" {
		t.Fatalf("unresolvable resume unexpectedly succeeded: %+v", result)
	}
}

// TestOmpFreshSessionRequiresStreamSessionID pins the other half of the same
// contract: a run whose stream never advertised a session id has no id to
// resume, so the empty value must survive to the result rather than being
// fabricated from the session directory.
func TestOmpFreshSessionRequiresStreamSessionID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := filepath.Join(t.TempDir(), "omp")
	writeTestExecutable(t, fake, []byte("#!/bin/sh\nwhile IFS= read -r _; do :; done\nprintf '%s\\n' '{\"type\":\"agent_end\"}'\n"))
	backend, err := ResolveBackend("omp", Config{ExecutablePath: fake, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	session, err := backend.Execute(t.Context(), "prompt", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "completed" || result.SessionID != "" {
		t.Fatalf("result = %+v, want completed with no session id", result)
	}
	if !strings.Contains(filepath.ToSlash(result.SessionID), "omp") && result.SessionID != "" {
		t.Fatalf("session id must not be synthesized from the session directory: %q", result.SessionID)
	}
}
