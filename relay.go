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

	"github.com/hoveychen/cc-adapter/internal/ide"
	"github.com/hoveychen/cc-adapter/internal/streamjson"
)

// runRelay spawns the real claude in relay mode and bridges the downstream SDK
// (os.Stdin/os.Stdout) to it until claude exits, returning the child's exit
// code. The Host carries the vscode env/baseline as in every other mode; what
// differs is that the Host does no decoding and the relay owns all routing. The
// RawSink closure captures r, which is assigned before Start launches the read
// goroutine that invokes it.
func runRelay(ctx context.Context, claudePath string, mcpServer *ide.MCPServer, extra []string, opts cliOpts, logger *log.Logger, emitTelemetry func(string, map[string]any)) int {
	var r *relay
	host := streamjson.NewHost(streamjson.Config{
		ClaudePath:    claudePath,
		MCPServer:     mcpServer,
		IDEServerName: "ide",
		ExtraArgs:     extra,
		Logger:        logger,
		RelayMode:     true,
		RawSink:       func(line []byte) { r.onUpstreamLine(line) },
	})
	r = newRelay(host, opts.denyWrites, logger)

	if err := host.Start(ctx); err != nil {
		logger.Printf("%v", err)
		emitTelemetry("claude_spawn_failed", map[string]any{"phase": "spawn"})
		return 1
	}
	logger.Printf("relay mode: SDK-driven stream-json bridge (entrypoint=claude-vscode)")

	code := r.run(ctx, os.Stdin)
	if code != 0 && ctx.Err() == nil {
		emitTelemetry("claude_subprocess_exited_unexpectedly", map[string]any{"exit_code": code})
	}
	return code
}

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

	// firstResult is closed when claude emits its first result frame (or when the
	// upstream read loop ends, as a safety net). Because cc-adapter always injects
	// in-process MCP servers (ide + claude-vscode), the control protocol needs
	// claude's stdin to stay open for the whole turn so the relay can answer
	// claude's mcp_message / can_use_tool round-trips and inject claude_launched —
	// so the relay must not close claude's stdin when the SDK closes its own until
	// the turn has produced a result. This mirrors the SDK's own
	// wait_for_result_and_end_input discipline.
	firstResult chan struct{}
	frOnce      sync.Once
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
		firstResult:  make(chan struct{}),
	}
}

// signalFirstResult unblocks run()'s gate on the claude stdin close. Idempotent.
func (r *relay) signalFirstResult() { r.frOnce.Do(func() { close(r.firstResult) }) }

// run pumps the downstream SDK stdin into the upstream claude until EOF, then
// signals end-of-input and waits for claude to exit, returning its exit code.
// The upstream→downstream direction is driven by the Host read goroutine calling
// onUpstreamLine (wired as RawSink), so it runs concurrently with this pump.
func (r *relay) run(ctx context.Context, stdin io.Reader) int {
	// claudeDead closes when the Host closes Events, i.e. when claude's stdout
	// reaches EOF — claude has exited for whatever reason. (In relay mode nothing
	// is pushed onto Events, so this drain just waits for that close.) Closing it
	// also unblocks the firstResult gate as a safety net.
	claudeDead := make(chan struct{})
	go func() {
		for range r.host.Events {
		}
		close(claudeDead)
		r.signalFirstResult()
	}()

	// Pump the SDK's stdin upstream on its own goroutine so claude's death or an
	// OS signal can drive run() to return WITHOUT waiting for this blocking read.
	// A blocking Read on os.Stdin is not interrupted by ctx cancellation, so if we
	// awaited pumpDownstream inline, cc-adapter would hang whenever the SDK keeps
	// its stdin open past claude's exit (the lifecycle-follow bug) or past a signal
	// (the no-prompt-exit bug). The leaked goroutine is harmless: run()'s caller
	// os.Exits right after.
	pumpDone := make(chan struct{})
	go func() {
		if err := r.pumpDownstream(stdin); err != nil {
			r.logger.Printf("relay: downstream read: %v", err)
		}
		close(pumpDone)
	}()

	select {
	case <-pumpDone:
		// The SDK closed its downstream stdin (normal end-of-input). Do NOT close
		// claude's stdin yet: because we always inject in-process MCP servers, the
		// control protocol needs it open until the turn produces a result (so
		// claude's mcp_message init and any can_use_tool round-trips can be
		// answered, and claude_launched lands). firstResult is also released if
		// claude dies first, so this never hangs.
		<-r.firstResult
		_ = r.host.CloseInput()
	case <-claudeDead:
		// claude exited on its own (or was killed externally). Follow it down
		// immediately — do not wait on a downstream stdin the SDK may keep open.
	case <-ctx.Done():
		// cc-adapter received an OS signal. The Host's cmd.Cancel forwards it to
		// claude; just tear down and let Wait collect the child's exit code.
	}
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
		// claude_launched log_event; a result frame releases the stdin-close gate.
		r.writeDownstream(line)
		switch t.Type {
		case streamjson.TypeSystem:
			r.maybeLaunched(line)
		case streamjson.TypeResult:
			r.signalFirstResult()
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
// It is sent synchronously on the upstream read goroutine — not deferred to a
// separate goroutine — so the single small frame reaches claude's stdin while
// it is still open. (A goroutine raced CloseInput on short one-shot queries and
// lost: the session ended and stdin closed before the deferred write ran,
// dropping the fingerprint event.) system:init arrives right after the
// handshake, long before the turn completes, so stdin is reliably open here.
func (r *relay) maybeLaunched(line []byte) {
	var m streamjson.SystemMessage
	if json.Unmarshal(line, &m) != nil || m.Subtype != "init" {
		return
	}
	r.launched.Do(func() {
		if err := r.host.SendLogEventAsync("claude_launched", map[string]any{"ide": "vscode", "isFullEditor": true}); err != nil {
			r.logger.Printf("relay: log_event claude_launched: %v", err)
		} else {
			r.logger.Printf("relay: sent log_event claude_launched")
		}
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
