package ide

import (
	"encoding/json"
	"testing"
)

func newTestCommServer() *CommServer {
	return NewCommServer("claude-vscode", nil)
}

func TestCommInitializeReportsServerName(t *testing.T) {
	s := newTestCommServer()
	out := s.Handle(request(t, "1", "initialize", InitializeParams{ProtocolVersion: "2025-06-18"}))
	if out == nil {
		t.Fatal("initialize returned nil response")
	}
	var resp Message
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	var res InitializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("initialize result: %v (raw=%s err=%v)", err, resp.Result, resp.Error)
	}
	if res.ProtocolVersion != "2025-06-18" {
		t.Errorf("protocolVersion not echoed: %q", res.ProtocolVersion)
	}
	if res.ServerInfo.Name != "claude-vscode" {
		t.Errorf("serverInfo.name = %q, want claude-vscode", res.ServerInfo.Name)
	}
}

func TestCommToolsListIsEmpty(t *testing.T) {
	s := newTestCommServer()
	out := s.Handle(request(t, "2", "tools/list", nil))
	if out == nil {
		t.Fatal("tools/list returned nil response")
	}
	var resp Message
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	var res ToolsListResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("tools/list: %v (err=%v)", err, resp.Error)
	}
	if len(res.Tools) != 0 {
		t.Fatalf("expected 0 tools, got %d", len(res.Tools))
	}
}

func TestCommPingReturnsEmpty(t *testing.T) {
	s := newTestCommServer()
	out := s.Handle(request(t, "3", "ping", nil))
	if out == nil {
		t.Fatal("ping returned nil response")
	}
	var resp Message
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("ping returned error: %+v", resp.Error)
	}
}

func TestCommNotificationReturnsNil(t *testing.T) {
	s := newTestCommServer()
	notif := Message{JSONRPC: "2.0", Method: "experiment_gates"}
	b, err := json.Marshal(notif)
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	if out := s.Handle(b); out != nil {
		t.Fatalf("expected nil for notification, got %s", out)
	}
}
