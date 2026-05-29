package cloud

import (
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
