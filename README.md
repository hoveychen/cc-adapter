# cc-adapter

Drives the real `claude` binary the way the VS Code "Claude Code" extension does.

`cc-adapter` spawns the real `claude` as a bidirectional stream-json child with `CLAUDE_CODE_ENTRYPOINT=claude-vscode`, registers the IDE tools as an in-process MCP server, and emits a `claude_launched` telemetry event on session start — so from Anthropic's backend the traffic is attributed to `claude_code_vscode`, identical to a real VS Code session.

Two ways to use it:

- **As a CLI**: run `cc-adapter "prompt"` directly; it acts as the host and starts a vscode session itself.
- **As the `claude` binary for the official Claude Agent SDK**: point the SDK's executable path at `cc-adapter`, and the SDK drives it exactly as it would the real claude — while the upstream session stays a full vscode session.

> Internals (control protocol, bidirectional relay, traffic fingerprint, A1–A6 replication) are in [docs/OVERVIEW.md](docs/OVERVIEW.md) and [docs/reverse-engineering.md](docs/reverse-engineering.md).

## Build

```bash
go build -o cc-adapter .
```

Real `claude` binary resolution order: `-claude-bin` > `$CLAUDE_REAL_BIN` > `claude` on `PATH`. The VS Code extension ships a ready-to-use one at:
`~/.vscode/extensions/anthropic.claude-code-*/resources/native-binary/claude`.

## Integrate: drive it with the official Claude Agent SDK

Point the SDK's executable path at `cc-adapter` and use `env` to tell it where the real claude is (`CLAUDE_REAL_BIN`). The SDK needs no other changes; the attribution and IDE fingerprint are all in place.

```python
# Python — claude-agent-sdk
import os
from claude_agent_sdk import query, ClaudeAgentOptions

opts = ClaudeAgentOptions(
    cli_path="/path/to/cc-adapter",
    env={**os.environ, "CLAUDE_REAL_BIN": "/abs/path/to/real/claude"},
)
async for msg in query(prompt="...", options=opts):
    ...
```

```js
// TypeScript — @anthropic-ai/claude-agent-sdk
import { query } from "@anthropic-ai/claude-agent-sdk";

for await (const msg of query({
  prompt: "...",
  options: {
    pathToClaudeCodeExecutable: "/path/to/cc-adapter",
    env: { ...process.env, CLAUDE_REAL_BIN: "/abs/path/to/real/claude" },
  },
})) {
  // ...
}
```

Multi-turn conversations, tool calls, the SDK's own in-process MCP servers, and hooks all work unchanged; the `mcp__ide__*` tools and the `ide` / `claude-vscode` servers show up `connected` in the session. Verified end-to-end with the Python (0.2.87) and TypeScript (0.3.153) official SDKs.

## Integrate: use it as a CLI

```bash
# One-shot prompt: send, print the result, exit
./cc-adapter "fix the bug in main.go"

# Interactive REPL: one stdin line per user turn
./cc-adapter

# claude -p compatible surface (upstream is always a full vscode session)
./cc-adapter -p "summarize"
./cc-adapter -p --output-format json "..."
./cc-adapter -p --model claude-opus-4-8 --permission-mode plan "..."

# Replicate the extension's functional requests (needs logged-in OAuth credentials)
./cc-adapter usage | profile | sessions | session <id> | voice
```

Session-style usage forwards any claude session flag (`--model`, `--add-dir`, `--system-prompt`, `--permission-mode`, …) verbatim to the child.

## Flags

| flag | effect |
|---|---|
| `-claude-bin <path>` | specify the real claude binary |
| `-model <m>` | forward `--model` to claude |
| `-no-ide` | don't start the IDE side channel (billing attribution still applies) |
| `-deny-writes` | deny write tools (Write/Edit/MultiEdit/NotebookEdit/Bash) |
| `-no-telemetry` | disable the A1 failure telemetry (event_logging) |

Permissions default to allow-all (headless automation with no human to prompt); `-deny-writes` is the conservative switch.

## Versioning: pinned to a claude CLI version

To stay a faithful `claude` stand-in, cc-adapter must split the positional prompt from flag values *exactly* as the real claude does — which means knowing each flag's arity (how many tokens it consumes). claude has many value-taking flags hidden from `--help` (`--thinking`, `--max-turns`, `--system-prompt-file`, …), so the arity table is **not** hand-maintained: it is generated from one specific claude version's own argument parser, and each release is pinned to that version.

- A cc-adapter release `vX.Y.Z` targets **claude CLI `X.Y.Z`** (e.g. `v2.1.141` mirrors claude `2.1.141`). When a newer claude has the same flag arity as the pinned table, that release tags the same commit, so `PinnedClaudeVersion` may legitimately trail the release tag.
- [`flags_gen.go`](flags_gen.go) holds the generated arity tables and `PinnedClaudeVersion`. It is produced by [`cmd/genclaudeflags`](cmd/genclaudeflags/main.go), which reads documented flags from `claude --help` and discovers/classifies the hidden ones by probing `claude -p --flag` — zero guessing, the source of truth is claude's own parser.
- The [`pin-claude-flags`](.github/workflows/pin-claude-flags.yml) workflow runs daily: it tracks the latest claude release, regenerates the table, and either commits the new table (when arity changed) or just tags the current commit (when only the version differs).

Regenerate when bumping the pinned claude:

```bash
go generate ./...   # runs cmd/genclaudeflags against the claude on PATH, rewrites flags_gen.go
```
