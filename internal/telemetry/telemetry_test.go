package telemetry

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// capturedRequest holds what the test server saw.
type capturedRequest struct {
	url     string
	headers http.Header
	body    []byte
}

// newTestServer returns a server that records the request into got and a client
// pointed at it. It rewrites the request so PostInternalEvent's real URL path is
// still observable.
func newTestServer(t *testing.T, got *capturedRequest, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		b, _ := io.ReadAll(r.Body)
		got.url = r.URL.Path
		got.headers = r.Header.Clone()
		got.body = b
		w.WriteHeader(http.StatusOK)
	}))
}

// installClientRedirect makes httpClient send every request to srv regardless of
// the URL PostInternalEvent constructs (so we can assert on the real path/body).
func installClientRedirect(t *testing.T, srvURL string) {
	t.Helper()
	prev := httpClient
	t.Cleanup(func() { httpClient = prev })
	httpClient = &http.Client{
		Transport: redirectTransport{target: srvURL},
	}
}

type redirectTransport struct{ target string }

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Preserve the original path; only swap host/scheme to the test server.
	origPath := req.URL.Path
	newReq := req.Clone(req.Context())
	u := req.URL
	// target looks like http://127.0.0.1:PORT
	parsed := rt.target + origPath
	if pr, err := req.URL.Parse(parsed); err == nil {
		newReq.URL = pr
		newReq.Host = pr.Host
	}
	_ = u
	return http.DefaultTransport.RoundTrip(newReq)
}

func TestPostInternalEvent_Shape(t *testing.T) {
	// Ensure no opt-out vars leak in from the environment.
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "")
	t.Setenv("DISABLE_TELEMETRY", "")
	t.Setenv("DO_NOT_TRACK", "")

	var got capturedRequest
	var hits int32
	srv := newTestServer(t, &got, &hits)
	defer srv.Close()
	installClientRedirect(t, srv.URL)

	PostInternalEvent("claude_spawn_failed", map[string]any{"phase": "spawn"}, "acct-123")

	if hits != 1 {
		t.Fatalf("expected exactly 1 POST, got %d", hits)
	}
	if got.url != "/api/event_logging/v2/batch" {
		t.Fatalf("url path = %q", got.url)
	}
	if ct := got.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if sn := got.headers.Get("x-service-name"); sn != "claude-code" {
		t.Errorf("x-service-name = %q", sn)
	}
	if auth := got.headers.Get("Authorization"); auth != "" {
		t.Errorf("expected NO Authorization header, got %q", auth)
	}

	// Decode the body envelope.
	var env struct {
		Events []struct {
			EventType string         `json:"event_type"`
			EventData map[string]any `json:"event_data"`
		} `json:"events"`
	}
	if err := json.Unmarshal(got.body, &env); err != nil {
		t.Fatalf("decode body: %v\nbody=%s", err, got.body)
	}
	if len(env.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(env.Events))
	}
	ev := env.Events[0]
	if ev.EventType != "ClaudeCodeInternalEvent" {
		t.Errorf("event_type = %q", ev.EventType)
	}
	x := ev.EventData
	if x["event_name"] != "tengu_vscode_claude_spawn_failed" {
		t.Errorf("event_name = %v", x["event_name"])
	}
	if x["entrypoint"] != "claude-vscode" {
		t.Errorf("entrypoint = %v", x["entrypoint"])
	}
	if x["client_type"] != "claude-vscode" {
		t.Errorf("client_type = %v", x["client_type"])
	}
	if x["event_id"] == "" || x["event_id"] == nil {
		t.Error("event_id missing")
	}
	if x["client_timestamp"] == nil {
		t.Error("client_timestamp missing")
	}
	if x["session_id"] == "" || x["session_id"] == nil {
		t.Error("session_id missing")
	}

	// auth.account_uuid present when accountUUID != "".
	auth, ok := x["auth"].(map[string]any)
	if !ok || auth["account_uuid"] != "acct-123" {
		t.Errorf("auth = %v, want account_uuid acct-123", x["auth"])
	}

	// env block.
	envBlock, ok := x["env"].(map[string]any)
	if !ok {
		t.Fatalf("env block missing: %v", x["env"])
	}
	if envBlock["version"] != "2.1.156" {
		t.Errorf("env.version = %v", envBlock["version"])
	}
	if envBlock["platform"] == nil || envBlock["arch"] == nil {
		t.Errorf("env.platform/arch missing: %v", envBlock)
	}

	// additional_metadata must base64-decode to JSON containing our eventData
	// plus ide/extensionVersion.
	amStr, _ := x["additional_metadata"].(string)
	raw, err := base64.StdEncoding.DecodeString(amStr)
	if err != nil {
		t.Fatalf("additional_metadata not base64: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("additional_metadata not JSON: %v", err)
	}
	if meta["phase"] != "spawn" {
		t.Errorf("eventData not merged into metadata: %v", meta)
	}
	if meta["extensionVersion"] != "2.1.156" {
		t.Errorf("extensionVersion in metadata = %v", meta["extensionVersion"])
	}
	if _, ok := meta["ide"].(map[string]any); !ok {
		t.Errorf("ide block missing from metadata: %v", meta["ide"])
	}
}

func TestPostInternalEvent_OmitsAuthWhenAnonymous(t *testing.T) {
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "")
	t.Setenv("DISABLE_TELEMETRY", "")
	t.Setenv("DO_NOT_TRACK", "")

	var got capturedRequest
	var hits int32
	srv := newTestServer(t, &got, &hits)
	defer srv.Close()
	installClientRedirect(t, srv.URL)

	PostInternalEvent("evt", nil, "")

	if hits != 1 {
		t.Fatalf("expected 1 POST, got %d", hits)
	}
	var env struct {
		Events []struct {
			EventData map[string]any `json:"event_data"`
		} `json:"events"`
	}
	if err := json.Unmarshal(got.body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := env.Events[0].EventData["auth"]; present {
		t.Errorf("auth must be omitted when accountUUID empty, got %v", env.Events[0].EventData["auth"])
	}
}

func TestPostInternalEvent_GateBlocks(t *testing.T) {
	for _, v := range []string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
		"DISABLE_TELEMETRY",
		"DO_NOT_TRACK",
	} {
		t.Run(v, func(t *testing.T) {
			// Clear all three first, then set the one under test.
			t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "")
			t.Setenv("DISABLE_TELEMETRY", "")
			t.Setenv("DO_NOT_TRACK", "")
			t.Setenv(v, "1")

			var got capturedRequest
			var hits int32
			srv := newTestServer(t, &got, &hits)
			defer srv.Close()
			installClientRedirect(t, srv.URL)

			PostInternalEvent("evt", map[string]any{"x": 1}, "acct")

			if hits != 0 {
				t.Fatalf("expected 0 POSTs when %s set, got %d", v, hits)
			}
		})
	}
}

func TestNodePlatformArch(t *testing.T) {
	// Just assert the mappings produce Node-style strings on this host.
	p := nodePlatform()
	switch p {
	case "darwin", "linux", "win32":
	default:
		t.Errorf("unexpected platform mapping: %q", p)
	}
	a := nodeArch()
	switch a {
	case "arm64", "x64", "ia32":
	default:
		t.Errorf("unexpected arch mapping: %q", a)
	}
}

func TestUUIDV4Shape(t *testing.T) {
	id := uuidV4()
	if len(id) != 36 {
		t.Fatalf("uuid length = %d (%q)", len(id), id)
	}
	if id[14] != '4' {
		t.Errorf("version nibble not 4: %q", id)
	}
}
