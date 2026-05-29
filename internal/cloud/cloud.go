// Package cloud replicates the three OAuth-authenticated GET endpoints the VS
// Code "Claude Code" extension calls against Anthropic's backend:
//
//   - A2 usage   — GET {base}/api/oauth/usage   (subscription usage / limits)
//   - A3 profile — GET {base}/api/oauth/profile  (account + organization info)
//   - A6 sessions — GET {base}/v1/sessions       (remote/cloud code sessions list)
//
// All three carry the shared OAuth bearer token (+ the oauth anthropic-beta
// header) the auth package manages, refreshing it transparently when expired.
// The base URL is $ANTHROPIC_BASE_URL or https://api.anthropic.com. (The token
// refresh endpoint lives on platform.claude.com, but that is handled entirely
// inside the auth package — these endpoints all hit the api.anthropic.com base.)
//
// Reverse-engineered from the shipped extension:
//   - A2: GET /api/oauth/usage, headers {Content-Type: application/json} plus the
//     OAuth headers, 5s timeout. Only meaningful for Claude subscription users;
//     non-subscription accounts get an error body which we pass through verbatim.
//   - A3: GET /api/oauth/profile, headers {Authorization: Bearer, anthropic-beta:
//     oauth-2025-04-20}, 10s timeout. Response carries .organization.uuid.
//   - A6: GET /v1/sessions, the "t80"-style header set {Authorization: Bearer,
//     Content-Type: application/json, anthropic-version: 2023-06-01,
//     anthropic-beta: ccr-byoc-2025-07-29, x-organization-uuid: <orgUUID>},
//     15s timeout. The org UUID comes from the stored credentials when present,
//     otherwise from a profile (A3) lookup.
//
// TODO: A6 has three derived endpoints the extension also calls that are not
// implemented here yet — GET /v1/sessions/{id}, GET
// /v1/code/sessions/{id}/teleport-events, and GET
// /v1/session_ingress/session/{id}. This batch implements only the /v1/sessions
// list.
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/hoveychen/cc-adapter/internal/auth"
)

const defaultBase = "https://api.anthropic.com"

const (
	usageTimeout    = 5 * time.Second
	profileTimeout  = 10 * time.Second
	sessionsTimeout = 15 * time.Second
)

// httpClient is the client used for all cloud requests; overridable in tests via
// SetHTTPClient.
var httpClient = &http.Client{}

// authHeaders returns the OAuth headers (Authorization + anthropic-beta) the
// requests carry. It is a package var so tests can inject fake headers without
// touching the keychain / network. Defaults to auth.AuthHeaders, which loads the
// shared credentials and refreshes an expired token.
var authHeaders = auth.AuthHeaders

// loadOrgUUID returns the organization UUID stored alongside the credentials, or
// "" if none. It is a package var so tests can inject one without the keychain.
var loadOrgUUID = func() string {
	creds, err := auth.Load()
	if err != nil {
		return ""
	}
	return creds.OrganizationUUID
}

// SetHTTPClient overrides the HTTP client used for cloud requests (test seam).
func SetHTTPClient(c *http.Client) { httpClient = c }

// SetAuthHeaders overrides the OAuth-header source (test seam); pass nil to
// restore the default auth.AuthHeaders behaviour.
func SetAuthHeaders(fn func() (map[string]string, error)) {
	if fn == nil {
		authHeaders = auth.AuthHeaders
		return
	}
	authHeaders = fn
}

// SetOrgUUIDLoader overrides the stored-org-UUID source (test seam); pass nil to
// restore the default auth.Load-based behaviour.
func SetOrgUUIDLoader(fn func() string) {
	if fn == nil {
		loadOrgUUID = func() string {
			creds, err := auth.Load()
			if err != nil {
				return ""
			}
			return creds.OrganizationUUID
		}
		return
	}
	loadOrgUUID = fn
}

// baseURL returns $ANTHROPIC_BASE_URL or the production default.
func baseURL() string {
	if b := os.Getenv("ANTHROPIC_BASE_URL"); b != "" {
		return b
	}
	return defaultBase
}

// doGet performs an authenticated GET against {base}{path} with the given extra
// headers (merged on top of the OAuth headers) and timeout, returning the raw
// response body. A non-2xx status is NOT treated as an error here — the body is
// returned as-is so callers/CLIs can surface the server's message verbatim. Only
// transport-level and request-construction failures return an error.
func doGet(path string, extra map[string]string, timeout time.Duration) ([]byte, error) {
	hdrs, err := authHeaders()
	if err != nil {
		return nil, fmt.Errorf("auth headers: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+path, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	return body, nil
}

// Usage implements A2: GET {base}/api/oauth/usage. Returns the raw JSON body.
// Only meaningful for Claude subscription accounts; non-subscription accounts
// receive an error body which is returned verbatim.
func Usage() ([]byte, error) {
	return doGet("/api/oauth/usage", map[string]string{
		"Content-Type": "application/json",
	}, usageTimeout)
}

// Profile implements A3: GET {base}/api/oauth/profile. It returns the parsed
// organization UUID (from .organization.uuid) alongside the raw JSON body. A
// missing/unparsable org UUID is not an error — orgUUID is "" and raw still holds
// the body so callers can inspect it.
func Profile() (orgUUID string, raw []byte, err error) {
	raw, err = doGet("/api/oauth/profile", nil, profileTimeout)
	if err != nil {
		return "", nil, err
	}
	var parsed struct {
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	// Ignore a JSON-parse error: a non-2xx body may not match the shape, but the
	// raw body is still useful to the caller.
	_ = json.Unmarshal(raw, &parsed)
	return parsed.Organization.UUID, raw, nil
}

// RemoteSessions implements A6: GET {base}/v1/sessions with the t80-style header
// set. It ensures an organization UUID is available first (preferring the stored
// credential, falling back to an A3 profile lookup) and sends it as
// x-organization-uuid. Returns the raw JSON body.
func RemoteSessions() ([]byte, error) {
	orgUUID := loadOrgUUID()
	if orgUUID == "" {
		var err error
		orgUUID, _, err = Profile()
		if err != nil {
			return nil, fmt.Errorf("resolving organization uuid via profile: %w", err)
		}
	}
	return doGet("/v1/sessions", map[string]string{
		"Content-Type":        "application/json",
		"anthropic-version":   "2023-06-01",
		"anthropic-beta":      "ccr-byoc-2025-07-29",
		"x-organization-uuid": orgUUID,
	}, sessionsTimeout)
}
