package main

import (
	"testing"
	"time"
)

func TestParseOptionsCoversCutoverSubcommands(t *testing.T) {
	commands := []string{"preflight", "begin", "migrate", "switch-hooks", "finalize", "restore", "status"}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			got, err := parseOptions([]string{command})
			if err != nil {
				t.Fatalf("parseOptions(%q): %v", command, err)
			}
			if got.command != command {
				t.Fatalf("command = %q, want %q", got.command, command)
			}
		})
	}
	for _, command := range []string{"replay", "verify-hooks"} {
		t.Run(command, func(t *testing.T) {
			got, err := parseOptions([]string{command, "--since", "2026-11-23T10:00:00Z"})
			if err != nil {
				t.Fatalf("parseOptions(%q): %v", command, err)
			}
			if got.command != command || !got.since.Equal(time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)) {
				t.Fatalf("parsed options = %#v", got)
			}
		})
	}
}

func TestParseOptionsRequiresUTCReplayWindow(t *testing.T) {
	for _, args := range [][]string{
		{"replay"},
		{"verify-hooks", "--since", "2026-11-23T10:00:00-05:00"},
		{"replay", "--since", "not-a-time"},
		{"migrate", "--since", "2026-11-23T10:00:00Z"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("parseOptions(%v) unexpectedly succeeded", args)
		}
	}
}
func TestParseOptionsAcceptsExactSystemHookAttestationOrigins(t *testing.T) {
	tests := []struct {
		command string
		origin  string
	}{
		{command: "preflight", origin: "https://gitlab.example.test"},
		{command: "begin", origin: "https://gitlab.example.test:8443"},
		{command: "preflight", origin: "https://another-gitlab.example.test"},
	}
	for _, test := range tests {
		got, err := parseOptions([]string{test.command, "--attest-no-legacy-system-hooks-for=" + test.origin})
		if err != nil {
			t.Fatalf("parseOptions(%q) rejected exact origin %q: %v", test.command, test.origin, err)
		}
		if got.systemHookAttestationOrigin != test.origin {
			t.Fatalf("attestation origin = %q, want %q", got.systemHookAttestationOrigin, test.origin)
		}
	}

	for _, args := range [][]string{
		{"preflight", "--attest-no-legacy-system-hooks-for=gitlab.example.test"},
		{"preflight", "--attest-no-legacy-system-hooks-for=http://gitlab.example.test"},
		{"preflight", "--attest-no-legacy-system-hooks-for=https://gitlab.example.test/"},
		{"preflight", "--attest-no-legacy-system-hooks-for=https://gitlab.example.test/group"},
		{"preflight", "--attest-no-legacy-system-hooks-for=https://user@gitlab.example.test"},
		{"preflight", "--attest-no-legacy-system-hooks-for=https://gitlab.example.test?x=1"},
		{"switch-hooks", "--attest-no-legacy-system-hooks-for=https://gitlab.example.test"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("parseOptions(%v) unexpectedly accepted attestation", args)
		}
	}
}
