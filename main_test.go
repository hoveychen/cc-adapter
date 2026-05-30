package main

import "testing"

// TestClaudeVersionMismatch checks the startup version guard: it warns only on a
// confirmed difference from PinnedClaudeVersion, and stays quiet for a matching
// version or an unparsable --version line.
func TestClaudeVersionMismatch(t *testing.T) {
	cases := []struct {
		name       string
		output     string
		wantGot    string
		wantMismat bool
	}{
		{"matches pinned", PinnedClaudeVersion + " (Claude Code)", PinnedClaudeVersion, false},
		{"newer version", "2.1.999 (Claude Code)", "2.1.999", true},
		{"older version", "2.1.112 (Claude Code)", "2.1.112", true},
		{"unparsable", "Claude Code", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, mismatched := claudeVersionMismatch(tc.output)
			if got != tc.wantGot || mismatched != tc.wantMismat {
				t.Fatalf("claudeVersionMismatch(%q) = (%q, %v), want (%q, %v)",
					tc.output, got, mismatched, tc.wantGot, tc.wantMismat)
			}
		})
	}
}
