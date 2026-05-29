package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/hoveychen/cc-adapter/internal/streamjson"
)

// relay bridges the downstream Claude Agent SDK (parent) and the upstream real
// claude (child) on the stream-json control protocol. cc-adapter spawns claude
// tagged claude-vscode (via the Host) and relays every frame between the two
// sides, with three interventions that keep the upstream session a *complete*
// VS Code session while still letting the SDK drive it:
//
//   - initialize: the SDK's initialize control_request is merged with
//     cc-adapter's in-process IDE + claude-vscode MCP servers before being
//     forwarded upstream, so claude exposes mcp__ide__* and folds log_event into
//     tengu_vscode_* telemetry.
//   - mcp_message routing: claude's mcp_message control_requests addressed to
//     ide / claude-vscode are serviced in-process; all others are forwarded down
//     to the SDK, which owns those servers.
//   - claude_launched: a log_event is injected once, after system:init, exactly
//     as the extension does.
//
// Everything else — user turns, assistant/result frames, can_use_tool, hooks,
// the SDK's control_responses — is relayed verbatim. control_requests the relay
// originates upstream (the log_event) carry streamjson.RelayIDPrefix so their
// acks are recognised on the upstream side and dropped rather than forwarded.
type relay struct {
	host   *streamjson.Host
	logger *log.Logger

	denyWrites bool

	// localServers are the in-process MCP server names serviced locally; an
	// mcp_message for any other server_name is forwarded to the SDK.
	localServers map[string]bool

	out   io.Writer // downstream sink (to the SDK); os.Stdout in production
	outMu sync.Mutex

	launched sync.Once
}

// newRelay builds a relay over an already-configured Host. The Host must be
// created with RelayMode:true and RawSink wired to the relay's onUpstreamLine.
func newRelay(host *streamjson.Host, denyWrites bool, logger *log.Logger) *relay {
	local := map[string]bool{host.CommServerName(): true}
	if host.HasIDEServer() {
		local[host.IDEServerName()] = true
	}
	return &relay{
		host:         host,
		logger:       logger,
		denyWrites:   denyWrites,
		localServers: local,
		out:          os.Stdout,
	}
}

// run pumps the downstream SDK stdin into the upstream claude until EOF, then
// signals end-of-input and waits for claude to exit, returning its exit code.
// The upstream→downstream direction is driven by the Host read goroutine calling
// onUpstreamLine (wired as RawSink), so it runs concurrently with this pump.
func (r *relay) run(_ context.Context, stdin io.Reader) int {
	if err := r.pumpDownstream(stdin); err != nil {
		r.logger.Printf("relay: downstream read: %v", err)
	}
	_ = r.host.CloseInput()
	return r.host.Wait()
}

// pumpDownstream reads NDJSON frames from the SDK and routes each one upstream.
func (r *relay) pumpDownstream(stdin io.Reader) error {
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		buf := make([]byte, len(line)) // scanner reuses its buffer; copy before use
		copy(buf, line)
		r.onDownstreamLine(buf)
	}
	return sc.Err()
}

// onDownstreamLine routes one frame from the SDK upstream to claude. The
// initialize control_request is intercepted to merge in cc-adapter's in-process
// MCP servers; every other frame is forwarded verbatim.
func (r *relay) onDownstreamLine(line []byte) {
	var t streamjson.TypeOnly
	if json.Unmarshal(line, &t) != nil {
		_ = r.host.WriteUpstreamRaw(line)
		return
	}
	if t.Type == streamjson.TypeControlRequest {
		var probe struct {
			Request struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.Request.Subtype == "initialize" {
			r.forwardMergedInitialize(line)
			return
		}
	}
	_ = r.host.WriteUpstreamRaw(line)
}

// forwardMergedInitialize rewrites the SDK's initialize control_request so its
// sdkMcpServers list also carries cc-adapter's in-process servers, then forwards
// it upstream. The request_id is preserved so claude's control_response routes
// back to the SDK unchanged.
func (r *relay) forwardMergedInitialize(line []byte) {
	var frame map[string]json.RawMessage
	if json.Unmarshal(line, &frame) != nil {
		_ = r.host.WriteUpstreamRaw(line)
		return
	}
	var names []string
	if r.host.HasIDEServer() {
		names = append(names, r.host.IDEServerName())
	}
	names = append(names, r.host.CommServerName())

	frame["request"] = streamjson.MergeSDKMcpServers(frame["request"], names...)
	out, err := json.Marshal(frame)
	if err != nil {
		_ = r.host.WriteUpstreamRaw(line)
		return
	}
	r.logger.Printf("relay: merged sdkMcpServers %v into SDK initialize", names)
	_ = r.host.WriteUpstreamRaw(out)
}

// onUpstreamLine routes one frame from claude downstream to the SDK. It is wired
// as the Host's RawSink and runs on the Host read goroutine.
func (r *relay) onUpstreamLine(line []byte) {
	var t streamjson.TypeOnly
	if json.Unmarshal(line, &t) != nil {
		r.writeDownstream(line) // non-JSON: pass through
		return
	}
	switch t.Type {
	case streamjson.TypeControlResponse:
		// Drop acks to the relay's own injected requests (RelayIDPrefix); forward
		// the rest — those answer the SDK's initialize / interrupt / etc.
		var cr streamjson.ControlResponse
		if json.Unmarshal(line, &cr) == nil && strings.HasPrefix(cr.Response.RequestID, streamjson.RelayIDPrefix) {
			return
		}
		r.writeDownstream(line)
	case streamjson.TypeControlRequest:
		r.handleUpstreamControlRequest(line)
	default:
		// Data frame: forward verbatim. system:init triggers the one-shot
		// claude_launched log_event, mirroring the extension.
		r.writeDownstream(line)
		if t.Type == streamjson.TypeSystem {
			r.maybeLaunched(line)
		}
	}
}

// handleUpstreamControlRequest services a control_request claude sent us: it
// answers in-process mcp_message and (-deny-writes) write denials itself, and
// forwards everything else — including the SDK's own servers' mcp_message,
// can_use_tool, and hook_callback — down to the SDK to answer.
func (r *relay) handleUpstreamControlRequest(line []byte) {
	var in streamjson.InControlRequest
	if json.Unmarshal(line, &in) != nil {
		r.writeDownstream(line) // best-effort: forward what we couldn't parse
		return
	}
	switch in.Request.Subtype {
	case streamjson.CtlMCPMessage:
		if r.localServers[in.Request.ServerName] {
			resp, ok := r.host.InProcMCP(in.Request.ServerName, in.Request.Message)
			if !ok {
				r.replyUpstreamError(in.RequestID, "unknown mcp server: "+in.Request.ServerName)
				return
			}
			r.replyUpstreamMCP(in.RequestID, resp)
			return
		}
		r.writeDownstream(line) // SDK-owned server
	case streamjson.CtlCanUseTool:
		if r.denyWrites && isWriteTool(in.Request.ToolName) {
			payload, _ := json.Marshal(streamjson.PermissionDeny{
				Behavior:  "deny",
				Message:   "writes denied by --deny-writes",
				ToolUseID: in.Request.ToolUseID,
			})
			r.replyUpstreamSuccess(in.RequestID, payload)
			return
		}
		r.writeDownstream(line) // SDK owns the permission decision
	default:
		// hook_callback / oauth_token_refresh / elicitation / ...: the SDK is the
		// real host; forward and let it answer.
		r.writeDownstream(line)
	}
}

// maybeLaunched fires claude_launched exactly once, on the first system:init.
func (r *relay) maybeLaunched(line []byte) {
	var m streamjson.SystemMessage
	if json.Unmarshal(line, &m) != nil || m.Subtype != "init" {
		return
	}
	r.launched.Do(func() {
		go func() {
			if err := r.host.SendLogEventAsync("claude_launched", map[string]any{"ide": "vscode", "isFullEditor": true}); err != nil {
				r.logger.Printf("relay: log_event claude_launched: %v", err)
			}
		}()
	})
}

func (r *relay) writeDownstream(line []byte) {
	r.outMu.Lock()
	defer r.outMu.Unlock()
	_, _ = r.out.Write(line)
	if n := len(line); n == 0 || line[n-1] != '\n' {
		_, _ = r.out.Write([]byte{'\n'})
	}
}

func (r *relay) replyUpstreamMCP(reqID string, jsonrpc json.RawMessage) {
	var payload json.RawMessage
	if jsonrpc != nil {
		payload, _ = json.Marshal(map[string]json.RawMessage{"mcp_response": jsonrpc})
	} else {
		payload = json.RawMessage(`{"mcp_response":{"jsonrpc":"2.0","result":{},"id":0}}`)
	}
	r.replyUpstreamSuccess(reqID, payload)
}

func (r *relay) replyUpstreamSuccess(reqID string, payload json.RawMessage) {
	_ = r.host.WriteUpstream(streamjson.ControlResponse{
		Type: streamjson.TypeControlResponse,
		Response: streamjson.ControlResponseBody{
			Subtype:   "success",
			RequestID: reqID,
			Response:  payload,
		},
	})
}

func (r *relay) replyUpstreamError(reqID, msg string) {
	_ = r.host.WriteUpstream(streamjson.ControlResponse{
		Type: streamjson.TypeControlResponse,
		Response: streamjson.ControlResponseBody{
			Subtype:   "error",
			RequestID: reqID,
			Error:     msg,
		},
	})
}
