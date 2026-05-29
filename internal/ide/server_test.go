package ide

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// request builds a JSON-RPC request line as the host would hand it to
// MCPServer.Handle (the message extracted from a control mcp_message frame).
func request(t *testing.T, id, method string, params any) json.RawMessage {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		raw = b
	}
	msg := Message{JSONRPC: "2.0", ID: json.RawMessage(`"` + id + `"`), Method: method, Params: raw}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return b
}

func newTestServer() *MCPServer {
	return NewMCPServer(NewHeadlessProvider([]string{"/tmp/ws"}), nil)
}

func TestInitializeEchoesProtocolAndServerInfo(t *testing.T) {
	s := newTestServer()
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
	if res.ServerInfo.Name != serverName || res.ServerInfo.Version != serverVersion {
		t.Errorf("serverInfo mismatch: %+v", res.ServerInfo)
	}
}

func TestToolsListHasTwelve(t *testing.T) {
	s := newTestServer()
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
	if len(res.Tools) != 12 {
		t.Fatalf("expected 12 tools, got %d", len(res.Tools))
	}
	want := map[string]bool{
		"openDiff": false, "getDiagnostics": false, "getOpenEditors": false,
		"getWorkspaceFolders": false, "getCurrentSelection": false, "getLatestSelection": false,
		"openFile": false, "close_tab": false, "closeAllDiffTabs": false,
		"checkDocumentDirty": false, "saveDocument": false, "executeCode": false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool %q", tool.Name)
		}
		want[tool.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing tool %q", name)
		}
	}
}

func TestOpenDiffWritesFileAndAutoAccepts(t *testing.T) {
	s := newTestServer()

	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")
	const contents = "hello from claude\n"

	out := s.Handle(request(t, "3", "tools/call", ToolCallParams{
		Name: "openDiff",
		Arguments: mustJSON(map[string]any{
			"old_file_path":     target,
			"new_file_path":     target,
			"new_file_contents": contents,
			"tab_name":          "out.txt",
		}),
	}))
	if out == nil {
		t.Fatal("tools/call returned nil response")
	}
	var resp Message
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	var res ToolCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("openDiff result: %v (err=%v)", err, resp.Error)
	}
	if len(res.Content) < 1 || res.Content[0].Text != "FILE_SAVED" {
		t.Fatalf("expected FILE_SAVED, got %+v", res.Content)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != contents {
		t.Fatalf("file contents mismatch: %q", string(got))
	}
}

func TestNotificationReturnsNil(t *testing.T) {
	s := newTestServer()
	// A notification has a method but no id.
	notif := Message{JSONRPC: "2.0", Method: "notifications/initialized"}
	b, err := json.Marshal(notif)
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	if out := s.Handle(b); out != nil {
		t.Fatalf("expected nil for notification, got %s", out)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
