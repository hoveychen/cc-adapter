# Claude Code VS Code Extension Reverse-Engineering Notes (v2.1.156, darwin-arm64)

Reverse-engineering targets:
- `~/.vscode/extensions/anthropic.claude-code-2.1.156-darwin-arm64/extension.js` (extension + bundled Claude Agent SDK, single minified file)
- `~/.vscode/extensions/anthropic.claude-code-2.1.156-darwin-arm64/resources/native-binary/claude` (205MB Mach-O arm64, native CLI compiled with Bun)

Core conclusion: the extension **still internally launches a `claude` process**, but uses a **bundled native binary**, driving a **bidirectional stream-json** session through the embedded **Claude Agent SDK (TS) v0.3.156**, rather than `claude -p`.

---

## 1. Process launch and command-line arguments

### 1.1 What gets launched

By default it spawns the extension's bundled `resources/native-binary/claude` (or, in newer versions, split per platform under `resources/native-binaries/<platform>-<arch>/claude`), **not** the claude on the user's global PATH. Resolution priority (`resolveClaudeBinary()`):

1. `claudeCode.claudeProcessWrapper` config → directly replaces the executable
2. Bundled native binary (by `<platform>-<arch>` or the old single-directory layout)
3. Fallback to `resources/claude-code/cli.js` + `process.execPath` (Electron node)

Only `claudeProcessWrapper` and the terminal-mode exception break away from the bundled binary.

### 1.2 spawn

The SDK transport's `spawnLocalProcess`:

```js
ro.spawn(V, N, {cwd:B, stdio:["pipe","pipe", debug?"pipe":"ignore"], signal:x, env:K, windowsHide:!0})
```

- `child_process.spawn`, with stdin/stdout always piped; stderr is only piped when `DEBUG_CLAUDE_AGENT_SDK` is set or the caller passes a `stderr` callback, otherwise it is ignored.
- The signal comes from the session AbortController.

### 1.3 Full command-line flags

The baseline four are constant:

```js
let p=["--output-format","stream-json","--verbose","--input-format","stream-json"];
```

Appended conditionally (excerpt; see the full table below):

| Flag | Trigger condition |
|---|---|
| `--permission-prompt-tool stdio` | Caller provides a `canUseTool` callback (mutually exclusive with `permissionPromptToolName`) |
| `--permission-mode <m>` | permissionMode is set |
| `--model` / `--fallback-model` | model is set |
| `--thinking adaptive` / `--max-thinking-tokens <n>` / `--thinking disabled` | thinkingConfig |
| `--effort <e>` / `--max-turns <n>` / `--max-budget-usd <usd>` / `--task-budget <n>` | Corresponding option |
| `--mcp-config <json>` / `--strict-mcp-config` / `--setting-sources=<csv>` | MCP / settings |
| `--add-dir <d>` (repeatable) / `--plugin-dir <p>` (repeatable) | Directories / plugins |
| `--session-id <uuid>` / `--resume <id>` / `--continue` / `--fork-session` / `--no-session-persistence` | Session |
| `--include-partial-messages` | Enables stream_event deltas |
| `--allowedTools/--disallowedTools/--tools <csv>` | Tool filtering |

**Key point: `--system-prompt` / `--append-system-prompt` do not go through the command line** — they are delivered via the `initialize` control message on stdin.

### 1.4 Terminal mode (`claudeCode.useTerminal=true`)

Falls back to typing the literal `claude` into the VS Code integrated terminal (relying on PATH), without the stream-json protocol flags. It prefers `shellIntegration.executeCommand` and falls back to `sendText`. This is a completely different code path from the default SDK mode.

---

## 2. Environment variables

### 2.1 Set actively by the extension (the `FV()` function)

```js
function FV(z){
  let V=BV0(_1("environmentVariables")), N={...process.env};
  if(z) N.PATH=z;
  N.MCP_CONNECTION_NONBLOCKING="true", N.CLAUDE_CODE_ENABLE_TASKS="0";
  for(let B of V) if(B.name) N[B.name]=B.value||"";
  return N.CLAUDE_CODE_ENTRYPOINT="claude-vscode", N;
}
```

| Variable | Value | Notes |
|---|---|---|
| `CLAUDE_CODE_ENTRYPOINT` | `claude-vscode` | **The core of attribution**, written last and unconditionally, cannot be overridden by the user |
| `CLAUDE_CODE_ENABLE_TASKS` | `0` | Extension force-disables tasks |
| `MCP_CONNECTION_NONBLOCKING` | `true` | |
| `PATH` | Value resolved by the login shell | |

### 2.2 SDK-layer additions

| Variable | Value/condition |
|---|---|
| `CLAUDE_AGENT_SDK_VERSION` | `0.3.156` (only if unset) |
| `CLAUDE_CODE_ENTRYPOINT` | `sdk-ts` (only if unset; since the extension already set claude-vscode, this is not triggered) |
| `CLAUDE_CODE_SDK_HAS_OAUTH_REFRESH` / `..._HAS_HOST_AUTH_REFRESH` | `1`, when the host provides a refresh callback |
| `NODE_OPTIONS` | **Deleted before spawn** |
| `DEBUG` | Derived from `DEBUG_CLAUDE_AGENT_SDK` |

### 2.3 IDE / authentication / cloud

- `CLAUDE_CODE_SSE_PORT`: injected into the **integrated terminal** env via `environmentVariableCollection.replace` (not the spawn env); see section 3.
- Authentication relies on a shared `CLAUDE_CONFIG_DIR` (`~/.claude`); no token is passed. On win32 it additionally syncs `CLAUDE_SECURESTORAGE_CONFIG_DIR`.
- If any of the cloud switches `CLAUDE_CODE_USE_BEDROCK/VERTEX/FOUNDRY/MANTLE/ANTHROPIC_AWS` is truthy → authentication is treated as `3p`.

---

## 3. IDE integration protocol

The IDE tools have **two** delivery mechanisms, matching the extension's two run modes:

- **Default (webview) mode — control-channel in-process MCP (used by cc-adapter).** The 12 IDE tools are registered as an **in-process SDK MCP server**: the `initialize` handshake declares `sdkMcpServers:["ide"]`, the CLI accordingly exposes them as `mcp__ide__*`, and when invoked they do not go through any socket — instead the JSON-RPC is wrapped into the stream-json **`mcp_message` frame of the control channel** and tunneled round-trip. Shapes:
  - Inbound (CLI→host): `{"type":"control_request","request_id":"<id>","request":{"subtype":"mcp_message","server_name":"ide","message":<jsonrpc request>}}`
  - The host hands the `message` (the JSON-RPC request containing method+id) to the in-process MCP server to process, producing a JSON-RPC response
  - Acknowledgement (host→CLI): `{"type":"control_response","response":{"subtype":"success","request_id":"<id>","response":{"mcp_response":<jsonrpc response>}}}` — key point: the response is wrapped in the `response.mcp_response` field
  - Notifications (no id): reply with `{"mcp_response":{"jsonrpc":"2.0","result":{},"id":0}}`
- **Terminal mode / external CLI reconnect — local WebSocket + lockfile + SSE_PORT (not used by cc-adapter).** Sections 3.1–3.4 below describe this path: the env var is named SSE_PORT but `transport:"ws"`, and **the extension is the server, the external CLI is the client**. This applies when the user runs a standalone `claude` in the integrated terminal and wants it to reconnect to the extension — it is **not** the tool-delivery mechanism for the SDK child process the extension itself spawns in webview mode.

### 3.0 Terminal-mode WebSocket side channel (for reference only)

### 3.1 lockfile + port discovery

- Port: random in `[10000,65535]`, bound only to `127.0.0.1`.
- lockfile: `~/.claude/ide/<port>.lock`, mode 0600, JSON:
  ```json
  {"pid":N,"workspaceFolders":[...],"ideName":"...","transport":"ws","runningInWindows":false,"authToken":"<uuid>"}
  ```
- The port is handed to the CLI via `CLAUDE_CODE_SSE_PORT`; the CLI reads the lockfile to obtain `authToken`.

### 3.2 Authentication handshake

The extension generates `authToken=randomUUID()`. When the CLI connects to the WebSocket it carries the header `x-claude-code-ide-authorization: <authToken>`; a mismatch results in close(1008).

### 3.3 Reconnect gate (expression measured from the binary)

```js
if(d8())return;                       // leading gate 1
if(k7()&&!K)return;                    // k7()=background (bg) mode
if(!((I_().autoConnectIde||OL()||process.env.CLAUDE_CODE_SSE_PORT||xH(process.env.CLAUDE_CODE_AUTO_CONNECT_IDE))
     &&!VK(process.env.CLAUDE_CODE_AUTO_CONNECT_IDE)))return;
```

The body of the gate **only looks at the env, not the TTY**: setting `CLAUDE_CODE_SSE_PORT` lets it through. This is why `cc-adapter` can trigger reconnect simply by injecting SSE_PORT + AUTO_CONNECT_IDE=true.

### 3.4 The 12 IDE tools (`mcp__ide__*`)

`openDiff`, `getDiagnostics`, `getOpenEditors`, `getWorkspaceFolders`, `getCurrentSelection`, `getLatestSelection`, `openFile`, `close_tab`, `closeAllDiffTabs`, `checkDocumentDirty`, `saveDocument`, `executeCode`. For the schemas see `internal/ide/tools.go`.

server→client push notifications: `selection_changed`, `diagnostics_changed`, `at_mentioned`, `ide_connected`.

openDiff's accept: `vscode.diff` opens a tab, waits for the user to accept → returns `["FILE_SAVED", <content>]`, reject → `DIFF_REJECTED`.

---

## 4. Billing attribution interface

### 4.1 `p2()` — source normalization

```js
function p2(){switch(process.env.CLAUDE_CODE_ENTRYPOINT){
  case"claude-vscode":return"claude_code_vscode";
  case"remote"|...:return"claude_code_remote";
  case"sdk-cli"|"sdk-ts"|"sdk-py":return"claude_code_sdk";
  case"mcp":return"claude_code_mcp";
  case"claude-code-github-action":return"claude_code_github_action";
  case"local-agent":return"claude_code_local_agent";
  case"claude_in_slack":return"claude_in_slack";
  case"cli":default:return"claude_code_cli";
}}
```

### 4.2 Attribution carrier — confirmed conclusion

The return value of `p2()` **has a single destination: the HTTP request header `anthropic-client-platform`** (not body metadata). The main request-header block:

```js
{ "x-app": k7()?"cli-bg":"cli",
  "User-Agent": ei(),                       // claude-cli/2.1.156 (external, <entrypoint>, agent-sdk/<v>, ...)
  "anthropic-client-platform": p2(),        // ← the core attribution field claude_code_vscode / claude_code_cli
  "X-Claude-Code-Session-Id": v_(), ... }
```

### 4.3 Signals the server can use to distinguish vscode / cli

| Signal | vscode value | cli value | Reliability |
|---|---|---|---|
| `anthropic-client-platform` header | `claude_code_vscode` | `claude_code_cli` | ★★★ Most reliable, covers all endpoints |
| `User-Agent` (`ei()`, contains the original entrypoint) | `...(external, claude-vscode, ...)` | `...(external, cli, ...)` | ★★★ |
| `x-app` header | The main traffic of the spawn is still `cli`/`cli-bg` | `cli`/`cli-bg` | ★ Cannot be used alone to distinguish |
| event_logging telemetry `client_type`/`entrypoint` | `claude-vscode` (only the extension process POSTs) | — | ★★ Non-API traffic |
| OTel attribute `entrypoint` | `claude-vscode` | `cli` | ★★ Requires the customer to configure an OTLP endpoint |

### 4.4 Reporting endpoints

- The extension itself: `POST https://api.anthropic.com/api/event_logging/v2/batch` (contains `entrypoint:"claude-vscode"`, `client_type:"claude-vscode"`); voice `wss://api.anthropic.com/.../voice_stream` (`x-app:"vscode"`).
- Built into the CLI: the main API `/v1/messages` etc.; Datadog logs `http-intake.logs.us5.datadoghq.com`; OTLP traces/logs/metrics (`OTEL_*` config, not exported by default, with metrics including `claude_code.token.usage` / `claude_code.cost.usage`).

**For re-implementers: as long as you set `CLAUDE_CODE_ENTRYPOINT=claude-vscode` when spawning claude, the `anthropic-client-platform` header and User-Agent of the main API traffic automatically switch to vscode attribution.** Note that this is a launch-time env var, can be overridden arbitrarily, and is not a tamper-proof fingerprint.

---

## 5. stream-json bidirectional protocol + authentication

The wire protocol is NDJSON (one JSON object per line). Serialization = `JSON.stringify(x)+"\n"`.

### 5.1 The user message written to stdin

```json
{"type":"user","session_id":"","parent_tool_use_id":null,"message":{"role":"user","content":"..."}}
```

`content` can be a string or a ContentBlock array (tool_result also goes through here).

### 5.2 Message types read from stdout

| type | Fields |
|---|---|
| `assistant` / `user` | `{message:<Anthropic Message>, uuid, session_id, parent_tool_use_id}` |
| `result` | `{subtype:success/error_*, num_turns, total_cost_usd, result, is_error, session_id}` |
| `system` | `{subtype:init/..., tools, mcp_servers, cwd, model, permissionMode, apiKeySource}` |
| `stream_event` | Deltas (only with `--include-partial-messages`) |
| `control_request` / `control_response` / `control_cancel_request` | Control channel |
| `keep_alive` | Heartbeat, ignored |

### 5.3 Control channel

Outbound (host→CLI): `{type:"control_request", request_id, request:{subtype, ...}}`, with subtype including `initialize`, `interrupt`, `set_permission_mode`, `set_model`, `mcp_message`, etc.

Inbound (CLI→host) subtypes: `can_use_tool`, `hook_callback`, `mcp_message`, `elicitation`, `oauth_token_refresh`, `host_auth_token_refresh`.

#### can_use_tool (`--permission-prompt-tool stdio`)

Request:
```json
{"type":"control_request","request_id":"<id>","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{...},"tool_use_id":"toolu_..","permission_suggestions":[...]}}
```

Response (wrapped in control_response; even a deny still has subtype success):
```json
{"type":"control_response","response":{"subtype":"success","request_id":"<id>","response":{"behavior":"allow","updatedInput":{...},"toolUseID":"toolu_.."}}}
```
behavior ∈ `allow` / `deny` / `ask` / `passthrough`. Only a failure in the processing itself returns `subtype:"error"`.

### 5.4 initialize handshake

Immediately after spawn, send `control_request{subtype:"initialize", hooks, sdkMcpServers, systemPrompt, appendSystemPrompt, agents, skills, ...}` and wait for the acknowledgement. The `system/init` message the CLI returns reports the effective tools/mcp_servers/permissionMode/model etc.

### 5.5 Authentication

- Prefers the env vars `ANTHROPIC_API_KEY` / `CLAUDE_CODE_OAUTH_TOKEN`.
- Otherwise the macOS Keychain: service `Claude Code-credentials`, account=`$USER` (`security find-generic-password`).
- Disk: `~/.claude/.credentials.json` (`{accessToken, refreshToken, expiresAt, scopes}`).
- refresh: when the host provides a callback, the CLI sends an `oauth_token_refresh` control request; otherwise the CLI does its own `POST /v1/oauth/token` (`anthropic-beta: oauth-2025-04-20`, public CLIENT_ID `9d1c250a-e61b-44d9-88ed-5944d1962f5e`).
- The extension itself **does not use** VS Code SecretStorage; credentials are managed by the same auth code packaged into the binary/SDK.

---

*This document is read-only reverse-engineering research output; all shapes/strings are extracted from real measurements of the two files above.*
