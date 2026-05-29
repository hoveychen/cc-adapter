# cc-adapter

像 VS Code「Claude Code」扩展一样驱动真实的 `claude` 二进制。

`cc-adapter` **不是**透传壳子。它就是 VS Code 扩展**默认（webview）模式**那套 **stream-json host**：把 IDE 那 12 个工具注册为**进程内 SDK MCP server**，再以 `CLAUDE_CODE_ENTRYPOINT=claude-vscode` 把 `claude` 作为双向 stream-json 子进程拉起来。IDE 的 JSON-RPC 经 stream-json **control 通道的 `mcp_message` 帧**隧道往返。结果是——从 Anthropic 后台看，这条流量被归因为 `claude_code_vscode`，且 IDE 工具（`mcp__ide__*`）可用，和真实 VS Code 会话一致。

> WebSocket + lockfile + `CLAUDE_CODE_SSE_PORT` 回连是 VS Code **终端模式**（外部 CLI 回连扩展 server）的机制，本工具不走那条路，相关代码已移除。

> 本仓库基于对 `anthropic.claude-code` 扩展 v2.1.156（darwin-arm64）的逆向。完整逆向笔记见 [docs/reverse-engineering.md](docs/reverse-engineering.md)。

## 它和 `claude -p` 的区别

| | `claude -p` | cc-adapter（= VS Code 起法） |
|---|---|---|
| 模式 | 一次性 print | `--input-format stream-json` 双向长驻会话 |
| 归因 source | `claude_code_cli` | **`claude_code_vscode`**（`CLAUDE_CODE_ENTRYPOINT=claude-vscode`） |
| 权限 | CLI 自行处理 | `--permission-prompt-tool stdio`，经 control 通道回传给 host |
| IDE 集成 | 无 | 进程内 SDK MCP server，JSON-RPC 经 control `mcp_message` 帧隧道 |

启动 claude 的精确命令（与 VS Code 扩展一致）：

```
claude --output-format stream-json --input-format stream-json --verbose --permission-prompt-tool stdio [--model ...]
```

注入的关键环境变量：`CLAUDE_CODE_ENTRYPOINT=claude-vscode`、`MCP_CONNECTION_NONBLOCKING=true`、`CLAUDE_CODE_ENABLE_TASKS=0`、`CLAUDE_AGENT_SDK_VERSION=0.3.156`，并删除 `NODE_OPTIONS`。（不再注入 `CLAUDE_CODE_SSE_PORT` / `CLAUDE_CODE_AUTO_CONNECT_IDE`——那是终端模式回连用的。）

IDE 集成走的是 control 通道隧道，握手时 host 在 `initialize` 里声明 `sdkMcpServers:["ide"]`，CLI 据此把工具暴露为 `mcp__ide__*`，调用时发 `mcp_message`：

- 入站：`{"type":"control_request","request_id":"<id>","request":{"subtype":"mcp_message","server_name":"ide","message":<jsonrpc request>}}`
- host 把 `message` 交给进程内 MCP server，得到 JSON-RPC response
- 回执：`{"type":"control_response","response":{"subtype":"success","request_id":"<id>","response":{"mcp_response":<jsonrpc response>}}}`

## 用官方 Claude Agent SDK 驱动（SDK-driven relay）

把官方 **Claude Agent SDK** 的可执行文件路径指向 `cc-adapter`，SDK 就能把它当作 `claude` 子进程驱动——而上游真 claude 仍被打成**完整 VS Code 会话**（entrypoint + IDE MCP server + `claude_launched` log_event）。SDK 那边几乎无感，归因却全部变成 `claude_code_vscode`。

```python
# Python（claude-agent-sdk）
from claude_agent_sdk import query, ClaudeAgentOptions
opts = ClaudeAgentOptions(
    cli_path="/path/to/cc-adapter",                  # 让 SDK spawn cc-adapter 而非真 claude
    env={**os.environ, "CLAUDE_REAL_BIN": "/abs/path/to/real/claude"},
)
async for msg in query(prompt="...", options=opts):
    ...
```

TS SDK 同理，把 `pathToClaudeCodeExecutable` 指向 `cc-adapter`、并在 `env` 里给出 `CLAUDE_REAL_BIN`。

**它怎么成立的——双向 control 中继。** SDK 跑的不是 `claude -p` 的单向输出流，而是**双向 stream-json 控制协议**（spawn `claude --input-format stream-json --output-format stream-json`，发 `initialize` 握手、经 control 通道处理权限/MCP）。`cc-adapter` 检测到下游 in/out 都是 `stream-json` 时进入 **relay 模式**，在 SDK（父）与真 claude（子）之间忠实中继每一帧，只做三处干预把上游补成完整 vscode 会话：

1. **initialize 合并**：把 SDK 的 `initialize` 里 `sdkMcpServers` 与 cc-adapter 的 `ide` + `claude-vscode` 进程内 server 求并集后再上送，于是真 claude 暴露 `mcp__ide__*` 并把 `log_event` 折成 `tengu_vscode_*` 遥测。
2. **mcp_message 路由**：claude 发给 `ide` / `claude-vscode` 的 `mcp_message` 由 cc-adapter 进程内应答；其余（SDK 自己注册的 server、`can_use_tool`、`hook_callback`）透传给 SDK 由它处理。
3. **claude_launched**：`system:init` 后注入一次，与插件一致。

其余（user turns、assistant/result 帧、SDK 的 control_response）原样中继。cc-adapter 自发的上游请求（log_event）带 `cca_` 前缀的 request_id，以便识别其 ack 并丢弃而非误转给 SDK。

**两个必须照顾的协议细节**（都已实现，端到端实测坐实）：
- **版本预检**：SDK 开会话前会跑 `<cli> -v`（2 秒超时）解析 `X.Y.Z`。cc-adapter 把 `-v` / `--version` 直接透传给真 claude 并回传其输出，预检才不会挂。
- **stdin 寿命**：因为恒注入进程内 MCP server，control 协议要求 claude 的 stdin 全程打开。SDK 关掉它那侧 stdin 后，cc-adapter **不立即**关 claude 的 stdin，而是等 claude 吐出 `result` 帧再关（镜像 SDK 自己的 `wait_for_result_and_end_input`）。否则 `ide`/`claude-vscode` 会 `failed`、`claude_launched` 也写不进去。

实测（`claude-agent-sdk` 0.2.87 → cc-adapter → 真 claude 2.1.156）：query 正常返回，`system/init` 里 `ide` 与 `claude-vscode` 均 `connected`、SDK 看得到 `mcp__ide__*` 工具、`claude_launched` 投递成功。

## 计费归因 vs IDE 回连——两条独立的线

- **计费归因** 100% 取决于 `CLAUDE_CODE_ENTRYPOINT`，二进制里 `p2()` 把它归一化成 `anthropic-client-platform` 请求头（`claude_code_vscode`）。这条**不需要任何 IDE 回连**就成立。
- **IDE 工具** 决定 claude 能否调用 `mcp__ide__*` 那 12 个工具（选区、诊断、diff）。`cc-adapter` 把它们注册为进程内 SDK MCP server（`initialize` 时声明 `sdkMcpServers:["ide"]`），CLI 调用时发 `mcp_message`，host 处理后经 control 通道回传——无需任何 WebSocket / lockfile / SSE_PORT 回连。headless 场景下编辑器状态由真实文件系统提供（openDiff 自动落盘、选区为空）。

## 插件网络互动复刻（流量指纹 = 100% vscode 插件）

真实 VS Code 插件 = **extension 进程**（自己发的遥测/功能请求）+ **spawn 的 claude 子进程**（messages + tengu 遥测）。`cc-adapter` 把两半都复刻，使 Anthropic 后台看到的请求集与真插件一致。已逆向坐实：插件进程的 HTTP 客户端只有 axios，主动请求就 6 个端点（A1–A6），正常会话遥测走 `log_event` MCP 通知由子进程代发。

| 请求 | 真插件触发 | cc-adapter 复刻方式 | 实现包 |
|---|---|---|---|
| messages + tengu 遥测 | 子进程恒发 | spawn 带 `CLAUDE_CODE_ENTRYPOINT=claude-vscode` → 全部 `claude_code_vscode` 归因 | streamjson host |
| **log_event**（`claude_launched` 等 UI 事件） | 会话启动经 MCP 通知 | 注册 `claude-vscode` 通信 server，会话启动主动发 `log_event` notification → 子进程折成 `tengu_vscode_<name>` | streamjson + `ide.CommServer` |
| **A1** `POST /api/event_logging/v2/batch` | spawn 失败/子进程异常退出 | 同时机发匿名 `ClaudeCodeInternalEvent`（无 auth，`x-service-name:claude-code`，gate 三个 DISABLE_* env） | `internal/telemetry` |
| **A2** `GET /api/oauth/usage` | 用户看用量 | 子命令 `cc-adapter usage` | `internal/cloud` |
| **A3** `GET /api/oauth/profile` | teleport 取 org uuid | 子命令 `cc-adapter profile` | `internal/cloud` |
| **A6** `GET /v1/sessions` | remote/teleport | 子命令 `cc-adapter sessions`（带 `anthropic-beta:ccr-byoc` + `x-organization-uuid`） | `internal/cloud` |
| **A4** `POST platform.claude.com/v1/oauth/token` | access token 过期 | `internal/auth` 在需要 bearer 时自动刷新 | `internal/auth` |
| **A5** `wss://.../voice_stream` | 语音输入 | 子命令 `cc-adapter voice`（从 stdin 读 PCM linear16 16k mono；header `x-app:vscode` + keyterms） | `internal/voice` |

认证：`internal/auth` 读 `~/.claude/.credentials.json`（`claudeAiOauth.accessToken`）+ macOS Keychain `Claude Code-credentials` fallback；`account_uuid` 由 JWT payload `.sub` 解出（带进 A1 遥测 body 的 `auth` 字段）。`-no-telemetry` 可关闭 A1。

## 用法

```bash
go build -o cc-adapter .

# 一次性 prompt：发送、打印结果、退出
./cc-adapter "fix the bug in main.go"

# 交互 REPL：每行 stdin 作为一个 user turn
./cc-adapter

# 透传 flag，并指定真实 claude 二进制
./cc-adapter -model claude-opus-4-8 -claude-bin /path/to/claude "..."

# 复刻插件功能性请求（A2/A3/A6/A5，需已登录的 OAuth 凭据）
./cc-adapter usage      # GET /api/oauth/usage
./cc-adapter profile    # GET /api/oauth/profile
./cc-adapter sessions   # GET /v1/sessions
./cc-adapter voice      # 连 voice_stream WS；从 stdin 读 PCM linear16/16k/mono
```

真实 `claude` 二进制解析顺序：`-claude-bin` > `$CLAUDE_REAL_BIN` > `PATH` 上的 `claude`。
一个现成可用的二进制是 VS Code 扩展自带的：
`~/.vscode/extensions/anthropic.claude-code-*/resources/native-binary/claude`。

### Flags

| flag | 作用 |
|---|---|
| `-model <m>` | 透传 `--model` 给 claude |
| `-no-ide` | 不起 IDE 侧信道（计费归因仍生效） |
| `-deny-writes` | 拒绝写类工具（Write/Edit/MultiEdit/NotebookEdit/Bash） |
| `-claude-bin <path>` | 指定真实 claude 二进制 |
| `-no-telemetry` | 关闭 A1 异常遥测（event_logging）|

权限默认策略是 **全部放行**（headless 自动化无人交互）；`-deny-writes` 提供一个保守开关。子命令 `usage`/`profile`/`sessions`/`voice` 不起会话，直接复刻对应插件请求。

## 已知约束

- 认证沿用真实 claude 的共享 config dir（`~/.claude`，macOS Keychain 条目 `Claude Code-credentials`）。`cc-adapter` 不管理 token 刷新；收到 `oauth_token_refresh` control 请求时回 `null`，让 CLI 自行处理。
- IO 层是最小实现：assistant 文本打印到 stdout，工具调用/会话信息打印到 stderr；不渲染流式增量（未开 `--include-partial-messages`）。
- 注册了 IDE 进程内 MCP server（`sdkMcpServers:["ide"]`）；未注册 hooks / elicitation，因此不会收到 `hook_callback` / `elicitation` 类 control 请求。

## 结构

```
main.go                      整合：host + 子命令分流（usage/profile/sessions/voice）
internal/ide/                IDE 进程内 MCP server（12 工具）+ CommServer（claude-vscode 通信 server）
internal/streamjson/         stream-json host：协议 + spawn + 读写循环 + control 通道（mcp_message 隧道 + log_event 下发）
internal/auth/               OAuth 凭据：读 credentials/keychain、JWT 解 account_uuid、token 刷新（A4）
internal/telemetry/          A1 event_logging 匿名遥测（ClaudeCodeInternalEvent）
internal/cloud/              A2 usage / A3 profile / A6 remote sessions（oauth bearer GET）
internal/voice/              A5 voice_stream WebSocket 客户端
docs/reverse-engineering.md  扩展 v2.1.156 的完整逆向笔记
```
