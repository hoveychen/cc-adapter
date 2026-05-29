package streamjson

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"testing"
	"time"
)

// newTestHost builds a Host wired to an in-memory stdin pipe so we can capture
// the control_request lines it writes, without spawning a real claude.
func newTestHost(t *testing.T) (*Host, *bufio.Reader) {
	t.Helper()
	pr, pw := io.Pipe()
	h := &Host{
		ideServerName:  "ide",
		commServerName: "claude-vscode",
		logger:         log.New(io.Discard, "", 0),
		pending:        make(map[string]chan ControlResponseBody),
		Events:         make(chan Event, 8),
		stdin:          pw,
	}
	return h, bufio.NewReader(pr)
}

// TestSendLogEventWire pins the exact control_request shape SendLogEvent writes
// to the child's stdin, and that it completes once the child acks.
func TestSendLogEventWire(t *testing.T) {
	h, r := newTestHost(t)

	done := make(chan error, 1)
	go func() {
		done <- h.SendLogEvent("claude_launched", map[string]any{"ide": "vscode", "isFullEditor": true})
	}()

	// Read the line SendLogEvent wrote.
	lineCh := make(chan []byte, 1)
	go func() {
		line, err := r.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			t.Errorf("read stdin line: %v", err)
		}
		lineCh <- line
	}()

	var line []byte
	select {
	case line = <-lineCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SendLogEvent to write a line")
	}

	var got map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("unmarshal control_request: %v (raw=%s)", err, line)
	}
	if got["type"] != "control_request" {
		t.Errorf("type = %v, want control_request", got["type"])
	}
	reqID, _ := got["request_id"].(string)
	if reqID == "" {
		t.Errorf("request_id missing: %v", got["request_id"])
	}
	req, ok := got["request"].(map[string]any)
	if !ok {
		t.Fatalf("request not an object: %v", got["request"])
	}
	if req["subtype"] != "mcp_message" {
		t.Errorf("request.subtype = %v, want mcp_message", req["subtype"])
	}
	if req["server_name"] != "claude-vscode" {
		t.Errorf("request.server_name = %v, want claude-vscode", req["server_name"])
	}
	msg, ok := req["message"].(map[string]any)
	if !ok {
		t.Fatalf("request.message not an object: %v", req["message"])
	}
	if msg["jsonrpc"] != "2.0" {
		t.Errorf("message.jsonrpc = %v, want 2.0", msg["jsonrpc"])
	}
	if msg["method"] != "log_event" {
		t.Errorf("message.method = %v, want log_event", msg["method"])
	}
	// Notification: must have NO id.
	if _, hasID := msg["id"]; hasID {
		t.Errorf("message must be a notification (no id), got id=%v", msg["id"])
	}
	params, ok := msg["params"].(map[string]any)
	if !ok {
		t.Fatalf("message.params not an object: %v", msg["params"])
	}
	if params["eventName"] != "claude_launched" {
		t.Errorf("params.eventName = %v, want claude_launched", params["eventName"])
	}
	ed, ok := params["eventData"].(map[string]any)
	if !ok {
		t.Fatalf("params.eventData not an object: %v", params["eventData"])
	}
	if ed["ide"] != "vscode" || ed["isFullEditor"] != true {
		t.Errorf("params.eventData = %v, want {ide:vscode, isFullEditor:true}", ed)
	}

	// Deliver the child's ack so SendLogEvent unblocks.
	ack := ControlResponse{
		Type: TypeControlResponse,
		Response: ControlResponseBody{
			Subtype:   "success",
			RequestID: reqID,
			Response:  json.RawMessage(`{"mcp_response":{"jsonrpc":"2.0","result":{},"id":0}}`),
		},
	}
	ackLine, _ := json.Marshal(ack)
	h.handleLine(ackLine)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SendLogEvent returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendLogEvent did not return after ack")
	}
}

// TestSendLogEventNilData verifies a nil eventData serializes as an empty object.
func TestSendLogEventNilData(t *testing.T) {
	h, r := newTestHost(t)

	done := make(chan error, 1)
	go func() { done <- h.SendLogEvent("some_event", nil) }()

	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		t.Fatalf("read stdin line: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reqID := got["request_id"].(string)
	req := got["request"].(map[string]any)
	msg := req["message"].(map[string]any)
	params := msg["params"].(map[string]any)
	ed, ok := params["eventData"].(map[string]any)
	if !ok {
		t.Fatalf("eventData not an object: %v", params["eventData"])
	}
	if len(ed) != 0 {
		t.Errorf("eventData = %v, want empty object", ed)
	}

	ack := ControlResponse{Type: TypeControlResponse, Response: ControlResponseBody{Subtype: "success", RequestID: reqID}}
	ackLine, _ := json.Marshal(ack)
	h.handleLine(ackLine)
	if err := <-done; err != nil {
		t.Fatalf("SendLogEvent returned error: %v", err)
	}
}
