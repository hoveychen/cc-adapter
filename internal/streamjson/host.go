package streamjson

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/hoveychen/cc-adapter/internal/ide"
)

// PermissionFunc decides a can_use_tool request. Returning allow=false denies
// the tool with the given reason. input is the raw tool input JSON.
type PermissionFunc func(toolName string, input json.RawMessage) (allow bool, reason string)

// EventKind enumerates the host-level events surfaced to the UI layer.
type EventKind string

const (
	EventSystemInit    EventKind = "system_init"
	EventAssistantText EventKind = "assistant_text"
	EventThinking      EventKind = "thinking"
	EventToolUse       EventKind = "tool_use"
	EventResult        EventKind = "result"
	EventError         EventKind = "error"
)

// Event is a decoded, UI-facing signal from the stream.
type Event struct {
	Kind      EventKind
	Text      string          // assistant_text / thinking / error message
	ToolName  string          // tool_use
	ToolInput json.RawMessage // tool_use
	System    *SystemMessage  // system_init
	Result    *ResultMessage  // result
}

// Host drives a real claude binary as a stream-json child, impersonating the
// VS Code extension's SDK transport.
type Host struct {
	claudePath     string
	mcpServer      *ide.MCPServer  // in-process IDE MCP server (nil = no IDE tools)
	ideServerName  string          // sdkMcpServers key the IDE tools are exposed under
	commServer     *ide.CommServer // claude-vscode comm server (UI telemetry side channel)
	commServerName string          // sdkMcpServers key the comm server is exposed under
	extraArgs      []string        // passthrough flags (e.g. --model, --add-dir)
	permission     PermissionFunc
	logger         *log.Logger

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	writeMu sync.Mutex

	mu        sync.Mutex
	sessionID string
	reqSeq    int
	pending   map[string]chan ControlResponseBody

	// Events is closed when the child's stdout reaches EOF.
	Events chan Event
}

// Config configures a Host.
type Config struct {
	ClaudePath    string
	MCPServer     *ide.MCPServer
	IDEServerName string
	ExtraArgs     []string
	Permission    PermissionFunc
	Logger        *log.Logger
}

// NewHost prepares (but does not start) a Host.
func NewHost(cfg Config) *Host {
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "[host] ", log.LstdFlags)
	}
	perm := cfg.Permission
	if perm == nil {
		// Default: allow everything (headless automation has no human to prompt).
		perm = func(string, json.RawMessage) (bool, string) { return true, "" }
	}
	ideName := cfg.IDEServerName
	if ideName == "" {
		ideName = "ide"
	}
	const commName = "claude-vscode"
	return &Host{
		claudePath:     cfg.ClaudePath,
		mcpServer:      cfg.MCPServer,
		ideServerName:  ideName,
		commServer:     ide.NewCommServer(commName, logger),
		commServerName: commName,
		extraArgs:      cfg.ExtraArgs,
		permission:     perm,
		logger:         logger,
		pending:        make(map[string]chan ControlResponseBody),
		Events:         make(chan Event, 64),
	}
}

// baselineArgs are the flags the VS Code extension always passes (agent A
// reverse-engineering of the SDK arg builder). --permission-prompt-tool stdio
// routes tool-permission prompts back over this same control channel.
func (h *Host) baselineArgs() []string {
	return []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
	}
}

// buildEnv mirrors the extension's FV() + SDK env construction: tag the
// entrypoint as claude-vscode (this is the billing-attribution signal that
// makes traffic count as claude_code_vscode) and drop NODE_OPTIONS. The IDE
// tools are reached as an in-process sdkMcpServer over the control channel, so
// no CLAUDE_CODE_SSE_PORT / auto-connect env is needed (that was the
// terminal-mode WebSocket mechanism).
func (h *Host) buildEnv() []string {
	overrides := map[string]string{
		"CLAUDE_CODE_ENTRYPOINT":     "claude-vscode",
		"MCP_CONNECTION_NONBLOCKING": "true",
		"CLAUDE_CODE_ENABLE_TASKS":   "0",
		"CLAUDE_AGENT_SDK_VERSION":   "0.3.156",
	}
	var env []string
	seen := make(map[string]bool)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := kv[:eq]
		if k == "NODE_OPTIONS" {
			continue // SDK deletes this before spawn
		}
		if v, ok := overrides[k]; ok {
			env = append(env, k+"="+v)
			seen[k] = true
			continue
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		if !seen[k] {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Start spawns claude and begins the read loop.
func (h *Host) Start(ctx context.Context) error {
	args := append(h.baselineArgs(), h.extraArgs...)
	h.cmd = exec.CommandContext(ctx, h.claudePath, args...)
	h.cmd.Env = h.buildEnv()
	h.cmd.Stderr = os.Stderr

	stdin, err := h.cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := h.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	h.stdin = stdin
	h.stdout = stdout

	h.logger.Printf("spawning: %s %s", h.claudePath, strings.Join(args, " "))
	if err := h.cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}
	go h.readLoop()
	return nil
}

// Initialize sends the initialize control_request and waits for its receipt.
func (h *Host) Initialize() error {
	resp, err := h.sendControlRequest(InitializeRequest{Subtype: "initialize", SDKMcpServers: []string{h.ideServerName, h.commServerName}})
	if err != nil {
		return err
	}
	if resp.Subtype == "error" {
		return fmt.Errorf("initialize failed: %s", resp.Error)
	}
	return nil
}

// SendUserText writes a plain-text user turn to claude's stdin.
func (h *Host) SendUserText(text string) error {
	h.mu.Lock()
	sid := h.sessionID
	h.mu.Unlock()
	return h.writeLine(NewUserText(sid, text))
}

// SendLogEvent fires a log_event notification to the claude-vscode comm server,
// replicating the extension's per-session UI telemetry. The child folds it into
// an internal metering event named tengu_vscode_<eventName>. eventData may be
// nil (sent as an empty object). It blocks until the child acks the mcp_message
// over the control channel (or the 30s control-request timeout elapses).
func (h *Host) SendLogEvent(eventName string, eventData map[string]any) error {
	if eventData == nil {
		eventData = map[string]any{}
	}
	notif := JSONRPCNotification{
		JSONRPC: "2.0",
		Method:  "log_event",
		Params:  LogEventParams{EventName: eventName, EventData: eventData},
	}
	msg, err := json.Marshal(notif)
	if err != nil {
		return err
	}
	resp, err := h.sendControlRequest(OutMCPMessageRequest{
		Subtype:    CtlMCPMessage,
		ServerName: h.commServerName,
		Message:    msg,
	})
	if err != nil {
		return err
	}
	if resp.Subtype == "error" {
		return fmt.Errorf("log_event %s failed: %s", eventName, resp.Error)
	}
	h.logger.Printf("sent log_event %s", eventName)
	return nil
}

// Interrupt asks claude to abort the current turn.
func (h *Host) Interrupt() error {
	_, err := h.sendControlRequest(map[string]string{"subtype": "interrupt"})
	return err
}

// CloseInput closes claude's stdin, signalling end-of-input so it can exit
// gracefully after finishing in-flight work.
func (h *Host) CloseInput() error {
	if h.stdin == nil {
		return nil
	}
	return h.stdin.Close()
}

// Wait blocks until the child exits and returns its exit code.
func (h *Host) Wait() int {
	err := h.cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

// --- internals ---

func (h *Host) nextID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqSeq++
	return fmt.Sprintf("req_%d", h.reqSeq)
}

func (h *Host) writeLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_, err = h.stdin.Write(b)
	return err
}

func (h *Host) sendControlRequest(req any) (ControlResponseBody, error) {
	id := h.nextID()
	ch := make(chan ControlResponseBody, 1)
	h.mu.Lock()
	h.pending[id] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}()

	if err := h.writeLine(OutControlRequest{Type: TypeControlRequest, RequestID: id, Request: req}); err != nil {
		return ControlResponseBody{}, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(30 * time.Second):
		return ControlResponseBody{}, fmt.Errorf("control request %s timed out", id)
	}
}

func (h *Host) readLoop() {
	defer close(h.Events)
	r := bufio.NewReaderSize(h.stdout, 1024*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			h.handleLine(line)
		}
		if err != nil {
			if err != io.EOF {
				h.logger.Printf("stdout read error: %v", err)
			}
			return
		}
	}
}

func (h *Host) handleLine(line []byte) {
	var t TypeOnly
	if err := json.Unmarshal(line, &t); err != nil {
		h.logger.Printf("bad json line: %v", err)
		return
	}
	switch t.Type {
	case TypeAssistant:
		h.handleAgentMessage(line)
	case TypeUser:
		// tool_result replay; nothing to surface in a simple UI.
	case TypeResult:
		var m ResultMessage
		if err := json.Unmarshal(line, &m); err == nil {
			h.Events <- Event{Kind: EventResult, Result: &m}
		}
	case TypeSystem:
		var m SystemMessage
		if err := json.Unmarshal(line, &m); err == nil {
			if m.Subtype == "init" {
				h.mu.Lock()
				h.sessionID = m.SessionID
				h.mu.Unlock()
				h.Events <- Event{Kind: EventSystemInit, System: &m}
			}
		}
	case TypeControlRequest:
		var in InControlRequest
		if err := json.Unmarshal(line, &in); err == nil {
			go h.handleControlRequest(in)
		}
	case TypeControlResponse:
		var cr ControlResponse
		if err := json.Unmarshal(line, &cr); err == nil {
			h.mu.Lock()
			ch := h.pending[cr.Response.RequestID]
			h.mu.Unlock()
			if ch != nil {
				ch <- cr.Response
			}
		}
	case TypeControlCancelRequest, TypeKeepAlive, TypeStreamEvent, TypeRateLimitEvent:
		// no-op (rate_limit_event carries throttling status we don't surface)
	default:
		h.logger.Printf("unknown message type: %q", t.Type)
	}
}

func (h *Host) handleAgentMessage(line []byte) {
	var m AgentMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return
	}
	for _, b := range m.Message.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				h.Events <- Event{Kind: EventAssistantText, Text: b.Text}
			}
		case "thinking":
			if b.Thinking != "" {
				h.Events <- Event{Kind: EventThinking, Text: b.Thinking}
			}
		case "tool_use":
			h.Events <- Event{Kind: EventToolUse, ToolName: b.Name, ToolInput: b.Input}
		}
	}
}

// handleControlRequest answers a control_request the CLI sent to us.
func (h *Host) handleControlRequest(in InControlRequest) {
	switch in.Request.Subtype {
	case CtlCanUseTool:
		allow, reason := h.permission(in.Request.ToolName, in.Request.Input)
		var payload json.RawMessage
		if allow {
			payload, _ = json.Marshal(PermissionAllow{
				Behavior:     "allow",
				UpdatedInput: in.Request.Input,
				ToolUseID:    in.Request.ToolUseID,
			})
		} else {
			if reason == "" {
				reason = "denied by cc-adapter policy"
			}
			payload, _ = json.Marshal(PermissionDeny{
				Behavior:  "deny",
				Message:   reason,
				ToolUseID: in.Request.ToolUseID,
			})
		}
		h.replyControlSuccess(in.RequestID, payload)
	case CtlMCPMessage:
		var jsonrpcResp json.RawMessage
		switch in.Request.ServerName {
		case h.ideServerName:
			if h.mcpServer == nil {
				h.replyControlError(in.RequestID, "unknown mcp server: "+in.Request.ServerName)
				return
			}
			jsonrpcResp = h.mcpServer.Handle(in.Request.Message)
		case h.commServerName:
			jsonrpcResp = h.commServer.Handle(in.Request.Message)
		default:
			h.replyControlError(in.RequestID, "unknown mcp server: "+in.Request.ServerName)
			return
		}
		var payload json.RawMessage
		if jsonrpcResp != nil {
			payload, _ = json.Marshal(map[string]json.RawMessage{"mcp_response": jsonrpcResp})
		} else {
			payload = json.RawMessage(`{"mcp_response":{"jsonrpc":"2.0","result":{},"id":0}}`)
		}
		h.replyControlSuccess(in.RequestID, payload)
	case CtlOAuthTokenRefresh:
		// We don't manage refresh; auth comes from the shared config dir.
		h.replyControlSuccess(in.RequestID, json.RawMessage(`{"accessToken":null}`))
	case CtlHostAuthTokenRefresh:
		h.replyControlSuccess(in.RequestID, json.RawMessage(`{"authToken":null}`))
	default:
		// hook_callback / elicitation: we registered none, so these shouldn't
		// arrive. Report an error rather than hang the CLI.
		h.replyControlError(in.RequestID, "unsupported control request subtype: "+in.Request.Subtype)
	}
}

func (h *Host) replyControlSuccess(reqID string, payload json.RawMessage) {
	_ = h.writeLine(ControlResponse{
		Type: TypeControlResponse,
		Response: ControlResponseBody{
			Subtype:   "success",
			RequestID: reqID,
			Response:  payload,
		},
	})
}

func (h *Host) replyControlError(reqID, msg string) {
	_ = h.writeLine(ControlResponse{
		Type: TypeControlResponse,
		Response: ControlResponseBody{
			Subtype:   "error",
			RequestID: reqID,
			Error:     msg,
		},
	})
}
