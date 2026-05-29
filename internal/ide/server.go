package ide

import (
	"encoding/json"
	"log"
	"os"
)

const (
	serverName    = "Claude Code VSCode MCP"
	serverVersion = "2.1.156"
)

// MCPServer is an in-process MCP server exposing the 12 IDE tools, driven by
// the host over the stream-json control channel's mcp_message frames (the
// default-webview-mode mechanism; not the terminal-mode WebSocket+lockfile).
type MCPServer struct {
	provider StateProvider
	logger   *log.Logger
}

func NewMCPServer(provider StateProvider, logger *log.Logger) *MCPServer {
	if logger == nil {
		logger = log.New(os.Stderr, "[ide] ", log.LstdFlags)
	}
	return &MCPServer{provider: provider, logger: logger}
}

// Handle processes one inbound JSON-RPC message and returns the JSON-RPC
// response to tunnel back, or nil for notifications (no id).
func (s *MCPServer) Handle(raw json.RawMessage) json.RawMessage {
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		s.logger.Printf("bad mcp jsonrpc: %v", err)
		return nil
	}
	if msg.Method == "" || len(msg.ID) == 0 {
		return nil
	}
	result, rpcErr := s.dispatch(&msg)
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

func (s *MCPServer) dispatch(msg *Message) (any, *RPCError) {
	switch msg.Method {
	case "initialize":
		var p InitializeParams
		_ = json.Unmarshal(msg.Params, &p)
		ver := p.ProtocolVersion
		if ver == "" {
			ver = "2025-06-18"
		}
		return InitializeResult{
			ProtocolVersion: ver,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      ServerInfo{Name: serverName, Version: serverVersion},
		}, nil
	case "tools/list":
		return ToolsListResult{Tools: toolDefinitions()}, nil
	case "tools/call":
		return s.callTool(msg.Params)
	case "ping":
		return map[string]any{}, nil
	default:
		return nil, &RPCError{Code: -32601, Message: "Method not found: " + msg.Method}
	}
}

func (s *MCPServer) callTool(params json.RawMessage) (any, *RPCError) {
	var p ToolCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid params"}
	}
	res, err := s.invoke(p.Name, p.Arguments)
	if err != nil {
		return nil, &RPCError{Code: -32603, Message: err.Error()}
	}
	return res, nil
}

func (s *MCPServer) invoke(name string, args json.RawMessage) (ToolCallResult, error) {
	get := func(key string) string {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(args, &m)
		var v string
		_ = json.Unmarshal(m[key], &v)
		return v
	}
	switch name {
	case "openDiff":
		return s.provider.OpenDiff(get("old_file_path"), get("new_file_path"), get("new_file_contents"), get("tab_name"))
	case "getDiagnostics":
		return s.provider.GetDiagnostics(get("uri"))
	case "getOpenEditors":
		return s.provider.GetOpenEditors()
	case "getWorkspaceFolders":
		return s.provider.GetWorkspaceFolders()
	case "getCurrentSelection":
		return s.provider.GetCurrentSelection()
	case "getLatestSelection":
		return s.provider.GetLatestSelection()
	case "openFile":
		var a OpenFileArgs
		_ = json.Unmarshal(args, &a)
		return s.provider.OpenFile(a)
	case "close_tab":
		return s.provider.CloseTab(get("tab_name"))
	case "closeAllDiffTabs":
		return s.provider.CloseAllDiffTabs()
	case "checkDocumentDirty":
		return s.provider.CheckDocumentDirty(get("filePath"))
	case "saveDocument":
		return s.provider.SaveDocument(get("filePath"))
	case "executeCode":
		return s.provider.ExecuteCode(get("code"))
	default:
		return ToolCallResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "unknown tool: " + name}}}, nil
	}
}
