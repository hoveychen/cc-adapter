// Package auth replicates the VS Code "Claude Code" extension's handling of the
// shared OAuth credentials. The extension reads the same on-disk credential blob
// that the claude CLI writes (~/.claude/.credentials.json, or the macOS keychain
// item "Claude Code-credentials"), derives the account UUID from the access
// token's JWT `sub` claim, refreshes expired tokens against
// platform.claude.com, and attaches the OAuth bearer + anthropic-beta header to
// outbound requests.
//
// The real credential blob (verified against this machine's keychain item) is:
//
//	{
//	  "claudeAiOauth": {
//	    "accessToken":  "sk-ant-oat01-...",
//	    "refreshToken": "sk-ant-ort01-...",
//	    "expiresAt":    1780067435229,            // unix millis
//	    "scopes":       ["user:inference", ...],
//	    "clientId":     "...",
//	    "subscriptionType": "...",
//	    "rateLimitTier":    "..."
//	  },
//	  "organizationUuid": "..."
//	}
package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// oauthClientID is the public client_id the extension/CLI use for the OAuth
// refresh-token grant (constant in the shipped extension).
const oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// refreshScope is the space-delimited scope string sent on refresh.
const refreshScope = "user:inference user:profile user:sessions:claude_code user:mcp_servers user:file_upload"

// tokenEndpoint is the OAuth refresh endpoint.
const tokenEndpoint = "https://platform.claude.com/v1/oauth/token"

// expirySkew is the safety margin: a token is treated as expired this long
// before its real expiry so we never present a token that dies mid-request.
const expirySkew = 60 * time.Second

// Credentials is the parsed `claudeAiOauth` object plus the top-level org id.
type Credentials struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // unix milliseconds
	Scopes           []string `json:"scopes,omitempty"`
	ClientID         string   `json:"clientId,omitempty"`
	SubscriptionType string   `json:"subscriptionType,omitempty"`
	RateLimitTier    string   `json:"rateLimitTier,omitempty"`
	OrganizationUUID string   `json:"-"` // hoisted from the top-level object
}

// credentialFile mirrors the on-disk / keychain JSON layout.
type credentialFile struct {
	ClaudeAiOauth    *Credentials `json:"claudeAiOauth"`
	OrganizationUUID string       `json:"organizationUuid"`
}

// configDir returns $CLAUDE_CONFIG_DIR or ~/.claude.
func configDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// Load reads credentials from $CLAUDE_CONFIG_DIR/.credentials.json. If that file
// is missing or carries no access token, on macOS it falls back to the keychain
// item the extension/CLI use ("Claude Code-credentials"), which stores the same
// JSON blob.
func Load() (*Credentials, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".credentials.json")

	if data, err := os.ReadFile(path); err == nil {
		if creds, perr := parseCredentialBlob(data); perr == nil && creds.AccessToken != "" {
			return creds, nil
		}
	}

	// Fallback: macOS keychain stores the identical JSON blob.
	if runtime.GOOS == "darwin" {
		if data, kerr := keychainCredentials(); kerr == nil {
			if creds, perr := parseCredentialBlob(data); perr == nil && creds.AccessToken != "" {
				return creds, nil
			}
		}
	}

	return nil, fmt.Errorf("no usable credentials at %s (and keychain fallback failed)", path)
}

// keychainCredentials runs the same `security` lookup the extension uses.
func keychainCredentials() ([]byte, error) {
	user := os.Getenv("USER")
	out, err := exec.Command("security", "find-generic-password",
		"-s", "Claude Code-credentials", "-a", user, "-w").Output()
	if err != nil {
		return nil, fmt.Errorf("keychain lookup: %w", err)
	}
	return bytes.TrimSpace(out), nil
}

// parseCredentialBlob unwraps the {"claudeAiOauth":{...}} envelope.
func parseCredentialBlob(data []byte) (*Credentials, error) {
	var f credentialFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.ClaudeAiOauth == nil {
		return nil, fmt.Errorf("credential blob missing claudeAiOauth")
	}
	creds := *f.ClaudeAiOauth
	creds.OrganizationUUID = f.OrganizationUUID
	return &creds, nil
}

// AccountUUID decodes the access token's JWT and returns its `sub` claim. The
// access token is a 3-segment JWT (header.payload.signature); we base64url-decode
// the payload and read `.sub`. Returns "" on any parse failure. This mirrors the
// extension's V80() helper exactly (split on ".", require 3 parts, base64url the
// middle, JSON-parse, return `.sub`).
func AccountUUID(accessToken string) string {
	if accessToken == "" {
		return ""
	}
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64URLDecode(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

// base64URLDecode decodes a base64url segment, tolerating missing padding (JWT
// segments are unpadded).
func base64URLDecode(s string) ([]byte, error) {
	// RawURLEncoding handles the - / _ alphabet without padding.
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	// Some encoders include padding; fall back to the padded variant.
	return base64.URLEncoding.DecodeString(s)
}

// IsExpired reports whether a token with the given expiry (unix milliseconds) is
// expired, applying expirySkew so a token about to die is treated as expired.
func IsExpired(expiresAtMs int64) bool {
	if expiresAtMs == 0 {
		return true
	}
	expiry := time.UnixMilli(expiresAtMs)
	return !time.Now().Add(expirySkew).Before(expiry)
}

// refreshResponse is the OAuth token endpoint's response.
type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"` // seconds
}

// Refresh exchanges a refresh token for a fresh access token against
// platform.claude.com. It returns the new credentials but does NOT persist them
// back to disk/keychain — persistence is the caller's (or the CLI's) concern, and
// writing here would create surprising side effects. The refresh token may be
// reused by the server, in which case we carry the old one forward.
func Refresh(refreshToken string) (*Credentials, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     oauthClientID,
		"scope":         refreshScope,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Match the extension's axios client UA (VERSION 1.9.0); the A4 token refresh
	// goes through the same axios instance as A1-A6, so it must not leak
	// Go-http-client/1.1 either.
	req.Header.Set("User-Agent", "axios/1.9.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth refresh: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("oauth refresh: status %d", resp.StatusCode)
	}

	var rr refreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return nil, fmt.Errorf("oauth refresh: decode: %w", err)
	}
	if rr.AccessToken == "" {
		return nil, fmt.Errorf("oauth refresh: empty access_token in response")
	}

	newRefresh := rr.RefreshToken
	if newRefresh == "" {
		newRefresh = refreshToken // server reused the old one
	}
	return &Credentials{
		AccessToken:  rr.AccessToken,
		RefreshToken: newRefresh,
		ExpiresAt:    time.Now().Add(time.Duration(rr.ExpiresIn) * time.Second).UnixMilli(),
	}, nil
}

// AuthHeaders loads credentials, refreshes them if expired, and returns the
// headers the extension attaches to authenticated requests.
func AuthHeaders() (map[string]string, error) {
	creds, err := Load()
	if err != nil {
		return nil, err
	}
	if IsExpired(creds.ExpiresAt) {
		refreshed, rerr := Refresh(creds.RefreshToken)
		if rerr != nil {
			return nil, fmt.Errorf("refreshing expired token: %w", rerr)
		}
		creds = refreshed
	}
	return map[string]string{
		"Authorization":  "Bearer " + creds.AccessToken,
		"anthropic-beta": "oauth-2025-04-20",
	}, nil
}
