// Command genclaudeflags regenerates flags_gen.go from a pinned claude binary.
//
// cc-adapter must split the positional prompt from flag values *exactly* as the
// real claude CLI does — otherwise a value token leaks into the prompt (the
// --thinking bug) or the prompt gets swallowed by a flag (the --plugin-dir bug).
// So the flag-arity table is not a hand-curated guess; it is a faithful mirror of
// one specific claude version's own argument parser. This generator produces that
// mirror and stamps the version it was built from. Re-run via `go generate` when
// bumping the pinned claude.
//
// Method (zero-guess — arity is read from claude's OWN parser, never inferred):
//
//  1. Documented flags: parse `claude --help`. commander prints each option's
//     aliases and metavar (`<x>` single, `<x...>` variadic, `[x]` optional, none
//     boolean) in a column-aligned signature.
//  2. Hidden flags: claude has many top-level flags omitted from --help
//     (--thinking, --max-turns, --system-prompt-file, ...). Discover candidates by
//     scanning the binary for `--flag <metavar>` / `--flag [metavar]` strings,
//     then for each one NOT already documented, probe `claude -p --flag`:
//     - "unknown option" in stderr  => not a top-level flag (a subcommand's or a
//     bundled third-party tool's), drop it.
//     - "option '<spec>' argument missing" => a top-level value flag; the exact
//     metavar in <spec> gives the arity authoritatively.
//     - anything else (parse succeeded) => optional/boolean; use the binary
//     metavar to tell optional (`[x]`) from boolean (skip).
//
// Boolean hidden flags are intentionally never probed (they default correctly in
// the parser and probing them would start a real claude session); only flags that
// carry a metavar in the binary — i.e. the value-taking ones that actually matter
// for arity — are probed.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

type arity int

const (
	arityBoolean  arity = iota // no value token
	aritySingle                // exactly one value token: --flag <x>
	arityVariadic              // consume until next leading-dash token: --flag <x...>
	arityOptional              // one value token only if not a leading-dash flag: --flag [x]
)

// adapterConsumed are flags cc-adapter intercepts before the forward path, so they
// must never land in the forwarded-arity tables even though claude defines them.
var adapterConsumed = map[string]bool{
	"--print": true, "-p": true,
	"--output-format": true, "--input-format": true,
	"--include-partial-messages": true, "--replay-user-messages": true,
}

func main() {
	claudeBin := flag.String("claude", "claude", "path to the claude binary to mirror")
	out := flag.String("out", "flags_gen.go", "output Go file")
	flag.Parse()

	bin, err := exec.LookPath(*claudeBin)
	if err != nil {
		fatalf("locate claude: %v", err)
	}
	version := strings.TrimSpace(firstWord(run(bin, "--version")))
	if version == "" {
		fatalf("could not read `claude --version`")
	}

	flags := map[string]arity{} // canonical flag token -> arity

	// 1. Documented options from --help.
	for _, opt := range parseHelpOptions(run(bin, "--help")) {
		for _, name := range opt.names {
			if !adapterConsumed[name] {
				flags[name] = opt.kind
			}
		}
	}

	// 2. Hidden value-taking flags, discovered from the binary and confirmed by probe.
	for name, metavar := range scanBinaryMetavarFlags(bin) {
		if _, documented := flags[name]; documented || adapterConsumed[name] {
			continue
		}
		spec, res := probe(bin, name)
		switch res {
		case probeUnknown:
			// Not a top-level flag (belongs to a subcommand or a bundled
			// third-party CLI tool); claude -p would never accept it.
			continue
		case probeArgMissing:
			// Required value: read the authoritative arity from claude's own
			// error spec (covers single <x> and variadic <x...>, plus aliases).
			for _, n := range splitAliases(spec) {
				if !adapterConsumed[n] {
					flags[n] = arityFromMetavar(specMetavar(spec))
				}
			}
		case probeAccepted:
			// Parser accepted `claude -p --flag` with no value: a genuine
			// top-level flag that is optional-value or boolean. The binary
			// metavar disambiguates; boolean flags need no entry.
			if strings.HasPrefix(metavar, "[") {
				flags[name] = arityOptional
			}
		}
	}

	render(*out, version, flags)
	fmt.Fprintf(os.Stderr, "genclaudeflags: wrote %s from claude %s (%d forwarded flags)\n", *out, version, len(flags))
}

// helpOption is one parsed `claude --help` option line.
type helpOption struct {
	names []string // all spellings, e.g. ["-n", "--name"]
	kind  arity
}

var (
	// signature column is separated from the description by 2+ spaces; commander
	// never puts 2+ consecutive spaces inside a signature.
	reHelpLine = regexp.MustCompile(`^\s+(--?[A-Za-z][^\n]*?)(?:\s{2,}.*)?$`)
	reMetavar  = regexp.MustCompile(`(<[^>]+>|\[[^\]]+\])\s*$`)
)

func parseHelpOptions(help string) []helpOption {
	var opts []helpOption
	inOptions := false
	for _, line := range strings.Split(help, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "Options:":
			inOptions = true
			continue
		case strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, "-"):
			// "Commands:", "Arguments:", etc. end the options block.
			inOptions = false
			continue
		}
		if !inOptions {
			continue
		}
		m := reHelpLine.FindStringSubmatch(line)
		if m == nil {
			continue // wrapped description continuation
		}
		sig := strings.TrimSpace(m[1])
		kind := arityBoolean
		if mv := reMetavar.FindString(sig); mv != "" {
			kind = arityFromMetavar(strings.TrimSpace(mv))
			sig = strings.TrimSpace(reMetavar.ReplaceAllString(sig, ""))
		}
		names := splitAliases(sig)
		if len(names) == 0 {
			continue
		}
		opts = append(opts, helpOption{names: names, kind: kind})
	}
	return opts
}

// splitAliases extracts the flag spellings from a signature/spec fragment like
// "-n, --name" or "--allowedTools, --allowed-tools".
func splitAliases(s string) []string {
	var names []string
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		tok = strings.TrimSpace(tok)
		if strings.HasPrefix(tok, "-") {
			names = append(names, tok)
		}
	}
	return names
}

// specMetavar returns the metavar of a commander error spec like
// "--thinking <mode>" or "--add-dir <directories...>".
func specMetavar(spec string) string {
	if mv := reMetavar.FindString(strings.TrimSpace(spec)); mv != "" {
		return strings.TrimSpace(mv)
	}
	return ""
}

func arityFromMetavar(mv string) arity {
	switch {
	case mv == "":
		return arityBoolean
	case strings.Contains(mv, "..."):
		return arityVariadic
	case strings.HasPrefix(mv, "["):
		return arityOptional
	default:
		return aritySingle
	}
}

// reBinaryFlag finds `--flag <metavar>` / `--flag [metavar]` option strings in the
// compiled binary. commander stores option definitions as these literal strings.
var reBinaryFlag = regexp.MustCompile(`(--[a-z][a-zA-Z0-9-]+) (<[^>\x00\n]+>|\[[^\]\x00\n]+\])`)

func scanBinaryMetavarFlags(bin string) map[string]string {
	data, err := os.ReadFile(bin)
	if err != nil {
		fatalf("read claude binary: %v", err)
	}
	out := map[string]string{}
	for _, m := range reBinaryFlag.FindAllStringSubmatch(string(data), -1) {
		if _, ok := out[m[1]]; !ok {
			out[m[1]] = m[2]
		}
	}
	return out
}

type probeResult int

const (
	probeUnknown    probeResult = iota // "unknown option": not a top-level flag
	probeArgMissing                    // required value missing: top-level value flag
	probeAccepted                      // parser accepted: top-level optional/boolean
)

var (
	reArgMissing = regexp.MustCompile(`option '([^']+)' argument missing`)
	reUnknownOpt = regexp.MustCompile(`unknown option`)
)

// probe runs `claude -p <flag>` and classifies the flag from commander's stderr.
// For probeArgMissing it also returns the full option spec, whose metavar gives
// the exact arity (single <x> vs variadic <x...>) and any aliases.
func probe(bin, flag string) (string, probeResult) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-p", flag)
	cmd.Stdin = bytes.NewReader(nil) // EOF: never block on stdin
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	_ = cmd.Run()
	s := buf.String()
	if reUnknownOpt.MatchString(s) {
		return "", probeUnknown
	}
	if m := reArgMissing.FindStringSubmatch(s); m != nil {
		return m[1], probeArgMissing
	}
	return "", probeAccepted
}

func render(out, version string, flags map[string]arity) {
	var single, variadic, optional []string
	for name, k := range flags {
		switch k {
		case aritySingle:
			single = append(single, name)
		case arityVariadic:
			variadic = append(variadic, name)
		case arityOptional:
			optional = append(optional, name)
		}
	}
	sort.Strings(single)
	sort.Strings(variadic)
	sort.Strings(optional)

	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by cmd/genclaudeflags from claude %s --help; DO NOT EDIT.\n", version)
	b.WriteString("// Regenerate with `go generate ./...` against a matching claude binary.\n\n")
	b.WriteString("package main\n\n")
	fmt.Fprintf(&b, "// PinnedClaudeVersion is the claude CLI version this arity table mirrors.\n")
	fmt.Fprintf(&b, "// cc-adapter parses argv exactly as this version does; running it against a\n")
	fmt.Fprintf(&b, "// different claude may misclassify flags it added or removed.\n")
	fmt.Fprintf(&b, "const PinnedClaudeVersion = %q\n\n", version)
	writeMap(&b, "forwardSingleValue", "consume exactly the next token as the flag's value.", single)
	writeMap(&b, "forwardVariadic", "consume following tokens until the next leading-dash token.", variadic)
	writeMap(&b, "forwardOptionalValue", "consume the next token only if it is not a leading-dash flag.", optional)

	formatted, err := format.Source(b.Bytes())
	if err != nil {
		fatalf("gofmt generated source: %v\n%s", err, b.String())
	}
	if err := os.WriteFile(out, formatted, 0o644); err != nil {
		fatalf("write %s: %v", out, err)
	}
}

func writeMap(b *bytes.Buffer, name, doc string, keys []string) {
	fmt.Fprintf(b, "// %s: %s\n", name, doc)
	fmt.Fprintf(b, "var %s = map[string]bool{\n", name)
	for _, k := range keys {
		fmt.Fprintf(b, "\t%q: true,\n", k)
	}
	b.WriteString("}\n\n")
}

func run(bin string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = bytes.NewReader(nil)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func firstWord(s string) string {
	if i := strings.IndexAny(strings.TrimSpace(s), " \t\n"); i >= 0 {
		return strings.TrimSpace(s)[:i]
	}
	return strings.TrimSpace(s)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genclaudeflags: "+format+"\n", args...)
	os.Exit(1)
}
