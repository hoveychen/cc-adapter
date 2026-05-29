# cc-adapter Overview (Architecture / Traffic Fingerprint / Usage)

## What It Is

`cc-adapter` is a Go program that drives the real `claude` binary as a stream-json child process and **faithfully reproduces every network interaction the VS Code "Claude Code" extension (v2.1.156) makes against Anthropic**. The goal: as seen from Anthropic's backend, the traffic fingerprint produced by cc-adapter = 100% the genuine VS Code extension.

The genuine extension = the **extension process** (telemetry/feature requests it sends itself) + the **spawned claude child process** (messages + tengu telemetry). cc-adapter reproduces both halves.

## Architecture

```
cc-adapter (Go)
├── main.go                  entry point: host mode + subcommand dispatch
├── internal/streamjson/     stream-json host: spawn claude, read/write loop, control channel
│                            (mcp_message tunnel + active log_event push)
├── internal/ide/            IDE tools MCP server (12 tools) + CommServer (claude-vscode communication server)
├── internal/auth/           OAuth: read credentials/keychain, decode account_uuid from JWT, token refresh (A4)
├── internal/telemetry/      A1 event_logging anonymous telemetry (ClaudeCodeInternalEvent)
├── internal/cloud/          A2 usage / A3 profile / A6 sessions (+3 derived endpoints) oauth GET
└── internal/voice/          A5 voice_stream WebSocket client
```

**Two-sided design**: upstream (the real claude + Anthropic backend) always sees a complete VS Code webview session; downstream (the caller) sees a `claude -p` compatible interface. The mismatch between the two sides is exactly the adapter's value—bridging cheap `claude -p` usage onto a full VS Code session.

The child process is **always** brought up in VS Code's default **webview mode** (no matter what downstream passes, it is never `claude -p`):
```
claude --output-format stream-json --input-format stream-json --verbose --permission-prompt-tool stdio
```
Injected env: `CLAUDE_CODE_ENTRYPOINT=claude-vscode` (the core of billing attribution), `MCP_CONNECTION_NONBLOCKING=true`, `CLAUDE_CODE_ENABLE_TASKS=0`, `CLAUDE_AGENT_SDK_VERSION=0.3.156`, and removes `NODE_OPTIONS`.

Downstream's `-p` / `--output-format` / `--input-format` only decide how the adapter talks to the **caller**: the adapter re-presents the child process's stream-json frames to downstream through the host's RawSink—`text` prints the final result text, `json` forwards the result frame, `stream-json` forwards frame by frame (filtering out control-channel frames so it matches the real `claude -p`). Apart from the I/O-class and management-class flags, all other claude session flags (`--model`, `--add-dir`, `--system-prompt`, `--permission-mode`, etc.) are forwarded verbatim to the child process, without needing a per-flag allowlist.

**SDK-driven relay mode**: when both downstream in/out are `stream-json` (i.e. the official Claude Agent SDK's bidirectional control-protocol transport), the adapter no longer does the one-way `claude -p` presentation above, but instead enters the **bidirectional control relay** in `relay.go`: it faithfully forwards every frame between the SDK (parent) and the real claude (child), and merges the SDK's `initialize` with cc-adapter's in-process `ide` + `claude-vscode` servers, routes `mcp_message` by server_name, and injects `claude_launched`, so that the SDK can drive cc-adapter as claude while upstream is still a complete vscode session. Two protocol details: `-v`/`--version` is passed through to the real claude (SDK version pre-check), and claude's stdin is kept open until the `result` frame (the control protocol requires it open throughout). See the README section "Driving with the official Claude Agent SDK" for details.

## Traffic Fingerprint Comparison (cc-adapter vs the genuine extension)

| Request | Genuine extension trigger | cc-adapter reproduction | Verification |
|---|---|---|---|
| **messages + tengu telemetry** | Child process always sends | spawn entrypoint=claude-vscode → `anthropic-client-platform: claude_code_vscode`, UA `claude-cli/2.1.156 (external, claude-vscode, agent-sdk/0.3.156)` | Byte-for-byte capture in real environment ✓ |
| **log_event** (claude_launched, etc.) | MCP notification at session start | Registers the `claude-vscode` communication server, sends a log_event notification at session start → child process folds it into `tengu_vscode_<name>` | smoke delivery acked ✓ |
| **IDE tools** (mcp__ide__*) | sdkMcpServer | in-process MCP server, round-tripped via the control-channel mcp_message | smoke `ide connected` ✓ |
| **A1** event_logging | spawn failure / abnormal child-process exit | `internal/telemetry` anonymous POST (no auth, x-service-name:claude-code, gated by DISABLE_*) | unit test ✓ |
| **A2** usage | User views usage | `cc-adapter usage` | Real call to API ✓ |
| **A3** profile | teleport fetches org | `cc-adapter profile` | unit test ✓ |
| **A4** oauth refresh | token expires | `internal/auth` auto-refresh (platform.claude.com) | unit test ✓ |
| **A5** voice WS | Voice input | `cc-adapter voice` (stdin PCM 16k mono) | unit test + fake WS handshake ✓ |
| **A6** sessions + 3 derived | remote/teleport | `cc-adapter sessions` / `session <id>` / `teleport-events <id>` / `session-ingress <id>` | unit test (incl. pagination aggregation) ✓ |

**UA fingerprint**: the child process's main-traffic UA carries `claude-vscode` (injected by the claude binary); the auxiliary requests cc-adapter sends itself (A1-A6) set their UA to `axios/1.9.0`, matching the genuine extension's axios client (does not leak `Go-http-client/1.1`).

**Authentication**: reads `~/.claude/.credentials.json` (`claudeAiOauth.accessToken`) + macOS Keychain `Claude Code-credentials` fallback; `account_uuid` is decoded from the JWT payload `.sub`.

## Usage

```bash
go build -o cc-adapter .
export CLAUDE_REAL_BIN=~/.vscode/extensions/anthropic.claude-code-*/resources/native-binary/claude

# Session (default host mode)
./cc-adapter "fix the bug"          # single prompt
./cc-adapter                        # interactive REPL (one turn per stdin line)

# Downstream claude -p compatible surface (upstream is always a full VS Code webview session)
./cc-adapter -p "summarize"                        # -p: print the result then exit
echo "summarize this" | ./cc-adapter -p            # read the prompt from the pipe
./cc-adapter -p --output-format json "..."         # json: forward the result frame (same as claude -p)
./cc-adapter -p --output-format stream-json "..."  # stream-json: forward frame by frame (control-channel frames already filtered)
printf '%s\n' '{"type":"user","message":{"content":"hi"}}' | ./cc-adapter -p --input-format stream-json
./cc-adapter -p --model opus --permission-mode plan "..."  # any claude session flag is forwarded verbatim to the child process

# Reproduce the extension's functional requests (requires logged-in OAuth credentials)
./cc-adapter usage
./cc-adapter profile
./cc-adapter sessions
./cc-adapter session <id>
./cc-adapter teleport-events <id>
./cc-adapter session-ingress <id>
./cc-adapter voice                  # read PCM linear16/16k/mono from stdin
```

The real claude binary resolution order: `-claude-bin` > `$CLAUDE_REAL_BIN` > `claude` on `PATH` (note that an alias does not affect `exec.LookPath`, but do not install cc-adapter itself as the `claude` on PATH, or it will recurse).

**Downstream claude -p flags (the adapter takes these over itself, not forwarded to the child process)**:

| flag | Effect |
|---|---|
| `-p` / `--print` | Non-interactive mode: feed in the prompt, print the result, then exit |
| `--output-format <text\|json\|stream-json>` | Downstream output format (default text, same as claude -p) |
| `--input-format <text\|stream-json>` | Downstream input format: text treats the whole stdin as the prompt; stream-json parses user turns line by line |
| `--include-partial-messages` | Forwarded to the child process to make it emit incremental stream_event frames; pairs with `--output-format stream-json` (verified working) |
| `--replay-user-messages` | Forwarded to the child process to make it echo user frames on stdout for downstream confirmation (verified working) |

All other claude session flags (`--model`, `--add-dir`, `--allowedTools`, `--system-prompt`, `--permission-mode`, `--resume`, `--session-id`…) are **forwarded verbatim** to the child process. When a variadic flag is adjacent to the positional prompt, use `--` to separate them, e.g.: `cc-adapter -p --allowedTools Bash Edit -- "summarize this"`.

**Adapter management flags (not forwarded; both `-x` / `--x` spellings are accepted)**:

| flag | Effect |
|---|---|
| `--no-ide` | Do not register the IDE in-process MCP server |
| `--no-telemetry` | Disable A1 abnormal-exit telemetry |
| `--deny-writes` | Reject write-class tools |
| `--claude-bin <path>` | Specify the real claude binary |

## Verified vs Stub

- **Verified in a real environment / by unit test**: messages attribution, log_event delivery, IDE server connection, A2 real API call, UA alignment, A6 pagination aggregation.
- **Stub / limited**: A5 voice's audio pipeline (headless has no audio source; the command entry point can read PCM from stdin or only verify the handshake); A2/A3/A5/A6 require explicit subcommand triggers in a headless scenario (the genuine extension also only sends them on the corresponding user action).

## Reverse-Engineering Basis

Full reverse-engineering notes are in [reverse-engineering.md](reverse-engineering.md): process startup/flags, environment variables, the IDE protocol, billing attribution (`p2()` → `anthropic-client-platform`), the stream-json protocol, and the list of the extension's active network interactions (A1-A6 + log_event).
