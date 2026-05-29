package ide

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func dial(t *testing.T, port int, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	url := fmt.Sprintf("ws://127.0.0.1:%d/", port)
	h := http.Header{}
	if token != "" {
		h.Set(authHeader, token)
	}
	return websocket.DefaultDialer.Dial(url, h)
}

func roundtrip(t *testing.T, c *websocket.Conn, id, method string, params any) Message {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	req := Message{JSONRPC: "2.0", ID: json.RawMessage(`"` + id + `"`), Method: method, Params: raw}
	b, _ := json.Marshal(req)
	if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("write: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp Message
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := NewServer(NewHeadlessProvider([]string{"/tmp/ws"}), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.Start()
	t.Cleanup(srv.Stop)
	time.Sleep(50 * time.Millisecond)
	return srv
}

func TestAuthRejectsWrongToken(t *testing.T) {
	srv := newTestServer(t)
	_, resp, err := dial(t, srv.Port(), "wrong-token")
	if err == nil {
		t.Fatal("expected dial to fail with bad token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got resp=%v err=%v", resp, err)
	}
}

func TestInitializeEchoesProtocolAndServerInfo(t *testing.T) {
	srv := newTestServer(t)
	c, _, err := dial(t, srv.Port(), srv.AuthToken())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	resp := roundtrip(t, c, "1", "initialize", InitializeParams{ProtocolVersion: "2025-06-18"})
	var res InitializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("initialize result: %v (raw=%s err=%v)", err, resp.Result, resp.Error)
	}
	if res.ProtocolVersion != "2025-06-18" {
		t.Errorf("protocolVersion not echoed: %q", res.ProtocolVersion)
	}
	if res.ServerInfo.Name != "claude-vscode" || res.ServerInfo.Version != "2.1.156" {
		t.Errorf("serverInfo mismatch: %+v", res.ServerInfo)
	}
}

func TestToolsListHasTwelve(t *testing.T) {
	srv := newTestServer(t)
	c, _, err := dial(t, srv.Port(), srv.AuthToken())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	resp := roundtrip(t, c, "2", "tools/list", nil)
	var res ToolsListResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("tools/list: %v", err)
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
	srv := newTestServer(t)
	c, _, err := dial(t, srv.Port(), srv.AuthToken())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")
	const contents = "hello from claude\n"

	resp := roundtrip(t, c, "3", "tools/call", ToolCallParams{
		Name: "openDiff",
		Arguments: mustJSON(map[string]any{
			"old_file_path":     target,
			"new_file_path":     target,
			"new_file_contents": contents,
			"tab_name":          "out.txt",
		}),
	})
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

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
