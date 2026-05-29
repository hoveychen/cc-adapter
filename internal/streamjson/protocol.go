// Package streamjson implements the bidirectional stream-json protocol the
// VS Code "Claude Code" extension speaks to the real claude binary over stdio.
//
// The wire format is NDJSON: one JSON object per line. The host writes user
// messages to claude's stdin and reads a stream of typed events from stdout.
// A side "control" channel (multiplexed over the same stdio) carries the
// initialize handshake and tool-permission round-trips.
//
// All shapes here mirror the reverse-engineered SDK transport in extension.js
// (v2.1.156 / agent-sdk 0.3.156).
package streamjson

import "encoding/json"

// ---------------------------------------------------------------------------
// Reading: top-level stdout messages.
//
// Every line claude emits is one JSON object with a "type" discriminator. We
// first decode just the type, then re-decode into the concrete struct.
// ---------------------------------------------------------------------------

// TypeOnly peeks the discriminator of an incoming line.
type TypeOnly struct {
	Type string `json:"type"`
}

// Message types emitted on stdout.
const (
	TypeAssistant            = "assistant"
	TypeUser                 = "user"
	TypeResult               = "result"
	TypeSystem               = "system"
	TypeStreamEvent          = "stream_event"
	TypeControlRequest       = "control_request"
	TypeControlResponse      = "control_response"
	TypeControlCancelRequest = "control_cancel_request"
	TypeKeepAlive            = "keep_alive"
	TypeRateLimitEvent       = "rate_limit_event"
)

// AgentMessage covers both "assistant" and "user" turns: a thin envelope around
// a standard Anthropic Messages API message.
type AgentMessage struct {
	Type            string           `json:"type"`
	Message         AnthropicMessage `json:"message"`
	SessionID       string           `json:"session_id"`
	UUID            string           `json:"uuid,omitempty"`
	ParentToolUseID *string          `json:"parent_tool_use_id"`
}

// AnthropicMessage is the {role, content[]} body. Assistant turns also carry
// id/model/stop_reason/usage which we don't need here.
type AnthropicMessage struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock is one item of message.content. We model the variants the host
// cares about; unknown fields are ignored.
type ContentBlock struct {
	Type string `json:"type"` // text | tool_use | tool_result | thinking

	// type=text
	Text string `json:"text,omitempty"`

	// type=thinking
	Thinking string `json:"thinking,omitempty"`

	// type=tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type=tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
}

// ResultMessage terminates a turn.
type ResultMessage struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"` // success | error_max_turns | error_during_execution | error_max_budget
	Result       string  `json:"result"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	DurationMS   int     `json:"duration_ms"`
	IsError      bool    `json:"is_error"`
	SessionID    string  `json:"session_id"`
}

// SystemMessage carries subtype-discriminated system events. subtype=="init"
// is the first handshake receipt and reports the effective configuration.
type SystemMessage struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype"`
	SessionID      string          `json:"session_id"`
	Model          string          `json:"model,omitempty"`
	CWD            string          `json:"cwd,omitempty"`
	Tools          []string        `json:"tools,omitempty"`
	MCPServers     []MCPServerInfo `json:"mcp_servers,omitempty"`
	APIKeySource   string          `json:"apiKeySource,omitempty"`
	PermissionMode string          `json:"permissionMode,omitempty"`
}

type MCPServerInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// ---------------------------------------------------------------------------
// Writing: user input messages to claude's stdin.
// ---------------------------------------------------------------------------

// UserInput is the envelope written to stdin for a user turn.
type UserInput struct {
	Type            string          `json:"type"` // always "user"
	SessionID       string          `json:"session_id"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	Message         UserMessageBody `json:"message"`
}

// UserMessageBody.Content is either a plain string or a []ContentBlock.
type UserMessageBody struct {
	Role    string `json:"role"` // always "user"
	Content any    `json:"content"`
}

// NewUserText builds a plain-text user message envelope.
func NewUserText(sessionID, text string) UserInput {
	return UserInput{
		Type:      TypeUser,
		SessionID: sessionID,
		Message:   UserMessageBody{Role: "user", Content: text},
	}
}

// ---------------------------------------------------------------------------
// Control channel (multiplexed over the same stdio).
// ---------------------------------------------------------------------------

// Control request subtypes the CLI sends to us (CLI -> host).
const (
	CtlCanUseTool           = "can_use_tool"
	CtlHookCallback         = "hook_callback"
	CtlMCPMessage           = "mcp_message"
	CtlElicitation          = "elicitation"
	CtlOAuthTokenRefresh    = "oauth_token_refresh"
	CtlHostAuthTokenRefresh = "host_auth_token_refresh"
)

// InControlRequest is a control_request line received from the CLI.
type InControlRequest struct {
	Type      string             `json:"type"` // "control_request"
	RequestID string             `json:"request_id"`
	Request   ControlRequestBody `json:"request"`
}

// ControlRequestBody is the inner request payload. Fields are a union across
// subtypes; only those relevant to the active subtype are populated.
type ControlRequestBody struct {
	Subtype string `json:"subtype"`

	// can_use_tool
	ToolName              string          `json:"tool_name,omitempty"`
	Input                 json.RawMessage `json:"input,omitempty"`
	ToolUseID             string          `json:"tool_use_id,omitempty"`
	AgentID               string          `json:"agent_id,omitempty"`
	PermissionSuggestions json.RawMessage `json:"permission_suggestions,omitempty"`
	BlockedPath           string          `json:"blocked_path,omitempty"`

	// hook_callback
	CallbackID string `json:"callback_id,omitempty"`

	// mcp_message / elicitation
	ServerName string          `json:"server_name,omitempty"`
	Message    json.RawMessage `json:"message,omitempty"`
}

// OutControlRequest is a control_request we initiate (host -> CLI), e.g.
// initialize / interrupt / set_permission_mode.
type OutControlRequest struct {
	Type      string `json:"type"` // "control_request"
	RequestID string `json:"request_id"`
	Request   any    `json:"request"`
}

// ControlResponse is the receipt frame, used both for our replies to the CLI's
// control_requests and for decoding the CLI's replies to ours.
type ControlResponse struct {
	Type     string              `json:"type"` // "control_response"
	Response ControlResponseBody `json:"response"`
}

type ControlResponseBody struct {
	Subtype   string          `json:"subtype"` // "success" | "error"
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// InitializeRequest is the body of the initialize control_request we send right
// after spawn. We declare no hooks and the IDE in-process MCP server via
// sdkMcpServers (e.g. ["ide"]); the CLI then exposes its tools as mcp__ide__*
// and reaches them by tunneling JSON-RPC over control mcp_message frames (the
// default-webview-mode mechanism, not the terminal-mode WebSocket+lockfile).
type InitializeRequest struct {
	Subtype       string   `json:"subtype"` // "initialize"
	Hooks         struct{} `json:"hooks"`
	SDKMcpServers []string `json:"sdkMcpServers"`
}

// Permission decision payloads returned for a can_use_tool request. The shape
// matches the SDK's PermissionResult (behavior + updatedInput/message).
type PermissionAllow struct {
	Behavior     string          `json:"behavior"` // "allow"
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	ToolUseID    string          `json:"toolUseID,omitempty"`
}

type PermissionDeny struct {
	Behavior  string `json:"behavior"` // "deny"
	Message   string `json:"message"`
	ToolUseID string `json:"toolUseID,omitempty"`
}
