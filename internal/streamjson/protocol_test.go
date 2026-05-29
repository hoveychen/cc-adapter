package streamjson

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestUserInputWire pins the exact stdin envelope shape claude expects for a
// user turn (reverse-engineered from the SDK transport).
func TestUserInputWire(t *testing.T) {
	b, err := json.Marshal(NewUserText("sess-1", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "user" {
		t.Errorf("type = %v, want user", got["type"])
	}
	if got["session_id"] != "sess-1" {
		t.Errorf("session_id = %v, want sess-1", got["session_id"])
	}
	// parent_tool_use_id must be present and null (not omitted).
	if v, ok := got["parent_tool_use_id"]; !ok || v != nil {
		t.Errorf("parent_tool_use_id = %v (present=%v), want null/present", got["parent_tool_use_id"], ok)
	}
	msg, ok := got["message"].(map[string]any)
	if !ok {
		t.Fatalf("message not an object: %v", got["message"])
	}
	if msg["role"] != "user" || msg["content"] != "hello" {
		t.Errorf("message = %v, want {role:user, content:hello}", msg)
	}
}

// TestPermissionAllowWire pins the can_use_tool allow reply shape.
func TestPermissionAllowWire(t *testing.T) {
	b, _ := json.Marshal(PermissionAllow{
		Behavior:     "allow",
		UpdatedInput: json.RawMessage(`{"command":"ls"}`),
		ToolUseID:    "toolu_1",
	})
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["behavior"] != "allow" {
		t.Errorf("behavior = %v, want allow", got["behavior"])
	}
	if got["toolUseID"] != "toolu_1" {
		t.Errorf("toolUseID = %v, want toolu_1", got["toolUseID"])
	}
}

// TestDecodeAssistant verifies we extract text + tool_use from an assistant line.
func TestDecodeAssistant(t *testing.T) {
	line := []byte(`{"type":"assistant","session_id":"s","message":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`)
	var m AgentMessage
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Message.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(m.Message.Content))
	}
	if m.Message.Content[0].Type != "text" || m.Message.Content[0].Text != "hi" {
		t.Errorf("block0 = %+v", m.Message.Content[0])
	}
	if m.Message.Content[1].Type != "tool_use" || m.Message.Content[1].Name != "Bash" {
		t.Errorf("block1 = %+v", m.Message.Content[1])
	}
}

// TestDecodeResult verifies result fields decode.
func TestDecodeResult(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","result":"done","num_turns":3,"total_cost_usd":0.012,"is_error":false,"session_id":"s"}`)
	var m ResultMessage
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	if m.Subtype != "success" || m.Result != "done" || m.NumTurns != 3 {
		t.Errorf("result = %+v", m)
	}
}

// TestDecodeCanUseTool verifies an inbound can_use_tool control_request decodes.
func TestDecodeCanUseTool(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"toolu_9"}}`)
	var in InControlRequest
	if err := json.Unmarshal(line, &in); err != nil {
		t.Fatal(err)
	}
	if in.Request.Subtype != CtlCanUseTool {
		t.Errorf("subtype = %v, want can_use_tool", in.Request.Subtype)
	}
	if in.Request.ToolName != "Bash" || in.Request.ToolUseID != "toolu_9" {
		t.Errorf("request = %+v", in.Request)
	}
}

// TestControlResponseWire pins our control_response success frame.
func TestControlResponseWire(t *testing.T) {
	b, _ := json.Marshal(ControlResponse{
		Type: TypeControlResponse,
		Response: ControlResponseBody{
			Subtype:   "success",
			RequestID: "r1",
			Response:  json.RawMessage(`{"behavior":"allow"}`),
		},
	})
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "control_response" {
		t.Errorf("type = %v", got["type"])
	}
	resp := got["response"].(map[string]any)
	if resp["subtype"] != "success" || resp["request_id"] != "r1" {
		t.Errorf("response = %v", resp)
	}
}

func TestMergeSDKMcpServers_AppendsAndDedups(t *testing.T) {
	// SDK declared one server "myserver"; cc-adapter injects ide + claude-vscode.
	body := json.RawMessage(`{"subtype":"initialize","hooks":{},"sdkMcpServers":["myserver"]}`)
	out := MergeSDKMcpServers(body, "ide", "claude-vscode")

	var got struct {
		Subtype       string          `json:"subtype"`
		Hooks         json.RawMessage `json:"hooks"`
		SDKMcpServers []string        `json:"sdkMcpServers"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	if got.Subtype != "initialize" {
		t.Fatalf("subtype = %q", got.Subtype)
	}
	if string(got.Hooks) != "{}" {
		t.Fatalf("hooks not preserved: %s", got.Hooks)
	}
	want := []string{"myserver", "ide", "claude-vscode"}
	if !reflect.DeepEqual(got.SDKMcpServers, want) {
		t.Fatalf("sdkMcpServers = %v, want %v", got.SDKMcpServers, want)
	}

	// Idempotent: re-merging the same names adds nothing.
	out2 := MergeSDKMcpServers(out, "ide", "claude-vscode")
	json.Unmarshal(out2, &got)
	if !reflect.DeepEqual(got.SDKMcpServers, want) {
		t.Fatalf("non-idempotent: %v", got.SDKMcpServers)
	}
}

func TestMergeSDKMcpServers_EmptyBody(t *testing.T) {
	out := MergeSDKMcpServers(nil, "ide", "claude-vscode")
	var got struct {
		Subtype       string   `json:"subtype"`
		SDKMcpServers []string `json:"sdkMcpServers"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Subtype != "initialize" {
		t.Fatalf("subtype = %q, want initialize", got.Subtype)
	}
	if !reflect.DeepEqual(got.SDKMcpServers, []string{"ide", "claude-vscode"}) {
		t.Fatalf("sdkMcpServers = %v", got.SDKMcpServers)
	}
}
