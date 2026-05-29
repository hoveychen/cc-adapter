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
// The three derived A6 endpoints the extension also calls share A6's t80 header
// set and org-UUID resolution:
//   - A6b session detail   — GET /v1/sessions/{id}                          (15s)
//   - A6c teleport events  — GET /v1/code/sessions/{id}/teleport-events     (20s, paged)
//   - A6d session ingress  — GET /v1/session_ingress/session/{id}           (20s)
//
// The teleport-events pagination is reverse-engineered verbatim from the
// extension's fetchLogsViaCCR (extension.js, build 2.1.156): query params
// {limit: 1000, cursor: <next_cursor>}, response root carries .data (an array of
// {payload} events) and .next_cursor; the loop stops when next_cursor is falsy
// and is capped at 100 pages. See teleportPageLimit / teleportMaxPages below.
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/hoveychen/cc-adapter/internal/auth"
)

const defaultBase = "https://api.anthropic.com"

const (
	usageTimeout    = 5 * time.Second
	profileTimeout  = 10 * time.Second
	sessionsTimeout = 15 * time.Second
	// A6b session detail shares A6's 15s budget.
	sessionDetailTimeout = 15 * time.Second
	// A6c teleport events / A6d session ingress both use 20s (confirmed from
	// extension.js: timeout:20000 on both fetchLogsViaCCR and
	// fetchLogsViaSessionIngress).
	teleportTimeout = 20 * time.Second
	ingressTimeout  = 20 * time.Second
)

// Teleport-events pagination constants, confirmed from extension.js
// fetchLogsViaCCR: limit=1000 per page, capped at 100 pages.
const (
	teleportPageLimit = 1000
	teleportMaxPages  = 100
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
	// Match the extension's axios client UA (axios sets "axios/"+VERSION via
	// setHeaderIfUnset; VERSION is 1.9.0 in extension.js) so cc-adapter's own
	// requests don't leak a Go-http-client/1.1 fingerprint.
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "axios/1.9.0")
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

// t80Headers builds the shared "t80"-style header set the A6 family of endpoints
// carries on top of the OAuth headers: the fixed anthropic-version /
// anthropic-beta / Content-Type plus x-organization-uuid. It resolves the
// organization UUID the same way RemoteSessions does — preferring the stored
// credential, falling back to an A3 profile lookup — and is shared by all four
// A6 endpoints (list + the three derived ones).
func t80Headers() (map[string]string, error) {
	orgUUID := loadOrgUUID()
	if orgUUID == "" {
		var err error
		orgUUID, _, err = Profile()
		if err != nil {
			return nil, fmt.Errorf("resolving organization uuid via profile: %w", err)
		}
	}
	return map[string]string{
		"Content-Type":        "application/json",
		"anthropic-version":   "2023-06-01",
		"anthropic-beta":      "ccr-byoc-2025-07-29",
		"x-organization-uuid": orgUUID,
	}, nil
}

// RemoteSessions implements A6: GET {base}/v1/sessions with the t80-style header
// set. Returns the raw JSON body.
func RemoteSessions() ([]byte, error) {
	hdrs, err := t80Headers()
	if err != nil {
		return nil, err
	}
	return doGet("/v1/sessions", hdrs, sessionsTimeout)
}

// SessionDetail implements A6b: GET {base}/v1/sessions/{id} with the t80-style
// header set. A 404 (or any other non-2xx) is not treated as an error — the body
// is returned verbatim, consistent with doGet's passthrough behaviour.
func SessionDetail(id string) ([]byte, error) {
	hdrs, err := t80Headers()
	if err != nil {
		return nil, err
	}
	return doGet("/v1/sessions/"+url.PathEscape(id), hdrs, sessionDetailTimeout)
}

// teleportPage is the response shape for a single teleport-events page. Each
// element of Data carries a Payload (the actual event); NextCursor advances the
// pagination and is empty/absent on the final page. Both names are confirmed
// from extension.js (H.data.data, L.payload, H.data.next_cursor).
type teleportPage struct {
	Data []struct {
		Payload json.RawMessage `json:"payload"`
	} `json:"data"`
	NextCursor string `json:"next_cursor"`
}

// TeleportEvents implements A6c: GET {base}/v1/code/sessions/{id}/teleport-events
// with the t80-style header set, aggregating all pages into a single JSON array
// of event payloads. Pagination is confirmed from extension.js fetchLogsViaCCR:
// query params {limit:1000, cursor:<next_cursor>}, response {data:[{payload}],
// next_cursor}; the loop stops when next_cursor is falsy and is capped at 100
// pages (logging a truncation notice if the cap is hit).
//
// Unlike the raw-passthrough endpoints, a non-2xx page cannot be aggregated, so
// it is surfaced: the first page's body is returned verbatim with an error so
// the caller still sees the server's message (e.g. a 404 body).
func TeleportEvents(id string) ([]byte, error) {
	hdrs, err := t80Headers()
	if err != nil {
		return nil, err
	}

	basePath := "/v1/code/sessions/" + url.PathEscape(id) + "/teleport-events"
	events := []json.RawMessage{}
	cursor := ""

	for page := 0; page < teleportMaxPages; page++ {
		q := url.Values{}
		q.Set("limit", fmt.Sprintf("%d", teleportPageLimit))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		body, err := doGet(basePath+"?"+q.Encode(), hdrs, teleportTimeout)
		if err != nil {
			return nil, err
		}

		var pg teleportPage
		if err := json.Unmarshal(body, &pg); err != nil {
			// A non-2xx (e.g. 404) body won't match the page shape. On the very
			// first page there's nothing aggregated yet, so surface the raw body
			// with the parse error; on a later page, stop and return what we have.
			if page == 0 {
				return body, fmt.Errorf("parsing teleport-events page 0: %w", err)
			}
			break
		}

		for _, e := range pg.Data {
			if len(e.Payload) > 0 {
				events = append(events, e.Payload)
			}
		}

		cursor = pg.NextCursor
		if cursor == "" {
			return json.Marshal(events)
		}
	}

	log.Printf("cc-adapter: teleport-events for %s hit the %d-page cap; results truncated", id, teleportMaxPages)
	return json.Marshal(events)
}

// SessionIngress implements A6d: GET {base}/v1/session_ingress/session/{id} with
// the t80-style header set. The raw body is returned verbatim (a 404 body is not
// an error), consistent with doGet's passthrough behaviour.
func SessionIngress(id string) ([]byte, error) {
	hdrs, err := t80Headers()
	if err != nil {
		return nil, err
	}
	return doGet("/v1/session_ingress/session/"+url.PathEscape(id), hdrs, ingressTimeout)
}
