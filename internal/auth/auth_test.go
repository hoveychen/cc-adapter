package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// makeJWT builds an unsigned 3-segment JWT whose payload carries the given sub.
func makeJWT(t *testing.T, sub string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadJSON, _ := json.Marshal(map[string]string{"sub": sub})
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + payload + "." + sig
}

func TestAccountUUID(t *testing.T) {
	tok := makeJWT(t, "test-uuid")
	if got := AccountUUID(tok); got != "test-uuid" {
		t.Fatalf("AccountUUID = %q, want %q", got, "test-uuid")
	}
}

func TestAccountUUID_Failures(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"not three parts":  "aaa.bbb",
		"bad base64":       "aaa.!!!notbase64!!!.ccc",
		"payload not json": "aaa." + base64.RawURLEncoding.EncodeToString([]byte("notjson")) + ".ccc",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if got := AccountUUID(tok); got != "" {
				t.Fatalf("AccountUUID(%q) = %q, want empty", tok, got)
			}
		})
	}
}

func TestAccountUUID_NoSubClaim(t *testing.T) {
	// Valid JWT shape but payload has no sub -> empty string.
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"x"}`))
	tok := "h." + payload + ".s"
	if got := AccountUUID(tok); got != "" {
		t.Fatalf("AccountUUID with no sub = %q, want empty", got)
	}
}

func TestIsExpired(t *testing.T) {
	now := time.Now()

	// Already past.
	if !IsExpired(now.Add(-time.Hour).UnixMilli()) {
		t.Error("expected expired for a past timestamp")
	}
	// Within the skew window -> treated as expired.
	if !IsExpired(now.Add(30 * time.Second).UnixMilli()) {
		t.Error("expected expired for a timestamp inside the 60s skew window")
	}
	// Comfortably in the future -> not expired.
	if IsExpired(now.Add(10 * time.Minute).UnixMilli()) {
		t.Error("expected not-expired for a timestamp well in the future")
	}
	// Zero -> expired.
	if !IsExpired(0) {
		t.Error("expected expired for zero expiry")
	}
}

func TestParseCredentialBlob(t *testing.T) {
	blob := []byte(`{"claudeAiOauth":{"accessToken":"at","refreshToken":"rt","expiresAt":1780067435229,"scopes":["user:inference"]},"organizationUuid":"org-1"}`)
	creds, err := parseCredentialBlob(blob)
	if err != nil {
		t.Fatalf("parseCredentialBlob: %v", err)
	}
	if creds.AccessToken != "at" || creds.RefreshToken != "rt" {
		t.Fatalf("tokens not parsed: %+v", creds)
	}
	if creds.ExpiresAt != 1780067435229 {
		t.Fatalf("expiresAt = %d", creds.ExpiresAt)
	}
	if creds.OrganizationUUID != "org-1" {
		t.Fatalf("org uuid = %q", creds.OrganizationUUID)
	}
}

func TestParseCredentialBlob_MissingOauth(t *testing.T) {
	if _, err := parseCredentialBlob([]byte(`{"organizationUuid":"x"}`)); err == nil {
		t.Fatal("expected error when claudeAiOauth missing")
	}
}
