package ide

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/gorilla/websocket"
)

const (
	serverName    = "claude-vscode"
	serverVersion = "2.1.156"
	authHeader    = "x-claude-code-ide-authorization"
)

// Server impersonates the VS Code extension's MCP-over-WebSocket endpoint.
type Server struct {
	ln        net.Listener
	port      int
	authToken string
	provider  StateProvider
	logger    *log.Logger

	upgrader websocket.Upgrader
	httpSrv  *http.Server

	mu      sync.Mutex
	conn    *websocket.Conn // single active client (newest wins)
	writeMu sync.Mutex
}

// NewServer binds a random localhost port and prepares (but does not start)
// the MCP server. Call Start to accept connections.
func NewServer(provider StateProvider, logger *log.Logger) (*Server, error) {
	ln, port, err := FindFreePort()
	if err != nil {
		return nil, err
	}
	token, err := NewAuthToken()
	if err != nil {
		ln.Close()
		return nil, err
	}
	if logger == nil {
		logger = log.New(os.Stderr, "[ide] ", log.LstdFlags)
	}
	return &Server{
		ln:        ln,
		port:      port,
		authToken: token,
		provider:  provider,
		logger:    logger,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}, nil
}

func (s *Server) Port() int         { return s.port }
func (s *Server) AuthToken() string { return s.authToken }

// Start begins serving on the already-bound listener.
func (s *Server) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleWS)
	s.httpSrv = &http.Server{Handler: mux}
	go func() {
		if err := s.httpSrv.Serve(s.ln); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("server error: %v", err)
		}
	}()
	s.logger.Printf("MCP server running on port %d (localhost only)", s.port)
}

// Stop shuts down the HTTP server.
func (s *Server) Stop() {
	if s.httpSrv != nil {
		_ = s.httpSrv.Shutdown(context.Background())
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Auth handshake: the real claude binary sends the lockfile's authToken in
	// the x-claude-code-ide-authorization header. Mismatch => close 1008.
	if r.Header.Get(authHeader) != s.authToken {
		s.logger.Printf("unauthorized WebSocket connection attempt")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Printf("upgrade failed: %v", err)
		return
	}

	// Single-client model: newest connection wins, like the extension.
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.conn = conn
	s.mu.Unlock()
	s.logger.Printf("new WS connection from %s", r.RemoteAddr)

	defer func() {
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		s.mu.Unlock()
		_ = conn.Close()
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			s.logger.Printf("ws read closed: %v", err)
			return
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			s.logger.Printf("bad json: %v", err)
			continue
		}
		s.handleMessage(conn, &msg)
	}
}

func (s *Server) handleMessage(conn *websocket.Conn, msg *Message) {
	switch {
	case msg.IsRequest():
		result, rpcErr := s.dispatch(msg)
		s.reply(conn, msg.ID, result, rpcErr)
	case msg.IsNotification():
		// e.g. notifications/initialized — nothing to do.
		s.logger.Printf("notification: %s", msg.Method)
	}
}

func (s *Server) dispatch(msg *Message) (any, *RPCError) {
	switch msg.Method {
	case "initialize":
		var p InitializeParams
		_ = json.Unmarshal(msg.Params, &p)
		ver := p.ProtocolVersion
		if ver == "" {
			ver = "2025-06-18"
		}
		return InitializeResult{
			ProtocolVersion: ver, // echo the client's negotiated version
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

func (s *Server) callTool(params json.RawMessage) (any, *RPCError) {
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

func (s *Server) invoke(name string, args json.RawMessage) (ToolCallResult, error) {
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

func (s *Server) reply(conn *websocket.Conn, id json.RawMessage, result any, rpcErr *RPCError) {
	resp := Message{JSONRPC: "2.0", ID: id}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		b, _ := json.Marshal(result)
		resp.Result = b
	}
	s.send(conn, resp)
}

func (s *Server) send(conn *websocket.Conn, msg Message) {
	b, err := json.Marshal(msg)
	if err != nil {
		s.logger.Printf("marshal: %v", err)
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		s.logger.Printf("ws write: %v", err)
	}
}

// Notify pushes a server->client JSON-RPC notification to the active client.
// These are the IDE-side push events: selection_changed, diagnostics_changed,
// at_mentioned, ide_connected.
func (s *Server) Notify(method string, params any) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	s.send(conn, Message{JSONRPC: "2.0", Method: method, Params: raw})
}
