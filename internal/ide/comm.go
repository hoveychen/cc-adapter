package ide

import (
	"encoding/json"
	"log"
	"os"
)

// CommServer is the minimal "claude-vscode" communication SDK MCP server the
// VS Code extension registers alongside the IDE tools server. The extension
// uses this server purely as a side channel for UI telemetry: the host pushes
// log_event notifications down it (host -> child), and the child folds them
// into internal metering events (tengu_vscode_<eventName>).
//
// As a server it exposes no tools — it only needs to satisfy the child's MCP
// handshake (initialize / tools/list / ping) so the connection stays live.
// Inbound notifications from the child (no id, e.g. experiment_gates) are
// acknowledged with no response.
type CommServer struct {
	name   string
	logger *log.Logger
}

// NewCommServer builds the claude-vscode comm server. name is the sdkMcpServer
// key it is exposed under (conventionally "claude-vscode").
func NewCommServer(name string, logger *log.Logger) *CommServer {
	if logger == nil {
		logger = log.New(os.Stderr, "[comm] ", log.LstdFlags)
	}
	return &CommServer{name: name, logger: logger}
}

// Name reports the sdkMcpServer key this server is exposed under.
func (s *CommServer) Name() string { return s.name }

// Handle answers one inbound JSON-RPC message from the child and returns the
// JSON-RPC response to tunnel back, or nil for notifications (no id).
func (s *CommServer) Handle(raw json.RawMessage) json.RawMessage {
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		s.logger.Printf("bad comm jsonrpc: %v", err)
		return nil
	}
	// Notifications (no id) get no response — just ack by returning nil.
	if msg.Method == "" || len(msg.ID) == 0 {
		return nil
	}

	var result any
	var rpcErr *RPCError
	switch msg.Method {
	case "initialize":
		var p InitializeParams
		_ = json.Unmarshal(msg.Params, &p)
		ver := p.ProtocolVersion
		if ver == "" {
			ver = "2025-06-18"
		}
		result = InitializeResult{
			ProtocolVersion: ver,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      ServerInfo{Name: s.name, Version: serverVersion},
		}
	case "tools/list":
		result = ToolsListResult{Tools: []Tool{}}
	case "ping":
		result = map[string]any{}
	default:
		rpcErr = &RPCError{Code: -32601, Message: "Method not found: " + msg.Method}
	}

	resp := Message{JSONRPC: "2.0", ID: msg.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		b, _ := json.Marshal(result)
		resp.Result = b
	}
	out, _ := json.Marshal(resp)
	return out
}
