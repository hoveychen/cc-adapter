# Claude Code VS Code 扩展逆向笔记（v2.1.156, darwin-arm64）

逆向对象：
- `~/.vscode/extensions/anthropic.claude-code-2.1.156-darwin-arm64/extension.js`（扩展 + 内嵌 Claude Agent SDK，单文件 minified）
- `~/.vscode/extensions/anthropic.claude-code-2.1.156-darwin-arm64/resources/native-binary/claude`（205MB Mach-O arm64，Bun 编译的原生 CLI）

核心结论：扩展**仍然内部启动一个 `claude` 进程**，但用的是**自带的原生二进制**，由内嵌的 **Claude Agent SDK (TS) v0.3.156** 驱动的 **双向 stream-json** 会话，而非 `claude -p`。

---

## 1. 进程启动与命令行参数

### 1.1 启动的是什么

默认 spawn 扩展自带的 `resources/native-binary/claude`（或新版按平台分目录 `resources/native-binaries/<platform>-<arch>/claude`），**不是**用户全局 PATH 上的 claude。解析优先级（`resolveClaudeBinary()`）：

1. `claudeCode.claudeProcessWrapper` 配置 → 直接顶替可执行体
2. 自带 native binary（按 `<platform>-<arch>` 或旧式单目录）
3. 回退 `resources/claude-code/cli.js` + `process.execPath`（Electron node）

只有 `claudeProcessWrapper` 与终端模式例外才会脱离自带 binary。

### 1.2 spawn

SDK transport 的 `spawnLocalProcess`：

```js
ro.spawn(V, N, {cwd:B, stdio:["pipe","pipe", debug?"pipe":"ignore"], signal:x, env:K, windowsHide:!0})
```

- `child_process.spawn`，stdin/stdout 恒 pipe；stderr 仅 `DEBUG_CLAUDE_AGENT_SDK` 或调用方传 `stderr` 回调时才 pipe，否则 ignore。
- signal 来自会话 AbortController。

### 1.3 完整命令行 flag

基线四件套恒定：

```js
let p=["--output-format","stream-json","--verbose","--input-format","stream-json"];
```

按条件追加（节选，完整见下表）：

| Flag | 触发条件 |
|---|---|
| `--permission-prompt-tool stdio` | 调用方提供 `canUseTool` 回调（与 `permissionPromptToolName` 互斥） |
| `--permission-mode <m>` | 设了 permissionMode |
| `--model` / `--fallback-model` | 设了 model |
| `--thinking adaptive` / `--max-thinking-tokens <n>` / `--thinking disabled` | thinkingConfig |
| `--effort <e>` / `--max-turns <n>` / `--max-budget-usd <usd>` / `--task-budget <n>` | 对应选项 |
| `--mcp-config <json>` / `--strict-mcp-config` / `--setting-sources=<csv>` | MCP / settings |
| `--add-dir <d>`（可重复） / `--plugin-dir <p>`（可重复） | 目录/插件 |
| `--session-id <uuid>` / `--resume <id>` / `--continue` / `--fork-session` / `--no-session-persistence` | 会话 |
| `--include-partial-messages` | 开启 stream_event 增量 |
| `--allowedTools/--disallowedTools/--tools <csv>` | 工具过滤 |

**关键：`--system-prompt` / `--append-system-prompt` 不走命令行**，由 stdin 的 `initialize` control 消息下发。

### 1.4 终端模式（`claudeCode.useTerminal=true`）

退化为在 VS Code 集成终端里敲字面量 `claude`（靠 PATH），不带 stream-json 协议 flag。优先 `shellIntegration.executeCommand`，退化 `sendText`。与默认 SDK 模式完全不同的代码路径。

---

## 2. 环境变量

### 2.1 扩展主动设置（`FV()` 函数）

```js
function FV(z){
  let V=BV0(_1("environmentVariables")), N={...process.env};
  if(z) N.PATH=z;
  N.MCP_CONNECTION_NONBLOCKING="true", N.CLAUDE_CODE_ENABLE_TASKS="0";
  for(let B of V) if(B.name) N[B.name]=B.value||"";
  return N.CLAUDE_CODE_ENTRYPOINT="claude-vscode", N;
}
```

| 变量 | 值 | 说明 |
|---|---|---|
| `CLAUDE_CODE_ENTRYPOINT` | `claude-vscode` | **归因核心**，最后硬写，用户不可覆盖 |
| `CLAUDE_CODE_ENABLE_TASKS` | `0` | 扩展强制关闭 tasks |
| `MCP_CONNECTION_NONBLOCKING` | `true` | |
| `PATH` | 登录 shell 解析值 | |

### 2.2 SDK 层补充

| 变量 | 值/条件 |
|---|---|
| `CLAUDE_AGENT_SDK_VERSION` | `0.3.156`（仅未设时） |
| `CLAUDE_CODE_ENTRYPOINT` | `sdk-ts`（仅未设时；扩展已设 claude-vscode 故不触发） |
| `CLAUDE_CODE_SDK_HAS_OAUTH_REFRESH` / `..._HAS_HOST_AUTH_REFRESH` | `1`，当宿主提供刷新回调 |
| `NODE_OPTIONS` | **spawn 前删除** |
| `DEBUG` | 由 `DEBUG_CLAUDE_AGENT_SDK` 派生 |

### 2.3 IDE / 认证 / 云

- `CLAUDE_CODE_SSE_PORT`：通过 `environmentVariableCollection.replace` 注入**集成终端** env（非 spawn env），见第 3 节。
- 认证靠共享 `CLAUDE_CONFIG_DIR`（`~/.claude`），不传 token；win32 额外同步 `CLAUDE_SECURESTORAGE_CONFIG_DIR`。
- 云开关 `CLAUDE_CODE_USE_BEDROCK/VERTEX/FOUNDRY/MANTLE/ANTHROPIC_AWS` 任一 truthy → 认证判为 `3p`。

---

## 3. IDE 集成协议

IDE 工具有**两套**到达机制，对应扩展的两种运行模式：

- **默认（webview）模式 —— control 通道 in-process MCP（cc-adapter 采用）。** IDE 那 12 个工具被注册为**进程内 SDK MCP server**：`initialize` 握手里声明 `sdkMcpServers:["ide"]`，CLI 据此把它们暴露为 `mcp__ide__*`，调用时不经任何 socket，而是把 JSON-RPC 包进 stream-json **control 通道的 `mcp_message` 帧**隧道往返。形状：
  - 入站（CLI→host）：`{"type":"control_request","request_id":"<id>","request":{"subtype":"mcp_message","server_name":"ide","message":<jsonrpc request>}}`
  - host 把 `message`（含 method+id 的 JSON-RPC 请求）交给 in-process MCP server 处理，得到 JSON-RPC response
  - 回执（host→CLI）：`{"type":"control_response","response":{"subtype":"success","request_id":"<id>","response":{"mcp_response":<jsonrpc response>}}}` —— 关键：响应包在 `response.mcp_response` 字段里
  - 通知（无 id）：回 `{"mcp_response":{"jsonrpc":"2.0","result":{},"id":0}}`
- **终端模式 / 外部 CLI 回连 —— 本地 WebSocket + lockfile + SSE_PORT（cc-adapter 不用）。** 下面 3.1–3.4 描述的就是这条路：env 名叫 SSE_PORT 但 `transport:"ws"`，**扩展是 server，外部 CLI 是 client**。它适用于用户在集成终端里跑独立 `claude` 时让其回连扩展，**不是** webview 模式下扩展自己 spawn 的 SDK 子进程的工具到达方式。

### 3.0 终端模式 WebSocket 侧信道（仅供对照）

### 3.1 lockfile + 端口发现

- 端口：随机 `[10000,65535]`，仅绑 `127.0.0.1`。
- lockfile：`~/.claude/ide/<port>.lock`，mode 0600，JSON：
  ```json
  {"pid":N,"workspaceFolders":[...],"ideName":"...","transport":"ws","runningInWindows":false,"authToken":"<uuid>"}
  ```
- 端口通过 `CLAUDE_CODE_SSE_PORT` 交给 CLI；CLI 读 lockfile 拿 `authToken`。

### 3.2 鉴权握手

扩展生成 `authToken=randomUUID()`。CLI 连 WebSocket 时带 header `x-claude-code-ide-authorization: <authToken>`，不匹配 close(1008)。

### 3.3 回连 gate（二进制实测表达式）

```js
if(d8())return;                       // 前置 gate 1
if(k7()&&!K)return;                    // k7()=后台(bg)模式
if(!((I_().autoConnectIde||OL()||process.env.CLAUDE_CODE_SSE_PORT||xH(process.env.CLAUDE_CODE_AUTO_CONNECT_IDE))
     &&!VK(process.env.CLAUDE_CODE_AUTO_CONNECT_IDE)))return;
```

gate 主体**只看 env，不看 TTY**：设了 `CLAUDE_CODE_SSE_PORT` 即放行。这就是为什么 `cc-adapter` 注入 SSE_PORT + AUTO_CONNECT_IDE=true 即可触发回连。

### 3.4 12 个 IDE 工具（`mcp__ide__*`）

`openDiff`、`getDiagnostics`、`getOpenEditors`、`getWorkspaceFolders`、`getCurrentSelection`、`getLatestSelection`、`openFile`、`close_tab`、`closeAllDiffTabs`、`checkDocumentDirty`、`saveDocument`、`executeCode`。schema 见 `internal/ide/tools.go`。

server→client 推送通知：`selection_changed`、`diagnostics_changed`、`at_mentioned`、`ide_connected`。

openDiff 的 accept：`vscode.diff` 打开 tab，等用户 accept → 返回 `["FILE_SAVED", <内容>]`，reject → `DIFF_REJECTED`。

---

## 4. 计费归因接口

### 4.1 `p2()` —— source 归一化

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

### 4.2 归因载体 —— 确定结论

`p2()` 的返回值**唯一去向是 HTTP 请求头 `anthropic-client-platform`**（不是 body metadata）。主请求头块：

```js
{ "x-app": k7()?"cli-bg":"cli",
  "User-Agent": ei(),                       // claude-cli/2.1.156 (external, <entrypoint>, agent-sdk/<v>, ...)
  "anthropic-client-platform": p2(),        // ← 核心归因字段 claude_code_vscode / claude_code_cli
  "X-Claude-Code-Session-Id": v_(), ... }
```

### 4.3 服务端可用于区分 vscode / cli 的信号

| 信号 | vscode 值 | cli 值 | 可靠性 |
|---|---|---|---|
| `anthropic-client-platform` 头 | `claude_code_vscode` | `claude_code_cli` | ★★★ 最可靠，覆盖所有端点 |
| `User-Agent`（`ei()`，含原始 entrypoint） | `...(external, claude-vscode, ...)` | `...(external, cli, ...)` | ★★★ |
| `x-app` 头 | spawn 的主流量仍是 `cli`/`cli-bg` | `cli`/`cli-bg` | ★ 不能单靠它区分 |
| event_logging 遥测 `client_type`/`entrypoint` | `claude-vscode`（仅扩展进程 POST） | — | ★★ 非 API 流量 |
| OTel 属性 `entrypoint` | `claude-vscode` | `cli` | ★★ 需客户配 OTLP endpoint |

### 4.4 上报端点

- 扩展自身：`POST https://api.anthropic.com/api/event_logging/v2/batch`（含 `entrypoint:"claude-vscode"`、`client_type:"claude-vscode"`）；语音 `wss://api.anthropic.com/.../voice_stream`（`x-app:"vscode"`）。
- CLI 内建：`/v1/messages` 等主 API；Datadog 日志 `http-intake.logs.us5.datadoghq.com`；OTLP traces/logs/metrics（`OTEL_*` 配置，默认不导出，metrics 含 `claude_code.token.usage` / `claude_code.cost.usage`）。

**给重新实现者：只要 spawn claude 时设 `CLAUDE_CODE_ENTRYPOINT=claude-vscode`，主 API 流量的 `anthropic-client-platform` 头与 User-Agent 即自动变为 vscode 归因。** 注意这是启动时 env，可人为覆盖，非防篡指纹。

---

## 5. stream-json 双向协议 + 认证

线协议是 NDJSON（每行一个 JSON 对象）。序列化 = `JSON.stringify(x)+"\n"`。

### 5.1 写给 stdin 的 user 消息

```json
{"type":"user","session_id":"","parent_tool_use_id":null,"message":{"role":"user","content":"..."}}
```

`content` 可为字符串或 ContentBlock 数组（tool_result 也走这里）。

### 5.2 从 stdout 读的消息类型

| type | 字段 |
|---|---|
| `assistant` / `user` | `{message:<Anthropic Message>, uuid, session_id, parent_tool_use_id}` |
| `result` | `{subtype:success/error_*, num_turns, total_cost_usd, result, is_error, session_id}` |
| `system` | `{subtype:init/..., tools, mcp_servers, cwd, model, permissionMode, apiKeySource}` |
| `stream_event` | 增量（仅 `--include-partial-messages`） |
| `control_request` / `control_response` / `control_cancel_request` | 控制通道 |
| `keep_alive` | 心跳，忽略 |

### 5.3 control 通道

出站（host→CLI）：`{type:"control_request", request_id, request:{subtype, ...}}`，subtype 含 `initialize`、`interrupt`、`set_permission_mode`、`set_model`、`mcp_message` 等。

入站（CLI→host）subtype：`can_use_tool`、`hook_callback`、`mcp_message`、`elicitation`、`oauth_token_refresh`、`host_auth_token_refresh`。

#### can_use_tool（`--permission-prompt-tool stdio`）

请求：
```json
{"type":"control_request","request_id":"<id>","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{...},"tool_use_id":"toolu_..","permission_suggestions":[...]}}
```

响应（包进 control_response，即使 deny 其 subtype 仍是 success）：
```json
{"type":"control_response","response":{"subtype":"success","request_id":"<id>","response":{"behavior":"allow","updatedInput":{...},"toolUseID":"toolu_.."}}}
```
behavior ∈ `allow` / `deny` / `ask` / `passthrough`。处理本身失败才回 `subtype:"error"`。

### 5.4 initialize 握手

spawn 后立即发 `control_request{subtype:"initialize", hooks, sdkMcpServers, systemPrompt, appendSystemPrompt, agents, skills, ...}`，等回执。CLI 回的 `system/init` 消息报告生效后的 tools/mcp_servers/permissionMode/model 等。

### 5.5 认证

- 优先 env `ANTHROPIC_API_KEY` / `CLAUDE_CODE_OAUTH_TOKEN`。
- 否则 macOS Keychain：service `Claude Code-credentials`，account=`$USER`（`security find-generic-password`）。
- 磁盘：`~/.claude/.credentials.json`（`{accessToken, refreshToken, expiresAt, scopes}`）。
- refresh：宿主提供回调时 CLI 发 `oauth_token_refresh` control 请求；否则 CLI 自己 `POST /v1/oauth/token`（`anthropic-beta: oauth-2025-04-20`，公开 CLIENT_ID `9d1c250a-e61b-44d9-88ed-5944d1962f5e`）。
- 扩展本身**不用** VS Code SecretStorage；凭据由打包进二进制/SDK 的同一套 auth 代码管理。

---

*本文档为只读逆向研究产出，所有形状/字符串均来自对上述两个文件的实测提取。*
