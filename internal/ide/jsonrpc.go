package ide

import "encoding/json"

// JSON-RPC 2.0 + minimal MCP framing, matching what the real claude binary
// speaks to the VS Code extension over the WebSocket.

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`     // request/response id (may be string or number)
	Method  string          `json:"method,omitempty"` // set on request/notification
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// IsRequest reports whether the message carries an id (i.e. expects a response).
func (m *Message) IsRequest() bool { return len(m.ID) > 0 && m.Method != "" }

// IsNotification reports a method call with no id.
func (m *Message) IsNotification() bool { return len(m.ID) == 0 && m.Method != "" }

// --- MCP initialize ---

type InitializeParams struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities,omitempty"`
	ClientInfo      ServerInfo      `json:"clientInfo,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      ServerInfo     `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// --- tools/list & tools/call ---

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolCallResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// TextResult is a convenience builder for a single/multi text-content result.
func TextResult(texts ...string) ToolCallResult {
	items := make([]ContentItem, 0, len(texts))
	for _, t := range texts {
		items = append(items, ContentItem{Type: "text", Text: t})
	}
	return ToolCallResult{Content: items}
}
