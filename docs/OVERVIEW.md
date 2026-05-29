# cc-adapter 总览（架构 / 流量指纹 / 用法）

## 它是什么

`cc-adapter` 是一个 Go 程序，把真实的 `claude` 二进制当 stream-json 子进程驱动，并**完整复刻 VS Code「Claude Code」扩展（v2.1.156）对 Anthropic 发起的全部网络互动**。目标：从 Anthropic 后台看，cc-adapter 产生的流量指纹 = 100% 真 VS Code 插件。

真插件 = **extension 进程**（自己发的遥测/功能请求）+ **spawn 的 claude 子进程**（messages + tengu 遥测）。cc-adapter 把两半都复刻。

## 架构

```
cc-adapter (Go)
├── main.go                  入口：host 模式 + 子命令分流
├── internal/streamjson/     stream-json host：spawn claude、读写循环、control 通道
│                            （mcp_message 隧道 + log_event 主动下发）
├── internal/ide/            IDE 工具 MCP server（12 工具）+ CommServer（claude-vscode 通信 server）
├── internal/auth/           OAuth：读 credentials/keychain、JWT 解 account_uuid、token 刷新(A4)
├── internal/telemetry/      A1 event_logging 匿名遥测（ClaudeCodeInternalEvent）
├── internal/cloud/          A2 usage / A3 profile / A6 sessions(+3衍生端点) oauth GET
└── internal/voice/          A5 voice_stream WebSocket 客户端
```

**双边设计**：上游（真 claude + Anthropic 后台）永远看到一个完整 VS Code webview 会话；下游（调用方）看到一个 `claude -p` 兼容接口。两边的不匹配正是 adapter 的价值——把廉价的 `claude -p` 用法桥接到完整 VS Code 会话上。

子进程**恒**以 VS Code 默认 **webview 模式**拉起（无论下游传什么，都不是 `claude -p`）：
```
claude --output-format stream-json --input-format stream-json --verbose --permission-prompt-tool stdio
```
注入 env：`CLAUDE_CODE_ENTRYPOINT=claude-vscode`（计费归因核心）、`MCP_CONNECTION_NONBLOCKING=true`、`CLAUDE_CODE_ENABLE_TASKS=0`、`CLAUDE_AGENT_SDK_VERSION=0.3.156`，删除 `NODE_OPTIONS`。

下游的 `-p` / `--output-format` / `--input-format` 只决定 adapter 怎么跟**调用方**说话：adapter 通过 host 的 RawSink 把子进程的 stream-json 帧重新呈现给下游——`text` 打最终 result 文本，`json` 透出 result 帧，`stream-json` 逐帧透出（过滤掉 control 通道帧，使之与真 `claude -p` 一致）。除 I/O 类与管理类 flag 外，其余 claude 会话 flag（`--model`、`--add-dir`、`--system-prompt`、`--permission-mode` 等）原样转发给子进程，无需逐个白名单。

**SDK-driven relay 模式**：当下游 in/out 都是 `stream-json`（即官方 Claude Agent SDK 的双向控制协议传输）时，adapter 不再做上面这套单向 `claude -p` 呈现，而是进入 `relay.go` 的**双向 control 中继**：在 SDK（父）与真 claude（子）之间忠实转发每一帧，并把 SDK 的 `initialize` 与 cc-adapter 的 `ide`+`claude-vscode` 进程内 server 合并、按 server_name 路由 `mcp_message`、注入 `claude_launched`，从而让 SDK 能把 cc-adapter 当 claude 驱动，上游仍是完整 vscode 会话。两个协议细节：`-v`/`--version` 透传给真 claude（SDK 版本预检），claude 的 stdin 保持到 `result` 帧才关（control 协议要求全程开）。详见 README「用官方 Claude Agent SDK 驱动」。

## 流量指纹对照（cc-adapter vs 真插件）

| 请求 | 真插件触发 | cc-adapter 复刻 | 验证 |
|---|---|---|---|
| **messages + tengu 遥测** | 子进程恒发 | spawn entrypoint=claude-vscode → `anthropic-client-platform: claude_code_vscode`、UA `claude-cli/2.1.156 (external, claude-vscode, agent-sdk/0.3.156)` | 真环境抓包逐字 ✓ |
| **log_event**（claude_launched 等） | 会话启动 MCP 通知 | 注册 `claude-vscode` 通信 server，会话启动发 log_event notification → 子进程折成 `tengu_vscode_<name>` | smoke 投递 acked ✓ |
| **IDE 工具**（mcp__ide__*） | sdkMcpServer | in-process MCP server，经 control 通道 mcp_message 往返 | smoke `ide connected` ✓ |
| **A1** event_logging | spawn 失败/子进程异常退出 | `internal/telemetry` 匿名 POST（无 auth，x-service-name:claude-code，gate DISABLE_*） | 单测 ✓ |
| **A2** usage | 用户看用量 | `cc-adapter usage` | 真调到 API ✓ |
| **A3** profile | teleport 取 org | `cc-adapter profile` | 单测 ✓ |
| **A4** oauth refresh | token 过期 | `internal/auth` 自动刷新（platform.claude.com） | 单测 ✓ |
| **A5** voice WS | 语音输入 | `cc-adapter voice`（stdin PCM 16k mono） | 单测+假WS握手 ✓ |
| **A6** sessions + 3 衍生 | remote/teleport | `cc-adapter sessions` / `session <id>` / `teleport-events <id>` / `session-ingress <id>` | 单测（含分页聚合）✓ |

**UA 指纹**：子进程主流量 UA 带 `claude-vscode`（claude 二进制注入）；cc-adapter 自己发的辅助请求（A1-A6）UA 设 `axios/1.9.0`，匹配真插件 extension 的 axios 客户端（不泄漏 `Go-http-client/1.1`）。

**认证**：读 `~/.claude/.credentials.json`（`claudeAiOauth.accessToken`）+ macOS Keychain `Claude Code-credentials` fallback；`account_uuid` 由 JWT payload `.sub` 解出。

## 用法

```bash
go build -o cc-adapter .
export CLAUDE_REAL_BIN=~/.vscode/extensions/anthropic.claude-code-*/resources/native-binary/claude

# 会话（默认 host 模式）
./cc-adapter "fix the bug"          # 单次 prompt
./cc-adapter                        # 交互 REPL（stdin 一行一个 turn）

# 下游 claude -p 兼容面（上游始终是完整 VS Code webview 会话）
./cc-adapter -p "summarize"                        # -p：打印结果后退出
echo "summarize this" | ./cc-adapter -p            # 从管道读 prompt
./cc-adapter -p --output-format json "..."         # json：透出 result 帧（同 claude -p）
./cc-adapter -p --output-format stream-json "..."  # stream-json：逐帧透出（已过滤控制通道帧）
printf '%s\n' '{"type":"user","message":{"content":"hi"}}' | ./cc-adapter -p --input-format stream-json
./cc-adapter -p --model opus --permission-mode plan "..."  # 任意 claude 会话 flag 原样转发给子进程

# 复刻插件功能性请求（需已登录的 OAuth 凭据）
./cc-adapter usage
./cc-adapter profile
./cc-adapter sessions
./cc-adapter session <id>
./cc-adapter teleport-events <id>
./cc-adapter session-ingress <id>
./cc-adapter voice                  # 从 stdin 读 PCM linear16/16k/mono
```

真实 claude 二进制解析顺序：`-claude-bin` > `$CLAUDE_REAL_BIN` > `PATH` 上的 `claude`（注意 alias 不影响 `exec.LookPath`，但别把 cc-adapter 本身装成 PATH 的 claude，否则递归）。

**下游 claude -p flag（adapter 自己接管，不转发给子进程）**：

| flag | 作用 |
|---|---|
| `-p` / `--print` | 非交互模式：喂入 prompt、打印结果后退出 |
| `--output-format <text\|json\|stream-json>` | 下游输出格式（默认 text，同 claude -p） |
| `--input-format <text\|stream-json>` | 下游输入格式：text 整段 stdin 当 prompt；stream-json 逐行解析 user turn |
| `--include-partial-messages` | 转发给子进程使其吐出增量 stream_event 帧；与 `--output-format stream-json` 配合（实测生效）|
| `--replay-user-messages` | 转发给子进程使其在 stdout 回显 user 帧供下游确认（实测生效）|

其余 claude 会话 flag（`--model`、`--add-dir`、`--allowedTools`、`--system-prompt`、`--permission-mode`、`--resume`、`--session-id`…）**原样转发**给子进程。变参 flag 紧贴位置 prompt 时用 `--` 分隔，例：`cc-adapter -p --allowedTools Bash Edit -- "summarize this"`。

**adapter 管理 flag（不转发，`-x` / `--x` 两种写法均可）**：

| flag | 作用 |
|---|---|
| `--no-ide` | 不注册 IDE in-process MCP server |
| `--no-telemetry` | 关闭 A1 异常遥测 |
| `--deny-writes` | 拒绝写类工具 |
| `--claude-bin <path>` | 指定真实 claude 二进制 |

## 已验证 vs stub

- **已真环境/单测验证**：messages 归因、log_event 投递、IDE server 连接、A2 真调 API、UA 对齐、A6 分页聚合。
- **stub / 受限**：A5 voice 的音频管线（headless 无音频源，命令入口可从 stdin 读 PCM 或仅验证握手）；A2/A3/A5/A6 在 headless 场景需显式子命令触发（真插件也只在对应用户操作时发）。

## 逆向依据

完整逆向笔记见 [reverse-engineering.md](reverse-engineering.md)：进程启动/flag、环境变量、IDE 协议、计费归因（`p2()` → `anthropic-client-platform`）、stream-json 协议、以及插件主动网络互动清单（A1-A6 + log_event）。
