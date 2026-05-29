package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hoveychen/cc-adapter/internal/ide"
	"github.com/hoveychen/cc-adapter/internal/streamjson"
)

// TestMain lets this test binary double as a fake `claude` child: when spawned
// with CCA_FAKE_CLAUDE=1 it speaks the stream-json control protocol instead of
// running the suite. The fake-claude wrapper (see the test below) re-execs this
// binary so the real code path (Host.Start exec → readLoop → runPrint) is
// exercised end to end against a deterministic child.
func TestMain(m *testing.M) {
	if os.Getenv("CCA_FAKE_CLAUDE") == "1" {
		os.Exit(fakeClaudeMain())
	}
	os.Exit(m.Run())
}

// fakeClaudeMain impersonates the real claude binary for one non-interactive
// turn. It mirrors the ordering the real claude exhibits with
// MCP_CONNECTION_NONBLOCKING: after the user turn arrives it asks the host
// (cc-adapter) to initialize the "ide" SDK MCP server over the control channel,
// then waits for the host's mcp_response. The emitted result encodes whether
// that handshake completed BEFORE the host closed our stdin:
//
//	MCP_OK                 — host answered the mcp_message before closing stdin
//	MCP_STDIN_CLOSED_EARLY — stdin hit EOF before the answer arrived
//
// The second outcome is the bug: runPrint must not CloseInput until the turn
// has produced a result, or the SDK MCP server handshake is severed and claude
// marks the server failed (ide_tools=0).
func fakeClaudeMain() int {
	out := bufio.NewWriter(os.Stdout)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}

	// Safety net: never hang the parent test if the protocol diverges.
	go func() {
		time.Sleep(8 * time.Second)
		emit(map[string]any{"type": "result", "subtype": "success", "result": "FAKE_TIMEOUT", "session_id": "s1"})
		os.Exit(0)
	}()

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	sentMCP := false
	for sc.Scan() {
		var probe struct {
			Type    string `json:"type"`
			ReqID   string `json:"request_id"`
			Request struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
			Response struct {
				RequestID string `json:"request_id"`
			} `json:"response"`
		}
		if json.Unmarshal(sc.Bytes(), &probe) != nil {
			continue
		}
		switch probe.Type {
		case "control_request":
			if probe.Request.Subtype == "initialize" {
				emit(map[string]any{
					"type":     "control_response",
					"response": map[string]any{"subtype": "success", "request_id": probe.ReqID, "response": map[string]any{}},
				})
			}
		case "user":
			// The prompt turn landed; kick off the SDK MCP server handshake.
			if !sentMCP {
				sentMCP = true
				jsonrpc := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
				emit(map[string]any{
					"type":       "control_request",
					"request_id": "fake_1",
					"request":    map[string]any{"subtype": "mcp_message", "server_name": "ide", "message": jsonrpc},
				})
			}
		case "control_response":
			if probe.Response.RequestID == "fake_1" {
				emit(map[string]any{"type": "result", "subtype": "success", "result": "MCP_OK", "session_id": "s1"})
				return 0
			}
		}
	}
	// stdin reached EOF (the host closed it). If we had a handshake in flight,
	// cc-adapter closed our stdin too early to answer it.
	marker := "NO_PROMPT"
	if sentMCP {
		marker = "MCP_STDIN_CLOSED_EARLY"
	}
	emit(map[string]any{"type": "result", "subtype": "success", "result": marker, "session_id": "s1"})
	return 0
}

// TestRunPrint_DeliversMCPHandshakeBeforeClosingStdin proves the print path
// keeps claude's stdin open until the turn produces a result, so claude's SDK
// MCP server initialization (the mcp_message round-trip cc-adapter answers
// in-process) completes instead of being severed by an early CloseInput.
func TestRunPrint_DeliversMCPHandshakeBeforeClosingStdin(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// A wrapper that re-execs this test binary in fake-claude mode without
	// setting CCA_FAKE_CLAUDE on the test process itself (which would recurse).
	wrapper := filepath.Join(t.TempDir(), "fake-claude.sh")
	script := "#!/bin/sh\nexport CCA_FAKE_CLAUDE=1\nexec " + strconv.Quote(self) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	mcpServer := ide.NewMCPServer(ide.NewHeadlessProvider(nil), logger)
	ps := newPrintState("text")
	var buf bytes.Buffer
	ps.out = &buf

	host := streamjson.NewHost(streamjson.Config{
		ClaudePath:    wrapper,
		MCPServer:     mcpServer,
		IDEServerName: "ide",
		Permission:    func(string, json.RawMessage) (bool, string) { return true, "" },
		Logger:        logger,
		RawSink:       ps.sink,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := host.Start(ctx); err != nil {
		t.Fatalf("host.Start: %v", err)
	}

	turnDone := make(chan *streamjson.ResultMessage, 1)
	go func() {
		for ev := range host.Events {
			if ev.Kind == streamjson.EventResult {
				select {
				case turnDone <- ev.Result:
				default:
				}
			}
		}
		select {
		case turnDone <- nil:
		default:
		}
	}()

	if err := host.Initialize(); err != nil {
		t.Fatalf("host.Initialize: %v", err)
	}

	opts := parseArgs([]string{"-p", "hello"})
	if err := runPrint(host, opts, ps, turnDone, logger); err != nil {
		t.Fatalf("runPrint: %v", err)
	}
	host.Wait()

	got := buf.String()
	if got == "MCP_STDIN_CLOSED_EARLY\n" {
		t.Fatalf("runPrint closed claude's stdin before the SDK MCP handshake completed (severs ide_tools); got %q", got)
	}
	if got != "MCP_OK\n" {
		t.Fatalf("unexpected result %q, want %q", got, "MCP_OK\n")
	}
}
