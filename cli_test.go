package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseArgs_PromptOnly(t *testing.T) {
	o := parseArgs([]string{"fix", "the", "bug"})
	if got := o.prompt(); got != "fix the bug" {
		t.Fatalf("prompt = %q, want %q", got, "fix the bug")
	}
	if o.print || len(o.forward) != 0 {
		t.Fatalf("unexpected: print=%v forward=%v", o.print, o.forward)
	}
}

func TestParseArgs_PrintAndOutputFormat(t *testing.T) {
	o := parseArgs([]string{"-p", "--output-format", "json", "hello"})
	if !o.print {
		t.Fatal("print not set")
	}
	if o.outputFormat != "json" {
		t.Fatalf("outputFormat = %q", o.outputFormat)
	}
	if o.prompt() != "hello" {
		t.Fatalf("prompt = %q", o.prompt())
	}
	if len(o.forward) != 0 {
		t.Fatalf("forward should be empty, got %v", o.forward)
	}
}

func TestParseArgs_OutputFormatEqualsForm(t *testing.T) {
	o := parseArgs([]string{"--print", "--output-format=stream-json"})
	if o.outputFormat != "stream-json" {
		t.Fatalf("outputFormat = %q", o.outputFormat)
	}
}

func TestParseArgs_ForwardSingleValue(t *testing.T) {
	o := parseArgs([]string{"-p", "--model", "claude-opus-4-8", "do it"})
	want := []string{"--model", "claude-opus-4-8"}
	if !reflect.DeepEqual(o.forward, want) {
		t.Fatalf("forward = %v, want %v", o.forward, want)
	}
	if o.prompt() != "do it" {
		t.Fatalf("prompt = %q, want %q", o.prompt(), "do it")
	}
}

func TestParseArgs_ForwardVariadicStopsAtPrompt(t *testing.T) {
	// --add-dir is variadic; it must NOT swallow the positional prompt that
	// follows a subsequent flag. Here the prompt comes after another flag.
	o := parseArgs([]string{"-p", "--add-dir", "/a", "/b", "--verbose", "summarize"})
	wantFwd := []string{"--add-dir", "/a", "/b", "--verbose"}
	if !reflect.DeepEqual(o.forward, wantFwd) {
		t.Fatalf("forward = %v, want %v", o.forward, wantFwd)
	}
	if o.prompt() != "summarize" {
		t.Fatalf("prompt = %q", o.prompt())
	}
}

func TestParseArgs_VariadicSwallowsTrailingPositional(t *testing.T) {
	// Known limitation: a variadic flag immediately before the positional prompt
	// will absorb it (commander has the same ambiguity). Document via test so the
	// behavior is intentional, not accidental.
	o := parseArgs([]string{"-p", "summarize", "--allowedTools", "Bash", "Edit"})
	// "summarize" is a leading positional, captured before the flag.
	if o.prompt() != "summarize" {
		t.Fatalf("prompt = %q, want %q", o.prompt(), "summarize")
	}
	wantFwd := []string{"--allowedTools", "Bash", "Edit"}
	if !reflect.DeepEqual(o.forward, wantFwd) {
		t.Fatalf("forward = %v, want %v", o.forward, wantFwd)
	}
}

func TestParseArgs_OptionalValueResume(t *testing.T) {
	o := parseArgs([]string{"-p", "--resume", "abc-123", "continue"})
	wantFwd := []string{"--resume", "abc-123"}
	if !reflect.DeepEqual(o.forward, wantFwd) {
		t.Fatalf("forward = %v, want %v", o.forward, wantFwd)
	}
	if o.prompt() != "continue" {
		t.Fatalf("prompt = %q", o.prompt())
	}
}

func TestParseArgs_OptionalValueResumeBare(t *testing.T) {
	// --resume followed by another flag must NOT consume it.
	o := parseArgs([]string{"--resume", "--verbose"})
	wantFwd := []string{"--resume", "--verbose"}
	if !reflect.DeepEqual(o.forward, wantFwd) {
		t.Fatalf("forward = %v, want %v", o.forward, wantFwd)
	}
}

func TestParseArgs_Management(t *testing.T) {
	o := parseArgs([]string{"--no-ide", "-deny-writes", "--claude-bin", "/x/claude", "-p", "hi"})
	if !o.noIDE || !o.denyWrites {
		t.Fatalf("management flags: noIDE=%v denyWrites=%v", o.noIDE, o.denyWrites)
	}
	if o.claudeBin != "/x/claude" {
		t.Fatalf("claudeBin = %q", o.claudeBin)
	}
	if len(o.forward) != 0 {
		t.Fatalf("management flags must not be forwarded, got %v", o.forward)
	}
}

func TestParseArgs_ClaudeBinEquals(t *testing.T) {
	o := parseArgs([]string{"--claude-bin=/y/claude", "-p", "hi"})
	if o.claudeBin != "/y/claude" {
		t.Fatalf("claudeBin = %q", o.claudeBin)
	}
}

func TestParseArgs_DashDashSeparatesPrompt(t *testing.T) {
	// The "--" escape hatch: a variadic flag no longer swallows the prompt.
	o := parseArgs([]string{"-p", "--allowedTools", "Bash", "Edit", "--", "summarize", "this"})
	wantFwd := []string{"--allowedTools", "Bash", "Edit"}
	if !reflect.DeepEqual(o.forward, wantFwd) {
		t.Fatalf("forward = %v, want %v", o.forward, wantFwd)
	}
	if o.prompt() != "summarize this" {
		t.Fatalf("prompt = %q, want %q", o.prompt(), "summarize this")
	}
}

func TestParseArgs_DashDashPreservesFlagLikePrompt(t *testing.T) {
	// After "--", tokens that look like flags are still part of the prompt.
	o := parseArgs([]string{"-p", "--", "explain", "--verbose", "mode"})
	if o.prompt() != "explain --verbose mode" {
		t.Fatalf("prompt = %q", o.prompt())
	}
	if len(o.forward) != 0 {
		t.Fatalf("forward should be empty, got %v", o.forward)
	}
}

func TestParseArgs_PartialAndReplay(t *testing.T) {
	o := parseArgs([]string{"-p", "--include-partial-messages", "--replay-user-messages", "go"})
	if !o.includePartial || !o.replayUserMessages {
		t.Fatalf("includePartial=%v replayUserMessages=%v", o.includePartial, o.replayUserMessages)
	}
	if len(o.forward) != 0 {
		t.Fatalf("adapter-owned flags must not be forwarded by the parser, got %v", o.forward)
	}
	if o.prompt() != "go" {
		t.Fatalf("prompt = %q", o.prompt())
	}
}

func TestParseArgs_UnknownFlagForwardedAsBoolean(t *testing.T) {
	o := parseArgs([]string{"-p", "--dangerously-skip-permissions", "go"})
	wantFwd := []string{"--dangerously-skip-permissions"}
	if !reflect.DeepEqual(o.forward, wantFwd) {
		t.Fatalf("forward = %v, want %v", o.forward, wantFwd)
	}
	if o.prompt() != "go" {
		t.Fatalf("prompt = %q", o.prompt())
	}
}

// --- SDK-driven relay flag handling (P1) ---

// TestParseArgs_SDKTransportFlags verifies the exact argv the Claude Agent SDK
// spawns its child with parses cleanly: input/output-format consumed, relay
// mode detected, --permission-prompt-tool consuming its `stdio` value (not
// leaking it as a positional prompt), and no stray prompt.
func TestParseArgs_SDKTransportFlags(t *testing.T) {
	o := parseArgs([]string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
		"--print",
	})
	if !o.relayMode() {
		t.Fatalf("relayMode = false, want true")
	}
	if o.prompt() != "" {
		t.Fatalf("prompt = %q, want empty (stdio must not leak as a positional)", o.prompt())
	}
	// --verbose and --permission-prompt-tool stdio are forwarded here; the
	// baseline dedup is what strips them before they reach the child.
	got := dedupBaselineFlags(o.forward)
	if len(got) != 0 {
		t.Fatalf("after dedup, child extra args = %v, want empty", got)
	}
}

func TestRelayMode_OnlyWhenBothStreamJSON(t *testing.T) {
	cases := []struct {
		in, out string
		want    bool
	}{
		{"stream-json", "stream-json", true},
		{"stream-json", "text", false},
		{"text", "stream-json", false},
		{"", "", false},
	}
	for _, c := range cases {
		o := cliOpts{inputFormat: c.in, outputFormat: c.out}
		if got := o.relayMode(); got != c.want {
			t.Fatalf("relayMode(in=%q,out=%q) = %v, want %v", c.in, c.out, got, c.want)
		}
	}
}

func TestDedupBaselineFlags(t *testing.T) {
	in := []string{
		"--permission-prompt-tool", "stdio", // dup of baseline (2 tokens)
		"--verbose",                  // dup of baseline (1 token)
		"--model", "claude-opus-4-8", // kept (not baseline)
		"--add-dir", "/tmp", "/var", // kept
	}
	got := dedupBaselineFlags(in)
	want := []string{"--model", "claude-opus-4-8", "--add-dir", "/tmp", "/var"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dedupBaselineFlags = %v, want %v", got, want)
	}
}

// TestParseArgs_ThinkingAndMaxTurnsForwarded guards the value-taking flags the
// claw-workspace sidecar passes for main-task dispatch: `--thinking <mode>
// --thinking-display <display>` (thinking-token counter) and `--max-turns <n>`.
// If any is absent from forwardSingleValue, parseArgs treats it as a boolean and
// its value token is misclassified as the positional prompt, so the child claude
// receives e.g. `--thinking --thinking-display` and rejects it
// ("option '--thinking <mode>' argument '--thinking-display' is invalid").
func TestParseArgs_ThinkingAndMaxTurnsForwarded(t *testing.T) {
	o := parseArgs([]string{
		"--output-format", "stream-json", "--input-format", "stream-json",
		"--dangerously-skip-permissions",
		"--thinking", "adaptive", "--thinking-display", "omitted",
		"--max-turns", "50",
	})
	// No positional prompt: the sidecar feeds the prompt over stream-json stdin.
	if p := o.prompt(); p != "" {
		t.Fatalf("prompt should be empty, got %q (a flag value leaked into the positional)", p)
	}
	joined := strings.Join(o.forward, " ")
	for _, want := range []string{"--thinking adaptive", "--thinking-display omitted", "--max-turns 50"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("forward must keep %q intact; got %v", want, o.forward)
		}
	}
}

// TestParseArgs_PluginDirSingleValue guards against the inverse arity bug: claude
// defines --plugin-dir <path> / --plugin-url <url> as single-value (repeatable),
// NOT variadic. If they are misclassified as variadic, the flag over-consumes and
// swallows the trailing positional prompt, leaving the child with no prompt.
func TestParseArgs_PluginDirSingleValue(t *testing.T) {
	o := parseArgs([]string{"-p", "--plugin-dir", "/some/dir", "the prompt"})
	if got := o.prompt(); got != "the prompt" {
		t.Fatalf("prompt = %q, want %q (--plugin-dir must consume only its single value, not swallow the positional)", got, "the prompt")
	}
	wantFwd := []string{"--plugin-dir", "/some/dir"}
	if !reflect.DeepEqual(o.forward, wantFwd) {
		t.Fatalf("forward = %v, want %v", o.forward, wantFwd)
	}
}
