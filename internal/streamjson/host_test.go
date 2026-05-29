package streamjson

import (
	"strings"
	"testing"
)

// envMap collapses a KEY=VALUE slice into a map, keeping the last value for
// duplicate keys (which is what exec uses for the child's environment).
func envMap(kvs []string) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			m[kv[:eq]] = kv[eq+1:]
		}
	}
	return m
}

func TestBuildEnvOverrides(t *testing.T) {
	t.Setenv("NODE_OPTIONS", "--max-old-space-size=4096")

	h := NewHost(Config{})
	env := envMap(h.buildEnv())

	want := map[string]string{
		"CLAUDE_CODE_ENTRYPOINT":     "claude-vscode",
		"MCP_CONNECTION_NONBLOCKING": "true",
		"CLAUDE_CODE_ENABLE_TASKS":   "0",
		"CLAUDE_AGENT_SDK_VERSION":   "0.3.156",
		"DISABLE_AUTOUPDATER":        "1",
	}
	for k, v := range want {
		if got := env[k]; got != v {
			t.Errorf("env[%q] = %q, want %q", k, got, v)
		}
	}

	if _, ok := env["NODE_OPTIONS"]; ok {
		t.Error("NODE_OPTIONS should be stripped from the child env")
	}
}
