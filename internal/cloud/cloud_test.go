package cloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// capturedRequest records what the fake server saw.
type capturedRequest struct {
	method string
	path   string
	header http.Header
}

// newFakeServer spins up an httptest server that records the request and replies
// with the given status + body, and returns the server plus a pointer to the
// captured request. It also points the package httpClient and base URL at the
// server and injects fake auth headers so no keychain/network is touched.
func newFakeServer(t *testing.T, status int, body string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.header = r.Header.Clone()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	SetHTTPClient(srv.Client())
	SetAuthHeaders(func() (map[string]string, error) {
		return map[string]string{
			"Authorization":  "Bearer fake-access-token",
			"anthropic-beta": "oauth-2025-04-20",
		}, nil
	})
	t.Cleanup(func() {
		srv.Close()
		SetHTTPClient(&http.Client{})
		SetAuthHeaders(nil)
		SetOrgUUIDLoader(nil)
	})
	return srv, captured
}

func TestUsage(t *testing.T) {
	_, captured := newFakeServer(t, 200, `{"usage":42}`)

	body, err := Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if string(body) != `{"usage":42}` {
		t.Errorf("body = %q, want %q", body, `{"usage":42}`)
	}
	if captured.method != http.MethodGet {
		t.Errorf("method = %q, want GET", captured.method)
	}
	if captured.path != "/api/oauth/usage" {
		t.Errorf("path = %q, want /api/oauth/usage", captured.path)
	}
	if got := captured.header.Get("Authorization"); got != "Bearer fake-access-token" {
		t.Errorf("Authorization = %q, want Bearer fake-access-token", got)
	}
	if got := captured.header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q, want oauth-2025-04-20", got)
	}
	if got := captured.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestUsageNonSubscriptionBodyPassedThrough(t *testing.T) {
	// A non-2xx error body must be returned verbatim, not turned into a Go error.
	_, _ = newFakeServer(t, 403, `{"error":"not a subscription account"}`)

	body, err := Usage()
	if err != nil {
		t.Fatalf("Usage returned error for non-2xx, want body passthrough: %v", err)
	}
	if string(body) != `{"error":"not a subscription account"}` {
		t.Errorf("body = %q, want error message passed through", body)
	}
}

func TestProfile(t *testing.T) {
	_, captured := newFakeServer(t, 200, `{"organization":{"uuid":"org-123"},"account":{"email":"x@y.z"}}`)

	orgUUID, raw, err := Profile()
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if orgUUID != "org-123" {
		t.Errorf("orgUUID = %q, want org-123", orgUUID)
	}
	if len(raw) == 0 {
		t.Errorf("raw body empty")
	}
	if captured.path != "/api/oauth/profile" {
		t.Errorf("path = %q, want /api/oauth/profile", captured.path)
	}
	if got := captured.header.Get("Authorization"); got != "Bearer fake-access-token" {
		t.Errorf("Authorization = %q, want Bearer fake-access-token", got)
	}
	if got := captured.header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q, want oauth-2025-04-20", got)
	}
}

func TestRemoteSessionsWithStoredOrgUUID(t *testing.T) {
	_, captured := newFakeServer(t, 200, `{"sessions":[]}`)
	// Stored org UUID present: no profile lookup should be needed.
	SetOrgUUIDLoader(func() string { return "stored-org-999" })

	body, err := RemoteSessions()
	if err != nil {
		t.Fatalf("RemoteSessions: %v", err)
	}
	if string(body) != `{"sessions":[]}` {
		t.Errorf("body = %q", body)
	}
	if captured.path != "/v1/sessions" {
		t.Errorf("path = %q, want /v1/sessions", captured.path)
	}
	if got := captured.header.Get("Authorization"); got != "Bearer fake-access-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := captured.header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
	if got := captured.header.Get("anthropic-beta"); got != "ccr-byoc-2025-07-29" {
		t.Errorf("anthropic-beta = %q, want ccr-byoc-2025-07-29", got)
	}
	if got := captured.header.Get("x-organization-uuid"); got != "stored-org-999" {
		t.Errorf("x-organization-uuid = %q, want stored-org-999", got)
	}
	if got := captured.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestRemoteSessionsFetchesProfileWhenNoStoredOrgUUID(t *testing.T) {
	// The same server answers both /api/oauth/profile and /v1/sessions; record
	// the sequence of paths to prove profile is fetched first.
	var paths []string
	var lastSessionsHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/oauth/profile":
			_, _ = w.Write([]byte(`{"organization":{"uuid":"profile-org-777"}}`))
		case "/v1/sessions":
			lastSessionsHeader = r.Header.Clone()
			_, _ = w.Write([]byte(`{"sessions":["s1"]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(func() {
		srv.Close()
		SetHTTPClient(&http.Client{})
		SetAuthHeaders(nil)
		SetOrgUUIDLoader(nil)
	})
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	SetHTTPClient(srv.Client())
	SetAuthHeaders(func() (map[string]string, error) {
		return map[string]string{
			"Authorization":  "Bearer fake-access-token",
			"anthropic-beta": "oauth-2025-04-20",
		}, nil
	})
	// No stored org UUID -> must fall back to profile.
	SetOrgUUIDLoader(func() string { return "" })

	body, err := RemoteSessions()
	if err != nil {
		t.Fatalf("RemoteSessions: %v", err)
	}
	if string(body) != `{"sessions":["s1"]}` {
		t.Errorf("body = %q", body)
	}
	if len(paths) != 2 || paths[0] != "/api/oauth/profile" || paths[1] != "/v1/sessions" {
		t.Fatalf("request sequence = %v, want [/api/oauth/profile /v1/sessions]", paths)
	}
	if got := lastSessionsHeader.Get("x-organization-uuid"); got != "profile-org-777" {
		t.Errorf("x-organization-uuid = %q, want profile-org-777 (from profile fallback)", got)
	}
}

func TestSessionDetail(t *testing.T) {
	_, captured := newFakeServer(t, 200, `{"id":"sess-abc","title":"t"}`)
	SetOrgUUIDLoader(func() string { return "org-1" })

	body, err := SessionDetail("sess-abc")
	if err != nil {
		t.Fatalf("SessionDetail: %v", err)
	}
	if string(body) != `{"id":"sess-abc","title":"t"}` {
		t.Errorf("body = %q", body)
	}
	if captured.path != "/v1/sessions/sess-abc" {
		t.Errorf("path = %q, want /v1/sessions/sess-abc", captured.path)
	}
	if got := captured.header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
	if got := captured.header.Get("anthropic-beta"); got != "ccr-byoc-2025-07-29" {
		t.Errorf("anthropic-beta = %q, want ccr-byoc-2025-07-29", got)
	}
	if got := captured.header.Get("x-organization-uuid"); got != "org-1" {
		t.Errorf("x-organization-uuid = %q, want org-1", got)
	}
	if got := captured.header.Get("Authorization"); got != "Bearer fake-access-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestSessionDetail404PassedThrough(t *testing.T) {
	// A 404 body must be returned verbatim, not turned into a Go error.
	_, _ = newFakeServer(t, 404, `{"error":"not found"}`)
	SetOrgUUIDLoader(func() string { return "org-1" })

	body, err := SessionDetail("missing")
	if err != nil {
		t.Fatalf("SessionDetail returned error for 404, want passthrough: %v", err)
	}
	if string(body) != `{"error":"not found"}` {
		t.Errorf("body = %q, want 404 body passed through", body)
	}
}

func TestSessionIngress(t *testing.T) {
	_, captured := newFakeServer(t, 200, `{"loglines":["a","b"]}`)
	SetOrgUUIDLoader(func() string { return "org-2" })

	body, err := SessionIngress("sess-xyz")
	if err != nil {
		t.Fatalf("SessionIngress: %v", err)
	}
	if string(body) != `{"loglines":["a","b"]}` {
		t.Errorf("body = %q", body)
	}
	if captured.path != "/v1/session_ingress/session/sess-xyz" {
		t.Errorf("path = %q, want /v1/session_ingress/session/sess-xyz", captured.path)
	}
	if got := captured.header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := captured.header.Get("x-organization-uuid"); got != "org-2" {
		t.Errorf("x-organization-uuid = %q, want org-2", got)
	}
}

// TestTeleportEventsPagination drives a fake server returning 3 pages: pages 0
// and 1 carry events plus a next_cursor, the final page has events and no
// cursor. It asserts the aggregated event count equals the sum of all pages,
// that the cursor returned by page N is sent on page N+1, and that limit=1000
// rides every request.
func TestTeleportEventsPagination(t *testing.T) {
	var sawCursors []string
	var sawLimits []string
	pageBodies := []string{
		`{"data":[{"payload":{"e":1}},{"payload":{"e":2}}],"next_cursor":"c1"}`,
		`{"data":[{"payload":{"e":3}}],"next_cursor":"c2"}`,
		`{"data":[{"payload":{"e":4}},{"payload":{"e":5}}]}`, // no next_cursor -> last page
	}
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/code/sessions/s1/teleport-events" {
			w.WriteHeader(404)
			return
		}
		sawCursors = append(sawCursors, r.URL.Query().Get("cursor"))
		sawLimits = append(sawLimits, r.URL.Query().Get("limit"))
		idx := page
		if idx >= len(pageBodies) {
			idx = len(pageBodies) - 1
		}
		_, _ = w.Write([]byte(pageBodies[idx]))
		page++
	}))
	t.Cleanup(func() {
		srv.Close()
		SetHTTPClient(&http.Client{})
		SetAuthHeaders(nil)
		SetOrgUUIDLoader(nil)
	})
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	SetHTTPClient(srv.Client())
	SetAuthHeaders(func() (map[string]string, error) {
		return map[string]string{"Authorization": "Bearer fake-access-token"}, nil
	})
	SetOrgUUIDLoader(func() string { return "org-3" })

	body, err := TeleportEvents("s1")
	if err != nil {
		t.Fatalf("TeleportEvents: %v", err)
	}

	var events []map[string]int
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatalf("unmarshal aggregated events: %v (body=%s)", err, body)
	}
	if len(events) != 5 {
		t.Errorf("aggregated event count = %d, want 5 (2+1+2)", len(events))
	}

	// Three requests should have been made: cursor "" then "c1" then "c2".
	wantCursors := []string{"", "c1", "c2"}
	if len(sawCursors) != len(wantCursors) {
		t.Fatalf("request count = %d, want %d (cursors seen: %v)", len(sawCursors), len(wantCursors), sawCursors)
	}
	for i, want := range wantCursors {
		if sawCursors[i] != want {
			t.Errorf("page %d cursor = %q, want %q", i, sawCursors[i], want)
		}
	}
	for i, lim := range sawLimits {
		if lim != "1000" {
			t.Errorf("page %d limit = %q, want 1000", i, lim)
		}
	}
}

func TestBaseURLDefault(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	if got := baseURL(); got != defaultBase {
		t.Errorf("baseURL() = %q, want %q", got, defaultBase)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "https://example.test")
	if got := baseURL(); got != "https://example.test" {
		t.Errorf("baseURL() = %q, want https://example.test", got)
	}
}
