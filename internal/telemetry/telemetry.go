// Package telemetry replicates the VS Code "Claude Code" extension's anonymous
// internal-event reporting (the A1 "event_logging" path). When the extension's
// claude child process fails to spawn or exits unexpectedly it posts a single
// ClaudeCodeInternalEvent to {base}/api/event_logging/v2/batch with no
// Authorization header (the account UUID, when known, rides inside the event
// body's `auth` field instead).
//
// Reverse-engineered from extension.js (v2.1.156), function N80:
//
//	x = {
//	  event_id, event_name: "tengu_vscode_"+name, client_timestamp,
//	  session_id, auth: accountUuid ? {account_uuid} : undefined,
//	  entrypoint: "claude-vscode", client_type: "claude-vscode",
//	  env: {version, platform: process.platform, arch: process.arch},
//	  additional_metadata: base64(JSON({...eventData, ide, extensionVersion})),
//	}
//	POST {base}/api/event_logging/v2/batch
//	     {events:[{event_type:"ClaudeCodeInternalEvent", event_data:x}]}
//	     timeout 5000, headers {Content-Type, x-service-name: "claude-code"}
//
// gated by isTelemetryEnabled && !(CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC ||
// DISABLE_TELEMETRY || DO_NOT_TRACK).
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Constants mirrored from the shipped extension.
const (
	extensionVersion = "2.1.156"
	ideAppName       = "Visual Studio Code"
	ideVersion       = "1.x"
	entrypoint       = "claude-vscode"
	clientType       = "claude-vscode"
	eventNamePrefix  = "tengu_vscode_"
	eventType        = "ClaudeCodeInternalEvent"

	prodBase    = "https://api.anthropic.com"
	stagingBase = "https://api-staging.anthropic.com"

	postTimeout = 5 * time.Second
)

// httpClient is the client used for posting events; overridable in tests.
var httpClient = &http.Client{Timeout: postTimeout}

// logger is the warn sink; overridable, defaults to stderr.
var logger = log.New(os.Stderr, "[telemetry] ", log.LstdFlags)

// SetHTTPClient overrides the HTTP client (test seam).
func SetHTTPClient(c *http.Client) { httpClient = c }

// SetLogger overrides the warn logger.
func SetLogger(l *log.Logger) {
	if l != nil {
		logger = l
	}
}

// telemetryDisabled returns true if any opt-out env var is non-empty.
func telemetryDisabled() bool {
	return os.Getenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC") != "" ||
		os.Getenv("DISABLE_TELEMETRY") != "" ||
		os.Getenv("DO_NOT_TRACK") != ""
}

// baseURL picks staging vs prod the same way the extension does.
func baseURL() string {
	if os.Getenv("ANTHROPIC_BASE_URL") == stagingBase {
		return stagingBase
	}
	return prodBase
}

// nodePlatform maps Go's GOOS to Node's process.platform values.
func nodePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	default:
		// darwin and linux already match Node's values.
		return runtime.GOOS
	}
}

// nodeArch maps Go's GOARCH to Node's process.arch values.
func nodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "ia32"
	default:
		return runtime.GOARCH
	}
}

// uuidV4 generates a random RFC4122 v4 UUID.
func uuidV4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; fall back to a timestamp-derived id.
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xFFFFFFFFFFFF)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// buildEventData assembles the inner `event_data` object (the extension's `x`).
func buildEventData(eventName string, eventData map[string]any, accountUUID string) map[string]any {
	// additional_metadata = base64(JSON(eventData merged with ide+extensionVersion)).
	meta := map[string]any{}
	for k, v := range eventData {
		meta[k] = v
	}
	meta["ide"] = map[string]any{"appName": ideAppName, "version": ideVersion}
	meta["extensionVersion"] = extensionVersion
	metaJSON, _ := json.Marshal(meta)

	x := map[string]any{
		"event_id":            uuidV4(),
		"event_name":          eventNamePrefix + eventName,
		"client_timestamp":    time.Now().Format(time.RFC3339),
		"session_id":          MachineID(),
		"entrypoint":          entrypoint,
		"client_type":         clientType,
		"env":                 map[string]any{"version": extensionVersion, "platform": nodePlatform(), "arch": nodeArch()},
		"additional_metadata": base64.StdEncoding.EncodeToString(metaJSON),
	}
	if accountUUID != "" {
		x["auth"] = map[string]any{"account_uuid": accountUUID}
	}
	return x
}

// PostInternalEvent posts a single anonymous ClaudeCodeInternalEvent, matching
// the extension's logEventDirect -> N80 path. It is best-effort: gated by the
// opt-out env vars, and any failure is logged at warn level without blocking.
func PostInternalEvent(eventName string, eventData map[string]any, accountUUID string) {
	if telemetryDisabled() {
		return
	}

	x := buildEventData(eventName, eventData, accountUUID)
	body, err := json.Marshal(map[string]any{
		"events": []any{
			map[string]any{"event_type": eventType, "event_data": x},
		},
	})
	if err != nil {
		logger.Printf("warn: marshal event: %v", err)
		return
	}

	url := baseURL() + "/api/event_logging/v2/batch"
	ctx, cancel := context.WithTimeout(context.Background(), postTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		logger.Printf("warn: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-service-name", "claude-code")
	// Note: no Authorization header — the account UUID rides in the body.

	resp, err := httpClient.Do(req)
	if err != nil {
		logger.Printf("warn: direct telemetry post failed: %v", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		logger.Printf("warn: direct telemetry post status %d", resp.StatusCode)
	}
}

// --- machine id ---

var (
	machineIDOnce  sync.Once
	machineIDValue string
)

// MachineID returns a stable anonymous machine id used as session_id, mirroring
// vscode.env.machineId. It prefers the real VS Code machineid file; if that is
// unavailable it generates a uuid once and persists it under the Claude config
// dir so it stays stable across runs.
func MachineID() string {
	machineIDOnce.Do(func() {
		machineIDValue = resolveMachineID()
	})
	return machineIDValue
}

func resolveMachineID() string {
	if id := readVSCodeMachineID(); id != "" {
		return id
	}
	// Persisted adapter-local id.
	path := adapterMachineIDPath()
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			if s := string(bytes.TrimSpace(b)); s != "" {
				return s
			}
		}
		id := uuidV4()
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, []byte(id), 0o600)
		return id
	}
	// Last resort: ephemeral (non-persisted) id.
	return uuidV4()
}

// readVSCodeMachineID reads the real VS Code machineid if present.
func readVSCodeMachineID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			filepath.Join(home, "Library", "Application Support", "Code", "machineid"),
		}
	case "linux":
		candidates = []string{
			filepath.Join(home, ".config", "Code", "machineid"),
		}
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			candidates = []string{filepath.Join(appData, "Code", "machineid")}
		}
	}
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			if s := string(bytes.TrimSpace(b)); s != "" {
				return s
			}
		}
	}
	return ""
}

// adapterMachineIDPath is $CLAUDE_CONFIG_DIR/.cc-adapter-machineid (or ~/.claude).
func adapterMachineIDPath() string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, ".cc-adapter-machineid")
}
