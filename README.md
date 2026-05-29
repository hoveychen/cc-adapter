# cc-adapter

像 VS Code「Claude Code」扩展一样驱动真实的 `claude` 二进制。

`cc-adapter` 以 `CLAUDE_CODE_ENTRYPOINT=claude-vscode` 把真实 `claude` 作为双向 stream-json 子进程拉起，并把 IDE 工具注册为进程内 MCP server、会话启动发 `claude_launched` 遥测——从 Anthropic 后台看，这条流量被归因为 `claude_code_vscode`，与真实 VS Code 会话一致。

两种用法：

- **当 CLI 用**：直接 `cc-adapter "prompt"`，它自己当 host 起一个 vscode 会话。
- **当官方 Claude Agent SDK 的 `claude` 用**：把 SDK 的可执行文件路径指向 `cc-adapter`，SDK 即可像驱动真 claude 一样驱动它，而上游会话仍是完整 vscode 会话。

> 内部原理（control 协议、双向中继、流量指纹、A1–A6 复刻）见 [docs/OVERVIEW.md](docs/OVERVIEW.md) 与 [docs/reverse-engineering.md](docs/reverse-engineering.md)。

## 构建

```bash
go build -o cc-adapter .
```

真实 `claude` 二进制解析顺序：`-claude-bin` > `$CLAUDE_REAL_BIN` > `PATH` 上的 `claude`。VS Code 扩展自带一个现成的：
`~/.vscode/extensions/anthropic.claude-code-*/resources/native-binary/claude`。

## 集成：用官方 Claude Agent SDK 驱动

把 SDK 的可执行文件路径指向 `cc-adapter`，并用 `env` 给出真实 claude 的位置（`CLAUDE_REAL_BIN`）。SDK 那边无需改动，归因与 IDE 指纹全部到位。

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

多轮对话、工具调用、SDK 自有 MCP server、hooks 均原样工作；`mcp__ide__*` 工具与 `ide` / `claude-vscode` server 在会话里可见且 `connected`。已用 Python（0.2.87）与 TypeScript（0.3.153）官方 SDK 端到端验证。

## 集成：当 CLI 用

```bash
# 一次性 prompt：发送、打印结果、退出
./cc-adapter "fix the bug in main.go"

# 交互 REPL：每行 stdin 作为一个 user turn
./cc-adapter

# claude -p 兼容面（上游始终是完整 vscode 会话）
./cc-adapter -p "summarize"
./cc-adapter -p --output-format json "..."
./cc-adapter -p --model claude-opus-4-8 --permission-mode plan "..."

# 复刻插件功能性请求（需已登录的 OAuth 凭据）
./cc-adapter usage | profile | sessions | session <id> | voice
```

会话类用法支持透传任意 claude 会话 flag（`--model`、`--add-dir`、`--system-prompt`、`--permission-mode` 等），原样转发给子进程。

## Flags

| flag | 作用 |
|---|---|
| `-claude-bin <path>` | 指定真实 claude 二进制 |
| `-model <m>` | 透传 `--model` 给 claude |
| `-no-ide` | 不起 IDE 侧信道（计费归因仍生效） |
| `-deny-writes` | 拒绝写类工具（Write/Edit/MultiEdit/NotebookEdit/Bash） |
| `-no-telemetry` | 关闭 A1 异常遥测（event_logging）|

权限默认全部放行（headless 自动化无人交互）；`-deny-writes` 是保守开关。
