package main

import (
	"reflect"
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
