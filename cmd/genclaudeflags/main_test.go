package main

import "testing"

// wrappedHelp reproduces the shape of `claude --help` as rendered by claude
// 2.1.156, which uses a fixed narrow help width (it ignores COLUMNS) and wraps
// long descriptions onto deeply-indented continuation lines. Two of those
// continuation lines are the ones that used to break the parser:
//
//   - a wrap that ends in ":" ("...(choices:") — must NOT be mistaken for the
//     end of the Options section (which would drop every later flag), and
//   - a wrap that begins with "--flag" tokens ("--settings, --agents, ...") —
//     must NOT be mistaken for a boolean option definition (which would
//     overwrite the real arity of those flags).
//
// Real option entries sit at a shallow indent; wrapped descriptions sit at the
// deep description column. The parser must key on that indentation.
const wrappedHelp = `Usage: claude [options] [prompt]

Arguments:
  prompt                                The prompt

Options:
  --add-dir <directories...>            Additional directories to allow tool
                                        access to. Provide context via
                                        --append-system-prompt[-file], --add-dir
                                        --settings, --agents, --plugin-dir.
  --input-format <format>               Input format (only works with --print):
                                        (realtime streaming input) (choices:
                                        "text", "stream-json")
  -r, --resume [value]                  Resume a conversation by session ID, or
                                        open interactive picker
  --settings <file-or-json>             Path to a settings JSON file
  -w, --worktree [name]                 Create a new git worktree

Commands:
  agents                                Manage background agents
`

func TestParseHelpOptionsHandlesWrappedDescriptions(t *testing.T) {
	got := map[string]arity{}
	for _, opt := range parseHelpOptions(wrappedHelp) {
		for _, name := range opt.names {
			got[name] = opt.kind
		}
	}

	want := map[string]arity{
		"--add-dir":     arityVariadic, // not clobbered to boolean by the "--settings, --agents" wrap
		"--input-format": aritySingle,
		"-r":            arityOptional, // reached despite the "(choices:" wrap ending in ":"
		"--resume":      arityOptional,
		"--settings":    aritySingle, // reached after the "(choices:" wrap
		"-w":            arityOptional,
		"--worktree":    arityOptional,
	}

	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("flag %s: got arity %d, want %d", name, got[name], kind)
		}
	}

	// The deep-indent wrap line "--settings, --agents, --plugin-dir." must not
	// have introduced a phantom boolean flag for --plugin-dir.
	if _, leaked := got["--plugin-dir."]; leaked {
		t.Errorf("phantom flag --plugin-dir. leaked from a wrapped description line")
	}
}
