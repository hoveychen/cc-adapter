# cc-adapter

像 VS Code「Claude Code」扩展一样驱动真实的 `claude` 二进制。

`cc-adapter` **不是**透传壳子。它就是 VS Code 扩展那套 **stream-json host**：自己起 IDE WebSocket 侧信道（lockfile + MCP server），再以 `CLAUDE_CODE_ENTRYPOINT=claude-vscode` 把 `claude` 作为双向 stream-json 子进程拉起来。结果是——从 Anthropic 后台看，这条流量被归因为 `claude_code_vscode`，并且走的是 IDE auto-connect 路径，和真实 VS Code 会话一致。

> 本仓库基于对 `anthropic.claude-code` 扩展 v2.1.156（darwin-arm64）的逆向。完整逆向笔记见 [docs/reverse-engineering.md](docs/reverse-engineering.md)。

## 它和 `claude -p` 的区别

| | `claude -p` | cc-adapter（= VS Code 起法） |
|---|---|---|
| 模式 | 一次性 print | `--input-format stream-json` 双向长驻会话 |
| 归因 source | `claude_code_cli` | **`claude_code_vscode`**（`CLAUDE_CODE_ENTRYPOINT=claude-vscode`） |
| 权限 | CLI 自行处理 | `--permission-prompt-tool stdio`，经 control 通道回传给 host |
| IDE 侧信道 | 无 | lockfile + WebSocket MCP server + `CLAUDE_CODE_SSE_PORT` 注入 |

启动 claude 的精确命令（与 VS Code 扩展一致）：

```
claude --output-format stream-json --input-format stream-json --verbose --permission-prompt-tool stdio [--model ...]
```

注入的关键环境变量：`CLAUDE_CODE_ENTRYPOINT=claude-vscode`、`CLAUDE_CODE_SSE_PORT=<port>`、`CLAUDE_CODE_AUTO_CONNECT_IDE=true`、`MCP_CONNECTION_NONBLOCKING=true`、`CLAUDE_CODE_ENABLE_TASKS=0`、`CLAUDE_AGENT_SDK_VERSION=0.3.156`，并删除 `NODE_OPTIONS`。

## 计费归因 vs IDE 回连——两条独立的线

- **计费归因** 100% 取决于 `CLAUDE_CODE_ENTRYPOINT`，二进制里 `p2()` 把它归一化成 `anthropic-client-platform` 请求头（`claude_code_vscode`）。这条**不需要任何 IDE 回连**就成立。
- **IDE 回连** 决定 claude 能否调用 `mcp__ide__*` 那 12 个工具（选区、诊断、diff）。`cc-adapter` 起了 headless IDE server 并注入 `CLAUDE_CODE_SSE_PORT`，gate 表达式 `(CLAUDE_CODE_SSE_PORT || CLAUDE_CODE_AUTO_CONNECT_IDE) && !disabled` 因此放行。headless 场景下编辑器状态由真实文件系统提供（openDiff 自动落盘、选区为空）。

## 用法

```bash
go build -o cc-adapter .

# 一次性 prompt：发送、打印结果、退出
./cc-adapter "fix the bug in main.go"

# 交互 REPL：每行 stdin 作为一个 user turn
./cc-adapter

# 透传 flag，并指定真实 claude 二进制
./cc-adapter -model claude-opus-4-8 -claude-bin /path/to/claude "..."
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

权限默认策略是 **全部放行**（headless 自动化无人交互）；`-deny-writes` 提供一个保守开关。

## 已知约束

- 认证沿用真实 claude 的共享 config dir（`~/.claude`，macOS Keychain 条目 `Claude Code-credentials`）。`cc-adapter` 不管理 token 刷新；收到 `oauth_token_refresh` control 请求时回 `null`，让 CLI 自行处理。
- IO 层是最小实现：assistant 文本打印到 stdout，工具调用/会话信息打印到 stderr；不渲染流式增量（未开 `--include-partial-messages`）。
- 未注册 hooks / SDK 侧 MCP server，因此不会收到 `hook_callback` / `elicitation` 类 control 请求。

## 结构

```
main.go                      整合：IDE server + lockfile + host + 单次/REPL IO
internal/ide/                IDE WebSocket MCP server（lockfile、12 工具、headless provider）
internal/streamjson/         stream-json host：协议类型 + spawn + 读写循环 + control 通道
docs/reverse-engineering.md  扩展 v2.1.156 的完整逆向笔记
```
