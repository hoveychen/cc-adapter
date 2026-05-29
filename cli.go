package main

import "strings"

// cliOpts is the parsed cc-adapter command line. The design goal: the adapter
// presents a `claude -p`-compatible surface downstream while always driving the
// real claude in VS Code webview stream-json mode upstream. So we intercept only
// the flags whose meaning is "how the adapter talks to the *downstream* caller"
// (the I/O-format flags) plus the adapter's own management flags, and forward
// every other flag verbatim to the child claude — no per-flag validity
// allowlist, so claude's future session flags align automatically.
type cliOpts struct {
	// Adapter management flags (consumed, never forwarded).
	claudeBin   string
	noIDE       bool
	noTelemetry bool
	denyWrites  bool

	// Downstream I/O flags — the `claude -p` surface (consumed, never forwarded;
	// the child is hardwired to stream-json regardless).
	print              bool
	outputFormat       string // "" (=> text) | "text" | "json" | "stream-json"
	inputFormat        string // "" (=> text) | "text" | "stream-json"
	includePartial     bool
	replayUserMessages bool

	// Flags forwarded verbatim to the child claude (session configuration:
	// --model, --add-dir, --allowedTools, --system-prompt, ...).
	forward []string

	// Positional prompt words (joined with spaces). Not forwarded — the child
	// receives the prompt over stdin as a stream-json user turn.
	promptParts []string
}

// prompt returns the positional prompt, or "" if none was given.
func (o cliOpts) prompt() string { return strings.Join(o.promptParts, " ") }

// Flag arity tables for forwarded claude flags. We don't validate which flags
// exist; we only need to know how many tokens each consumes so the positional
// prompt is separated from flag values correctly. Unknown leading-dash tokens
// are treated as boolean (forward the flag alone) — a future value-taking flag
// would need a one-line addition here, but boolean flags align automatically.
var (
	// Single-value: consume exactly the next token as the flag's value.
	forwardSingleValue = map[string]bool{
		"--agent": true, "--agents": true, "--append-system-prompt": true,
		"--system-prompt": true, "--debug-file": true, "--effort": true,
		"--fallback-model": true, "--json-schema": true, "--max-budget-usd": true,
		"--model": true, "--name": true, "-n": true, "--permission-mode": true,
		"--remote-control-session-name-prefix": true, "--session-id": true,
		"--setting-sources": true, "--settings": true,
	}
	// Variadic: consume following tokens until the next leading-dash token.
	forwardVariadic = map[string]bool{
		"--add-dir": true, "--allowedTools": true, "--allowed-tools": true,
		"--betas": true, "--disallowedTools": true, "--disallowed-tools": true,
		"--file": true, "--mcp-config": true, "--plugin-dir": true,
		"--plugin-url": true, "--tools": true,
	}
	// Optional-value: consume the next token only if it is not a leading-dash flag.
	forwardOptionalValue = map[string]bool{
		"-d": true, "--debug": true, "--from-pr": true, "--remote-control": true,
		"-r": true, "--resume": true, "-w": true, "--worktree": true,
	}
)

func isFlag(s string) bool { return len(s) > 1 && s[0] == '-' }

// normalizeManagement maps the historical single-dash management flag spellings
// (-no-ide, -claude-bin, ...) to a canonical double-dash key so both forms work.
func managementKey(s string) string {
	switch s {
	case "-no-ide", "--no-ide":
		return "--no-ide"
	case "-no-telemetry", "--no-telemetry":
		return "--no-telemetry"
	case "-deny-writes", "--deny-writes":
		return "--deny-writes"
	case "-claude-bin", "--claude-bin":
		return "--claude-bin"
	}
	return ""
}

// parseArgs parses cc-adapter's argv (excluding the program name). It never
// errors: anything it doesn't recognise as adapter-owned is forwarded to the
// child, mirroring claude's own permissive forwarding.
func parseArgs(args []string) cliOpts {
	var o cliOpts
	i := 0
	for i < len(args) {
		a := args[i]

		// End-of-options separator: everything after a bare "--" is the prompt.
		// This is the unambiguous escape hatch for the case where a variadic flag
		// (e.g. --allowedTools) would otherwise swallow the trailing positional
		// prompt: `cc-adapter -p --allowedTools Bash Edit -- "do the thing"`.
		if a == "--" {
			o.promptParts = append(o.promptParts, args[i+1:]...)
			break
		}

		// Adapter management flags (both -x and --x spellings).
		if k := managementKey(a); k != "" {
			switch k {
			case "--no-ide":
				o.noIDE = true
			case "--no-telemetry":
				o.noTelemetry = true
			case "--deny-writes":
				o.denyWrites = true
			case "--claude-bin":
				if i+1 < len(args) {
					o.claudeBin = args[i+1]
					i += 2
					continue
				}
			}
			i++
			continue
		}
		// --claude-bin=PATH form.
		if v, ok := splitEq(a, "--claude-bin"); ok {
			o.claudeBin = v
			i++
			continue
		}

		// Adapter-owned downstream I/O flags.
		switch a {
		case "-p", "--print":
			o.print = true
			i++
			continue
		case "--include-partial-messages":
			o.includePartial = true
			i++
			continue
		case "--replay-user-messages":
			o.replayUserMessages = true
			i++
			continue
		case "--output-format":
			if i+1 < len(args) {
				o.outputFormat = args[i+1]
				i += 2
				continue
			}
			i++
			continue
		case "--input-format":
			if i+1 < len(args) {
				o.inputFormat = args[i+1]
				i += 2
				continue
			}
			i++
			continue
		}
		if v, ok := splitEq(a, "--output-format"); ok {
			o.outputFormat = v
			i++
			continue
		}
		if v, ok := splitEq(a, "--input-format"); ok {
			o.inputFormat = v
			i++
			continue
		}

		// Forwarded flags.
		if isFlag(a) {
			// --flag=value: a single self-contained token.
			if strings.HasPrefix(a, "--") && strings.Contains(a, "=") {
				o.forward = append(o.forward, a)
				i++
				continue
			}
			switch {
			case forwardVariadic[a]:
				o.forward = append(o.forward, a)
				i++
				for i < len(args) && !isFlag(args[i]) {
					o.forward = append(o.forward, args[i])
					i++
				}
			case forwardSingleValue[a]:
				o.forward = append(o.forward, a)
				if i+1 < len(args) {
					o.forward = append(o.forward, args[i+1])
					i += 2
				} else {
					i++
				}
			case forwardOptionalValue[a]:
				o.forward = append(o.forward, a)
				i++
				if i < len(args) && !isFlag(args[i]) {
					o.forward = append(o.forward, args[i])
					i++
				}
			default:
				// Unknown flag: assume boolean.
				o.forward = append(o.forward, a)
				i++
			}
			continue
		}

		// Positional: part of the prompt.
		o.promptParts = append(o.promptParts, a)
		i++
	}
	return o
}

// splitEq returns the value of a "--key=value" token when its key matches.
func splitEq(arg, key string) (string, bool) {
	prefix := key + "="
	if strings.HasPrefix(arg, prefix) {
		return arg[len(prefix):], true
	}
	return "", false
}
